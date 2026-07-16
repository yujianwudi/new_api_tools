package toolstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, time.July, 16, 4, 0, 0, 0, time.UTC)

// These literals freeze every migration shipped in v0.5.0. Deriving the
// expected value with migrationChecksum would let an accidental edit update
// both sides of the assertion and silently rewrite deployed history.
var publishedMigrationChecksums = [...]string{
	1: "710bd112549db1aaccbcae84561aa25de75febbbd75937e9a6d817b14c3a0f5a",
	2: "a1a89fbaf2a42e52ecf176942c580fb3f969a303452e8e2b9c684c713a271e91",
	3: "3ff595a9a311f469a1e4c3fcd4f9a94953cd7765a8f9775b0077a2a1158e9325",
	4: "9bb558d10420426eb7adf8558397562aa9c466eee0650c3914ee1e62e98b497e",
	5: "893e236c9ae0bff35164ae5dd85875cbac819e38b5da1f25e56b36bd20499fd0",
	6: "eb327500f42f69382e84e92ae60796150951b3f4f993a676dfb625921f1131d6",
	7: "46bf2630ba99b331e05928bd77d19057a488796918c6c613f728b4e40b10f5af",
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolstore.db")
	store, err := Init(path)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	store.now = func() time.Time { return testNow }
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store, path
}

func TestInitHealthAndIdempotentMigrations(t *testing.T) {
	store, path := newTestStore(t)
	ctx := context.Background()
	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if health.SchemaVersion != latestSchemaVersion || !health.ForeignKeys || health.Synchronous != 2 {
		t.Fatalf("Health() = %+v", health)
	}
	if health.JournalMode != "wal" {
		t.Fatalf("JournalMode = %q, want wal", health.JournalMode)
	}
	if health.BusyTimeoutMillis < busyTimeoutMillis {
		t.Fatalf("BusyTimeoutMillis = %d", health.BusyTimeoutMillis)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("database permissions = %#o, want 0600", got)
		}
	}

	var migrationCount int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != latestSchemaVersion {
		t.Fatalf("migration count = %d, want %d", migrationCount, latestSchemaVersion)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Init(path)
	if err != nil {
		t.Fatalf("second Init() error = %v", err)
	}
	if err := reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != latestSchemaVersion {
		t.Fatalf("migration count after reopen = %d, want %d", migrationCount, latestSchemaVersion)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = nil // cleanup was already handled explicitly.
}

func TestSQLitePragmasSurviveConnectionReplacement(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	store.db.SetMaxIdleConns(0)

	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health() after connection replacement error = %v", err)
	}
	if !health.ForeignKeys || health.Synchronous != 2 || health.BusyTimeoutMillis < busyTimeoutMillis ||
		!strings.EqualFold(health.JournalMode, "wal") {
		t.Fatalf("Health() after connection replacement = %+v", health)
	}
	var synchronous, trustedSchema int
	if err := store.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, "PRAGMA trusted_schema").Scan(&trustedSchema); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 || trustedSchema != 0 {
		t.Fatalf("replacement connection pragmas synchronous=%d trusted_schema=%d, want 2/0",
			synchronous, trustedSchema)
	}
}

func TestHealthRejectsWeakenedSynchronousMode(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); err != nil {
		t.Fatal(err)
	}
	health, err := store.Health(ctx)
	if err == nil || health.Synchronous != 1 {
		t.Fatalf("Health() after weakened synchronous = %+v, %v; want synchronous=1 error", health, err)
	}
}

func TestInitMigratesRiskEventIdempotencyFromVersion5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolstore-v5.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	legacy := &Store{db: db, path: path, now: func() time.Time { return testNow }}
	if err := legacy.configure(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, item := range migrations[:5] {
		applyLegacyMigrationForTest(t, db, item)
	}
	for _, statement := range []string{
		`CREATE TRIGGER schema_migrations_no_update
			BEFORE UPDATE ON schema_migrations BEGIN
				SELECT RAISE(ABORT, 'schema_migrations is append-only');
			END`,
		`CREATE TRIGGER schema_migrations_no_delete
			BEFORE DELETE ON schema_migrations BEGIN
				SELECT RAISE(ABORT, 'schema_migrations is append-only');
			END`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}

	caseResult, err := db.Exec(`INSERT INTO risk_cases(
		case_key, title, subject_type, subject_id, severity, status, assignee,
		summary, opened_at, closed_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		"legacy-risk", "Legacy risk", "user", "42", RiskSeverityHigh, RiskCaseOpen,
		"", "pre-v6 evidence", dbTime(testNow), dbTime(testNow), dbTime(testNow))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	caseID, err := caseResult.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	eventResult, err := db.Exec(`INSERT INTO risk_case_events(
		case_id, event_type, actor, details_json, occurred_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`, caseID, "legacy_event", "risk-engine",
		`{"source":"v5"}`, dbTime(testNow), dbTime(testNow))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	eventID, err := eventResult.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Init(path)
	if err != nil {
		t.Fatalf("Init() v5 upgrade error = %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	health, err := upgraded.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.SchemaVersion != latestSchemaVersion {
		t.Fatalf("schema version after v5 upgrade = %d, want %d", health.SchemaVersion, latestSchemaVersion)
	}
	legacyEvent, err := upgraded.GetRiskCaseEvent(context.Background(), eventID)
	if err != nil {
		t.Fatalf("read preserved v5 event: %v", err)
	}
	if legacyEvent.EventType != "legacy_event" || legacyEvent.IdempotencyKey != "" {
		t.Fatalf("preserved v5 event = %+v", legacyEvent)
	}

	input := RiskCaseEventInput{
		CaseID: caseID, EventType: "post_upgrade", Actor: "risk-engine",
		DetailsJSON: json.RawMessage(`{"source":"v6"}`), IdempotencyKey: "event-after-v5-upgrade",
	}
	first, err := upgraded.AppendRiskCaseEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := upgraded.AppendRiskCaseEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != first.ID {
		t.Fatalf("post-upgrade retry ID = %d, want %d", retry.ID, first.ID)
	}
	for _, item := range migrations {
		var checksum string
		if err := upgraded.db.QueryRow(
			"SELECT checksum FROM schema_migrations WHERE version = ?", item.version).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		if checksum != publishedMigrationChecksums[item.version] {
			t.Fatalf("migration %d checksum = %q, want published golden %q",
				item.version, checksum, publishedMigrationChecksums[item.version])
		}
	}
	if _, err := upgraded.db.Exec("UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1"); err == nil {
		t.Fatal("backfilled migration ledger update unexpectedly succeeded")
	}
}

func TestPublishedMigrationChecksumsAreGolden(t *testing.T) {
	if len(publishedMigrationChecksums)-1 != latestSchemaVersion {
		t.Fatalf("published checksum count = %d, want %d", len(publishedMigrationChecksums)-1, latestSchemaVersion)
	}
	for _, item := range migrations {
		if got, want := migrationChecksum(item), publishedMigrationChecksums[item.version]; got != want {
			t.Errorf("migration %d checksum = %q, want literal %q", item.version, got, want)
		}
	}
}

func TestInitRejectsPreexistingMigrationObjectWithoutLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preexisting-object.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE operation_audit (id INTEGER PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Init(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "apply migration 1") {
		t.Fatalf("Init() error = %v, want strict migration collision", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed strict migration recorded %d ledger rows", count)
	}
}

func TestInitRejectsTamperedAppliedSchemaObject(t *testing.T) {
	store, path := newTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = nil
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER operation_audit_no_update`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER operation_audit_no_update
		BEFORE UPDATE ON operation_audit BEGIN SELECT 1; END`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Init(path)
	if reopened != nil {
		_ = reopened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), `schema object "operation_audit_no_update" is incompatible`) {
		t.Fatalf("Init() error = %v, want tampered trigger rejection", err)
	}
}

func TestInitRejectsUnexpectedTriggersOnManagedTables(t *testing.T) {
	tests := []struct {
		name      string
		tableName string
	}{
		{name: "migration table", tableName: "risk_cases"},
		{name: "migration ledger", tableName: "schema_migrations"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, path := newTestStore(t)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store.db = nil
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			triggerName := "unexpected_" + tt.tableName + "_trigger"
			if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER %s
				AFTER INSERT ON %s BEGIN SELECT 1; END`, triggerName, tt.tableName)); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Init(path)
			if reopened != nil {
				_ = reopened.Close()
			}
			want := fmt.Sprintf("toolstore managed table %q has unexpected trigger %q", tt.tableName, triggerName)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Init() error = %v, want %q", err, want)
			}
		})
	}
}

