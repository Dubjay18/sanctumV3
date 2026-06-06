package tui

import (
	"math/rand"
	"time"
)

// Backoff computes the exponential backoff duration with 25% jitter.
func Backoff(attempt int, base, cap time.Duration) time.Duration {
	if attempt > 30 {
		attempt = 30 // prevent overflow on 1 << attempt
	}
	delay := base * (1 << attempt)
	if delay > cap || delay < 0 {
		delay = cap
	}

	jitterLimit := delay / 4
	if jitterLimit > 0 {
		delay += time.Duration(rand.Int63n(int64(jitterLimit)))
	}

	return delay
}
