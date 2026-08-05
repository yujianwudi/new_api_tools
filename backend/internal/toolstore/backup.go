package toolstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	sqlite "modernc.org/sqlite"
)

// BackupMetadata is the evidence emitted after a backup or verification. It
// deliberately contains no row data or credentials.
type BackupMetadata struct {
	Path                  string           `json:"path"`
	SchemaVersion         int              `json:"schema_version"`
	SizeBytes             int64            `json:"size_bytes"`
	SHA256                string           `json:"sha256"`
	Integrity             string           `json:"integrity"`
	MigrationLedgerSHA256 string           `json:"migration_ledger_sha256"`
	SchemaObjectsSHA256   string           `json:"schema_objects_sha256"`
	CoreTableCounts       map[string]int64 `json:"core_table_counts"`
}

type rollbackCriticalTable struct {
	name       string
	minVersion int
}

// rollbackCriticalTables is version-aware because rollback must prove that
// every durable table which existed before a candidate remains intact. In
// particular, invoice and probe evidence cannot be hidden behind a successful
// count of the original six control-plane tables.
var rollbackCriticalTables = [...]rollbackCriticalTable{
	{name: "operation_audit", minVersion: 1},
	{name: "risk_cases", minVersion: 2},
	{name: "risk_case_events", minVersion: 2},
	{name: "support_notes", minVersion: 3},
	{name: "price_snapshots", minVersion: 4},
	{name: "reconciliation_runs", minVersion: 5},
	{name: "risk_case_transition_replays", minVersion: 7},
	{name: "invoice_documents", minVersion: 8},
	{name: "invoice_events", minVersion: 8},
	{name: "model_probe_runs", minVersion: 9},
	{name: "model_probe_attempts", minVersion: 9},
	{name: "model_probe_rollups", minVersion: 9},
	{name: "model_probe_attempt_lifecycles", minVersion: 10},
	{name: "invoice_document_relations", minVersion: 11},
	{name: "model_status_config_versions", minVersion: 12},
}

// v0.6.0 was the last published schema whose migration ledger could lack the
// checksum column. Newer schemas must carry the immutable migration checksum
// for every row; otherwise a backup cannot prove which SQL was applied.
const legacyChecksumlessBackupMaxSchemaVersion = 9

type sqliteOnlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
	NewRestore(string) (*sqlite.Backup, error)
}

