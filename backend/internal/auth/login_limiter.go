package auth

import (
	"container/list"
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/config"
)

const maxTrackedLoginClients = 10000

type loginAttempt struct {
	failures    int
	windowStart time.Time
	nextAllowed time.Time
	lastSeen    time.Time
}

// LoginLimiter is an in-memory, per-client brute-force guard. Failed attempts
// receive exponential backoff; reaching maxAttempts locks the client for the
// remainder of the configured attempt window.
type LoginLimiter struct {
	mu          sync.Mutex
	attempts    map[string]loginAttempt
	recency     *list.List
	positions   map[string]*list.Element
	maxAttempts int
	window      time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
	now         func() time.Time
	operations  uint64
}

func NewLoginLimiter(maxAttempts int, window, baseBackoff, maxBackoff time.Duration) *LoginLimiter {
	if maxAttempts < 1 {
		maxAttempts = 8
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	if baseBackoff <= 0 {
		baseBackoff = 500 * time.Millisecond
	}
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	if maxBackoff < baseBackoff {
		maxBackoff = baseBackoff
	}
	return &LoginLimiter{
		attempts:    make(map[string]loginAttempt),
		recency:     list.New(),
		positions:   make(map[string]*list.Element),
		maxAttempts: maxAttempts,
		window:      window,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		now:         time.Now,
	}
}

// Reserve atomically checks the current limit and consumes one login attempt.
// Reserving before password verification closes the gap where many concurrent
// requests could all pass a read-only Allow check before any failure was
// recorded. A successful login calls Reset; every other outcome keeps the
// reservation and its computed backoff.
func (l *LoginLimiter) Reserve(key string) (bool, time.Duration) {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)
	attempt, ok := l.attempts[key]
	if ok && now.Sub(attempt.windowStart) >= l.window {
		l.removeAttemptLocked(key)
		ok = false
	}
	if ok && now.Before(attempt.nextAllowed) {
		return false, attempt.nextAllowed.Sub(now)
	}
	if !ok {
		if !l.makeRoomLocked(now) {
			return false, l.maxBackoff
		}
		attempt = loginAttempt{windowStart: now, lastSeen: now}
	}
	attempt.failures++
	attempt.lastSeen = now

	delay := l.baseBackoff
	for i := 1; i < attempt.failures && delay < l.maxBackoff; i++ {
		if delay > l.maxBackoff/2 {
			delay = l.maxBackoff
			break
		}
		delay *= 2
	}
	if delay > l.maxBackoff {
		delay = l.maxBackoff
	}
	attempt.nextAllowed = now.Add(delay)
	if attempt.failures >= l.maxAttempts {
		attempt.nextAllowed = attempt.windowStart.Add(l.window)
		if attempt.nextAllowed.Before(now) {
			attempt.nextAllowed = now.Add(l.maxBackoff)
		}
	}
	l.recordAttemptLocked(key, attempt)
	return true, attempt.nextAllowed.Sub(now)
}

func (l *LoginLimiter) Reset(key string) {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	l.removeAttemptLocked(key)
	l.mu.Unlock()
}

func (l *LoginLimiter) cleanupLocked(now time.Time) {
	l.operations++
	if l.operations%128 != 0 {
		return
	}
	l.removeExpiredLocked(now)
}

func (l *LoginLimiter) removeExpiredLocked(now time.Time) {
	// Entries are kept in expiry order and an entry's expiry never changes during
	// its window. Stop as soon as the earliest entry is still active instead of
	// scanning every tracked client.
	for l.recency != nil {
		front := l.recency.Front()
		if front == nil {
			return
		}
		key, _ := front.Value.(string)
		attempt, ok := l.attempts[key]
		if !ok {
			l.recency.Remove(front)
			delete(l.positions, key)
			continue
		}
		if !l.attemptExpiredLocked(attempt, now) {
			return
		}
		l.removeAttemptLocked(key)
	}
}

func (l *LoginLimiter) attemptExpiredLocked(attempt loginAttempt, now time.Time) bool {
	expiresAt := l.attemptExpiryLocked(attempt)
	return !expiresAt.IsZero() && !now.Before(expiresAt)
}

func (l *LoginLimiter) attemptExpiryLocked(attempt loginAttempt) time.Time {
	windowStart := attempt.windowStart
	if windowStart.IsZero() {
		windowStart = attempt.lastSeen
	}
	if windowStart.IsZero() {
		return time.Time{}
	}
	return windowStart.Add(l.window)
}

func (l *LoginLimiter) makeRoomLocked(now time.Time) bool {
	if len(l.attempts) < maxTrackedLoginClients {
		return true
	}
	// Capacity pressure must never evict an active client. The ordered expiry
	// index removes only the due prefix, so unseen-key traffic cannot turn a
	// full limiter into an O(n) scan or silently unlock an active client.
	l.removeExpiredLocked(now)
	return len(l.attempts) < maxTrackedLoginClients
}

func (l *LoginLimiter) recordAttemptLocked(key string, attempt loginAttempt) {
	if l.attempts == nil {
		l.attempts = make(map[string]loginAttempt)
	}
	if l.recency == nil {
		l.recency = list.New()
	}
	if l.positions == nil {
		l.positions = make(map[string]*list.Element)
	}
	l.attempts[key] = attempt
	if _, ok := l.positions[key]; ok {
		// windowStart (and therefore expiry) is immutable until this key is
		// removed, so an update must retain its position in expiry order.
		return
	}

	// Wall-clock corrections and deterministic tests can introduce an expiry
	// earlier than the current tail. The common monotonic case remains O(1);
	// only an out-of-order insertion walks backward to preserve the invariant.
	expiresAt := l.attemptExpiryLocked(attempt)
	for element := l.recency.Back(); element != nil; element = element.Prev() {
		existingKey, _ := element.Value.(string)
		existing, ok := l.attempts[existingKey]
		if !ok {
			continue
		}
		existingExpiry := l.attemptExpiryLocked(existing)
		if existingExpiry.IsZero() || expiresAt.IsZero() || !expiresAt.Before(existingExpiry) {
			l.positions[key] = l.recency.InsertAfter(key, element)
			return
		}
	}
	l.positions[key] = l.recency.PushFront(key)
}

func (l *LoginLimiter) removeAttemptLocked(key string) {
	delete(l.attempts, key)
	if position, ok := l.positions[key]; ok {
		if l.recency != nil {
			l.recency.Remove(position)
		}
		delete(l.positions, key)
	}
}

var (
	defaultLoginLimiterMu  sync.Mutex
	defaultLoginLimiterCfg *config.Config
	defaultLoginLimiter    *LoginLimiter
)

func configuredLoginLimiter() *LoginLimiter {
	cfg := config.Get()
	defaultLoginLimiterMu.Lock()
	defer defaultLoginLimiterMu.Unlock()
	if defaultLoginLimiter == nil || defaultLoginLimiterCfg != cfg {
		defaultLoginLimiter = NewLoginLimiter(
			cfg.LoginMaxAttempts,
			cfg.LoginAttemptWindow,
			cfg.LoginBackoffBase,
			cfg.LoginBackoffMax,
		)
		defaultLoginLimiterCfg = cfg
	}
	return defaultLoginLimiter
}

func ReserveLoginAttempt(key string) (bool, time.Duration) {
	return configuredLoginLimiter().Reserve(key)
}

func ResetLoginFailures(key string) {
	configuredLoginLimiter().Reset(key)
}
