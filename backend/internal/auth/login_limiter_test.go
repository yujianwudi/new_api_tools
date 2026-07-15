package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestLoginLimiterUsesExponentialBackoffAndWindowLock(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(1000, 0)
	limiter.now = func() time.Time { return now }

	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("first attempt should be allowed")
	}
	if got := limiter.RecordFailure("client"); got != time.Second {
		t.Fatalf("first backoff = %s, want 1s", got)
	}
	if allowed, retry := limiter.Allow("client"); allowed || retry != time.Second {
		t.Fatalf("attempt should be delayed for 1s, allowed=%v retry=%s", allowed, retry)
	}

	now = now.Add(time.Second)
	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("attempt should be allowed after first backoff")
	}
	if got := limiter.RecordFailure("client"); got != 2*time.Second {
		t.Fatalf("second backoff = %s, want 2s", got)
	}

	now = now.Add(2 * time.Second)
	if got := limiter.RecordFailure("client"); got != 57*time.Second {
		t.Fatalf("max-attempt window lock = %s, want 57s", got)
	}
	if allowed, _ := limiter.Allow("client"); allowed {
		t.Fatal("client should remain locked for the attempt window")
	}

	now = time.Unix(1060, 0)
	if allowed, _ := limiter.Allow("client"); !allowed {
		t.Fatal("client should be allowed after the attempt window")
	}
}

func TestLoginLimiterResetClearsFailures(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	limiter.RecordFailure("client")
	limiter.Reset("client")
	if allowed, retry := limiter.Allow("client"); !allowed || retry != 0 {
		t.Fatalf("reset did not clear limiter: allowed=%v retry=%s", allowed, retry)
	}
}

func TestLoginLimiterSeparatesPeriodicCleanupFromCapacityEviction(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(2000, 0)
	limiter.now = func() time.Time { return now }

	for i := 0; i < maxTrackedLoginClients-1; i++ {
		key := fmt.Sprintf("active-%d", i)
		limiter.attempts[key] = loginAttempt{windowStart: now, lastSeen: now}
	}
	limiter.attempts["expired"] = loginAttempt{
		windowStart: now.Add(-2 * time.Minute),
		lastSeen:    now.Add(-2 * time.Minute),
	}

	for i := 0; i < 127; i++ {
		if allowed, _ := limiter.Allow("active-0"); !allowed {
			t.Fatal("active client should remain allowed")
		}
	}
	if _, exists := limiter.attempts["expired"]; !exists {
		t.Fatal("expiry sweep ran before the 128-operation interval")
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("existing-client checks evicted entries at capacity: %d", len(limiter.attempts))
	}

	limiter.Allow("active-0")
	if _, exists := limiter.attempts["expired"]; exists {
		t.Fatal("scheduled expiry sweep did not remove the expired entry")
	}

	limiter.RecordFailure("new-client-1")
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("map size after filling capacity = %d, want %d", len(limiter.attempts), maxTrackedLoginClients)
	}
	limiter.RecordFailure("new-client-2")
	if _, exists := limiter.attempts["new-client-2"]; !exists {
		t.Fatal("new client was not recorded after capacity eviction")
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("map exceeded capacity: %d", len(limiter.attempts))
	}
}
