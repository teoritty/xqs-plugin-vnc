package rfb

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestSecurityResultOK(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSecurityResultOK(&buf); err != nil {
		t.Fatalf("WriteSecurityResultOK: %v", err)
	}
	result, reason, err := ReadSecurityResult(&buf, V38)
	if err != nil {
		t.Fatalf("ReadSecurityResult: %v", err)
	}
	if !result.OK() {
		t.Errorf("expected OK result, got %v", result)
	}
	if reason != "" {
		t.Errorf("expected no reason on success, got %q", reason)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no trailing bytes after success result, got %d", buf.Len())
	}
}

func TestSecurityResultFailedWithReason(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSecurityResultFailed(&buf, "authentication failed"); err != nil {
		t.Fatalf("WriteSecurityResultFailed: %v", err)
	}
	result, reason, err := ReadSecurityResult(&buf, V38)
	if err != nil {
		t.Fatalf("ReadSecurityResult: %v", err)
	}
	if result.OK() {
		t.Error("expected failed result")
	}
	if reason != "authentication failed" {
		t.Errorf("reason = %q", reason)
	}
}

func TestSecurityResultFailedPre38NoReason(t *testing.T) {
	var buf bytes.Buffer
	// Pre-3.8: just the 4-byte result, no reason string on the wire.
	if err := WriteSecurityResult(&buf, SecurityResultFailed); err != nil {
		t.Fatalf("WriteSecurityResult: %v", err)
	}
	result, reason, err := ReadSecurityResult(&buf, Version{3, 3})
	if err != nil {
		t.Fatalf("ReadSecurityResult: %v", err)
	}
	if result.OK() {
		t.Error("expected failed result")
	}
	if reason != "" {
		t.Errorf("expected no reason parsed for pre-3.8, got %q", reason)
	}
}

func TestSecurityResultTruncated(t *testing.T) {
	buf := bytes.NewReader([]byte{0, 0})
	_, _, err := ReadSecurityResult(buf, V38)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	var terr *ErrTruncated
	if !errors.As(err, &terr) {
		t.Fatalf("error type = %T, want *ErrTruncated", err)
	}
}

func TestSecurityResultReasonTruncated(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteSecurityResult(&buf, SecurityResultFailed)
	// Claim a reason length but don't provide the length prefix in full.
	buf.Write([]byte{0, 0})
	_, _, err := ReadSecurityResult(&buf, V38)
	if err == nil {
		t.Fatal("expected truncation error reading reason length")
	}
	var terr *ErrTruncated
	if !errors.As(err, &terr) {
		t.Fatalf("error type = %T, want *ErrTruncated", err)
	}
}

func TestSecurityResultReasonBodyTruncated(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteSecurityResult(&buf, SecurityResultFailed)
	// Length says 10 bytes but body is short.
	lenBuf := make([]byte, 4)
	lenBuf[3] = 10
	buf.Write(lenBuf)
	buf.WriteString("abc")
	_, _, err := ReadSecurityResult(&buf, V38)
	if err == nil {
		t.Fatal("expected truncation error reading reason body")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected wrapped io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadReasonStringSanityLimit(t *testing.T) {
	var buf bytes.Buffer
	lenBuf := make([]byte, 4)
	lenBuf[0] = 0xFF // absurdly large claimed length
	buf.Write(lenBuf)
	_, err := ReadReasonString(&buf)
	if err == nil {
		t.Fatal("expected sanity limit error")
	}
}
