package auth

import (
	"testing"
	"time"
)

func newTestLimiter() (*AttemptLimiter, *time.Time) {
	l := NewAttemptLimiter(12*time.Second, 5)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterAllowsABurstThenRefills(t *testing.T) {
	l, now := newTestLimiter()
	for i := 0; i < 5; i++ {
		if !l.Allow("a@example.com") {
			t.Fatalf("attempt %d refused inside the burst", i+1)
		}
	}
	if l.Allow("a@example.com") {
		t.Fatal("the sixth attempt was allowed")
	}
	*now = now.Add(11 * time.Second)
	if l.Allow("a@example.com") {
		t.Error("allowed before a token came back")
	}
	*now = now.Add(2 * time.Second)
	if !l.Allow("a@example.com") {
		t.Error("refused after a token came back")
	}
	if l.Allow("a@example.com") {
		t.Error("one token bought two attempts")
	}
}

// One account's budget is its own, and case or padding does not buy another.
func TestLimiterKeysAreIndependentAndNormalised(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.Allow("a@example.com")
	}
	if l.Allow("  A@Example.COM ") {
		t.Error("a differently written email got a fresh budget")
	}
	if !l.Allow("b@example.com") {
		t.Error("another account was limited by the first")
	}
}

func TestLimiterForgetsIdleKeys(t *testing.T) {
	l, now := newTestLimiter()
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	*now = now.Add(2 * time.Minute)
	l.Allow("d")
	if len(l.buckets) != 1 {
		t.Errorf("%d buckets kept, want only the active one", len(l.buckets))
	}
}