// OnlineBackup uses SQLite's online backup API, then verifies and atomically
// publishes the destination. Existing destinations are never overwritten.
func OnlineBackup(ctx context.Context, sourcePath, destinationPath string) (BackupMetadata, error) {
	source, err := secureExistingSQLitePath(sourcePath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("validate backup source: %w", err)
	}
	destination, err := secureNewSQLitePath(destinationPath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("validate backup destination: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("create backup temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(temporaryPath)
		return BackupMetadata{}, fmt.Errorf("close backup temporary file: %w", closeErr)
	}
	defer os.Remove(temporaryPath)
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return BackupMetadata{}, fmt.Errorf("secure backup temporary file: %w", err)
	}

	db, err := openRawToolStore(source)
	if err != nil {
		return BackupMetadata{}, err
	}
	if err := runSQLiteBackup(ctx, db, temporaryPath, false); err != nil {
		_ = db.Close()
		return BackupMetadata{}, fmt.Errorf("run SQLite online backup: %w", err)
	}
	if err := db.Close(); err != nil {
		return BackupMetadata{}, fmt.Errorf("close backup source: %w", err)
	}
	metadata, err := VerifyBackup(ctx, temporaryPath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("verify online backup: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return BackupMetadata{}, fmt.Errorf("publish online backup: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return BackupMetadata{}, fmt.Errorf("secure online backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return BackupMetadata{}, fmt.Errorf("sync backup directory: %w", err)
	}
	metadata.Path = destination
	return metadata, nil
}

// RestoreBackup replaces the destination database through SQLite's restore
// API. Callers must stop the application and retain a separately verified
// safety backup of the destination before invoking this operation.
func RestoreBackup(ctx context.Context, backupPath, destinationPath string) (BackupMetadata, error) {
	backup, err := secureExistingSQLitePath(backupPath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("validate restore source: %w", err)
	}
	if _, err := VerifyBackup(ctx, backup); err != nil {
		return BackupMetadata{}, fmt.Errorf("verify restore source: %w", err)
	}
	destination, err := secureRestorableSQLitePath(destinationPath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("validate restore destination: %w", err)
	}
	db, err := openRawToolStore(destination)
	if err != nil {
		return BackupMetadata{}, err
	}
	if err := runSQLiteBackup(ctx, db, backup, true); err != nil {
		_ = db.Close()
		return BackupMetadata{}, fmt.Errorf("run SQLite online restore: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return BackupMetadata{}, fmt.Errorf("checkpoint restored database: %w", err)
	}
	if err := db.Close(); err != nil {
		return BackupMetadata{}, fmt.Errorf("close restored database: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return BackupMetadata{}, fmt.Errorf("secure restored database: %w", err)
	}
	return VerifyBackup(ctx, destination)
}

// VerifyBackup performs an integrity check and validates the complete Tool
// Store migration ledger and schema object set before emitting the evidence
// needed by an installer rollback transaction.
func VerifyBackup(ctx context.Context, path string) (BackupMetadata, error) {
	cleanPath, err := secureExistingSQLitePath(path)
	if err != nil {
		return BackupMetadata{}, err
	}
	db, err := openRawToolStore(cleanPath)
	if err != nil {
		return BackupMetadata{}, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return BackupMetadata{}, fmt.Errorf("run integrity_check: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(integrity), "ok") {
		return BackupMetadata{}, fmt.Errorf("integrity_check returned %q", integrity)
	}
	schemaVersion, ledgerDigest, err := validateBackupMigrationLedger(ctx, db)
	if err != nil {
		return BackupMetadata{}, err
	}
	schemaObjectsDigest, err := validateBackupSchemaObjects(ctx, db, schemaVersion)
	if err != nil {
		return BackupMetadata{}, err
	}
	info, err := os.Stat(cleanPath)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("stat backup: %w", err)
	}
	digest, err := fileSHA256(cleanPath)
	if err != nil {
		return BackupMetadata{}, err
	}
	coreTableCounts, err := readRollbackCoreTableCounts(ctx, db, schemaVersion)
	if err != nil {
		return BackupMetadata{}, err
	}
	return BackupMetadata{
		Path: cleanPath, SchemaVersion: schemaVersion, SizeBytes: info.Size(), SHA256: digest,
		Integrity: "ok", MigrationLedgerSHA256: ledgerDigest, SchemaObjectsSHA256: schemaObjectsDigest,
		CoreTableCounts: coreTableCounts,
	}, nil
}

func validateBackupMigrationLedger(ctx context.Context, db *sql.DB) (int, string, error) {
	if err := validateMigrationDefinitions(); err != nil {
		return 0, "", err
	}
	if err := validateMigrationLedgerTableDefinition(ctx, db); err != nil {
		return 0, "", fmt.Errorf("validate backup migration ledger definition: %w", err)
	}
	columns, err := migrationLedgerColumns(ctx, db)
	if err != nil {
		return 0, "", fmt.Errorf("inspect backup migration ledger columns: %w", err)
	}
	_, hasChecksum := columns["checksum"]
	query := "SELECT version, name, '', applied_at FROM schema_migrations ORDER BY version"
	if hasChecksum {
		query = "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, "", fmt.Errorf("read backup migration ledger: %w", err)
	}
	defer rows.Close()

	hash := sha256.New()
	if hasChecksum {
		_, _ = hash.Write([]byte("toolstore-migration-ledger-v1\x00checksums\x00"))
	} else {
		_, _ = hash.Write([]byte("toolstore-migration-ledger-v1\x00legacy-no-checksums\x00"))
	}
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		var appliedAt int64
		if err := rows.Scan(&version, &name, &checksum, &appliedAt); err != nil {
			return 0, "", fmt.Errorf("scan backup migration ledger: %w", err)
		}
		if applied >= len(migrations) {
			return 0, "", fmt.Errorf("backup migration ledger contains unsupported version %d", version)
		}
		expected := migrations[applied]
		if version != expected.version {
			return 0, "", fmt.Errorf("backup migration ledger is not a contiguous prefix: expected version %d, found %d", expected.version, version)
		}
		if name != expected.name {
			return 0, "", fmt.Errorf("backup migration ledger name mismatch at version %d: expected %q, found %q", version, expected.name, name)
		}
		if hasChecksum && checksum != migrationChecksum(expected) {
			return 0, "", fmt.Errorf("backup migration ledger checksum mismatch at version %d", version)
		}
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%d\x00", version, name, checksum, appliedAt)
		applied++
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("iterate backup migration ledger: %w", err)
	}
	if applied == 0 {
		return 0, "", fmt.Errorf("backup migration ledger is empty")
	}
	if !hasChecksum && applied > legacyChecksumlessBackupMaxSchemaVersion {
		return 0, "", fmt.Errorf("checksumless backup migration ledger is unsupported at schema version %d", applied)
	}
	return applied, hex.EncodeToString(hash.Sum(nil)), nil
}

func validateBackupSchemaObjects(ctx context.Context, db *sql.DB, schemaVersion int) (string, error) {
	expected, err := expectedBackupMigrationObjects(ctx, schemaVersion)
	if err != nil {
		return "", err
	}
	var ledgerType, ledgerTable string
	var ledgerStatement sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT type, tbl_name, sql FROM sqlite_schema WHERE name = ?", "schema_migrations").
		Scan(&ledgerType, &ledgerTable, &ledgerStatement); err != nil {
		return "", fmt.Errorf("inspect backup migration ledger object: %w", err)
	}
	if !ledgerStatement.Valid {
		return "", fmt.Errorf("backup migration ledger definition is missing")
	}
	expected["schema_migrations"] = schemaObjectDefinition{
		objectType: ledgerType, tableName: ledgerTable, statement: ledgerStatement.String,
	}
	for index, name := range migrationLedgerProtectionNames() {
		expected[name] = schemaObjectDefinition{
			objectType: "trigger", tableName: "schema_migrations",
			statement: migrationLedgerProtectionStatements[index],
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("read backup schema objects: %w", err)
	}
	defer rows.Close()

	remaining := make(map[string]schemaObjectDefinition, len(expected))
	for name, definition := range expected {
		remaining[name] = definition
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("toolstore-schema-objects-v1\x00"))
	for rows.Next() {
		var objectType, name, tableName, statement string
		if err := rows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			return "", fmt.Errorf("scan backup schema object: %w", err)
		}
		want, ok := remaining[name]
		if !ok {
			return "", fmt.Errorf("backup schema contains unexpected object %q", name)
		}
		normalized := normalizeSchemaSQL(statement)
		if objectType != want.objectType || tableName != want.tableName ||
			normalized != normalizeSchemaSQL(want.statement) {
			return "", fmt.Errorf("backup schema object %q is incompatible", name)
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\x00", objectType, name, tableName, normalized)
		delete(remaining, name)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate backup schema objects: %w", err)
	}
	if len(remaining) != 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return "", fmt.Errorf("backup schema objects are missing: %s", strings.Join(missing, ", "))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func expectedBackupMigrationObjects(ctx context.Context, schemaVersion int) (map[string]schemaObjectDefinition, error) {
	if schemaVersion < 1 || schemaVersion > len(migrations) {
		return nil, fmt.Errorf("unsupported backup schema version %d", schemaVersion)
	}
	reference, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open backup reference schema: %w", err)
	}
	reference.SetMaxOpenConns(1)
	defer reference.Close()
	tx, err := reference.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin backup reference schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range migrations[:schemaVersion] {
		for _, statement := range item.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return nil, fmt.Errorf("build backup reference schema at version %d: %w", item.version, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit backup reference schema: %w", err)
	}
	rows, err := reference.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("read backup reference schema: %w", err)
	}
	defer rows.Close()
	objects := make(map[string]schemaObjectDefinition)
	for rows.Next() {
		var objectType, name, tableName, statement string
		if err := rows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			return nil, fmt.Errorf("scan backup reference schema: %w", err)
		}
		objects[name] = schemaObjectDefinition{
			objectType: objectType, tableName: tableName, statement: statement,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup reference schema: %w", err)
	}
	return objects, nil
}

func readRollbackCoreTableCounts(ctx context.Context, db *sql.DB, schemaVersion int) (map[string]int64, error) {
	counts := make(map[string]int64, len(rollbackCriticalTables))
	for _, table := range rollbackCriticalTables {
		if table.minVersion > schemaVersion {
			continue
		}
		var exists int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table.name,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("inspect rollback-critical table %s: %w", table.name, err)
		}
		if exists == 0 {
			return nil, fmt.Errorf("rollback-critical table %s is missing at schema version %d", table.name, schemaVersion)
		}
		var count int64
		// table is selected exclusively from rollbackCriticalTables above; it never
		// contains runtime input or an identifier supplied by a caller.
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil {
			return nil, fmt.Errorf("count rollback-critical table %s: %w", table.name, err)
		}
		counts[table.name] = count
	}
	return counts, nil
}

func runSQLiteBackup(ctx context.Context, db *sql.DB, remotePath string, restore bool) error {
	connection, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite connection: %w", err)
	}
	defer connection.Close()
	return connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(sqliteOnlineBackuper)
		if !ok {
			return fmt.Errorf("SQLite driver does not expose the online backup API")
		}
		var operation *sqlite.Backup
		if restore {
			operation, err = backuper.NewRestore(remotePath)
		} else {
			operation, err = backuper.NewBackup(remotePath)
		}
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				_ = operation.Finish()
				return err
			}
			more, err = operation.Step(128)
			if err != nil {
				_ = operation.Finish()
				return err
			}
		}
		return operation.Finish()
	})
}

