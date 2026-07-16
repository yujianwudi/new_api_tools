package toolstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type migration struct {
	version    int
	name       string
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		name:    "operation audit",
		statements: []string{
			`CREATE TABLE operation_audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				request_id TEXT NOT NULL CHECK(length(trim(request_id)) > 0),
				actor TEXT NOT NULL CHECK(length(trim(actor)) > 0),
				source_ip TEXT NOT NULL CHECK(length(trim(source_ip)) > 0),
				auth_method TEXT NOT NULL CHECK(length(trim(auth_method)) > 0),
				action TEXT NOT NULL CHECK(length(trim(action)) > 0),
				target_type TEXT NOT NULL CHECK(length(trim(target_type)) > 0),
				target_id TEXT NOT NULL CHECK(length(trim(target_id)) > 0),
				reason TEXT NOT NULL DEFAULT '',
				before_json TEXT CHECK(before_json IS NULL OR json_valid(before_json)),
				after_json TEXT CHECK(after_json IS NULL OR json_valid(after_json)),
				status TEXT NOT NULL CHECK(status IN ('succeeded', 'failed', 'denied', 'cancelled')),
				error_code TEXT NOT NULL DEFAULT '',
				idempotency_key TEXT,
				occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE UNIQUE INDEX idx_operation_audit_idempotency
				ON operation_audit(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_operation_audit_request
				ON operation_audit(request_id, id DESC)`,
			`CREATE INDEX idx_operation_audit_action
				ON operation_audit(action, id DESC)`,
			`CREATE INDEX idx_operation_audit_target
				ON operation_audit(target_type, target_id, id DESC)`,
			`CREATE INDEX idx_operation_audit_actor
				ON operation_audit(actor, id DESC)`,
			`CREATE INDEX idx_operation_audit_failures
				ON operation_audit(status, id DESC) WHERE status <> 'succeeded'`,
			`CREATE TRIGGER operation_audit_no_update
				BEFORE UPDATE ON operation_audit BEGIN
					SELECT RAISE(ABORT, 'operation_audit is append-only');
				END`,
			`CREATE TRIGGER operation_audit_no_delete
				BEFORE DELETE ON operation_audit BEGIN
					SELECT RAISE(ABORT, 'operation_audit is append-only');
				END`,
		},
	},
	{
		version: 2,
		name:    "risk cases",
		statements: []string{
			`CREATE TABLE risk_cases (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				case_key TEXT NOT NULL UNIQUE CHECK(length(trim(case_key)) > 0),
				title TEXT NOT NULL CHECK(length(trim(title)) > 0),
				subject_type TEXT NOT NULL CHECK(length(trim(subject_type)) > 0),
				subject_id TEXT NOT NULL CHECK(length(trim(subject_id)) > 0),
				severity TEXT NOT NULL CHECK(severity IN ('low', 'medium', 'high', 'critical')),
				status TEXT NOT NULL CHECK(status IN ('open', 'investigating', 'mitigated', 'closed')),
				assignee TEXT NOT NULL DEFAULT '',
				summary TEXT NOT NULL DEFAULT '',
				opened_at INTEGER NOT NULL CHECK(opened_at >= 0),
				closed_at INTEGER CHECK(closed_at IS NULL OR closed_at >= opened_at),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK((status = 'closed' AND closed_at IS NOT NULL) OR (status <> 'closed' AND closed_at IS NULL))
			)`,
			`CREATE INDEX idx_risk_cases_subject
				ON risk_cases(subject_type, subject_id, id DESC)`,
			`CREATE INDEX idx_risk_cases_status
				ON risk_cases(status, severity, id DESC)`,
			`CREATE INDEX idx_risk_cases_assignee_open
				ON risk_cases(assignee, id DESC) WHERE status <> 'closed'`,
			`CREATE TABLE risk_case_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				case_id INTEGER NOT NULL REFERENCES risk_cases(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				event_type TEXT NOT NULL CHECK(length(trim(event_type)) > 0),
				actor TEXT NOT NULL CHECK(length(trim(actor)) > 0),
				details_json TEXT CHECK(details_json IS NULL OR json_valid(details_json)),
				occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE INDEX idx_risk_case_events_case
				ON risk_case_events(case_id, id DESC)`,
			`CREATE INDEX idx_risk_case_events_type
				ON risk_case_events(event_type, id DESC)`,
			`CREATE TRIGGER risk_case_events_no_update
				BEFORE UPDATE ON risk_case_events BEGIN
					SELECT RAISE(ABORT, 'risk_case_events is append-only');
				END`,
			`CREATE TRIGGER risk_case_events_no_delete
				BEFORE DELETE ON risk_case_events BEGIN
					SELECT RAISE(ABORT, 'risk_case_events is append-only');
				END`,
		},
	},
	{
		version: 3,
		name:    "support notes",
		statements: []string{
			`CREATE TABLE support_notes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				subject_type TEXT NOT NULL CHECK(length(trim(subject_type)) > 0),
				subject_id TEXT NOT NULL CHECK(length(trim(subject_id)) > 0),
				author TEXT NOT NULL CHECK(length(trim(author)) > 0),
				body TEXT NOT NULL CHECK(length(trim(body)) > 0),
				visibility TEXT NOT NULL CHECK(visibility IN ('internal', 'customer')),
				idempotency_key TEXT,
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				deleted_at INTEGER CHECK(deleted_at IS NULL OR deleted_at >= created_at)
			)`,
			`CREATE UNIQUE INDEX idx_support_notes_idempotency
				ON support_notes(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_support_notes_subject_active
				ON support_notes(subject_type, subject_id, id DESC) WHERE deleted_at IS NULL`,
			`CREATE INDEX idx_support_notes_author
				ON support_notes(author, id DESC)`,
		},
	},
	{
		version: 4,
		name:    "price snapshots",
		statements: []string{
			`CREATE TABLE price_snapshots (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				provider TEXT NOT NULL CHECK(length(trim(provider)) > 0),
				model TEXT NOT NULL CHECK(length(trim(model)) > 0),
				operation TEXT NOT NULL CHECK(length(trim(operation)) > 0),
				component TEXT NOT NULL CHECK(length(trim(component)) > 0),
				currency TEXT NOT NULL CHECK(length(currency) = 3 AND currency = upper(currency) AND currency GLOB '[A-Z][A-Z][A-Z]'),
				unit TEXT NOT NULL CHECK(length(trim(unit)) > 0),
				unit_size INTEGER NOT NULL CHECK(unit_size > 0),
				amount_decimal TEXT NOT NULL CHECK(
					length(amount_decimal) > 0 AND
					amount_decimal NOT GLOB '*[^0-9.]*' AND
					amount_decimal NOT LIKE '.%' AND
					amount_decimal NOT LIKE '%.' AND
					amount_decimal NOT GLOB '*.*.*'
				),
				amount_minor INTEGER NOT NULL CHECK(amount_minor >= 0),
				minor_unit_scale INTEGER NOT NULL CHECK(minor_unit_scale BETWEEN 0 AND 18),
				source TEXT NOT NULL CHECK(length(trim(source)) > 0),
				metadata_json TEXT CHECK(metadata_json IS NULL OR json_valid(metadata_json)),
				idempotency_key TEXT,
				effective_at INTEGER NOT NULL CHECK(effective_at >= 0),
				expires_at INTEGER CHECK(expires_at IS NULL OR expires_at > effective_at),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE UNIQUE INDEX idx_price_snapshots_idempotency
				ON price_snapshots(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_price_snapshots_lookup
				ON price_snapshots(provider, model, operation, component, effective_at DESC, id DESC)`,
			`CREATE INDEX idx_price_snapshots_active
				ON price_snapshots(provider, model, id DESC) WHERE expires_at IS NULL`,
			`CREATE TRIGGER price_snapshots_no_update
				BEFORE UPDATE ON price_snapshots BEGIN
					SELECT RAISE(ABORT, 'price_snapshots is immutable');
				END`,
			`CREATE TRIGGER price_snapshots_no_delete
				BEFORE DELETE ON price_snapshots BEGIN
					SELECT RAISE(ABORT, 'price_snapshots is immutable');
				END`,
		},
	},
	{
		version: 5,
		name:    "reconciliation runs",
		statements: []string{
			`CREATE TABLE reconciliation_runs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				run_key TEXT NOT NULL UNIQUE CHECK(length(trim(run_key)) > 0),
				kind TEXT NOT NULL CHECK(length(trim(kind)) > 0),
				status TEXT NOT NULL CHECK(status IN ('running', 'succeeded', 'failed', 'cancelled')),
				window_start INTEGER NOT NULL CHECK(window_start >= 0),
				window_end INTEGER NOT NULL CHECK(window_end > window_start),
				started_at INTEGER NOT NULL CHECK(started_at >= 0),
				finished_at INTEGER CHECK(finished_at IS NULL OR finished_at >= started_at),
				scanned_count INTEGER NOT NULL DEFAULT 0 CHECK(scanned_count >= 0),
				matched_count INTEGER NOT NULL DEFAULT 0 CHECK(matched_count >= 0),
				discrepancy_count INTEGER NOT NULL DEFAULT 0 CHECK(discrepancy_count >= 0),
				discrepancy_minor INTEGER NOT NULL DEFAULT 0,
				currency TEXT NOT NULL CHECK(length(currency) = 3 AND currency = upper(currency) AND currency GLOB '[A-Z][A-Z][A-Z]'),
				summary_json TEXT CHECK(summary_json IS NULL OR json_valid(summary_json)),
				error_code TEXT NOT NULL DEFAULT '',
				error_message TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK((status = 'running' AND finished_at IS NULL) OR (status <> 'running' AND finished_at IS NOT NULL))
			)`,
			`CREATE INDEX idx_reconciliation_runs_kind
				ON reconciliation_runs(kind, id DESC)`,
			`CREATE INDEX idx_reconciliation_runs_status
				ON reconciliation_runs(status, id DESC)`,
			`CREATE INDEX idx_reconciliation_runs_active
				ON reconciliation_runs(started_at, id DESC) WHERE status = 'running'`,
		},
	},
	{
		version: 6,
		name:    "risk event idempotency",
		statements: []string{
			`ALTER TABLE risk_case_events ADD COLUMN idempotency_key TEXT`,
			`CREATE UNIQUE INDEX idx_risk_case_events_idempotency
				ON risk_case_events(idempotency_key) WHERE idempotency_key IS NOT NULL`,
		},
	},
}

