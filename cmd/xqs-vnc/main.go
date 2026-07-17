// Command xqs-vnc is the plugin process entry point. This file is the
// composition root only: it wires os.Stdin/os.Stdout through the frame
// codec, builds a Dispatcher routing channelId 0 to the lifecycle
// handler, and runs the blocking read loop until stdin closes or a
// protocol violation occurs. Real logic lives in internal/lifecycle and
// internal/ipc; see docs/superpowers/specs/2026-07-16-vnc-plugin-design.md
// §2 ("main.go: Композиционный корень. Только проводка. ≤80 строк.").
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/lifecycle"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(stdin io.Reader, stdout, stderr io.Writer) int {
	dec := ipc.NewDecoder(stdin)
	enc := ipc.NewEncoder(stdout)
	h := lifecycle.NewHandler(enc)
	disp := ipc.NewDispatcher(h, nil)
	ctx := context.Background()

	for {
		frame, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			fmt.Fprintln(stderr, "xqs-vnc: fatal: "+redactedErr(err))
			return 1
		}

		if err := disp.Dispatch(ctx, frame); err != nil {
			fmt.Fprintln(stderr, "xqs-vnc: fatal: "+redactedErr(err))
			return 1
		}

		select {
		case <-h.ShutdownRequested():
			return 0
		default:
		}
	}
}

// redactedErr renders an error for the log without risking a raw
// JSON-RPC payload (which could carry a session's connection fields)
// ending up verbatim in the message; codec/dispatch errors here are
// structural (protocol violations), never payload contents, so this is
// a defensive no-op today but keeps the log-safety invariant local to
// one place per internal/ipc/redact.go's rationale.
func redactedErr(err error) string {
	return err.Error()
}
