package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginLimiterUsesExponentialBackoffAndWindowLock(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(1000, 0)
	limiter.now = func() time.Time { return now }

	if allowed, retry := limiter.Reserve("client"); !allowed || retry != time.Second {
		t.Fatal("first attempt should be allowed")
	}
	if allowed, retry := limiter.Reserve("client"); allowed || retry != time.Second {
		t.Fatalf("attempt should be delayed for 1s, allowed=%v retry=%s", allowed, retry)
	}

	now = now.Add(time.Second)
	if allowed, retry := limiter.Reserve("client"); !allowed || retry != 2*time.Second {
		t.Fatalf("second reservation = allowed=%v retry=%s, want allowed with 2s backoff", allowed, retry)
	}

	now = now.Add(2 * time.Second)
	if allowed, retry := limiter.Reserve("client"); !allowed || retry != 57*time.Second {
		t.Fatalf("third reservation = allowed=%v retry=%s, want allowed with 57s lock", allowed, retry)
	}
	if allowed, _ := limiter.Reserve("client"); allowed {
		t.Fatal("client should remain locked for the attempt window")
	}

	now = time.Unix(1060, 0)
	if allowed, _ := limiter.Reserve("client"); !allowed {
		t.Fatal("client should be allowed after the attempt window")
	}
}

func TestLoginLimiterResetClearsFailures(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	limiter.Reserve("client")
	limiter.Reset("client")
	if allowed, retry := limiter.Reserve("client"); !allowed || retry != time.Second {
		t.Fatalf("reset did not clear limiter: allowed=%v retry=%s", allowed, retry)
	}
}

func TestLoginLimiterPreservesBackoffMaxBaseInvariant(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, 45*time.Second, 30*time.Second)
	now := time.Unix(1200, 0)
	limiter.now = func() time.Time { return now }

	if limiter.maxBackoff != limiter.baseBackoff {
		t.Fatalf("maxBackoff = %s, want baseBackoff %s", limiter.maxBackoff, limiter.baseBackoff)
	}
	if allowed, retry := limiter.Reserve("client"); !allowed || retry != 45*time.Second {
		t.Fatalf("first reservation = allowed=%v retry=%s, want allowed with 45s backoff", allowed, retry)
	}
}

func TestLoginLimiterConcurrentBurstReservesAtomically(t *testing.T) {
	limiter := NewLoginLimiter(8, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(1500, 0)
	limiter.now = func() time.Time { return now }

	const workers = 64
	start := make(chan struct{})
	var allowed atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := limiter.Reserve("same-client"); ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 1 {
		t.Fatalf("concurrent burst allowed %d attempts, want exactly one in flight", got)
	}
}

func TestLoginLimiterSeparatesPeriodicCleanupFromCapacityEviction(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(2000, 0)
	limiter.now = func() time.Time { return now }

	for i := 0; i < maxTrackedLoginClients-1; i++ {
		key := fmt.Sprintf("active-%d", i)
		limiter.recordAttemptLocked(key, loginAttempt{windowStart: now, lastSeen: now})
	}
	limiter.recordAttemptLocked("expired", loginAttempt{
		windowStart: now.Add(-2 * time.Minute),
		lastSeen:    now.Add(-2 * time.Minute),
	})

	for i := 0; i < 127; i++ {
		limiter.cleanupLocked(now)
	}
	if _, exists := limiter.attempts["expired"]; !exists {
		t.Fatal("expiry sweep ran before the 128-operation interval")
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("existing-client checks evicted entries at capacity: %d", len(limiter.attempts))
	}

	limiter.cleanupLocked(now)
	if _, exists := limiter.attempts["expired"]; exists {
		t.Fatal("scheduled expiry sweep did not remove the expired entry")
	}

	limiter.Reserve("new-client-1")
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("map size after filling capacity = %d, want %d", len(limiter.attempts), maxTrackedLoginClients)
	}
	allowed, retry := limiter.Reserve("new-client-2")
	if allowed || retry != limiter.maxBackoff {
		t.Fatalf("full active limiter = allowed=%v retry=%s, want rejected with max backoff", allowed, retry)
	}
	if _, exists := limiter.attempts["new-client-2"]; exists {
		t.Fatal("rejected client was recorded at capacity")
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("map exceeded capacity: %d", len(limiter.attempts))
	}
}

func TestLoginLimiterCapacityNeverEvictsActiveClient(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(3000, 0)
	for i := 0; i < maxTrackedLoginClients; i++ {
		key := fmt.Sprintf("client-%d", i)
		limiter.recordAttemptLocked(key, loginAttempt{
			windowStart: now,
			lastSeen:    now.Add(time.Duration(i) * time.Second),
		})
	}

	if limiter.makeRoomLocked(now) {
		t.Fatal("capacity check made room by evicting an active client")
	}
	for _, key := range []string{"client-0", fmt.Sprintf("client-%d", maxTrackedLoginClients-1)} {
		if _, exists := limiter.attempts[key]; !exists {
			t.Fatalf("active client %q was evicted", key)
		}
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("tracked clients after rejection = %d, want %d", len(limiter.attempts), maxTrackedLoginClients)
	}
	if limiter.recency.Len() != len(limiter.attempts) || len(limiter.positions) != len(limiter.attempts) {
		t.Fatalf("capacity index drifted: recency=%d positions=%d attempts=%d", limiter.recency.Len(), len(limiter.positions), len(limiter.attempts))
	}
}

func TestLoginLimiterCapacityReclaimsOnlyExpiredClient(t *testing.T) {
	limiter := NewLoginLimiter(3, time.Minute, time.Second, 8*time.Second)
	now := time.Unix(4000, 0)
	limiter.now = func() time.Time { return now }

	for i := 0; i < maxTrackedLoginClients-1; i++ {
		key := fmt.Sprintf("active-%d", i)
		limiter.recordAttemptLocked(key, loginAttempt{windowStart: now, lastSeen: now})
	}
	limiter.recordAttemptLocked("expired", loginAttempt{
		windowStart: now.Add(-time.Minute),
		lastSeen:    now.Add(-time.Minute),
	})

	allowed, retry := limiter.Reserve("new-client")
	if !allowed || retry != time.Second {
		t.Fatalf("new client = allowed=%v retry=%s, want reclaimed expired slot", allowed, retry)
	}
	if _, exists := limiter.attempts["expired"]; exists {
		t.Fatal("expired client remained after capacity reclamation")
	}
	if _, exists := limiter.attempts["active-0"]; !exists {
		t.Fatal("active client was evicted instead of expired client")
	}
	if len(limiter.attempts) != maxTrackedLoginClients {
		t.Fatalf("tracked clients = %d, want %d", len(limiter.attempts), maxTrackedLoginClients)
	}
}
