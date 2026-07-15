package database

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	_ "modernc.org/sqlite"
)

func TestInitLogDBFailureExposesUnsafeFallback(t *testing.T) {
	mainDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	mgr = &Manager{DB: mainDB, IsPG: false}
	logMgr = nil
	setLogSourceStatus(LogSourceStatus{Mode: LogSourceModeUnknown})
	t.Cleanup(func() {
		_ = mainDB.Close()
		mgr = nil
		logMgr = nil
		setLogSourceStatus(LogSourceStatus{Mode: LogSourceModeUnknown})
	})

	cfg := &config.Config{
		SQLDSN:            "main-database",
		DatabaseEngine:    config.MySQL,
		LogSQLDSN:         "postgres://%",
		LogDatabaseEngine: config.PostgreSQL,
	}
	if err := initLogDB(cfg, 1, 1); err != nil {
		t.Fatalf("log initialization should keep read-only fallback available: %v", err)
	}

	status := GetLogSourceStatus()
	if status.Mode != LogSourceModeFallback || !status.Configured || !status.UsingFallback || status.Healthy {
		t.Fatalf("unexpected fallback status: %+v", status)
	}
	if status.LastError == "" {
		t.Fatal("expected the dedicated log connection error to be exposed")
	}
	if status.SafeForDestructiveReads() {
		t.Fatal("configured log fallback must never be safe for destructive reads")
	}
	if logMgr != mgr {
		t.Fatal("read-only log queries should retain the explicit main-database fallback")
	}
}