func TestInitRepairsMigrationLedgerProtectionTransactionally(t *testing.T) {
	store, path := newTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = nil
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER schema_migrations_no_update`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER schema_migrations_no_update
		BEFORE UPDATE ON schema_migrations BEGIN SELECT 1; END`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := Init(path)
	if err != nil {
		t.Fatalf("Init() repair error = %v", err)
	}
	defer repaired.Close()
	if _, err := repaired.db.Exec("UPDATE schema_migrations SET name = 'tampered' WHERE version = 1"); err == nil {
		t.Fatal("rebuilt migration ledger update protection did not fire")
	}
	if err := validateMigrationLedgerProtection(context.Background(), repaired.db); err != nil {
		t.Fatalf("rebuilt migration ledger protection = %v", err)
	}
}

func TestInitRejectsLookalikeMigrationLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lookalike-ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL DEFAULT '',
		applied_at INTEGER NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Init(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "migration ledger table definition is incompatible") {
		t.Fatalf("Init() error = %v, want lookalike ledger rejection", err)
	}
}

func TestInitRejectsIncompatibleMigrationLedger(t *testing.T) {
	tests := []struct {
		name    string
		entries []migrationLedgerFixture
		want    string
	}{
		{
			name: "version six with missing older record",
			entries: []migrationLedgerFixture{
				{1, migrations[0].name, ""},
				{2, migrations[1].name, ""},
				{4, migrations[3].name, ""},
				{5, migrations[4].name, ""},
				{6, migrations[5].name, ""},
			},
			want: "expected version 3, found 4",
		},
		{
			name: "version six with wrong older name",
			entries: []migrationLedgerFixture{
				{1, migrations[0].name, ""},
				{2, migrations[1].name, ""},
				{3, "support notes rewritten", ""},
				{4, migrations[3].name, ""},
				{5, migrations[4].name, ""},
				{6, migrations[5].name, ""},
			},
			want: "name mismatch at version 3",
		},
		{
			name: "recorded checksum does not match immutable migration",
			entries: []migrationLedgerFixture{
				{1, migrations[0].name, "tampered"},
			},
			want: "checksum mismatch at version 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "incompatible-ledger.db")
			createMigrationLedgerFixture(t, path, tt.entries)
			store, err := Init(path)
			if store != nil {
				_ = store.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Init() error = %v, want substring %q", err, tt.want)
			}
		})
	}

}

func TestMigrationChecksumBackfillRollsBackAndRestoresProtectionOnInvalidLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-legacy-ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, applied_at)
		VALUES (1, 'rewritten operation audit', ?)`, dbTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, statement := range migrationLedgerProtectionStatements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Init(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "name mismatch at version 1") {
		t.Fatalf("Init() error = %v, want migration name mismatch", err)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns, err := migrationLedgerColumns(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := columns["checksum"]; exists {
		t.Fatal("checksum column remained after failed transactional backfill")
	}
	if _, err := db.Exec("UPDATE schema_migrations SET name = 'tampered' WHERE version = 1"); err == nil {
		t.Fatal("legacy append-only protection was not restored after rollback")
	}
	var triggerCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name IN ('schema_migrations_no_update', 'schema_migrations_no_delete')`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 2 {
		t.Fatalf("migration ledger trigger count = %d, want 2", triggerCount)
	}
}

