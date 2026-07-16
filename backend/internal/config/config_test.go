package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestGenerateRandomSecretUsesCSPRNG(t *testing.T) {
	secret := generateRandomSecret(32)
	if len(secret) != 64 {
		t.Fatalf("secret length = %d, want 64", len(secret))
	}
	if strings.HasPrefix(secret, "auto-") {
		t.Fatalf("secret used predictable fallback: %q", secret)
	}
}

func TestGenerateRandomSecretFailsClosedWhenEntropyUnavailable(t *testing.T) {
	original := cryptoRandomRead
	cryptoRandomRead = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	defer func() { cryptoRandomRead = original }()

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("expected CSPRNG failure to stop startup")
		}
	}()
	_ = generateRandomSecret(32)
}

func TestLoadPreservesLoginBackoffMaxBaseInvariant(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "test-secret")
	t.Setenv("LOGIN_BACKOFF_BASE_MS", "60000")
	t.Setenv("LOGIN_BACKOFF_MAX_SECONDS", "30")

	loaded := Load()
	if loaded.LoginBackoffBase != time.Minute {
		t.Fatalf("LoginBackoffBase = %s, want 1m", loaded.LoginBackoffBase)
	}
	if loaded.LoginBackoffMax != loaded.LoginBackoffBase {
		t.Fatalf("LoginBackoffMax = %s, want at least base %s", loaded.LoginBackoffMax, loaded.LoginBackoffBase)
	}
}

func TestAPIKeyRoleIsValidated(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "test-secret")
	t.Setenv("API_KEY_ROLE", "viewer")
	if role := Load().APIKeyRole; role != "viewer" {
		t.Fatalf("APIKeyRole = %q", role)
	}
	t.Setenv("API_KEY_ROLE", "superuser")
	if role := Load().APIKeyRole; role != "" {
		t.Fatalf("invalid APIKeyRole = %q, want fail-closed empty role", role)
	}
	t.Setenv("API_KEY_ROLE", "")
	if role := Load().APIKeyRole; role != "" {
		t.Fatalf("empty APIKeyRole = %q, want fail-closed empty role", role)
	}
}

func TestObservabilityDefaultsAreFailClosedAndBounded(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("JWT_SECRET_KEY", "test-secret")
	t.Setenv("OBSERVABILITY_TOKEN", "")
	t.Setenv("LOG_FRESHNESS_MAX_SECONDS", "1")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("TOOL_STORE_PATH", "")

	loaded := Load()
	if loaded.ObservabilityToken != "" {
		t.Fatal("metrics token must be empty unless explicitly configured")
	}
	if loaded.LogFreshnessMaxAge != 15*time.Minute {
		t.Fatalf("freshness max age = %s, want bounded default", loaded.LogFreshnessMaxAge)
	}
	if loaded.DataDir != dataDir {
		t.Fatalf("data dir = %q, want %q", loaded.DataDir, dataDir)
	}
	if loaded.ToolStorePath != filepath.Join(loaded.DataDir, "control-plane.db") {
		t.Fatalf("tool store path = %q", loaded.ToolStorePath)
	}
}

func TestNormalizeMySQLURLDSNPreservesCredentialsAndOptions(t *testing.T) {
	raw := "mysql://user:p%40ss@[2001:db8::1]:3307/prod%2Ddb?parseTime=true&tls=preferred&timeout=5s&loc=Asia%2FShanghai"
	normalized := normalizeMySQLURLDSN(raw)
	parsed, err := mysqlDriver.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("normalized DSN is not driver-compatible: %v (%s)", err, normalized)
	}
	if parsed.User != "user" || parsed.Passwd != "p@ss" {
		t.Fatalf("credentials changed: user=%q password=%q", parsed.User, parsed.Passwd)
	}
	if parsed.Net != "tcp" || parsed.Addr != "[2001:db8::1]:3307" || parsed.DBName != "prod-db" {
		t.Fatalf("address or database changed: net=%q addr=%q db=%q", parsed.Net, parsed.Addr, parsed.DBName)
	}
	if !parsed.ParseTime || parsed.TLSConfig != "preferred" || parsed.Timeout != 5*time.Second {
		t.Fatalf("query options changed: parseTime=%t tls=%q timeout=%s", parsed.ParseTime, parsed.TLSConfig, parsed.Timeout)
	}
	if parsed.Loc == nil || parsed.Loc.String() != "Asia/Shanghai" {
		t.Fatalf("location option changed: %v", parsed.Loc)
	}
}
