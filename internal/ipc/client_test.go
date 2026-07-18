package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeHost reads requests off a pipe fed by a Client's Encoder and lets
// the test script canned responses back via a supplied Client.Deliver
// call, simulating the host side of the control plane without a second
// real process.
type fakeHost struct {
	dec *Decoder
	reqs chan *Request
}

func newFakeHost(r io.Reader) *fakeHost {
	return &fakeHost{dec: NewDecoder(r), reqs: make(chan *Request, 16)}
}

func (h *fakeHost) run() {
	for {
		f, err := h.dec.Decode()
		if err != nil {
			close(h.reqs)
			return
		}
		_, msg, err := DecodeMessage(f.Payload)
		if err != nil {
			continue
		}
		if req, ok := msg.(*Request); ok {
			h.reqs <- req
		}
	}
}

func TestClient_CallReceivesMatchingResponse(t *testing.T) {
	r, w := io.Pipe()
	enc := NewEncoder(w)
	client := NewClient(enc)

	host := newFakeHost(r)
	go host.run()

	go func() {
		req := <-host.reqs
		result, _ := json.Marshal(map[string]any{"pong": "ok"})
		client.Deliver(&Response{ID: req.ID, Result: result})
	}()

	var result struct {
		Pong string `json:"pong"`
	}
	if err := client.Call(context.Background(), "ping", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Pong != "ok" {
		t.Fatalf("result.Pong = %q, want ok", result.Pong)
	}
}

func TestClient_CallSurfacesRPCError(t *testing.T) {
	r, w := io.Pipe()
	enc := NewEncoder(w)
	client := NewClient(enc)

	host := newFakeHost(r)
	go host.run()

	go func() {
		req := <-host.reqs
		client.Deliver(&Response{ID: req.ID, Error: &RPCError{Code: -32000, Message: "boom"}})
	}()

	err := client.Call(context.Background(), "channel.open", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32000 {
		t.Fatalf("code = %d, want -32000", rpcErr.Code)
	}
}

func asRPCError(err error, target **RPCError) bool {
	if e, ok := err.(*RPCError); ok {
		*target = e
		return true
	}
	return false
}

func TestClient_CallTimesOutWithoutResponse(t *testing.T) {
	r, w := io.Pipe()
	enc := NewEncoder(w)
	client := NewClient(enc)

	host := newFakeHost(r)
	go host.run()
	// Never deliver a response.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.Call(ctx, "ping", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Call took %v, expected it to return promptly after the context deadline", elapsed)
	}
}

func TestClient_ConcurrentOutstandingRequestsCorrelateIndependently(t *testing.T) {
	r, w := io.Pipe()
	enc := NewEncoder(w)
	client := NewClient(enc)

	host := newFakeHost(r)
	go host.run()

	go func() {
		for req := range host.reqs {
			var p struct {
				N int `json:"n"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result, _ := json.Marshal(map[string]int{"n": p.N})
			client.Deliver(&Response{ID: req.ID, Result: result})
		}
	}()

	const concurrency = 20
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var result struct {
				N int `json:"n"`
			}
			if err := client.Call(context.Background(), "echo", map[string]int{"n": n}, &result); err != nil {
				errs <- err
				return
			}
			if result.N != n {
				errs <- errFmt("got n=%d, want %d", result.N, n)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClient_NotifySendsNoIDAndDoesNotBlock(t *testing.T) {
	r, w := io.Pipe()
	enc := NewEncoder(w)
	client := NewClient(enc)

	dec := NewDecoder(r)
	done := make(chan struct{})
	go func() {
		f, err := dec.Decode()
		if err != nil {
			t.Errorf("Decode: %v", err)
			close(done)
			return
		}
		_, msg, err := DecodeMessage(f.Payload)
		if err != nil {
			t.Errorf("DecodeMessage: %v", err)
		}
		if _, ok := msg.(*Notification); !ok {
			t.Errorf("expected *Notification, got %T", msg)
		}
		close(done)
	}()

	if err := client.Notify(context.Background(), "channel.close", map[string]any{"channelId": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	<-done
}

func TestClient_DeliverForUnknownIDIsHarmless(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	client := NewClient(NewEncoder(w))

	if client.Deliver(&Response{ID: "nonexistent"}) {
		t.Fatal("expected Deliver to report false for an unknown id")
	}
}

func errFmt(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
