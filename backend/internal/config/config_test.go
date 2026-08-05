package config

import (
	"errors"
	"os"
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

func TestAPIKeyRoleDefaultsToViewerWhenUnset(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "test-secret")
	t.Setenv("API_KEY_ROLE", "temporary")
	if err := os.Unsetenv("API_KEY_ROLE"); err != nil {
		t.Fatal(err)
	}
	if role := Load().APIKeyRole; role != "viewer" {
		t.Fatalf("default APIKeyRole = %q, want viewer", role)
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

func TestModelProbeConfigurationIsOptInAndBounded(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "test-secret")
	t.Setenv("MODEL_PROBE_ENABLED", "true")
	t.Setenv("MODEL_PROBE_API_KEY", "dedicated-probe-key")
	t.Setenv("MODEL_PROBE_MODELS", "gpt-test,text-embedding-test,gpt-test")
	t.Setenv("MODEL_PROBE_CAPABILITY_MAP", "gpt-test=responses,text-embedding-test=embeddings,broken")
	t.Setenv("MODEL_PROBE_INTERVAL_SECONDS", "5")
	t.Setenv("MODEL_PROBE_TIMEOUT_SECONDS", "999")
	t.Setenv("MODEL_PROBE_MAX_CONCURRENCY", "99")
	t.Setenv("MODEL_PROBE_DAILY_REQUEST_BUDGET", "0")

	loaded := Load()
	if !loaded.ModelProbeEnabled || loaded.ModelProbeAPIKey != "dedicated-probe-key" {
		t.Fatalf("model probe opt-in configuration was not loaded: %+v", loaded)
	}
	if len(loaded.ModelProbeModels) != 2 || loaded.ModelProbeCapabilityMap["gpt-test"] != "responses" {
		t.Fatalf("model probe allowlist/mapping = %#v / %#v", loaded.ModelProbeModels, loaded.ModelProbeCapabilityMap)
	}
	if loaded.ModelProbeInterval != 5*time.Minute || loaded.ModelProbeTimeout != 20*time.Second ||
		loaded.ModelProbeMaxConcurrency != 2 || loaded.ModelProbeDailyRequestBudget != 500 {
		t.Fatalf("model probe bounds were not restored: interval=%s timeout=%s concurrency=%d budget=%d",
			loaded.ModelProbeInterval, loaded.ModelProbeTimeout, loaded.ModelProbeMaxConcurrency, loaded.ModelProbeDailyRequestBudget)
	}
}

func TestValidateSecurityRejectsCredentialReuseAcrossTrustBoundaries(t *testing.T) {
	base := &Config{
		APIKey: "viewer-key", AdminPassword: "admin-password", JWTSecretKey: "jwt-secret",
		NewAPIAdminAccessToken: "newapi-admin", ModelProbeAPIKey: "probe-key", ObservabilityToken: "metrics-key",
	}
	if err := base.ValidateSecurity(); err != nil {
		t.Fatalf("distinct credentials rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		left   string
		right  string
	}{
		{name: "viewer key becomes admin password", mutate: func(c *Config) { c.AdminPassword = c.APIKey }, left: "API_KEY", right: "ADMIN_PASSWORD"},
		{name: "probe reuses NewAPI admin", mutate: func(c *Config) { c.ModelProbeAPIKey = c.NewAPIAdminAccessToken }, left: "NEWAPI_ADMIN_ACCESS_TOKEN", right: "MODEL_PROBE_API_KEY"},
		{name: "JWT secret reused for metrics", mutate: func(c *Config) { c.ObservabilityToken = c.JWTSecretKey }, left: "JWT_SECRET_KEY", right: "OBSERVABILITY_TOKEN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *base
			test.mutate(&copy)
			err := copy.ValidateSecurity()
			if err == nil || !strings.Contains(err.Error(), test.left) || !strings.Contains(err.Error(), test.right) {
				t.Fatalf("reuse error = %v, want names %s/%s", err, test.left, test.right)
			}
			for _, secret := range []string{copy.APIKey, copy.AdminPassword, copy.JWTSecretKey, copy.NewAPIAdminAccessToken, copy.ModelProbeAPIKey, copy.ObservabilityToken} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("validation error leaked credential value: %v", err)
				}
			}
		})
	}
}

func TestValidateSecurityIgnoresUnsetCredentials(t *testing.T) {
	if err := (&Config{JWTSecretKey: "generated-jwt-secret"}).ValidateSecurity(); err != nil {
		t.Fatalf("unset optional credentials rejected: %v", err)
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