func openRawToolStore(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", backupSQLiteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	return db, nil
}

func backupSQLiteDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	query.Add("_pragma", "trusted_schema(OFF)")
	return path + "?" + query.Encode()
}

func secureExistingSQLitePath(path string) (string, error) {
	cleanPath, err := cleanSQLitePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("path must be a regular non-symlink file")
	}
	resolved, err := filepath.EvalSymlinks(cleanPath)
	if err != nil {
		return "", err
	}
	if !sameCleanPath(resolved, cleanPath) {
		return "", fmt.Errorf("path may not traverse symlinks")
	}
	return cleanPath, nil
}

func secureNewSQLitePath(path string) (string, error) {
	cleanPath, err := cleanSQLitePath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(cleanPath); err == nil {
		return "", fmt.Errorf("destination already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := secureSQLiteParent(cleanPath); err != nil {
		return "", err
	}
	return cleanPath, nil
}

func secureRestorableSQLitePath(path string) (string, error) {
	cleanPath, err := cleanSQLitePath(path)
	if err != nil {
		return "", err
	}
	if err := secureSQLiteParent(cleanPath); err != nil {
		return "", err
	}
	if info, err := os.Lstat(cleanPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("restore destination must be a regular non-symlink file")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return cleanPath, nil
}

func cleanSQLitePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == "." || trimmed == string(filepath.Separator) || trimmed == ":memory:" {
		return "", fmt.Errorf("unsafe SQLite path")
	}
	if strings.ContainsAny(trimmed, "?#") {
		return "", fmt.Errorf("unsafe SQLite path: DSN delimiters are not allowed")
	}
	absolute, err := filepath.Abs(filepath.Clean(trimmed))
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(absolute)
	if absolute == string(filepath.Separator) || (volume != "" && absolute == volume+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe SQLite root path")
	}
	return absolute, nil
}

func secureSQLiteParent(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("SQLite parent must be a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if !sameCleanPath(resolved, parent) {
		return fmt.Errorf("SQLite parent may not traverse symlinks")
	}
	return nil
}

func sameCleanPath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open backup for hashing: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash backup: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		// Windows does not support fsync on directory handles. The file itself
		// is closed before rename; Linux production paths still fsync the parent.
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
