package rfb

import (
	"errors"
	"io"
	"testing"
)

func TestFrontshake_FullSuccessAgainstRealNoVNCLikeBrowser(t *testing.T) {
	plugin, browser := pipe(t)

	browserErrCh := make(chan error, 1)
	go func() {
		browserErrCh <- func() error {
			if _, err := ReadVersion(browser); err != nil {
				return err
			}
			if err := WriteVersion(browser, V38); err != nil {
				return err
			}
			list, err := ReadSecurityTypeList(browser)
			if err != nil {
				return err
			}
			if len(list.Types) != 1 || list.Types[0] != SecTypeNone {
				t.Errorf("offered types = %v, want [None]", list.Types)
			}
			if _, err := browser.Write([]byte{byte(SecTypeNone)}); err != nil {
				return err
			}
			result, _, err := ReadSecurityResult(browser, V38)
			if err != nil {
				return err
			}
			if !result.OK() {
				t.Errorf("SecurityResult = %v, want OK", result)
			}
			return nil
		}()
	}()

	withTimeout(t, func() {
		result, err := Frontshake(plugin)
		if err != nil {
			t.Fatalf("Frontshake: %v", err)
		}
		if result == nil {
			t.Fatal("Frontshake returned nil result on success")
		}
	})

	if err := <-browserErrCh; err != nil {
		t.Fatalf("browser goroutine: %v", err)
	}
}

func TestFrontshake_BrowserVersionBelow38FailsFast(t *testing.T) {
	plugin, browser := pipe(t)

	go func() {
		_, _ = ReadVersion(browser) // drain plugin's version line (always valid 3.8)
		_, _ = io.WriteString(browser, "RFB 003.003\n")
	}()

	withTimeout(t, func() {
		_, err := Frontshake(plugin)
		var unsupported *ErrUnsupportedVersion
		if !errors.As(err, &unsupported) {
			t.Fatalf("Frontshake err = %v, want *ErrUnsupportedVersion", err)
		}
	})
}

func TestFrontshake_BrowserSelectsNonNoneIsProtocolViolation(t *testing.T) {
	plugin, browser := pipe(t)

	go func() {
		_, _ = ReadVersion(browser)
		_ = WriteVersion(browser, V38)
		_, _ = ReadSecurityTypeList(browser)
		_, _ = browser.Write([]byte{byte(SecTypeVNCAuth)}) // not offered, not allowed
	}()

	withTimeout(t, func() {
		_, err := Frontshake(plugin)
		if err == nil {
			t.Fatal("Frontshake err = nil, want protocol violation error")
		}
	})
}

func TestFrontshake_DoesNotConsumeBytesAfterSecurityResult(t *testing.T) {
	plugin, browser := pipe(t)

	// The fake browser writes everything a real noVNC would, then extra
	// bytes that belong to ClientInit and beyond. Frontshake must never
	// read those: we verify by having the "real relay" continuation read
	// exactly the sentinel byte immediately after Frontshake returns.
	const clientInitSentinel = 0x01 // "shared" flag byte, arbitrary marker here
	browserDone := make(chan struct{})
	go func() {
		defer close(browserDone)
		_, _ = ReadVersion(browser)
		_ = WriteVersion(browser, V38)
		_, _ = ReadSecurityTypeList(browser)
		_, _ = browser.Write([]byte{byte(SecTypeNone)})
		var result [4]byte
		_, _ = io.ReadFull(browser, result[:])
		// Extra bytes representing ClientInit + beyond, sent only after
		// SecurityResult has been read by the plugin side.
		_, _ = browser.Write([]byte{clientInitSentinel, 0xAA, 0xBB})
	}()

	withTimeout(t, func() {
		result, err := Frontshake(plugin)
		if err != nil {
			t.Fatalf("Frontshake: %v", err)
		}
		if result == nil {
			t.Fatal("Frontshake returned nil result on success")
		}
		// Now read what a relay would read next: it must be exactly the
		// sentinel bytes the fake browser sent after SecurityResult,
		// proving Frontshake consumed nothing beyond SecurityResult.
		// Reading all of them (rather than just one) lets the browser's
		// blocking net.Pipe Write complete so the goroutine can exit.
		var next [3]byte
		if _, err := io.ReadFull(plugin, next[:]); err != nil {
			t.Fatalf("post-handoff read: %v", err)
		}
		if next[0] != clientInitSentinel {
			t.Errorf("first byte after Frontshake = %#x, want %#x (Frontshake over-consumed)", next[0], clientInitSentinel)
		}
	})

	<-browserDone
}
