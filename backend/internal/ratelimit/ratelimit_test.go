package ratelimit

// Unit locks for the per-IP token bucket (AUD-004): burst exhaustion, key
// isolation (one noisy client never starves another), janitor eviction
// (the map must not grow forever), and the non-positive-input fallbacks
// (a bad env var can never disable the wall).

import (
	"testing"
	"time"
)

func TestBurstExhaustionThenReject(t *testing.T) {
	l := NewIPRateLimiter(60, 5) // 1 rps refill, burst 5 — deterministic in-test
	defer l.Stop()
	for i := 0; i < 5; i++ {
		if !l.Allow("203.0.113.10") {
			t.Fatalf("request %d within burst rejected", i+1)
		}
	}
	if l.Allow("203.0.113.10") {
		t.Fatal("request beyond burst admitted — no wall")
	}
}

func TestKeysAreIsolated(t *testing.T) {
	l := NewIPRateLimiter(60, 5)
	defer l.Stop()
	for i := 0; i < 5; i++ {
		l.Allow("203.0.113.10")
	}
	if !l.Allow("203.0.113.11") {
		t.Fatal("second IP starved by the first's exhaustion")
	}
	if !l.Allow("198.51.100.1") {
		t.Fatal("third IP starved")
	}
}

func TestNonPositiveInputsFallBackToDefaults(t *testing.T) {
	l := NewIPRateLimiter(0, -3)
	defer l.Stop()
	if l.burst != DefaultBurst {
		t.Fatalf("burst = %d want default %d", l.burst, DefaultBurst)
	}
	if l.limit <= 0 {
		t.Fatalf("limit = %v want positive default", l.limit)
	}
}

func TestJanitorEvictsIdleBuckets(t *testing.T) {
	l := NewIPRateLimiter(60, 5)
	defer l.Stop()
	l.maxIdle = 30 * time.Millisecond
	l.janitor = 10 * time.Millisecond // accelerate the loop for the test
	l.Allow("203.0.113.10")
	l.Allow("203.0.113.11")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		n := len(l.buckets)
		l.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("janitor never evicted idle buckets — unbounded map growth in production")
}

func TestStopHaltsJanitor(t *testing.T) {
	l := NewIPRateLimiter(60, 5)
	l.janitor = 10 * time.Millisecond
	l.Stop()
	// A second Stop must not panic (sync.Once guard).
	l.Stop()
}
