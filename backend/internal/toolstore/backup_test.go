package toolstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOnlineBackupAndRestorePreserveCommittedSnapshot(t *testing.T) {
	store, sourcePath := newTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO support_notes(
		subject_type, subject_id, author, body, visibility, idempotency_key, created_at, updated_at, deleted_at
	) VALUES ('test', 'before', 'backup-test', 'before backup', 'internal', 'backup-before', 1, 1, NULL)`); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "rollback", "toolstore.db")
	metadata, err := OnlineBackup(ctx, sourcePath, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Integrity != "ok" || metadata.SchemaVersion != latestSchemaVersion || len(metadata.SHA256) != 64 ||
		len(metadata.MigrationLedgerSHA256) != 64 || len(metadata.SchemaObjectsSHA256) != 64 || metadata.SizeBytes <= 0 {
		t.Fatalf("backup metadata = %+v", metadata)
	}
	if got, want := len(metadata.CoreTableCounts), len(rollbackCriticalTables); got != want {
		t.Fatalf("backup rollback-critical table count = %d, want %d: %+v", got, want, metadata.CoreTableCounts)
	}
	if got := metadata.CoreTableCounts["support_notes"]; got != 1 {
		t.Fatalf("backup support_notes count = %d, want 1", got)
	}
	// Windows does not expose POSIX permission bits through os.Stat. The
	// production Linux path and Linux CI still prove that backups are mode 0600.
	if mode := fileMode(t, backupPath); runtime.GOOS != "windows" && mode.Perm() != 0o600 {
		t.Fatalf("backup permissions = %o, want 600", mode.Perm())
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO support_notes(
		subject_type, subject_id, author, body, visibility, idempotency_key, created_at, updated_at, deleted_at
	) VALUES ('test', 'after', 'backup-test', 'after backup', 'internal', 'backup-after', 2, 2, NULL)`); err != nil {
		t.Fatal(err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored", "toolstore.db")
	restored, err := RestoreBackup(ctx, backupPath, restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.SchemaVersion != latestSchemaVersion || restored.Integrity != "ok" {
		t.Fatalf("restored metadata = %+v", restored)
	}
	if got := restored.CoreTableCounts["support_notes"]; got != 1 {
		t.Fatalf("restored support_notes count = %d, want 1", got)
	}
	restoredStore, err := Init(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	var count int
	if err := restoredStore.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM support_notes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("restored support note count = %d, want snapshot count 1", count)
	}
}

func TestOnlineBackupFailsClosedForExistingDestinationAndSymlinks(t *testing.T) {
	_, sourcePath := newTestStore(t)
	destination := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(destination, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OnlineBackup(context.Background(), sourcePath, destination); err == nil {
		t.Fatal("existing backup destination was overwritten")
	}
	if string(mustReadFile(t, destination)) != "do not overwrite" {
		t.Fatal("existing destination content changed")
	}

	symlink := filepath.Join(t.TempDir(), "source-link.db")
	if err := os.Symlink(sourcePath, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := VerifyBackup(context.Background(), symlink); err == nil {
		t.Fatal("symlink backup source was accepted")
	}
}

func TestVerifyBackupRejectsCorruptOrNonToolStoreFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(context.Background(), path); err == nil {
		t.Fatal("corrupt backup was accepted")
	}
}

func TestVerifyBackupAcceptsPublishedChecksumlessV9(t *testing.T) {
	path := migrationBackupFixture(t, legacyChecksumlessBackupMaxSchemaVersion, false)
	metadata, err := VerifyBackup(context.Background(), path)
	if err != nil {
		t.Fatalf("VerifyBackup(v9 checksumless fixture) error = %v", err)
	}
	if metadata.SchemaVersion != legacyChecksumlessBackupMaxSchemaVersion ||
		len(metadata.MigrationLedgerSHA256) != 64 || len(metadata.SchemaObjectsSHA256) != 64 {
		t.Fatalf("v9 checksumless metadata = %+v", metadata)
	}
	for _, table := range rollbackCriticalTables {
		_, exists := metadata.CoreTableCounts[table.name]
		if table.minVersion <= legacyChecksumlessBackupMaxSchemaVersion && !exists {
			t.Errorf("v9 metadata omitted rollback-critical table %q", table.name)
		}
		if table.minVersion > legacyChecksumlessBackupMaxSchemaVersion && exists {
			t.Errorf("v9 metadata included future table %q", table.name)
		}
	}
}

func TestVerifyBackupRejectsChecksumlessPostV9Ledger(t *testing.T) {
	path := migrationBackupFixture(t, legacyChecksumlessBackupMaxSchemaVersion+1, false)
	_, err := VerifyBackup(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "checksumless backup migration ledger is unsupported") {
		t.Fatalf("VerifyBackup(checksumless v10) error = %v", err)
	}
}

func TestVerifyBackupRejectsLedgerSchemaAndCriticalTableTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB)
		want   string
	}{
		{
			name: "migration checksum",
			mutate: func(t *testing.T, db *sql.DB) {
				dropLedgerProtectionForBackupTest(t, db)
				if _, err := db.Exec("UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 9"); err != nil {
					t.Fatal(err)
				}
			},
			want: "checksum mismatch",
		},
		{
			name: "unexpected object",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec("CREATE TABLE injected_backup_object(id INTEGER)"); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected object",
		},
		{
			name: "missing critical table",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec("DROP TABLE model_status_config_versions"); err != nil {
					t.Fatal(err)
				}
			},
			want: "schema objects are missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := migrationBackupFixture(t, latestSchemaVersion, true)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = VerifyBackup(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyBackup(tampered fixture) error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestToolStoreV9CandidateFailureRollbackDrill(t *testing.T) {
	ctx := context.Background()
	livePath := migrationBackupFixture(t, legacyChecksumlessBackupMaxSchemaVersion, false)
	raw, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO support_notes(
		subject_type, subject_id, author, body, visibility, idempotency_key, created_at, updated_at, deleted_at
	) VALUES ('release-drill', 'v9', 'ci', 'authoritative v9 evidence', 'internal', 'drill-v9', 1, 1, NULL)`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(t.TempDir(), "authoritative-v9.db")
	before, err := OnlineBackup(ctx, livePath, backupPath)
	if err != nil {
		t.Fatalf("create authoritative v9 backup: %v", err)
	}
	if before.SchemaVersion != 9 || before.CoreTableCounts["support_notes"] != 1 {
		t.Fatalf("authoritative v9 metadata = %+v", before)
	}

	candidate, err := Init(livePath)
	if err != nil {
		t.Fatalf("migrate v9 candidate to current schema: %v", err)
	}
	health, err := candidate.Health(ctx)
	if err != nil || health.SchemaVersion != latestSchemaVersion {
		_ = candidate.Close()
		t.Fatalf("candidate health after migration = %+v, %v", health, err)
	}
	if _, err := candidate.db.ExecContext(ctx, `INSERT INTO support_notes(
		subject_type, subject_id, author, body, visibility, idempotency_key, created_at, updated_at, deleted_at
	) VALUES ('release-drill', 'candidate', 'ci', 'must disappear on rollback', 'internal', 'drill-candidate', 2, 2, NULL)`); err != nil {
		_ = candidate.Close()
		t.Fatal(err)
	}
	var currentOnlyObjects int
	if err := candidate.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'table' AND name IN (
			'model_probe_attempt_lifecycles', 'invoice_document_relations', 'model_status_config_versions'
		)`).Scan(&currentOnlyObjects); err != nil {
		_ = candidate.Close()
		t.Fatal(err)
	}
	if currentOnlyObjects != 3 {
		_ = candidate.Close()
		t.Fatalf("current-only candidate tables = %d, want 3", currentOnlyObjects)
	}
	// The candidate is deliberately rejected after it has performed the real
	// v9-to-current migration and a post-migration write.
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreBackup(ctx, backupPath, livePath)
	if err != nil {
		t.Fatalf("restore authoritative v9 backup after candidate failure: %v", err)
	}
	if restored.SchemaVersion != 9 || restored.CoreTableCounts["support_notes"] != 1 ||
		restored.MigrationLedgerSHA256 != before.MigrationLedgerSHA256 ||
		restored.SchemaObjectsSHA256 != before.SchemaObjectsSHA256 {
		t.Fatalf("restored v9 metadata = %+v, authoritative = %+v", restored, before)
	}

	oldReader, err := sql.Open("sqlite", livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldReader.Close()
	var schemaVersion, supportCount, candidateRows, futureTables int
	if err := oldReader.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := oldReader.QueryRowContext(ctx, "SELECT COUNT(*) FROM support_notes").Scan(&supportCount); err != nil {
		t.Fatal(err)
	}
	if err := oldReader.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM support_notes WHERE idempotency_key = 'drill-candidate'").Scan(&candidateRows); err != nil {
		t.Fatal(err)
	}
	if err := oldReader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'table' AND name IN (
			'model_probe_attempt_lifecycles', 'invoice_document_relations', 'model_status_config_versions'
		)`).Scan(&futureTables); err != nil {
		t.Fatal(err)
	}
	columns, err := migrationLedgerColumns(ctx, oldReader)
	if err != nil {
		t.Fatal(err)
	}
	if _, hasChecksum := columns["checksum"]; schemaVersion != 9 || supportCount != 1 ||
		candidateRows != 0 || futureTables != 0 || hasChecksum {
		t.Fatalf("old-schema readability: version=%d support=%d candidate=%d future=%d checksum_column=%v",
			schemaVersion, supportCount, candidateRows, futureTables, hasChecksum)
	}
}

func migrationBackupFixture(t *testing.T, schemaVersion int, withChecksums bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolstore-fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ledger := legacyMigrationLedgerCreateStatement
	if withChecksums {
		ledger = migrationLedgerCreateStatement
	}
	if _, err := db.Exec(ledger); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, item := range migrations[:schemaVersion] {
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		for _, statement := range item.statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				_ = db.Close()
				t.Fatalf("apply fixture migration %d: %v", item.version, err)
			}
		}
		if withChecksums {
			_, err = tx.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
				VALUES (?, ?, ?, ?)`, item.version, item.name, migrationChecksum(item), dbTime(testNow))
		} else {
			_, err = tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at)
				VALUES (?, ?, ?)`, item.version, item.name, dbTime(testNow))
		}
		if err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatalf("record fixture migration %d: %v", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
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
	return path
}

func dropLedgerProtectionForBackupTest(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, name := range migrationLedgerProtectionNames() {
		if _, err := db.Exec("DROP TRIGGER " + name); err != nil {
			t.Fatal(err)
		}
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
