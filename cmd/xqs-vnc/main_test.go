package main

import (
	"bytes"
	"testing"

	"xqs-plugin-vnc/internal/ipc"
)

// encodeFrame is a tiny test helper mirroring ipc.Encoder for building an
// input stream without pulling in a second writer.
func encodeFrame(t *testing.T, buf *bytes.Buffer, method string, id any) {
	t.Helper()
	payload, err := ipc.EncodeRequest(ipc.Request{ID: id, Method: method})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	enc := ipc.NewEncoder(buf)
	if err := enc.Encode(ipc.KindJSONRPC, 0, payload); err != nil {
		t.Fatalf("Encode: %v", err)
	}
}

// TestRunHandlesInitializePingShutdownThenExits feeds the fixed host
// lifecycle sequence (initialize, ping, shutdown) into run() and checks
// it exits cleanly (code 0) after the shutdown response is written,
// without waiting for stdin to close on its own.
func TestRunHandlesInitializePingShutdownThenExits(t *testing.T) {
	var in bytes.Buffer
	encodeFrame(t, &in, "initialize", float64(1))
	encodeFrame(t, &in, "ping", float64(2))
	encodeFrame(t, &in, "shutdown", float64(3))

	var out, errOut bytes.Buffer
	code := run(&in, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0; stderr=%s", code, errOut.String())
	}

	dec := ipc.NewDecoder(&out)
	var methods []string
	for {
		f, err := dec.Decode()
		if err != nil {
			break
		}
		_, msg, err := ipc.DecodeMessage(f.Payload)
		if err != nil {
			t.Fatalf("DecodeMessage: %v", err)
		}
		resp, ok := msg.(*ipc.Response)
		if !ok {
			t.Fatalf("unexpected message type %T", msg)
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %+v", resp.Error)
		}
		methods = append(methods, string(resp.Result))
	}
	if len(methods) != 3 {
		t.Fatalf("got %d responses, want 3: %v", len(methods), methods)
	}
}

// TestRunExitsCleanlyOnEmptyStdin verifies a stream that closes at a
// frame boundary with no lifecycle traffic at all exits 0, not an error.
func TestRunExitsCleanlyOnEmptyStdin(t *testing.T) {
	var in, out, errOut bytes.Buffer
	code := run(&in, &out, &errOut)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0; stderr=%s", code, errOut.String())
	}
}

// TestRunExitsWithErrorOnProtocolViolation feeds a reserved/unknown kind
// byte on the control plane and checks run() reports failure (exit 1)
// rather than trying to recover, per the codec's fail-fast contract.
func TestRunExitsWithErrorOnProtocolViolation(t *testing.T) {
	var in bytes.Buffer
	hdr := []byte{0, 0, 0, 0, 0x0F, 0, 0, 0, 0} // kind 0x0F is reserved
	in.Write(hdr)

	var out, errOut bytes.Buffer
	code := run(&in, &out, &errOut)
	if code != 1 {
		t.Fatalf("run() exit code = %d, want 1", code)
	}
	if errOut.Len() == 0 {
		t.Error("expected a fatal message on stderr")
	}
}
