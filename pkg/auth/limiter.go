package auth

import (
	"strings"
	"sync"
	"time"
)

// AttemptLimiter is a token bucket per key. Sign-in over REST and over the
// dashboard share one, keyed by email, so guessing at an account is limited
// once however the guesses arrive.
type AttemptLimiter struct {
	mu        sync.Mutex
	every     time.Duration // one token comes back this often
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewAttemptLimiter allows burst attempts at once, then one every interval.
func NewAttemptLimiter(every time.Duration, burst int) *AttemptLimiter {
	return &AttemptLimiter{every: every, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// Allow spends one attempt for key and reports whether it was available. The
// key is compared case-insensitively.
func (l *AttemptLimiter) Allow(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.last))/float64(l.every))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets keys whose bucket has refilled completely, so keys that are
// never seen again do not accumulate. It runs at most once a minute.
func (l *AttemptLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst * float64(l.every))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
}
