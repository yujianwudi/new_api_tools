package service

import (
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func setMutationSafetyConfigForTest(t *testing.T, cfg *config.Config) {
	t.Helper()
	previous := getMutationSafetyConfig
	getMutationSafetyConfig = func() *config.Config { return cfg }
	t.Cleanup(func() { getMutationSafetyConfig = previous })
}

func TestDirectMutationSafetyFailsClosedForRedisUnknownOrEnabled(t *testing.T) {
	if err := validateNewAPIDirectMutationSafety(&config.Config{}); err == nil || !strings.Contains(err.Error(), "NEWAPI_REDIS_DISABLED=true") {
		t.Fatalf("expected Redis fail-closed error, got %v", err)
	}
	if err := validateNewAPIDirectMutationSafety(&config.Config{NewAPIRedisDisabled: true}); err != nil {
		t.Fatalf("explicitly disabled NewAPI Redis should allow protected direct writes: %v", err)
	}
	if err := validateNewAPIDirectMutationSafety(nil); err == nil || !strings.Contains(err.Error(), "configuration is unavailable") {
		t.Fatalf("missing configuration did not fail closed: %v", err)
	}
}

func TestUnsafeHardDeleteRequiresBothExplicitGuards(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{
			name:    "configuration unavailable",
			cfg:     nil,
			wantErr: "configuration is unavailable",
		},
		{
			name:    "hard-delete opt-in missing",
			cfg:     &config.Config{NewAPIRedisDisabled: true},
			wantErr: "ALLOW_UNSAFE_HARD_DELETE=true",
		},
		{
			name:    "Redis state unsafe",
			cfg:     &config.Config{AllowUnsafeHardDelete: true},
			wantErr: "NEWAPI_REDIS_DISABLED=true",
		},
		{
			name: "both explicitly enabled",
			cfg: &config.Config{
				NewAPIRedisDisabled:   true,
				AllowUnsafeHardDelete: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUnsafeHardDeleteSafety(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestUnsafeBatchDeleteRequiresExplicitRiskAcceptance(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{
			name:    "configuration unavailable",
			cfg:     nil,
			wantErr: "configuration is unavailable",
		},
		{
			name:    "batch-delete opt-in missing",
			cfg:     &config.Config{NewAPIRedisDisabled: true},
			wantErr: "ALLOW_UNSAFE_BATCH_DELETE=true",
		},
		{
			name:    "Redis state unsafe",
			cfg:     &config.Config{AllowUnsafeBatchDelete: true},
			wantErr: "NEWAPI_REDIS_DISABLED=true",
		},
		{
			name: "both explicitly enabled",
			cfg: &config.Config{
				NewAPIRedisDisabled:    true,
				AllowUnsafeBatchDelete: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUnsafeBatchDeleteSafety(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestNewAPIDirectWriteEntrypointsUseMutationGuard(t *testing.T) {
	setMutationSafetyConfigForTest(t, &config.Config{})

	autoGroup := &AutoGroupService{}
	for name, result := range map[string]map[string]interface{}{
		"assign user": autoGroup.assignUser(1, "vip", "admin"),
		"run scan":    autoGroup.RunScan(false),
		"batch move":  autoGroup.BatchMoveUsers([]int64{1}, "vip"),
		"revert user": autoGroup.RevertUser(1),
	} {
		if success, _ := result["success"].(bool); success {
			t.Errorf("%s unexpectedly succeeded", name)
		}
		if message := toString(result["message"]); !strings.Contains(message, "direct user/token mutation blocked") {
			t.Errorf("%s did not return mutation guard error: %q", name, message)
		}
	}

	if _, err := (&IPMonitoringService{}).EnableAllIPRecording(); err == nil || !strings.Contains(err.Error(), "direct user/token mutation blocked") {
		t.Fatalf("EnableAllIPRecording did not use mutation guard: %v", err)
	}
}

func TestAutoGroupAssignUserProtectsRoot(t *testing.T) {
	setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		role INTEGER NOT NULL,
		"group" TEXT,
		deleted_at INTEGER
	)`)
	db.MustExec(`INSERT INTO users (id, username, role, "group") VALUES (1, 'root', 100, 'default')`)

	svc := &AutoGroupService{db: &database.Manager{DB: db, IsPG: false}}
	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatal("root auto-group assignment unexpectedly succeeded")
	}
	if message := toString(result["message"]); !strings.Contains(message, "root") {
		t.Fatalf("expected protected-root message, got %q", message)
	}
	var group string
	if err := db.Get(&group, `SELECT "group" FROM users WHERE id = 1`); err != nil {
		t.Fatalf("read root group: %v", err)
	}
	if group != "default" {
		t.Fatalf("root group changed to %q", group)
	}
}