const latestSchemaVersion = 6

const migrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL DEFAULT '',
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
)`

const legacyMigrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
)`

// SQLite appends columns added with ALTER TABLE to the stored CREATE TABLE
// statement. Keep that historical shape recognizable so a pre-checksum v0.5
// database can be upgraded without accepting arbitrary look-alike ledgers.
const upgradedLegacyMigrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0),
	checksum TEXT NOT NULL DEFAULT ''
)`

var migrationLedgerProtectionStatements = []string{
	`CREATE TRIGGER schema_migrations_no_update
		BEFORE UPDATE ON schema_migrations BEGIN
			SELECT RAISE(ABORT, 'schema_migrations is append-only');
		END`,
	`CREATE TRIGGER schema_migrations_no_delete
		BEFORE DELETE ON schema_migrations BEGIN
			SELECT RAISE(ABORT, 'schema_migrations is append-only');
		END`,
}

func (s *Store) migrate(ctx context.Context) error {
	if err := validateMigrationDefinitions(); err != nil {
		return err
	}
	if err := s.bootstrapMigrationLedger(ctx); err != nil {
		return err
	}
	if err := s.ensureMigrationLedgerSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureMigrationLedgerProtection(ctx); err != nil {
		return err
	}
	applied, err := s.validateMigrationLedger(ctx)
	if err != nil {
		return err
	}

	for index := applied; index < len(migrations); index++ {
		if err := s.applyMigration(ctx, migrations[index]); err != nil {
			return err
		}
	}
	finalApplied, err := s.validateMigrationLedger(ctx)
	if err != nil {
		return err
	}
	if finalApplied != len(migrations) {
		return fmt.Errorf("toolstore migration ledger incomplete after migration: applied=%d expected=%d", finalApplied, len(migrations))
	}
	if err := s.validateAppliedMigrationObjects(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) bootstrapMigrationLedger(ctx context.Context) error {
	var objectType string
	err := s.db.QueryRowContext(ctx,
		"SELECT type FROM sqlite_schema WHERE name = ?", "schema_migrations").Scan(&objectType)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, migrationLedgerCreateStatement); err != nil {
			return fmt.Errorf("bootstrap schema migrations: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect schema migrations object: %w", err)
	}
	if objectType != "table" {
		return fmt.Errorf("toolstore migration ledger object has type %q, want table", objectType)
	}
	return nil
}

type migrationLedgerColumn struct {
	typeName string
	notNull  bool
	primary  bool
}

type migrationLedgerExecutor interface {
	execQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) ensureMigrationLedgerSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration ledger upgrade: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := validateMigrationLedgerTableDefinition(ctx, tx); err != nil {
		return err
	}
	columns, err := migrationLedgerColumns(ctx, tx)
	if err != nil {
		return err
	}
	for name, expected := range map[string]migrationLedgerColumn{
		"version":    {typeName: "INTEGER", primary: true},
		"name":       {typeName: "TEXT", notNull: true},
		"applied_at": {typeName: "INTEGER", notNull: true},
	} {
		actual, ok := columns[name]
		if !ok || !strings.EqualFold(actual.typeName, expected.typeName) ||
			(expected.notNull && !actual.notNull) || (expected.primary && !actual.primary) {
			return fmt.Errorf("toolstore migration ledger schema is incompatible at column %q", name)
		}
	}
	checksum, hasChecksum := columns["checksum"]
	if hasChecksum {
		if !strings.EqualFold(checksum.typeName, "TEXT") || !checksum.notNull {
			return fmt.Errorf("toolstore migration ledger schema is incompatible at column %q", "checksum")
		}
	}

	needsBackfill := !hasChecksum
	if hasChecksum {
		var emptyChecksums int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE checksum = ''").Scan(&emptyChecksums); err != nil {
			return fmt.Errorf("inspect migration ledger checksums: %w", err)
		}
		needsBackfill = emptyChecksums > 0
	}
	if !needsBackfill {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration ledger inspection: %w", err)
		}
		return nil
	}

	if err := dropMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if !hasChecksum {
		if _, err := tx.ExecContext(ctx,
			"ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add migration ledger checksum: %w", err)
		}
		if err := validateMigrationLedgerTableDefinition(ctx, tx); err != nil {
			return err
		}
	}
	applied, err := validateMigrationLedgerRows(ctx, tx, true)
	if err != nil {
		return err
	}
	for index := 0; index < applied; index++ {
		item := migrations[index]
		result, err := tx.ExecContext(ctx,
			"UPDATE schema_migrations SET checksum = ? WHERE version = ? AND checksum = ''",
			migrationChecksum(item), item.version)
		if err != nil {
			return fmt.Errorf("backfill migration ledger checksum at version %d: %w", item.version, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read migration ledger checksum backfill at version %d: %w", item.version, err)
		}
		if rows > 1 {
			return fmt.Errorf("toolstore migration ledger checksum backfill touched multiple rows at version %d", item.version)
		}
	}
	if _, err := validateMigrationLedgerRows(ctx, tx, false); err != nil {
		return err
	}
	if err := protectMigrationLedger(ctx, tx); err != nil {
		return err
	}
	if err := validateMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration ledger checksum backfill: %w", err)
	}
	return nil
}

func validateMigrationLedgerTableDefinition(ctx context.Context, queryer migrationLedgerExecutor) error {
	var objectType string
	var definition sql.NullString
	if err := queryer.QueryRowContext(ctx,
		"SELECT type, sql FROM sqlite_schema WHERE name = ?", "schema_migrations").
		Scan(&objectType, &definition); err != nil {
		return fmt.Errorf("inspect migration ledger definition: %w", err)
	}
	if objectType != "table" || !definition.Valid {
		return fmt.Errorf("toolstore migration ledger definition is incompatible: type=%q", objectType)
	}
	actual := normalizeSchemaSQL(definition.String)
	for _, allowed := range []string{
		migrationLedgerCreateStatement,
		legacyMigrationLedgerCreateStatement,
		upgradedLegacyMigrationLedgerCreateStatement,
	} {
		if actual == normalizeSchemaSQL(allowed) {
			return nil
		}
	}
	return errors.New("toolstore migration ledger table definition is incompatible")
}

func migrationLedgerColumns(ctx context.Context, queryer migrationLedgerExecutor) (map[string]migrationLedgerColumn, error) {
	rows, err := queryer.QueryContext(ctx, "PRAGMA table_info(schema_migrations)")
	if err != nil {
		return nil, fmt.Errorf("inspect migration ledger schema: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]migrationLedgerColumn)
	for rows.Next() {
		var cid, notNull, primary int
		var name, typeName string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primary); err != nil {
			return nil, fmt.Errorf("scan migration ledger schema: %w", err)
		}
		columns[name] = migrationLedgerColumn{
			typeName: strings.TrimSpace(typeName),
			notNull:  notNull != 0,
			primary:  primary != 0,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration ledger schema: %w", err)
	}
	return columns, nil
}

func protectMigrationLedger(ctx context.Context, executor execQueryer) error {
	for _, statement := range migrationLedgerProtectionStatements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("protect schema migrations: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureMigrationLedgerProtection(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration ledger protection check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rebuild := false
	for index, name := range migrationLedgerProtectionNames() {
		var objectType string
		var definition sql.NullString
		err := tx.QueryRowContext(ctx,
			"SELECT type, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &definition)
		if errors.Is(err, sql.ErrNoRows) {
			rebuild = true
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect migration ledger protection %s: %w", name, err)
		}
		if objectType != "trigger" {
			return fmt.Errorf("migration ledger protection %s has type %q, want trigger", name, objectType)
		}
		if !definition.Valid || normalizeSchemaSQL(definition.String) != normalizeSchemaSQL(migrationLedgerProtectionStatements[index]) {
			rebuild = true
		}
	}
	if rebuild {
		if err := dropMigrationLedgerProtection(ctx, tx); err != nil {
			return err
		}
		if err := protectMigrationLedger(ctx, tx); err != nil {
			return err
		}
	}
	if err := validateMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration ledger protection check: %w", err)
	}
	return nil
}

func validateMigrationLedgerProtection(ctx context.Context, queryer migrationLedgerExecutor) error {
	for index, name := range migrationLedgerProtectionNames() {
		var objectType string
		var definition sql.NullString
		if err := queryer.QueryRowContext(ctx,
			"SELECT type, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &definition); err != nil {
			return fmt.Errorf("validate migration ledger protection %s: %w", name, err)
		}
		if objectType != "trigger" || !definition.Valid ||
			normalizeSchemaSQL(definition.String) != normalizeSchemaSQL(migrationLedgerProtectionStatements[index]) {
			return fmt.Errorf("migration ledger protection %s is incompatible", name)
		}
	}
	return nil
}

func migrationLedgerProtectionNames() []string {
	return []string{"schema_migrations_no_update", "schema_migrations_no_delete"}
}

func dropMigrationLedgerProtection(ctx context.Context, executor execQueryer) error {
	for _, name := range migrationLedgerProtectionNames() {
		if _, err := executor.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return fmt.Errorf("temporarily remove migration ledger protection %s: %w", name, err)
		}
	}
	return nil
}

func validateMigrationDefinitions() error {
	if len(migrations) != latestSchemaVersion {
		return fmt.Errorf("toolstore migration definitions are incompatible: count=%d latest=%d", len(migrations), latestSchemaVersion)
	}
	seenNames := make(map[string]struct{}, len(migrations))
	for index, item := range migrations {
		expectedVersion := index + 1
		if item.version != expectedVersion || strings.TrimSpace(item.name) == "" {
			return fmt.Errorf("toolstore migration definitions are incompatible at version %d", expectedVersion)
		}
		if _, exists := seenNames[item.name]; exists {
			return fmt.Errorf("toolstore migration definitions contain duplicate name %q", item.name)
		}
		seenNames[item.name] = struct{}{}
	}
	return nil
}

func (s *Store) validateMigrationLedger(ctx context.Context) (int, error) {
	return validateMigrationLedgerRows(ctx, s.db, false)
}

func validateMigrationLedgerRows(ctx context.Context, queryer migrationLedgerExecutor, allowEmptyChecksum bool) (int, error) {
	rows, err := queryer.QueryContext(ctx,
		"SELECT version, name, COALESCE(checksum, '') FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()

	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, fmt.Errorf("scan migration ledger: %w", err)
		}
		if applied >= len(migrations) {
			return 0, fmt.Errorf("toolstore migration ledger contains unsupported version %d", version)
		}
		expected := migrations[applied]
		if version != expected.version {
			return 0, fmt.Errorf("toolstore migration ledger is not a contiguous prefix: expected version %d, found %d", expected.version, version)
		}
		if name != expected.name {
			return 0, fmt.Errorf("toolstore migration ledger name mismatch at version %d: expected %q, found %q", version, expected.name, name)
		}
		expectedChecksum := migrationChecksum(expected)
		if checksum != expectedChecksum && !(allowEmptyChecksum && checksum == "") {
			return 0, fmt.Errorf("toolstore migration ledger checksum mismatch at version %d", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate migration ledger: %w", err)
	}
	return applied, nil
}

func migrationChecksum(item migration) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d\x00%s\x00", item.version, item.name)
	for _, statement := range item.statements {
		_, _ = hash.Write([]byte(statement))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (s *Store) applyMigration(ctx context.Context, item migration) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", item.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingName, existingChecksum string
	err = tx.QueryRowContext(ctx,
		"SELECT name, COALESCE(checksum, '') FROM schema_migrations WHERE version = ?", item.version).
		Scan(&existingName, &existingChecksum)
	if err == nil {
		if existingName != item.name || existingChecksum != migrationChecksum(item) {
			return fmt.Errorf("toolstore migration ledger conflict at version %d", item.version)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit existing migration %d: %w", item.version, err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check migration %d: %w", item.version, err)
	}
	for _, statement := range item.statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", item.version, item.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		item.version, item.name, migrationChecksum(item), dbTime(s.clock())); err != nil {
		return fmt.Errorf("record migration %d: %w", item.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", item.version, err)
	}
	return nil
}

type schemaObjectDefinition struct {
	objectType string
	tableName  string
	statement  string
}

// validateAppliedMigrationObjects compares the live schema with a clean schema
// produced by the immutable migration set. This catches a dropped or replaced
// append-only trigger (and any other same-name object) even when the migration
// ledger itself still contains the expected checksums.
func (s *Store) validateAppliedMigrationObjects(ctx context.Context) error {
	expected, err := expectedMigrationObjects(ctx)
	if err != nil {
		return err
	}
	for name, want := range expected {
		var objectType, tableName string
		var statement sql.NullString
		err := s.db.QueryRowContext(ctx,
			"SELECT type, tbl_name, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &tableName, &statement)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("toolstore schema object %q is missing", name)
		}
		if err != nil {
			return fmt.Errorf("inspect toolstore schema object %q: %w", name, err)
		}
		if objectType != want.objectType || tableName != want.tableName || !statement.Valid ||
			normalizeSchemaSQL(statement.String) != normalizeSchemaSQL(want.statement) {
			return fmt.Errorf("toolstore schema object %q is incompatible", name)
		}
	}
	return nil
}

func expectedMigrationObjects(ctx context.Context) (map[string]schemaObjectDefinition, error) {
	reference, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open reference migration schema: %w", err)
	}
	reference.SetMaxOpenConns(1)
	defer reference.Close()

	tx, err := reference.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin reference migration schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range migrations {
		for _, statement := range item.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return nil, fmt.Errorf("build reference migration schema at version %d: %w", item.version, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reference migration schema: %w", err)
	}

	rows, err := reference.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("read reference migration schema: %w", err)
	}
	defer rows.Close()
	objects := make(map[string]schemaObjectDefinition)
	for rows.Next() {
		var objectType, name, tableName, statement string
		if err := rows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			return nil, fmt.Errorf("scan reference migration schema: %w", err)
		}
		objects[name] = schemaObjectDefinition{
			objectType: objectType,
			tableName:  tableName,
			statement:  statement,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reference migration schema: %w", err)
	}
	return objects, nil
}

// normalizeSchemaSQL ignores presentation-only differences while preserving
// quoted string contents, where whitespace and case can change constraints or
// trigger behavior. Older v0.5 development builds used IF NOT EXISTS; SQLite
// may retain that clause in sqlite_schema even though the resulting object is
// otherwise identical.
func normalizeSchemaSQL(statement string) string {
	statement = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
	var normalized strings.Builder
	normalized.Grow(len(statement))
	var quote byte
	for index := 0; index < len(statement); index++ {
		character := statement[index]
		if quote != 0 {
			normalized.WriteByte(character)
			if character == quote {
				if index+1 < len(statement) && statement[index+1] == quote {
					index++
					normalized.WriteByte(statement[index])
					continue
				}
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"', '`':
			quote = character
			normalized.WriteByte(character)
		case ' ', '\t', '\r', '\n', '\f':
			continue
		default:
			if character >= 'A' && character <= 'Z' {
				character += 'a' - 'A'
			}
			normalized.WriteByte(character)
		}
	}
	result := normalized.String()
	for _, replacement := range []struct{ old, new string }{
		{"createuniqueindexifnotexists", "createuniqueindex"},
		{"createindexifnotexists", "createindex"},
		{"createtriggerifnotexists", "createtrigger"},
		{"createtableifnotexists", "createtable"},
	} {
		result = strings.Replace(result, replacement.old, replacement.new, 1)
	}
	return result
}