func TestOperationAuditAppendOnlyIdempotencyAndCursor(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := OperationAuditInput{
		RequestID:  "req-1",
		Actor:      "admin@example.com",
		SourceIP:   "203.0.113.7",
		AuthMethod: "jwt",
		Action:     "user.ban",
		TargetType: "user",
		TargetID:   "42",
		Reason:     "abuse evidence confirmed",
		BeforeJSON: json.RawMessage(`{ "status": "active" }`),
		AfterJSON:  json.RawMessage(`{"status":"banned"}`),
		Status:     OperationSucceeded,
	}
	base.IdempotencyKey = "audit-1"
	base.OccurredAt = testNow.Add(-time.Hour)
	first, err := store.AppendOperationAudit(ctx, base)
	if err != nil {
		t.Fatalf("AppendOperationAudit() error = %v", err)
	}
	metadataRetry := base
	metadataRetry.RequestID = "req-retry"
	metadataRetry.SourceIP = "198.51.100.9"
	metadataRetry.OccurredAt = testNow.Add(time.Hour)
	retry, err := store.AppendOperationAudit(ctx, metadataRetry)
	if err != nil {
		t.Fatalf("idempotent AppendOperationAudit() error = %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry ID = %d, want %d", retry.ID, first.ID)
	}
	if retry.RequestID != first.RequestID || retry.SourceIP != first.SourceIP || !retry.OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("retry did not return original audit metadata: first=%+v retry=%+v", first, retry)
	}
	conflicting := base
	conflicting.Action = "user.delete"
	if _, err := store.AppendOperationAudit(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused audit idempotency key error = %v, want ErrConflict", err)
	}
	for i, key := range []string{"audit-2", "audit-3"} {
		input := base
		input.IdempotencyKey = key
		input.RequestID = "req-" + string(rune('2'+i))
		if _, err := store.AppendOperationAudit(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListOperationAudits(ctx, OperationAuditFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor != page.Items[1].ID {
		t.Fatalf("first page = %+v", page)
	}
	next, err := store.ListOperationAudits(ctx, OperationAuditFilter{BeforeID: page.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.HasMore || next.Items[0].ID != first.ID {
		t.Fatalf("next page = %+v", next)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE operation_audit SET reason = 'tampered' WHERE id = ?", first.ID); err == nil {
		t.Fatal("operation_audit update unexpectedly succeeded")
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM operation_audit WHERE id = ?", first.ID); err == nil {
		t.Fatal("operation_audit delete unexpectedly succeeded")
	}
	bad := base
	bad.IdempotencyKey = "audit-bad"
	bad.BeforeJSON = json.RawMessage(`{"broken":`)
	if _, err := store.AppendOperationAudit(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid JSON error = %v, want ErrInvalid", err)
	}
}

func TestOperationAuditAndReconciliationTransactionRetryAndConflictSemantics(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	auditInput := OperationAuditInput{
		RequestID: "atomic-audit-request", Actor: "admin", SourceIP: "203.0.113.9", AuthMethod: "jwt",
		Action: "top_ups.export.intent", TargetType: "financial_export", TargetID: "atomic-audit-request",
		BeforeJSON: json.RawMessage(`{"status":"success"}`), AfterJSON: json.RawMessage(`{"expected_row_count":2}`),
		Status: OperationSucceeded, IdempotencyKey: "export:atomic-audit-request:intent",
	}
	runInput := ReconciliationRunInput{
		RunKey: "top-up-export-audit:atomic-audit-request", Kind: "top_up_export_audit_outcome",
		Status: ReconciliationRunning, WindowStart: testNow.Add(-time.Second), WindowEnd: testNow,
		StartedAt: testNow, ScannedCount: 1, DiscrepancyCount: 1, Currency: "XXX",
		SummaryJSON: json.RawMessage(`{"phase":"pending"}`), ErrorCode: "OPERATION_AUDIT_APPEND_PENDING",
	}

	firstAudit, firstRun, err := store.AppendOperationAuditWithReconciliationRun(ctx, auditInput, runInput)
	if err != nil {
		t.Fatalf("first atomic append: %v", err)
	}
	retryAudit, retryRun, err := store.AppendOperationAuditWithReconciliationRun(ctx, auditInput, runInput)
	if err != nil {
		t.Fatalf("idempotent atomic retry: %v", err)
	}
	if retryAudit.ID != firstAudit.ID || retryRun.ID != firstRun.ID {
		t.Fatalf("atomic retry IDs = audit:%d run:%d, want audit:%d run:%d",
			retryAudit.ID, retryRun.ID, firstAudit.ID, firstRun.ID)
	}
	differentReplayRun := runInput
	differentReplayRun.RunKey = "top-up-export-audit:different-replay"
	if _, _, err := store.AppendOperationAuditWithReconciliationRun(ctx, auditInput, differentReplayRun); !errors.Is(err, ErrConflict) {
		t.Fatalf("same audit with different reconciliation run error = %v, want ErrConflict", err)
	}

	conflictingRun := runInput
	conflictingRun.Kind = "different_reconciliation_kind"
	if _, _, err := store.AppendOperationAuditWithReconciliationRun(ctx, auditInput, conflictingRun); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting reconciliation retry error = %v, want ErrConflict", err)
	}
	conflictingAudit := auditInput
	conflictingAudit.Action = "top_ups.export.different"
	newRun := runInput
	newRun.RunKey = "top-up-export-audit:must-not-be-created"
	if _, _, err := store.AppendOperationAuditWithReconciliationRun(ctx, conflictingAudit, newRun); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting audit retry error = %v, want ErrConflict", err)
	}

	var auditCount, runCount, differentReplayRunCount, forbiddenRunCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_audit
		WHERE idempotency_key = ?`, auditInput.IdempotencyKey).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reconciliation_runs
		WHERE run_key = ?`, runInput.RunKey).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reconciliation_runs
		WHERE run_key = ?`, differentReplayRun.RunKey).Scan(&differentReplayRunCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reconciliation_runs
		WHERE run_key = ?`, newRun.RunKey).Scan(&forbiddenRunCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || runCount != 1 || differentReplayRunCount != 0 || forbiddenRunCount != 0 {
		t.Fatalf("atomic retry/conflict rows = audit:%d run:%d different_replay_run:%d forbidden_run:%d, want 1/1/0/0",
			auditCount, runCount, differentReplayRunCount, forbiddenRunCount)
	}
}

func TestCreatedAtOrderedListsUseTupleCursorWithoutChangingDefaultIDOrder(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := testNow.Truncate(time.Millisecond)
	createdTimes := []time.Time{base.Add(3 * time.Second), base.Add(time.Second), base.Add(2 * time.Second), base.Add(2 * time.Second)}
	for index, createdAt := range createdTimes {
		current := createdAt
		store.now = func() time.Time { return current }
		sequence := index + 1
		if _, err := store.AppendOperationAudit(ctx, OperationAuditInput{
			RequestID: fmt.Sprintf("tuple-audit-%d", sequence), Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
			Action: "user.review", TargetType: "user", TargetID: "42", Status: OperationSucceeded,
			IdempotencyKey: fmt.Sprintf("tuple-audit-key-%d", sequence),
		}); err != nil {
			t.Fatalf("append tuple audit %d: %v", sequence, err)
		}
		if _, err := store.CreateRiskCase(ctx, RiskCaseInput{
			CaseKey: fmt.Sprintf("tuple-risk-%d", sequence), Title: "Tuple risk", SubjectType: "user", SubjectID: "42",
			Severity: RiskSeverityHigh, Status: RiskCaseOpen,
		}); err != nil {
			t.Fatalf("create tuple risk %d: %v", sequence, err)
		}
		if _, err := store.CreateSupportNote(ctx, SupportNoteInput{
			SubjectType: "user", SubjectID: "42", Author: "support", Body: fmt.Sprintf("Tuple note %d", sequence),
			Visibility: NoteInternal, IdempotencyKey: fmt.Sprintf("tuple-note-key-%d", sequence),
		}); err != nil {
			t.Fatalf("create tuple note %d: %v", sequence, err)
		}
	}

	assertIDs := func(name string, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s ids = %v, want %v", name, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("%s ids = %v, want %v", name, got, want)
			}
		}
	}
	defaultWant := []int64{4, 3, 2, 1}
	tupleWant := []int64{1, 4, 3, 2}

	auditDefault, err := store.ListOperationAudits(ctx, OperationAuditFilter{TargetType: "user", TargetID: "42", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	auditDefaultIDs := make([]int64, len(auditDefault.Items))
	for index, item := range auditDefault.Items {
		auditDefaultIDs[index] = item.ID
	}
	assertIDs("default operation audit", auditDefaultIDs, defaultWant)
	auditTupleIDs := make([]int64, 0, len(tupleWant))
	var auditBefore time.Time
	var auditBeforeID int64
	for len(auditTupleIDs) < len(tupleWant) {
		page, err := store.ListOperationAudits(ctx, OperationAuditFilter{
			TargetType: "user", TargetID: "42", BeforeCreatedAt: auditBefore, BeforeID: auditBeforeID,
			OrderByCreatedAt: true, Limit: 1,
		})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("created_at operation audit page = %+v, error=%v", page, err)
		}
		item := page.Items[0]
		auditTupleIDs = append(auditTupleIDs, item.ID)
		auditBefore, auditBeforeID = item.CreatedAt, item.ID
	}
	assertIDs("created_at operation audit", auditTupleIDs, tupleWant)

	riskDefault, err := store.ListRiskCases(ctx, RiskCaseFilter{SubjectType: "user", SubjectID: "42", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	riskDefaultIDs := make([]int64, len(riskDefault.Items))
	for index, item := range riskDefault.Items {
		riskDefaultIDs[index] = item.ID
	}
	assertIDs("default risk case", riskDefaultIDs, defaultWant)
	riskTupleIDs := make([]int64, 0, len(tupleWant))
	var riskBefore time.Time
	var riskBeforeID int64
	for len(riskTupleIDs) < len(tupleWant) {
		page, err := store.ListRiskCases(ctx, RiskCaseFilter{
			SubjectType: "user", SubjectID: "42", BeforeCreatedAt: riskBefore, BeforeID: riskBeforeID,
			OrderByCreatedAt: true, Limit: 1,
		})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("created_at risk case page = %+v, error=%v", page, err)
		}
		item := page.Items[0]
		riskTupleIDs = append(riskTupleIDs, item.ID)
		riskBefore, riskBeforeID = item.CreatedAt, item.ID
	}
	assertIDs("created_at risk case", riskTupleIDs, tupleWant)

	noteDefault, err := store.ListSupportNotes(ctx, SupportNoteFilter{SubjectType: "user", SubjectID: "42", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	noteDefaultIDs := make([]int64, len(noteDefault.Items))
	for index, item := range noteDefault.Items {
		noteDefaultIDs[index] = item.ID
	}
	assertIDs("default support note", noteDefaultIDs, defaultWant)
	noteTupleIDs := make([]int64, 0, len(tupleWant))
	var noteBefore time.Time
	var noteBeforeID int64
	for len(noteTupleIDs) < len(tupleWant) {
		page, err := store.ListSupportNotes(ctx, SupportNoteFilter{
			SubjectType: "user", SubjectID: "42", BeforeCreatedAt: noteBefore, BeforeID: noteBeforeID,
			OrderByCreatedAt: true, Limit: 1,
		})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("created_at support note page = %+v, error=%v", page, err)
		}
		item := page.Items[0]
		noteTupleIDs = append(noteTupleIDs, item.ID)
		if page.HasMore {
			if page.NextCursor != item.ID || !page.NextCreatedAt.Equal(item.CreatedAt) {
				t.Fatalf("created_at support note cursor = (%s, %d), want (%s, %d)",
					page.NextCreatedAt, page.NextCursor, item.CreatedAt, item.ID)
			}
		} else if page.NextCursor != 0 || !page.NextCreatedAt.IsZero() {
			t.Fatalf("terminal support note cursor = (%s, %d), want zero values", page.NextCreatedAt, page.NextCursor)
		}
		noteBefore, noteBeforeID = page.NextCreatedAt, page.NextCursor
	}
	assertIDs("created_at support note", noteTupleIDs, tupleWant)
}

func TestOperationAuditClaimReportsSingleOwner(t *testing.T) {
	store, path := newTestStore(t)
	secondStore, err := Init(path)
	if err != nil {
		t.Fatalf("open second toolstore handle: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second toolstore handle: %v", err)
		}
	})
	ctx := context.Background()
	input := OperationAuditInput{
		RequestID:      "claim-request-1",
		Actor:          "admin@example.com",
		SourceIP:       "203.0.113.7",
		AuthMethod:     "jwt",
		Action:         "user.disable.intent",
		TargetType:     "user",
		TargetID:       "42",
		Reason:         "confirmed operator action",
		BeforeJSON:     json.RawMessage(`{"request":{"action":"user.disable"}}`),
		Status:         OperationSucceeded,
		IdempotencyKey: "claim-operation-1",
	}

	type claimResult struct {
		audit   OperationAudit
		claimed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for index, claimStore := range []*Store{store, secondStore} {
		claimInput := input
		claimInput.RequestID = fmt.Sprintf("claim-request-%d", index+1)
		claimInput.SourceIP = fmt.Sprintf("198.51.100.%d", index+1)
		go func() {
			<-start
			audit, claimed, claimErr := claimStore.ClaimOperationAudit(ctx, claimInput)
			results <- claimResult{audit: audit, claimed: claimed, err: claimErr}
		}()
	}
	close(start)

	claimedCount := 0
	var claimedID int64
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent ClaimOperationAudit() error = %v", result.err)
		}
		if result.audit.ID == 0 {
			t.Fatalf("concurrent ClaimOperationAudit() returned no audit: %+v", result)
		}
		if claimedID == 0 {
			claimedID = result.audit.ID
		} else if result.audit.ID != claimedID {
			t.Fatalf("concurrent claim IDs differ: got %d and %d", claimedID, result.audit.ID)
		}
		if result.claimed {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("concurrent claim owners = %d, want 1", claimedCount)
	}

	var count int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM operation_audit WHERE idempotency_key = ?", input.IdempotencyKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("claimed operation rows = %d, want 1", count)
	}

	withoutKey := input
	withoutKey.IdempotencyKey = ""
	if _, _, err := store.ClaimOperationAudit(ctx, withoutKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("keyless claim error = %v, want ErrInvalid", err)
	}
}

