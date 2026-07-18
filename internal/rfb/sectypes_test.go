package rfb

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReadSecurityTypeListRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	types := []SecurityType{SecTypeNone, SecTypeVNCAuth, SecTypeVeNCrypt}
	if err := WriteSecurityTypeList(&buf, types); err != nil {
		t.Fatalf("WriteSecurityTypeList: %v", err)
	}
	list, err := ReadSecurityTypeList(&buf)
	if err != nil {
		t.Fatalf("ReadSecurityTypeList: %v", err)
	}
	if list.Empty {
		t.Fatal("expected non-empty list")
	}
	if len(list.Types) != 3 {
		t.Fatalf("got %d types, want 3", len(list.Types))
	}
	for i, want := range types {
		if list.Types[i] != want {
			t.Errorf("type[%d] = %v, want %v", i, list.Types[i], want)
		}
	}
}

func TestReadSecurityTypeListEmpty(t *testing.T) {
	// n=0 followed by a reason string, per RFC 6143 §7.1.2.
	var buf bytes.Buffer
	buf.WriteByte(0)
	if err := WriteReasonString(&buf, "no matching security type"); err != nil {
		t.Fatalf("WriteReasonString: %v", err)
	}

	list, err := ReadSecurityTypeList(&buf)
	if err != nil {
		t.Fatalf("ReadSecurityTypeList: %v", err)
	}
	if !list.Empty {
		t.Fatal("expected Empty=true for n=0")
	}
	if len(list.Types) != 0 {
		t.Errorf("expected no types, got %v", list.Types)
	}

	reason, err := ReadReasonString(&buf)
	if err != nil {
		t.Fatalf("ReadReasonString: %v", err)
	}
	if reason != "no matching security type" {
		t.Errorf("reason = %q", reason)
	}
}

func TestReadSecurityTypeListTruncated(t *testing.T) {
	// n=3 but only 1 byte of types follows.
	buf := bytes.NewReader([]byte{3, 1})
	_, err := ReadSecurityTypeList(buf)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	var terr *ErrTruncated
	if !errors.As(err, &terr) {
		t.Fatalf("error type = %T, want *ErrTruncated", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected wrapped io.ErrUnexpectedEOF, got %v", terr.Err)
	}
}

func TestReadSecurityTypeListTruncatedAtCount(t *testing.T) {
	buf := bytes.NewReader(nil)
	_, err := ReadSecurityTypeList(buf)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected wrapped io.EOF, got %v", err)
	}
}

func TestSelectSecurityTypePrefersVNCAuth(t *testing.T) {
	got, err := SelectSecurityType([]SecurityType{SecTypeNone, SecTypeVNCAuth}, SupportedSecurityTypes)
	if err != nil {
		t.Fatalf("SelectSecurityType: %v", err)
	}
	if got != SecTypeVNCAuth {
		t.Errorf("got %v, want VNCAuth", got)
	}
}

func TestSelectSecurityTypeFallsBackToNone(t *testing.T) {
	got, err := SelectSecurityType([]SecurityType{SecTypeNone}, SupportedSecurityTypes)
	if err != nil {
		t.Fatalf("SelectSecurityType: %v", err)
	}
	if got != SecTypeNone {
		t.Errorf("got %v, want None", got)
	}
}

func TestSelectSecurityTypeUnsupportedOnly(t *testing.T) {
	_, err := SelectSecurityType([]SecurityType{SecTypeVeNCrypt}, SupportedSecurityTypes)
	if err == nil {
		t.Fatal("expected error when only unsupported types offered")
	}
	var nerr *ErrNoSupportedSecurityType
	if !errors.As(err, &nerr) {
		t.Fatalf("error type = %T, want *ErrNoSupportedSecurityType", err)
	}
	if len(nerr.Offered) != 1 || nerr.Offered[0] != SecTypeVeNCrypt {
		t.Errorf("Offered = %v", nerr.Offered)
	}
	if len(nerr.Supported) != len(SupportedSecurityTypes) {
		t.Errorf("Supported = %v", nerr.Supported)
	}
}

func TestSelectSecurityTypeEmptyOffered(t *testing.T) {
	_, err := SelectSecurityType(nil, SupportedSecurityTypes)
	if err == nil {
		t.Fatal("expected error for empty offered list")
	}
}

func TestSecurityTypeStringUnknown(t *testing.T) {
	got := SecurityType(200).String()
	if got != "Unknown(200)" {
		t.Errorf("got %q", got)
	}
}
