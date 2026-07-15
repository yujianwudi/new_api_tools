package auth

import (
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
	if maxBackoff < baseBackoff {
		maxBackoff = 30 * time.Second
	}
	return &LoginLimiter{
		attempts:    make(map[string]loginAttempt),
		maxAttempts: maxAttempts,
		window:      window,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		now:         time.Now,
	}
}

// Allow reports whether the client may make a login attempt now.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)
	attempt, ok := l.attempts[key]
	if !ok {
		return true, 0
	}
	if now.Sub(attempt.windowStart) >= l.window {
		delete(l.attempts, key)
		return true, 0
	}
	if now.Before(attempt.nextAllowed) {
		return false, attempt.nextAllowed.Sub(now)
	}
	return true, 0
}

// RecordFailure advances the exponential backoff for a client.
func (l *LoginLimiter) RecordFailure(key string) time.Duration {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanupLocked(now)
	attempt, ok := l.attempts[key]
	if !ok {
		l.makeRoomLocked()
	}
	if !ok || now.Sub(attempt.windowStart) >= l.window {
		attempt = loginAttempt{windowStart: now}
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
	l.attempts[key] = attempt
	return attempt.nextAllowed.Sub(now)
}

func (l *LoginLimiter) Reset(key string) {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

func (l *LoginLimiter) cleanupLocked(now time.Time) {
	l.operations++
	if l.operations%128 != 0 {
		return
	}
	for key, attempt := range l.attempts {
		if now.Sub(attempt.lastSeen) >= l.window {
			delete(l.attempts, key)
		}
	}
}

func (l *LoginLimiter) makeRoomLocked() {
	if len(l.attempts) < maxTrackedLoginClients {
		return
	}
	for key := range l.attempts {
		delete(l.attempts, key)
		return
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

func AllowLoginAttempt(key string) (bool, time.Duration) {
	return configuredLoginLimiter().Allow(key)
}

func RecordLoginFailure(key string) time.Duration {
	return configuredLoginLimiter().RecordFailure(key)
}

func ResetLoginFailures(key string) {
	configuredLoginLimiter().Reset(key)
}