func TestRiskCasesAndImmutableEvents(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-20260716-1", Title: "Credential sharing", SubjectType: "user",
		SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendRiskCaseEvent(ctx, RiskCaseEventInput{
		CaseID: created.ID, EventType: "evidence_attached", Actor: "risk-engine",
		DetailsJSON: json.RawMessage(`{"request_ids":["req-1"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	closedAt := testNow.Add(time.Minute)
	updated, transition, err := store.UpdateRiskCaseWithEvent(ctx, RiskCaseUpdate{
		ID: created.ID, Title: created.Title, Severity: RiskSeverityHigh,
		Status: RiskCaseClosed, Assignee: "analyst-1", Summary: "confirmed",
		ClosedAt: &closedAt,
	}, RiskCaseEventInput{
		CaseID: created.ID, EventType: "case_closed", Actor: "analyst-1",
		DetailsJSON: json.RawMessage(`{"resolution":"ban upheld"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != RiskCaseClosed || updated.ClosedAt == nil || transition.ID <= event.ID {
		t.Fatalf("updated=%+v transition=%+v", updated, transition)
	}
	page, err := store.ListRiskCaseEvents(ctx, RiskCaseEventFilter{CaseID: created.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.HasMore || page.Items[0].EventType != "case_closed" {
		t.Fatalf("event page = %+v", page)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE risk_case_events SET actor = 'tampered' WHERE id = ?", event.ID); err == nil {
		t.Fatal("risk event update unexpectedly succeeded")
	}
	if _, err := store.AppendRiskCaseEvent(ctx, RiskCaseEventInput{
		CaseID: 999999, EventType: "x", Actor: "tester",
	}); err == nil {
		t.Fatal("foreign-key-invalid risk event unexpectedly succeeded")
	}
}

func TestUpdateRiskCaseWithEventReplayFingerprintsUpdateAndEvent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := testNow
	store.now = func() time.Time { return now }
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-transition-fingerprint", Title: "Transition fingerprint",
		SubjectType: "user", SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	update := RiskCaseUpdate{
		ID: created.ID, Title: created.Title, Severity: RiskSeverityCritical,
		Status: RiskCaseInvestigating, Assignee: "analyst-1", Summary: "reviewing evidence",
	}
	eventInput := RiskCaseEventInput{
		CaseID: created.ID, EventType: "investigation_started", Actor: "analyst-1",
		DetailsJSON: json.RawMessage(`{"queue":"priority"}`), IdempotencyKey: "transition-1",
	}
	firstCase, firstEvent, err := store.UpdateRiskCaseWithEvent(ctx, update, eventInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	replayedCase, replayedEvent, err := store.UpdateRiskCaseWithEvent(ctx, update, eventInput)
	if err != nil {
		t.Fatalf("transition replay error = %v", err)
	}
	if replayedEvent.ID != firstEvent.ID || !replayedCase.UpdatedAt.Equal(firstCase.UpdatedAt) {
		t.Fatalf("transition replay changed state: first=%+v/%+v replay=%+v/%+v",
			firstCase, firstEvent, replayedCase, replayedEvent)
	}
	now = now.Add(time.Minute)
	laterCase, err := store.UpdateRiskCase(ctx, RiskCaseUpdate{
		ID: created.ID, Title: "Transition after mitigation", Severity: RiskSeverityMedium,
		Status: RiskCaseMitigated, Assignee: "analyst-2", Summary: "mitigation completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	replayedAfterLaterUpdate, replayedEvent, err := store.UpdateRiskCaseWithEvent(ctx, update, eventInput)
	if err != nil {
		t.Fatalf("transition replay after later update error = %v", err)
	}
	firstJSON, err := json.Marshal(firstCase)
	if err != nil {
		t.Fatal(err)
	}
	replayedJSON, err := json.Marshal(replayedAfterLaterUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayedJSON) != string(firstJSON) || replayedEvent.ID != firstEvent.ID {
		t.Fatalf("transition replay did not return persisted original result: first=%+v/%+v replay=%+v/%+v",
			firstCase, firstEvent, replayedAfterLaterUpdate, replayedEvent)
	}
	if _, err := store.db.Exec(`UPDATE risk_case_transition_replays
		SET request_fingerprint = lower(hex(randomblob(32))) WHERE idempotency_key = ?`, eventInput.IdempotencyKey); err == nil {
		t.Fatal("risk transition replay metadata update unexpectedly succeeded")
	}
	if _, err := store.db.Exec(`DELETE FROM risk_case_transition_replays
		WHERE idempotency_key = ?`, eventInput.IdempotencyKey); err == nil {
		t.Fatal("risk transition replay metadata delete unexpectedly succeeded")
	}

	changedUpdate := update
	changedUpdate.Summary = "different transition payload"
	if _, _, err := store.UpdateRiskCaseWithEvent(ctx, changedUpdate, eventInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed update replay error = %v, want ErrConflict", err)
	}
	changedEvent := eventInput
	changedEvent.DetailsJSON = json.RawMessage(`{"queue":"standard"}`)
	if _, _, err := store.UpdateRiskCaseWithEvent(ctx, update, changedEvent); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed event replay error = %v, want ErrConflict", err)
	}
	stored, err := store.GetRiskCase(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary != laterCase.Summary || !stored.UpdatedAt.Equal(laterCase.UpdatedAt) {
		t.Fatalf("conflicting replay mutated risk case: %+v", stored)
	}
	var eventCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM risk_case_events WHERE idempotency_key = ?", eventInput.IdempotencyKey).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("transition event count = %d, want 1", eventCount)
	}
}

func TestUpdateRiskCaseWithEventRejectsReplayWithoutImmutableMetadata(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-transition-no-metadata", Title: "Legacy transition",
		SubjectType: "user", SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventInput := RiskCaseEventInput{
		CaseID: created.ID, EventType: "investigation_started", Actor: "analyst-1",
		DetailsJSON: json.RawMessage(`{"queue":"legacy"}`), IdempotencyKey: "legacy-transition-event",
	}
	if _, err := store.AppendRiskCaseEvent(ctx, eventInput); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.UpdateRiskCaseWithEvent(ctx, RiskCaseUpdate{
		ID: created.ID, Title: created.Title, Severity: RiskSeverityCritical,
		Status: RiskCaseInvestigating, Assignee: "analyst-1", Summary: "reviewing legacy evidence",
	}, eventInput)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "replay metadata is missing") {
		t.Fatalf("metadata-free replay error = %v, want fail-closed ErrConflict", err)
	}
	stored, err := store.GetRiskCase(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RiskCaseOpen || stored.Summary != "" {
		t.Fatalf("metadata-free replay mutated risk case: %+v", stored)
	}
	assertTableCount(t, store, "risk_case_transition_replays", 0)
}

func TestUpdateRiskCaseRejectsCloseBeforePersistedOpenTime(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	openedAt := testNow.Add(time.Hour)
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-close-order", Title: "Closure ordering", SubjectType: "user",
		SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen, OpenedAt: openedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	closedAt := openedAt.Add(-time.Millisecond)
	_, err = store.UpdateRiskCase(ctx, RiskCaseUpdate{
		ID: created.ID, Title: created.Title, Severity: created.Severity,
		Status: RiskCaseClosed, ClosedAt: &closedAt,
	})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "closed_at cannot be before opened_at") {
		t.Fatalf("UpdateRiskCase() error = %v, want explicit ErrInvalid ordering failure", err)
	}
	unchanged, err := store.GetRiskCase(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != RiskCaseOpen || unchanged.ClosedAt != nil {
		t.Fatalf("risk case changed after rejected close: %+v", unchanged)
	}
}

func TestSupportNoteCRUDPreservesDeletedHistory(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	input := SupportNoteInput{
		SubjectType: "request", SubjectID: "req-1", Author: "support-1",
		Body: "Customer reports a stream interruption.", Visibility: NoteInternal,
		IdempotencyKey: "note-1",
	}
	created, err := store.CreateSupportNote(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateSupportNote(ctx, input)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("idempotent note retry = %+v, %v", retry, err)
	}
	wrongSubject := input
	wrongSubject.SubjectID = "req-2"
	if _, err := store.CreateSupportNote(ctx, wrongSubject); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused note idempotency key error = %v, want ErrConflict", err)
	}
	conflicts := []SupportNoteInput{
		func() SupportNoteInput { changed := input; changed.SubjectType = "user"; return changed }(),
		func() SupportNoteInput { changed := input; changed.Author = "support-2"; return changed }(),
		func() SupportNoteInput { changed := input; changed.Body = "Different body"; return changed }(),
		func() SupportNoteInput { changed := input; changed.Visibility = NoteCustomer; return changed }(),
	}
	for _, conflicting := range conflicts {
		if _, err := store.CreateSupportNote(ctx, conflicting); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting note replay %+v error = %v, want ErrConflict", conflicting, err)
		}
	}
	updated, err := store.UpdateSupportNote(ctx, SupportNoteUpdate{
		ID: created.ID, Body: "Upstream stream interruption confirmed.", Visibility: NoteCustomer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Visibility != NoteCustomer {
		t.Fatalf("updated visibility = %q", updated.Visibility)
	}
	deleted, err := store.DeleteSupportNote(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedAt == nil {
		t.Fatal("soft-deleted note has nil DeletedAt")
	}
	if _, err := store.CreateSupportNote(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("soft-deleted note replay error = %v, want ErrConflict", err)
	}
	active, err := store.ListSupportNotes(ctx, SupportNoteFilter{SubjectType: "request", SubjectID: "req-1"})
	if err != nil || len(active.Items) != 0 {
		t.Fatalf("active notes = %+v, %v", active, err)
	}
	all, err := store.ListSupportNotes(ctx, SupportNoteFilter{IncludeDeleted: true})
	if err != nil || len(all.Items) != 1 || all.Items[0].ID != created.ID {
		t.Fatalf("all notes = %+v, %v", all, err)
	}
}

func TestPriceSnapshotsUseExactAmountsAndAreImmutable(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	input := PriceSnapshotInput{
		Provider: "openai", Model: "gpt-test", Operation: "responses",
		Component: "input", Currency: "usd", Unit: "token", UnitSize: 1,
		AmountDecimal: "0.000001", AmountMinor: 1, MinorUnitScale: 6,
		Source: "provider-price-sheet", MetadataJSON: json.RawMessage(`{"version":"2026-07"}`),
		IdempotencyKey: "price-1", EffectiveAt: testNow.Add(-time.Hour),
	}
	created, err := store.CreatePriceSnapshot(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Currency != "USD" || created.AmountDecimal != "0.000001" || created.AmountMinor != 1 {
		t.Fatalf("created price = %+v", created)
	}
	retry, err := store.CreatePriceSnapshot(ctx, input)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("idempotent price retry = %+v, %v", retry, err)
	}
	conflicting := input
	conflicting.AmountDecimal = "0.000002"
	conflicting.AmountMinor = 2
	if _, err := store.CreatePriceSnapshot(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused price idempotency key error = %v, want ErrConflict", err)
	}
	invalid := input
	invalid.IdempotencyKey = "price-invalid"
	invalid.AmountMinor = 2
	if _, err := store.CreatePriceSnapshot(ctx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("amount mismatch error = %v, want ErrInvalid", err)
	}
	activeAt := testNow
	page, err := store.ListPriceSnapshots(ctx, PriceSnapshotFilter{
		Provider: "openai", Model: "gpt-test", ActiveAt: &activeAt,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("active prices = %+v, %v", page, err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM price_snapshots WHERE id = ?", created.ID); err == nil {
		t.Fatal("price snapshot delete unexpectedly succeeded")
	}
}

func TestPriceSnapshotExpiresAtUsesMillisecondPrecision(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	base := PriceSnapshotInput{
		Provider: "openai", Model: "gpt-test", Operation: "responses", Component: "output",
		Currency: "USD", Unit: "token", UnitSize: 1, AmountDecimal: "0.000002",
		AmountMinor: 2, MinorUnitScale: 6, Source: "provider-price-sheet",
		EffectiveAt: testNow,
	}
	sameMillisecond := testNow.Add(900 * time.Microsecond)
	invalid := base
	invalid.IdempotencyKey = "price-expiry-same-ms"
	invalid.ExpiresAt = &sameMillisecond
	if _, err := store.CreatePriceSnapshot(ctx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same-millisecond expiry error = %v, want ErrInvalid", err)
	}

	expiresAt := testNow.Add(time.Millisecond + 900*time.Microsecond)
	valid := base
	valid.IdempotencyKey = "price-expiry-normalized"
	valid.ExpiresAt = &expiresAt
	created, err := store.CreatePriceSnapshot(ctx, valid)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiresAt := testNow.Add(time.Millisecond)
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(wantExpiresAt) {
		t.Fatalf("stored expires_at = %v, want %v", created.ExpiresAt, wantExpiresAt)
	}
	retry, err := store.CreatePriceSnapshot(ctx, valid)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("normalized expiry retry = %+v, %v", retry, err)
	}
}

func TestReconciliationRunLifecycle(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	input := ReconciliationRunInput{
		RunKey: "daily-2026-07-15", Kind: "daily_usage", Status: ReconciliationRunning,
		WindowStart: testNow.Add(-24 * time.Hour), WindowEnd: testNow,
		Currency: "CNY",
	}
	created, err := store.CreateReconciliationRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateReconciliationRun(ctx, input)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("idempotent reconciliation retry = %+v, %v", retry, err)
	}
	wrongWindow := input
	wrongWindow.WindowStart = input.WindowStart.Add(-time.Hour)
	if _, err := store.CreateReconciliationRun(ctx, wrongWindow); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused run key error = %v, want ErrConflict", err)
	}
	finishedAt := testNow.Add(time.Minute)
	completed, err := store.UpdateReconciliationRun(ctx, ReconciliationRunUpdate{
		ID: created.ID, Status: ReconciliationSucceeded, FinishedAt: &finishedAt,
		ScannedCount: 100, MatchedCount: 98, DiscrepancyCount: 2,
		DiscrepancyMinor: 17, SummaryJSON: json.RawMessage(`{"unexplained":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != ReconciliationSucceeded || completed.FinishedAt == nil || completed.DiscrepancyMinor != 17 {
		t.Fatalf("completed run = %+v", completed)
	}
	replayedAfterCompletion, err := store.CreateReconciliationRun(ctx, input)
	if err != nil || replayedAfterCompletion.ID != completed.ID ||
		replayedAfterCompletion.Status != ReconciliationSucceeded || replayedAfterCompletion.FinishedAt == nil {
		t.Fatalf("completed reconciliation replay = %+v, %v; completed=%+v",
			replayedAfterCompletion, err, completed)
	}
	if _, err := store.UpdateReconciliationRun(ctx, ReconciliationRunUpdate{
		ID: created.ID, Status: ReconciliationSucceeded, FinishedAt: &finishedAt,
		ScannedCount: 100, MatchedCount: 98, DiscrepancyCount: 2,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal update error = %v, want ErrConflict", err)
	}
	page, err := store.ListReconciliationRuns(ctx, ReconciliationRunFilter{
		Kind: "daily_usage", Status: ReconciliationSucceeded,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("reconciliation list = %+v, %v", page, err)
	}
}

func TestReconciliationRunDuplicateUsesStableRequestFingerprint(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	startedAt := testNow.Add(-10 * time.Minute)
	finishedAt := testNow.Add(-time.Minute)
	input := ReconciliationRunInput{
		RunKey: "reconcile-stable-fingerprint", Kind: "daily_usage", Status: ReconciliationFailed,
		WindowStart: testNow.Add(-24 * time.Hour), WindowEnd: testNow,
		StartedAt: startedAt, FinishedAt: &finishedAt,
		ScannedCount: 10, MatchedCount: 8, DiscrepancyCount: 2, DiscrepancyMinor: 17,
		Currency: "CNY", SummaryJSON: json.RawMessage(`{"discrepancies":2}`),
		ErrorCode: "UPSTREAM_TIMEOUT", ErrorMessage: "provider timed out",
	}
	created, err := store.CreateReconciliationRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	equivalent := input
	equivalent.Kind = " daily_usage "
	equivalent.Currency = " cny "
	equivalent.WindowStart = equivalent.WindowStart.Add(500 * time.Microsecond)
	equivalent.WindowEnd = equivalent.WindowEnd.Add(500 * time.Microsecond)
	equivalent.SummaryJSON = json.RawMessage(` { "discrepancies": 2 } `)
	retry, err := store.CreateReconciliationRun(ctx, equivalent)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("equivalent reconciliation retry = %+v, %v", retry, err)
	}

	mutableEvidence := []struct {
		name   string
		mutate func(*ReconciliationRunInput)
	}{
		{"status", func(v *ReconciliationRunInput) { v.Status = ReconciliationCancelled }},
		{"started at", func(v *ReconciliationRunInput) { v.StartedAt = v.StartedAt.Add(time.Millisecond) }},
		{"finished at", func(v *ReconciliationRunInput) {
			changed := v.FinishedAt.Add(time.Millisecond)
			v.FinishedAt = &changed
		}},
		{"scanned count", func(v *ReconciliationRunInput) { v.ScannedCount++ }},
		{"matched count", func(v *ReconciliationRunInput) { v.MatchedCount-- }},
		{"discrepancy count", func(v *ReconciliationRunInput) { v.DiscrepancyCount-- }},
		{"discrepancy minor", func(v *ReconciliationRunInput) { v.DiscrepancyMinor++ }},
		{"summary", func(v *ReconciliationRunInput) { v.SummaryJSON = json.RawMessage(`{"discrepancies":1}`) }},
		{"error code", func(v *ReconciliationRunInput) { v.ErrorCode = "DATA_MISMATCH" }},
		{"error message", func(v *ReconciliationRunInput) { v.ErrorMessage = "different failure" }},
	}
	for _, tt := range mutableEvidence {
		t.Run(tt.name+" is replay evidence", func(t *testing.T) {
			changed := input
			tt.mutate(&changed)
			replayed, err := store.CreateReconciliationRun(ctx, changed)
			if err != nil || replayed.ID != created.ID || replayed.Status != created.Status ||
				replayed.StartedAt.UnixMilli() != created.StartedAt.UnixMilli() {
				t.Fatalf("mutable evidence replay = %+v, %v; original=%+v", replayed, err, created)
			}
		})
	}

	stableParameters := []struct {
		name   string
		mutate func(*ReconciliationRunInput)
	}{
		{"kind", func(v *ReconciliationRunInput) { v.Kind = "hourly_usage" }},
		{"window start", func(v *ReconciliationRunInput) { v.WindowStart = v.WindowStart.Add(time.Minute) }},
		{"window end", func(v *ReconciliationRunInput) { v.WindowEnd = v.WindowEnd.Add(-time.Minute) }},
		{"currency", func(v *ReconciliationRunInput) { v.Currency = "USD" }},
	}
	for _, tt := range stableParameters {
		t.Run(tt.name+" conflicts", func(t *testing.T) {
			conflicting := input
			tt.mutate(&conflicting)
			if _, err := store.CreateReconciliationRun(ctx, conflicting); !errors.Is(err, ErrConflict) {
				t.Fatalf("stable fingerprint conflict error = %v, want ErrConflict", err)
			}
		})
	}

	now := testNow
	store.now = func() time.Time { return now }
	implicitStartedAt := ReconciliationRunInput{
		RunKey: "reconcile-implicit-start", Kind: "daily_usage", Status: ReconciliationRunning,
		WindowStart: testNow.Add(-24 * time.Hour), WindowEnd: testNow, Currency: "CNY",
	}
	implicitCreated, err := store.CreateReconciliationRun(ctx, implicitStartedAt)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	implicitRetry, err := store.CreateReconciliationRun(ctx, implicitStartedAt)
	if err != nil || implicitRetry.ID != implicitCreated.ID ||
		!implicitRetry.StartedAt.Equal(implicitCreated.StartedAt) {
		t.Fatalf("implicit started_at retry = %+v, %v; first=%+v", implicitRetry, err, implicitCreated)
	}
}

func TestReconciliationRunConcurrentDefaultStartedAtReplay(t *testing.T) {
	first, path := newTestStore(t)
	second, err := Init(path)
	if err != nil {
		t.Fatalf("open second toolstore handle: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second toolstore handle: %v", err)
		}
	})
	first.now = func() time.Time { return testNow }
	second.now = func() time.Time { return testNow.Add(2 * time.Hour) }

	input := ReconciliationRunInput{
		RunKey: "reconcile-concurrent-default-start", Kind: "daily_usage",
		Status: ReconciliationRunning, WindowStart: testNow.Add(-24 * time.Hour),
		WindowEnd: testNow, Currency: "CNY",
	}
	type result struct {
		run ReconciliationRun
		err error
	}
	const workers = 12
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		store := first
		if index%2 == 1 {
			store = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, createErr := store.CreateReconciliationRun(context.Background(), input)
			results <- result{run: run, err: createErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var logicalRun ReconciliationRun
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent reconciliation create error = %v", result.err)
		}
		if logicalRun.ID == 0 {
			logicalRun = result.run
			continue
		}
		if result.run.ID != logicalRun.ID ||
			result.run.StartedAt.UnixMilli() != logicalRun.StartedAt.UnixMilli() {
			t.Fatalf("concurrent replay returned different logical runs: first=%+v next=%+v",
				logicalRun, result.run)
		}
	}
	assertTableCount(t, first, "reconciliation_runs", 1)
}

func TestReconciliationRunConcurrentStableFingerprintConflict(t *testing.T) {
	first, path := newTestStore(t)
	second, err := Init(path)
	if err != nil {
		t.Fatalf("open second toolstore handle: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second toolstore handle: %v", err)
		}
	})
	first.now = func() time.Time { return testNow }
	second.now = func() time.Time { return testNow.Add(time.Hour) }

	base := ReconciliationRunInput{
		RunKey: "reconcile-concurrent-conflict", Kind: "daily_usage",
		Status: ReconciliationRunning, WindowStart: testNow.Add(-24 * time.Hour),
		WindowEnd: testNow, Currency: "CNY",
	}
	conflicting := base
	conflicting.Kind = "hourly_usage"
	type result struct {
		run ReconciliationRun
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, call := range []struct {
		store *Store
		input ReconciliationRunInput
	}{{first, base}, {second, conflicting}} {
		call := call
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, createErr := call.store.CreateReconciliationRun(context.Background(), call.input)
			results <- result{run: run, err: createErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var succeeded, conflicted int
	var winningRun ReconciliationRun
	for result := range results {
		switch {
		case result.err == nil:
			succeeded++
			winningRun = result.run
		case errors.Is(result.err, ErrConflict):
			conflicted++
		default:
			t.Fatalf("concurrent conflicting create error = %v", result.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent stable conflict outcomes succeeded=%d conflicted=%d, want 1/1",
			succeeded, conflicted)
	}
	stored, err := first.GetReconciliationRunByKey(context.Background(), base.RunKey)
	if err != nil || stored.ID != winningRun.ID || stored.Kind != winningRun.Kind {
		t.Fatalf("stored concurrent winner = %+v, %v; returned winner=%+v", stored, err, winningRun)
	}
	assertTableCount(t, first, "reconciliation_runs", 1)
}

type migrationLedgerFixture struct {
	version  int
	name     string
	checksum string
}

func createMigrationLedgerFixture(t *testing.T, path string, entries []migrationLedgerFixture) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		checksum TEXT NOT NULL DEFAULT '',
		applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
	)`); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
			VALUES (?, ?, ?, ?)`, entry.version, entry.name, entry.checksum, dbTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
}

func applyLegacyMigrationForTest(t *testing.T, db *sql.DB, item migration) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range item.statements {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatalf("apply legacy migration %d: %v", item.version, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at)
		VALUES (?, ?, ?)`, item.version, item.name, dbTime(testNow)); err != nil {
		t.Fatalf("record legacy migration %d: %v", item.version, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
