package rfb

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestVersionString(t *testing.T) {
	cases := []struct {
		v    Version
		want string
	}{
		{Version{3, 8}, "RFB 003.008\n"},
		{Version{3, 3}, "RFB 003.003\n"},
		{Version{3, 7}, "RFB 003.007\n"},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.want {
			t.Errorf("Version{%d,%d}.String() = %q, want %q", c.v.Major, c.v.Minor, got, c.want)
		}
	}
}

func TestParseVersionRoundTrip(t *testing.T) {
	v := Version{3, 8}
	line := v.String()
	got, err := ParseVersion([]byte(line))
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if got != v {
		t.Errorf("round trip: got %+v, want %+v", got, v)
	}
}

func TestParseVersionMalformed(t *testing.T) {
	cases := []string{
		"RFB 003.008",   // missing newline, wrong length
		"XYZ 003.008\n", // wrong prefix
		"RFB 00A.008\n", // non-digit
		"",
		"short",
	}
	for _, c := range cases {
		if _, err := ParseVersion([]byte(c)); err == nil {
			t.Errorf("ParseVersion(%q) expected error, got nil", c)
		} else {
			var verr *ErrUnsupportedVersion
			if !errors.As(err, &verr) {
				t.Errorf("ParseVersion(%q) error type = %T, want *ErrUnsupportedVersion", c, err)
			}
		}
	}
}

func TestVersionLessAndAtLeast(t *testing.T) {
	v38 := Version{3, 8}
	v33 := Version{3, 3}
	v37 := Version{3, 7}
	v39 := Version{3, 9}
	v40 := Version{4, 0}

	if !v33.Less(v38) {
		t.Error("3.3 should be less than 3.8")
	}
	if !v37.Less(v38) {
		t.Error("3.7 should be less than 3.8")
	}
	if v38.Less(v38) {
		t.Error("3.8 should not be less than 3.8")
	}
	if !v38.AtLeast(v38) {
		t.Error("3.8 should be AtLeast 3.8")
	}
	if v33.AtLeast(v38) {
		t.Error("3.3 should not be AtLeast 3.8")
	}
	if !v39.AtLeast(v38) {
		t.Error("3.9 should be AtLeast 3.8")
	}
	if !v40.AtLeast(v38) {
		t.Error("4.0 should be AtLeast 3.8")
	}
}

func TestReadWriteVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteVersion(&buf, V38); err != nil {
		t.Fatalf("WriteVersion: %v", err)
	}
	got, err := ReadVersion(&buf)
	if err != nil {
		t.Fatalf("ReadVersion: %v", err)
	}
	if got != V38 {
		t.Errorf("got %+v, want %+v", got, V38)
	}
}

func TestReadVersionTruncated(t *testing.T) {
	r := bytes.NewReader([]byte("RFB 003."))
	_, err := ReadVersion(r)
	if err == nil {
		t.Fatal("expected truncation error, got nil")
	}
	var terr *ErrTruncated
	if !errors.As(err, &terr) {
		t.Fatalf("error type = %T, want *ErrTruncated", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected wrapped io.ErrUnexpectedEOF, got %v", terr.Err)
	}
}

func TestReadVersionCleanEOF(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := ReadVersion(r)
	if err == nil {
		t.Fatal("expected truncation error, got nil")
	}
	var terr *ErrTruncated
	if !errors.As(err, &terr) {
		t.Fatalf("error type = %T, want *ErrTruncated", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected wrapped io.EOF for clean early close, got %v", terr.Err)
	}
}
