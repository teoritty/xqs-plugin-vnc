// This file (coupling.go) implements the credit-window coupling design
// doc §3's "Кредиты — и почему две трубы ведут себя по-разному" /
// "credit.go — оба направления" and §7's edge-case row "Кредит
// embed-stream близок к нулю" require: the plugin's only backpressure
// lever against a VNC server that has no idea the browser side is slow
// is withholding tcp-relay credit grants (which makes the host pause
// reading further bytes from the real VNC TCP socket, per
// docs/plugin-api.md's tcp-relay exhaustion policy).
//
// The policy itself is a pure function of two credit-window numbers, kept
// deliberately separate from pump.go's read/write loop so it is testable
// with fake numbers — no real socket, channel, or goroutine required.
package relay

import "xqs-plugin-vnc/internal/transport"

// CouplingPolicy computes how much tcp-relay credit to grant back to the
// host (i.e. how much further reading from the real VNC TCP socket to
// allow) based on how much outbound send-credit currently remains on the
// embed-stream channel (the plugin's ability to keep pushing
// server->browser bytes toward the host without blocking).
//
// Capacity is embed-stream's full credit window (transport.
// EmbedStreamInitialCredit — 8, per docs/plugin-api.md's Flow control
// table) — the value Remaining() can never exceed in steady state, used
// here only to judge how depleted the window currently is, not as a
// literal ceiling enforced elsewhere.
type CouplingPolicy struct {
	Capacity int
}

// DefaultCouplingPolicy is the policy pump.go uses in production, sized
// to embed-stream's real credit window.
var DefaultCouplingPolicy = CouplingPolicy{Capacity: transport.EmbedStreamInitialCredit}

// Grant decides how much of the requested tcp-relay credit grant to
// actually issue, given how much embed-stream send-credit currently
// remains:
//
//   - remaining <= 0 (embed-stream window fully exhausted): withhold
//     entirely — return 0. The design doc is explicit that this is the
//     point where reading from tcp-relay must stop, so the VNC server's
//     upstream TCP read pauses before the browser-bound pipe would
//     otherwise be forced to buffer unboundedly.
//   - remaining >= half of Capacity: plenty of headroom — grant the full
//     amount requested.
//   - 0 < remaining < half of Capacity: low-water throttle — grant only a
//     fraction of what was requested, scaled by how little headroom is
//     left (remaining/Capacity), so tcp-relay reads slow down
//     progressively as embed-stream credit shrinks, reaching zero grant
//     right as remaining reaches zero, rather than snapping from "full
//     speed" to "stopped" at the last possible instant.
//
// A non-positive Capacity is treated as 1 (degenerate but never divides
// by zero); a non-positive requested amount always yields 0.
// couplingGrant is the tcp-relay grant issued for one server->browser frame just relayed. It
// always returns at least 1 — the credit that frame consumed — plus any read-ahead the coupling
// policy allows on top when embed-stream has headroom.
//
// Returning 0 here is a deadlock: pumpServerToBrowser grants tcp-relay credit ONLY after a read, so
// the moment the policy withholds it to 0 (which it does as soon as embed-stream send-credit hits
// 0 — e.g. a scroll or fullscreen-exit burst the browser is briefly slow to drain) the host's
// tcp-relay window drains, the next read blocks with no path left to re-grant, and the picture
// freezes permanently while input keeps working on the other channel. Backpressure toward the VNC
// server is still enforced by embedCh.Write blocking on embed-stream credit (which pauses this loop,
// and thus the grants, so the host stops draining the real TCP socket); the read pipe just never
// loses the credit it needs to resume once the browser catches up.
//
// hasCredit is false for a channel with no real credit window (test fakes): nothing to couple
// against, so grant the plain 1.
func couplingGrant(remaining int, hasCredit bool, policy CouplingPolicy) int {
	grant := 1
	if hasCredit {
		if extra := policy.Grant(remaining, 1); extra > grant {
			grant = extra
		}
	}
	return grant
}

func (p CouplingPolicy) Grant(remaining, requested int) int {
	if requested <= 0 {
		return 0
	}
	capacity := p.Capacity
	if capacity <= 0 {
		capacity = 1
	}
	if remaining <= 0 {
		return 0
	}
	half := capacity / 2
	if half <= 0 {
		half = 1
	}
	if remaining >= half {
		return requested
	}
	scaled := requested * remaining / capacity
	return scaled
}
