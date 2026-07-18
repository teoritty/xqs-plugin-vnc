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
