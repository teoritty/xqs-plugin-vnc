package rfb

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestErrUnsupportedVersionMessage(t *testing.T) {
	err := &ErrUnsupportedVersion{Raw: "junk", Reason: "wrong length"}
	if !strings.Contains(err.Error(), "junk") || !strings.Contains(err.Error(), "wrong length") {
		t.Errorf("unexpected message: %s", err.Error())
	}
}

func TestErrNoSupportedSecurityTypeMessage(t *testing.T) {
	err := &ErrNoSupportedSecurityType{
		Offered:   []SecurityType{SecTypeVeNCrypt},
		Supported: SupportedSecurityTypes,
	}
	msg := err.Error()
	if !strings.Contains(msg, "VeNCrypt") {
		t.Errorf("expected offered type in message, got %s", msg)
	}
}

func TestErrSecurityRejectedMessage(t *testing.T) {
	withReason := &ErrSecurityRejected{Reason: "bad password"}
	if !strings.Contains(withReason.Error(), "bad password") {
		t.Errorf("expected reason in message, got %s", withReason.Error())
	}
	noReason := &ErrSecurityRejected{}
	if !strings.Contains(noReason.Error(), "rejected") {
		t.Errorf("expected generic message, got %s", noReason.Error())
	}
}

func TestErrTruncatedUnwrap(t *testing.T) {
	err := &ErrTruncated{Field: "test field", Err: io.ErrUnexpectedEOF}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error("expected Unwrap to expose io.ErrUnexpectedEOF")
	}
	if !strings.Contains(err.Error(), "test field") {
		t.Errorf("expected field name in message, got %s", err.Error())
	}
}

func TestWrapTruncatedNil(t *testing.T) {
	if err := wrapTruncated("field", nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestSecurityTypeStringKnown(t *testing.T) {
	cases := map[SecurityType]string{
		SecTypeInvalid:  "Invalid(0)",
		SecTypeNone:     "None(1)",
		SecTypeVNCAuth:  "VNCAuth(2)",
		SecTypeVeNCrypt: "VeNCrypt(19)",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("SecurityType(%d).String() = %q, want %q", st, got, want)
		}
	}
}

func TestReadVersionLine(t *testing.T) {
	r := bufio.NewReader(bytes.NewBufferString(V38.String()))
	got, err := ReadVersionLine(r)
	if err != nil {
		t.Fatalf("ReadVersionLine: %v", err)
	}
	if got != V38 {
		t.Errorf("got %+v, want %+v", got, V38)
	}
}

func TestReadVersionLineTruncated(t *testing.T) {
	r := bufio.NewReader(bytes.NewBufferString("short"))
	_, err := ReadVersionLine(r)
	if err == nil {
		t.Fatal("expected error")
	}
}

// failingWriter always returns an error, to exercise write-error paths.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("boom")
}

func TestWriteVersionError(t *testing.T) {
	if err := WriteVersion(failingWriter{}, V38); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteSecurityTypeListError(t *testing.T) {
	if err := WriteSecurityTypeList(failingWriter{}, []SecurityType{SecTypeNone}); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteSecurityTypeListTooMany(t *testing.T) {
	types := make([]SecurityType, 256)
	if err := WriteSecurityTypeList(&bytes.Buffer{}, types); err == nil {
		t.Fatal("expected error for too many types")
	}
}

func TestWriteSecurityResultError(t *testing.T) {
	if err := WriteSecurityResult(failingWriter{}, SecurityResultOK); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteSecurityResultFailedError(t *testing.T) {
	if err := WriteSecurityResultFailed(failingWriter{}, "reason"); err == nil {
		t.Fatal("expected error")
	}
}

// writeNTimesFails fails on write attempt number n (1-indexed).
type writeNTimesFails struct {
	n     int
	calls int
}

func (w *writeNTimesFails) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.n {
		return 0, errors.New("boom")
	}
	return len(p), nil
}

func TestWriteReasonStringLengthWriteError(t *testing.T) {
	w := &writeNTimesFails{n: 1}
	if err := WriteReasonString(w, "hi"); err == nil {
		t.Fatal("expected error on length write")
	}
}

func TestWriteReasonStringBodyWriteError(t *testing.T) {
	w := &writeNTimesFails{n: 2}
	if err := WriteReasonString(w, "hi"); err == nil {
		t.Fatal("expected error on body write")
	}
}

func TestWriteSecurityResultFailedReasonError(t *testing.T) {
	w := &writeNTimesFails{n: 2} // result write ok, reason length write fails
	if err := WriteSecurityResultFailed(w, "reason"); err == nil {
		t.Fatal("expected error propagated from reason write")
	}
}
