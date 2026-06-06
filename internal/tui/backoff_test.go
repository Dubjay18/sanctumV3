package tui

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	// Attempt 0: base 1s, cap 60s
	// delay = min(1s * 2^0, 60s) = 1s
	// delay + jitter (up to 250ms jitter) -> [1s, 1.25s]
	d0 := Backoff(0, 1*time.Second, 60*time.Second)
	if d0 < 1*time.Second || d0 > 1250*time.Millisecond {
		t.Errorf("attempt 0 out of bounds: %v", d0)
	}

	// Attempt 3: base 1s, cap 60s
	// delay = min(1s * 2^3, 60s) = 8s
	// delay + jitter (up to 2s jitter) -> [8s, 10s]
	d3 := Backoff(3, 1*time.Second, 60*time.Second)
	if d3 < 8*time.Second || d3 > 10*time.Second {
		t.Errorf("attempt 3 out of bounds: %v", d3)
	}

	// Attempt 10: base 1s, cap 60s
	// delay = min(1s * 2^10, 60s) = min(1024s, 60s) = 60s
	// delay + jitter (up to 15s jitter) -> [60s, 75s]
	d10 := Backoff(10, 1*time.Second, 60*time.Second)
	if d10 < 60*time.Second || d10 > 75*time.Second {
		t.Errorf("attempt 10 out of bounds: %v", d10)
	}
}
