package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMemoryLimiterEnforcesLimit(t *testing.T) {
	l, err := NewMemoryLimiter(1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var allowed, blocked int
	for i := 0; i < 15; i++ {
		d, err := l.Allow(ctx, "k", 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			allowed++
		} else {
			blocked++
		}
	}
	if allowed != 10 || blocked != 5 {
		t.Errorf("allowed=%d blocked=%d, want 10/5", allowed, blocked)
	}
}

func TestMemoryLimiterReportsRemaining(t *testing.T) {
	l, _ := NewMemoryLimiter(1000)
	ctx := context.Background()

	d, _ := l.Allow(ctx, "k", 3, time.Minute)
	if !d.Allowed || d.Remaining != 2 {
		t.Errorf("first call: allowed=%v remaining=%d, want true/2", d.Allowed, d.Remaining)
	}
	_, _ = l.Allow(ctx, "k", 3, time.Minute)
	_, _ = l.Allow(ctx, "k", 3, time.Minute)

	d, _ = l.Allow(ctx, "k", 3, time.Minute)
	if d.Allowed {
		t.Error("fourth call should be blocked")
	}
	if d.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", d.Remaining)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Errorf("retry_after = %v, want a value within the window", d.RetryAfter)
	}
}

func TestMemoryLimiterIsolatesKeys(t *testing.T) {
	l, _ := NewMemoryLimiter(1000)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if d, _ := l.Allow(ctx, "a", 5, time.Minute); !d.Allowed {
			t.Fatalf("key a was blocked early at %d", i)
		}
	}
	if d, _ := l.Allow(ctx, "a", 5, time.Minute); d.Allowed {
		t.Error("key a should be exhausted")
	}
	// A different key must have its own budget.
	if d, _ := l.Allow(ctx, "b", 5, time.Minute); !d.Allowed {
		t.Error("key b was blocked by key a's traffic")
	}
}

// Rate limit keys come from request data, so an attacker can mint unbounded
// keys. The limiter must not grow without limit.
func TestMemoryLimiterBoundsKeyCardinality(t *testing.T) {
	const maxKeys = 100
	l, err := NewMemoryLimiter(maxKeys)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < maxKeys*10; i++ {
		if _, err := l.Allow(ctx, fmt.Sprintf("attacker-%d", i), 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if n := l.cache.Len(); n > maxKeys {
		t.Errorf("limiter holds %d keys, want at most %d", n, maxKeys)
	}
}

func TestMemoryLimiterWindowRollsForward(t *testing.T) {
	l, _ := NewMemoryLimiter(100)
	ctx := context.Background()

	// Exhaust a very short window.
	for i := 0; i < 3; i++ {
		_, _ = l.Allow(ctx, "k", 3, 40*time.Millisecond)
	}
	if d, _ := l.Allow(ctx, "k", 3, 40*time.Millisecond); d.Allowed {
		t.Fatal("expected the window to be exhausted")
	}

	// After two full windows the budget is clear again.
	time.Sleep(90 * time.Millisecond)
	if d, _ := l.Allow(ctx, "k", 3, 40*time.Millisecond); !d.Allowed {
		t.Error("the window did not roll forward")
	}
}

func TestRegistryReusesMemoryLimiterPerName(t *testing.T) {
	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two routes naming the same middleware must share one set of counters,
	// otherwise the configured limit applies per route rather than per key.
	a, err := r.Limiter("per-ip", "memory", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Limiter("per-ip", "memory", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("the same middleware name produced two independent limiters")
	}

	c, err := r.Limiter("per-user", "memory", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("different middleware names should not share a limiter")
	}
}

func TestRedisLimiterRequiresConfiguration(t *testing.T) {
	r, _ := NewRegistry(nil)
	_, err := r.Limiter("x", "redis", 100, true)
	if err == nil {
		t.Fatal("expected an error when redis is not configured")
	}
	// The message has to say what to do about it.
	if want := "stores.redis"; !contains(err.Error(), want) {
		t.Errorf("error should mention %q, got: %v", want, err)
	}
}

func TestUnknownStoreIsRejected(t *testing.T) {
	r, _ := NewRegistry(nil)
	if _, err := r.Limiter("x", "memcached", 100, true); err == nil {
		t.Error("an unknown store should be refused at build time")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
