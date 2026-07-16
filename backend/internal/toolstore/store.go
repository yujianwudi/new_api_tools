// Package toolstore owns the private, sidecar data used by the operations
// console. It deliberately does not share NewAPI's schema: upstream data is
// observed through adapters, while audit evidence and control-plane state are
// kept in this store.
package toolstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	busyTimeoutMillis = 5000
	defaultPageSize   = 50
	maxPageSize       = 200
)

var (
	ErrNotFound    = errors.New("toolstore: not found")
	ErrInvalid     = errors.New("toolstore: invalid input")
	ErrConflict    = errors.New("toolstore: conflict")
	ErrStoreClosed = errors.New("toolstore: closed")
)

// Store is a small SQLite-backed control-plane store. SQLite connections are
// intentionally serialized: writes are short and this avoids connection-local
// PRAGMA drift while still allowing WAL readers from other processes.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

// HealthStatus is the verifiable state returned by Health.
type HealthStatus struct {
	Path                string
	SchemaVersion       int
	LatestSchemaVersion int
	JournalMode         string
	Synchronous         int
	ForeignKeys         bool
	BusyTimeoutMillis   int
	CheckedAt           time.Time
}

// Init opens or creates a tool store, applies idempotent migrations, and
// configures WAL, foreign-key enforcement and bounded lock waiting.
func Init(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: database path is required", ErrInvalid)
	}

	if path != ":memory:" {
		cleanPath := filepath.Clean(path)
		dir := filepath.Dir(cleanPath)
		if dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create toolstore directory: %w", err)
			}
		}
		f, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create toolstore file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close toolstore file: %w", err)
		}
		if err := os.Chmod(cleanPath, 0o600); err != nil {
			return nil, fmt.Errorf("secure toolstore file: %w", err)
		}
		path = cleanPath
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open toolstore: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	store := &Store{db: db, path: path, now: time.Now}
	if err := store.configure(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("secure migrated toolstore file: %w", err)
		}
	}
	return store, nil
}

// sqliteDSN applies connection-local safety settings whenever database/sql or
// the driver replaces the underlying SQLite connection. The explicit
// configure pass below remains useful for immediate error reporting and for
// setting the persistent WAL mode, but it cannot by itself protect a reconnect.
func sqliteDSN(path string) string {
	query := url.Values{}
	for _, pragma := range []string{
		"foreign_keys(1)",
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"trusted_schema(OFF)",
	} {
		query.Add("_pragma", pragma)
	}
	return path + "?" + query.Encode()
}

func (s *Store) configure(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis),
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA trusted_schema = OFF",
	}
	for _, statement := range pragmas {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure toolstore (%s): %w", statement, err)
		}
	}
	return nil
}

// Close flushes and closes the SQLite handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close toolstore: %w", err)
	}
	return nil
}

// Health performs a live connection check and verifies the connection-local
// safety settings and schema version.
func (s *Store) Health(ctx context.Context) (HealthStatus, error) {
	if s == nil || s.db == nil {
		return HealthStatus{}, ErrStoreClosed
	}
	if err := s.db.PingContext(ctx); err != nil {
		return HealthStatus{}, fmt.Errorf("ping toolstore: %w", err)
	}

	status := HealthStatus{
		Path:                s.path,
		LatestSchemaVersion: latestSchemaVersion,
		CheckedAt:           s.clock(),
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return HealthStatus{}, fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	status.ForeignKeys = foreignKeys == 1
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&status.JournalMode); err != nil {
		return HealthStatus{}, fmt.Errorf("read journal_mode pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&status.Synchronous); err != nil {
		return HealthStatus{}, fmt.Errorf("read synchronous pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&status.BusyTimeoutMillis); err != nil {
		return HealthStatus{}, fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&status.SchemaVersion); err != nil {
		return HealthStatus{}, fmt.Errorf("read schema version: %w", err)
	}
	if !status.ForeignKeys || status.Synchronous != 2 || status.BusyTimeoutMillis < busyTimeoutMillis || status.SchemaVersion != latestSchemaVersion {
		return status, fmt.Errorf("toolstore health invariant failed: foreign_keys=%t synchronous=%d busy_timeout=%d schema=%d/%d",
			status.ForeignKeys, status.Synchronous, status.BusyTimeoutMillis, status.SchemaVersion, latestSchemaVersion)
	}
	if s.path != ":memory:" && !strings.EqualFold(status.JournalMode, "wal") {
		return status, fmt.Errorf("toolstore health invariant failed: journal_mode=%s", status.JournalMode)
	}
	return status, nil
}

func (s *Store) clock() time.Time {
	return s.now().UTC().Truncate(time.Millisecond)
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	if limit > maxPageSize {
		return maxPageSize
	}
	return limit
}

func dbTime(t time.Time) int64 {
	return t.UTC().UnixMilli()
}

func fromDBTime(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func nullableDBTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return dbTime(*value)
}
