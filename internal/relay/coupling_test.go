package relay

import "testing"

func TestCouplingPolicy_Grant_AmpleHeadroom(t *testing.T) {
	p := CouplingPolicy{Capacity: 8}
	if got := p.Grant(8, 1); got != 1 {
		t.Errorf("Grant(8, 1) = %d, want 1 (full headroom, grant fully)", got)
	}
	if got := p.Grant(6, 3); got != 3 {
		t.Errorf("Grant(6, 3) = %d, want 3 (>= half capacity, grant fully)", got)
	}
}

func TestCouplingPolicy_Grant_LowWaterThrottle(t *testing.T) {
	p := CouplingPolicy{Capacity: 8}
	got := p.Grant(1, 1)
	if got < 0 || got > 1 {
		t.Fatalf("Grant(1, 1) = %d, out of range", got)
	}
	if got != 0 {
		t.Logf("Grant(1, 1) = %d (scaled down, not necessarily 0)", got)
	}

	// A larger requested amount at low remaining should scale down
	// proportionally, not grant the full amount.
	if got := p.Grant(1, 8); got >= 8 {
		t.Errorf("Grant(1, 8) = %d, want less than requested (throttled)", got)
	}
}

func TestCouplingPolicy_Grant_Exhausted(t *testing.T) {
	p := CouplingPolicy{Capacity: 8}
	if got := p.Grant(0, 1); got != 0 {
		t.Errorf("Grant(0, 1) = %d, want 0 (withhold entirely on exhaustion)", got)
	}
	if got := p.Grant(-1, 1); got != 0 {
		t.Errorf("Grant(-1, 1) = %d, want 0", got)
	}
}

func TestCouplingPolicy_Grant_NonPositiveRequested(t *testing.T) {
	p := CouplingPolicy{Capacity: 8}
	if got := p.Grant(8, 0); got != 0 {
		t.Errorf("Grant(8, 0) = %d, want 0", got)
	}
	if got := p.Grant(8, -5); got != 0 {
		t.Errorf("Grant(8, -5) = %d, want 0", got)
	}
}

func TestCouplingPolicy_Grant_ZeroCapacityDoesNotPanic(t *testing.T) {
	p := CouplingPolicy{Capacity: 0}
	// Must not divide by zero; exact value isn't load-bearing here.
	_ = p.Grant(1, 1)
	_ = p.Grant(0, 1)
}

func TestCouplingPolicy_Grant_MonotonicInRemaining(t *testing.T) {
	// Sanity: for a fixed requested amount, granting should never
	// decrease as remaining credit increases.
	p := CouplingPolicy{Capacity: 8}
	prev := -1
	for remaining := 0; remaining <= 8; remaining++ {
		got := p.Grant(remaining, 8)
		if got < prev {
			t.Errorf("Grant(%d, 8) = %d, went down from previous %d (non-monotonic)", remaining, got, prev)
		}
		prev = got
	}
}

func TestDefaultCouplingPolicy_Capacity(t *testing.T) {
	if DefaultCouplingPolicy.Capacity != 8 {
		t.Errorf("DefaultCouplingPolicy.Capacity = %d, want 8 (embed-stream initial credit)", DefaultCouplingPolicy.Capacity)
	}
}

// TestCouplingGrant_NeverZero is the frozen-picture regression: the tcp-relay grant issued per
// relayed frame must always be at least 1, even when embed-stream credit is exhausted (Grant==0) or
// rounds to 0 in the low-water band. Returning 0 drained the tcp-relay window and deadlocked the
// server->browser read with no way to re-grant, freezing the image while input still worked.
func TestCouplingGrant_NeverZero(t *testing.T) {
	p := CouplingPolicy{Capacity: 8}
	cases := []struct {
		remaining int
		hasCredit bool
	}{
		{0, true},   // embed window fully exhausted — Grant returns 0
		{1, true},   // low-water: Grant rounds 1*1/8 to 0
		{2, true},   // low-water
		{8, true},   // ample headroom
		{0, false},  // not a credit-windowed channel (test fake)
	}
	for _, c := range cases {
		if got := couplingGrant(c.remaining, c.hasCredit, p); got < 1 {
			t.Errorf("couplingGrant(remaining=%d, hasCredit=%v) = %d, want >= 1 (never starve the read pipe)", c.remaining, c.hasCredit, got)
		}
	}
}
