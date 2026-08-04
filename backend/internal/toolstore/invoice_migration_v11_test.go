package toolstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type legacyInvoiceRow struct {
	ID                   int64
	InvoiceNumber        string
	SellerEntity         string
	DocumentKind         InvoiceDocumentKind
	RelatedInvoiceNumber string
	Currency             string
	AmountMinor          int64
	TaxAmountMinor       int64
	MinorUnitScale       int
	Status               InvoiceStatus
	IssuedAt             time.Time
}

func TestInvoiceMigrationV10ToV11PreservesEvidenceAndFailsClosed(t *testing.T) {
	validIssuedAt := testNow.Add(-4 * time.Hour)
	unreconciledIssuedAt := testNow.Add(-time.Hour)
	path := createVersion10InvoiceFixture(t, []legacyInvoiceRow{
		{ID: 1, InvoiceNumber: "BLUE-HISTORICAL", SellerEntity: "Example Seller", DocumentKind: InvoiceBlue,
			Currency: "CNY", AmountMinor: 1000, TaxAmountMinor: 100, MinorUnitScale: 2, Status: InvoiceIssued, IssuedAt: validIssuedAt},
		{ID: 2, InvoiceNumber: "RED-HISTORICAL-VALID", SellerEntity: "Example Seller", DocumentKind: InvoiceRed,
			RelatedInvoiceNumber: "BLUE-HISTORICAL", Currency: "CNY", AmountMinor: 250, TaxAmountMinor: 25,
			MinorUnitScale: 2, Status: InvoiceIssued, IssuedAt: validIssuedAt},
		{ID: 3, InvoiceNumber: "RED-HISTORICAL-ORPHAN", SellerEntity: "Example Seller", DocumentKind: InvoiceRed,
			RelatedInvoiceNumber: "BLUE-MISSING", Currency: "CNY", AmountMinor: 50, TaxAmountMinor: 5,
			MinorUnitScale: 2, Status: InvoiceIssued, IssuedAt: unreconciledIssuedAt},
		{ID: 4, InvoiceNumber: "RED-HISTORICAL-CROSS-SELLER", SellerEntity: "Other Seller", DocumentKind: InvoiceRed,
			RelatedInvoiceNumber: "BLUE-HISTORICAL", Currency: "CNY", AmountMinor: 75, TaxAmountMinor: 7,
			MinorUnitScale: 2, Status: InvoiceIssued, IssuedAt: unreconciledIssuedAt},
	})

	before := readLegacyInvoiceRows(t, path)
	store, err := Init(path)
	if err != nil {
		t.Fatalf("Init(v10 fixture) error = %v", err)
	}
	defer store.Close()

	after := readLegacyInvoiceRowsFromDB(t, store.db)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("v10 to v11 rewrote invoice evidence:\n before=%+v\n after=%+v", before, after)
	}

	cutoff := testNow.Add(-2 * time.Hour)
	trusted, err := store.InvoiceSummary(context.Background(), InvoiceSummaryFilter{IssuedTo: &cutoff})
	if err != nil || len(trusted.Groups) != 1 {
		t.Fatalf("trusted historical summary = %+v, %v", trusted, err)
	}
	trustedGroup := trusted.Groups[0]
	if trustedGroup.NetIssuedMinor == nil || *trustedGroup.NetIssuedMinor != "750" ||
		trustedGroup.BlueIssuedMinor != "1000" || trustedGroup.RedIssuedMinor != "250" ||
		trustedGroup.SourceHealth != "ok" || trusted.SourceHealth != "ok" {
		t.Fatalf("trusted historical net changed = %+v overall=%+v", trustedGroup, trusted)
	}

	overall, err := store.InvoiceSummary(context.Background(), InvoiceSummaryFilter{})
	if err != nil || len(overall.Groups) != 1 {
		t.Fatalf("overall historical summary = %+v, %v", overall, err)
	}
	overallGroup := overall.Groups[0]
	if overallGroup.NetIssuedMinor != nil || overallGroup.SourceHealth != "unreconciled" ||
		overallGroup.UnreconciledCount != 2 || overallGroup.RedIssuedMinor != "375" ||
		overall.SourceHealth != "unreconciled" || overall.UnreconciledCount != 2 {
		t.Fatalf("unreconciled historical summary did not fail closed = %+v overall=%+v", overallGroup, overall)
	}

	type relation struct {
		redID     int64
		relatedID sql.NullInt64
		state     string
		reason    string
	}
	rows, err := store.db.Query(`SELECT red_invoice_id, related_invoice_id, relation_state, reason_code
		FROM invoice_document_relations ORDER BY red_invoice_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	gotRelations := make([]relation, 0, 3)
	for rows.Next() {
		var item relation
		if err := rows.Scan(&item.redID, &item.relatedID, &item.state, &item.reason); err != nil {
			t.Fatal(err)
		}
		gotRelations = append(gotRelations, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(gotRelations) != 3 || gotRelations[0].redID != 2 || !gotRelations[0].relatedID.Valid ||
		gotRelations[0].relatedID.Int64 != 1 || gotRelations[0].state != "verified" || gotRelations[0].reason != "" ||
		gotRelations[1].redID != 3 || gotRelations[1].relatedID.Valid || gotRelations[1].state != "unreconciled" ||
		gotRelations[1].reason != "original_not_found" || gotRelations[2].redID != 4 ||
		gotRelations[2].relatedID.Valid || gotRelations[2].state != "unreconciled" ||
		gotRelations[2].reason != "original_not_found" {
		t.Fatalf("v11 historical relation backfill = %+v", gotRelations)
	}
}

func TestInvoiceRelationV11TriggersRejectIdentityMutationAndDeletion(t *testing.T) {
	store, _ := newTestStore(t)
	blue := createTestInvoice(t, store, testInvoiceInput("relation-trigger-blue", "BLUE-RELATION-TRIGGER", InvoiceBlue, 100))
	otherBlue := createTestInvoice(t, store, testInvoiceInput("relation-trigger-other", "BLUE-RELATION-OTHER", InvoiceBlue, 100))
	redInput := testInvoiceInput("relation-trigger-red", "RED-RELATION-TRIGGER", InvoiceRed, 25)
	redInput.RelatedInvoiceID = &blue.ID
	red := createTestInvoice(t, store, redInput)

	mutations := []struct {
		name      string
		statement string
		args      []any
	}{
		{name: "retarget original", statement: `UPDATE invoice_document_relations SET related_invoice_id = ? WHERE red_invoice_id = ?`, args: []any{otherBlue.ID, red.ID}},
		{name: "rewrite number snapshot", statement: `UPDATE invoice_document_relations SET related_invoice_number_snapshot = 'TAMPERED' WHERE red_invoice_id = ?`, args: []any{red.ID}},
		{name: "rewrite seller snapshot", statement: `UPDATE invoice_document_relations SET seller_entity_snapshot = 'Attacker' WHERE red_invoice_id = ?`, args: []any{red.ID}},
		{name: "delete relation", statement: `DELETE FROM invoice_document_relations WHERE red_invoice_id = ?`, args: []any{red.ID}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if _, err := store.db.Exec(mutation.statement, mutation.args...); err == nil {
				t.Fatalf("relation tampering unexpectedly succeeded: %s", mutation.statement)
			}
		})
	}

	if _, err := store.db.Exec(`INSERT INTO invoice_document_relations(
		red_invoice_id, related_invoice_id, relation_state, reason_code,
		related_invoice_number_snapshot, seller_entity_snapshot, currency_snapshot,
		minor_unit_scale_snapshot, created_at, updated_at
	) VALUES (?, NULL, 'unreconciled', 'forged', 'FORGED', 'Example Seller', 'CNY', 2, ?, ?)`,
		otherBlue.ID, dbTime(testNow), dbTime(testNow)); err == nil {
		t.Fatal("relation identity trigger accepted a blue document as a red relation")
	}

	var relatedID int64
	var state, reason, number, seller string
	if err := store.db.QueryRow(`SELECT related_invoice_id, relation_state, reason_code,
		related_invoice_number_snapshot, seller_entity_snapshot
		FROM invoice_document_relations WHERE red_invoice_id = ?`, red.ID).
		Scan(&relatedID, &state, &reason, &number, &seller); err != nil {
		t.Fatal(err)
	}
	if relatedID != blue.ID || state != "verified" || reason != "" ||
		number != blue.InvoiceNumber || seller != red.SellerEntity {
		t.Fatalf("relation changed after rejected tampering: related=%d state=%q reason=%q number=%q seller=%q",
			relatedID, state, reason, number, seller)
	}
}

func TestVersion10OnlineBackupRestoresOldReadableSchemaAfterV11Upgrade(t *testing.T) {
	path := createVersion10InvoiceFixture(t, []legacyInvoiceRow{
		{ID: 1, InvoiceNumber: "BLUE-V10-BACKUP", SellerEntity: "Example Seller", DocumentKind: InvoiceBlue,
			Currency: "CNY", AmountMinor: 1234, TaxAmountMinor: 123, MinorUnitScale: 2,
			Status: InvoiceIssued, IssuedAt: testNow.Add(-time.Hour)},
		{ID: 2, InvoiceNumber: "RED-V10-BACKUP", SellerEntity: "Example Seller", DocumentKind: InvoiceRed,
			RelatedInvoiceNumber: "BLUE-V10-BACKUP", Currency: "CNY", AmountMinor: 234, TaxAmountMinor: 23,
			MinorUnitScale: 2, Status: InvoiceIssued, IssuedAt: testNow.Add(-time.Hour)},
	})
	wantRows := readLegacyInvoiceRows(t, path)
	backupPath := filepath.Join(t.TempDir(), "rollback", "toolstore-v10.db")
	metadata, err := OnlineBackup(context.Background(), path, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SchemaVersion != 10 || metadata.Integrity != "ok" || len(metadata.SHA256) != 64 {
		t.Fatalf("v10 backup metadata = %+v", metadata)
	}

	upgraded, err := Init(path)
	if err != nil {
		t.Fatalf("upgrade source to v11: %v", err)
	}
	if health, err := upgraded.Health(context.Background()); err != nil || health.SchemaVersion != latestSchemaVersion {
		_ = upgraded.Close()
		t.Fatalf("upgraded source health = %+v, %v", health, err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreBackup(context.Background(), backupPath, path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.SchemaVersion != 10 || restored.Integrity != "ok" {
		t.Fatalf("restored v10 metadata = %+v", restored)
	}

	db, err := openRawToolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var schemaVersion, relationTables int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'table' AND name = 'invoice_document_relations'`).Scan(&relationTables); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 10 || relationTables != 0 {
		t.Fatalf("restored rollback boundary schema version=%d relation_tables=%d", schemaVersion, relationTables)
	}
	if gotRows := readLegacyInvoiceRowsFromDB(t, db); !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("restored v10 invoice rows changed: got=%+v want=%+v", gotRows, wantRows)
	}

	columns := readTableColumns(t, db, "invoice_documents")
	wantColumns := []string{
		"id", "invoice_number", "seller_entity", "buyer_name", "buyer_tax_id", "document_kind",
		"related_invoice_number", "currency", "amount_minor", "tax_amount_minor", "minor_unit_scale",
		"status", "source", "idempotency_key", "request_fingerprint", "issued_at", "voided_at",
		"void_reason", "created_by", "created_at", "updated_at",
	}
	if !reflect.DeepEqual(columns, wantColumns) {
		t.Fatalf("restored v10 invoice_documents columns = %v, want %v", columns, wantColumns)
	}
}

func createVersion10InvoiceFixture(t *testing.T, invoices []legacyInvoiceRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolstore-v10.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(migrationLedgerCreateStatement); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:10] {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range item.statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				t.Fatalf("apply v10 fixture migration %d: %v", item.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
			VALUES (?, ?, ?, ?)`, item.version, item.name, migrationChecksum(item), dbTime(testNow)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("record v10 fixture migration %d: %v", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := protectMigrationLedger(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for index, invoice := range invoices {
		if invoice.ID <= 0 || invoice.Status == "" || invoice.IssuedAt.IsZero() {
			t.Fatalf("invalid legacy invoice fixture at index %d: %+v", index, invoice)
		}
		fingerprint := fmt.Sprintf("%064x", invoice.ID)
		if _, err := db.Exec(`INSERT INTO invoice_documents(
			id, invoice_number, seller_entity, buyer_name, buyer_tax_id, document_kind,
			related_invoice_number, currency, amount_minor, tax_amount_minor, minor_unit_scale,
			status, source, idempotency_key, request_fingerprint, issued_at, voided_at,
			void_reason, created_by, created_at, updated_at
		) VALUES (?, ?, ?, 'Historical Buyer', '', ?, ?, ?, ?, ?, ?, ?, 'manual', ?, ?, ?, NULL, '', 'migration-test', ?, ?)`,
			invoice.ID, invoice.InvoiceNumber, invoice.SellerEntity, invoice.DocumentKind,
			invoice.RelatedInvoiceNumber, invoice.Currency, invoice.AmountMinor, invoice.TaxAmountMinor,
			invoice.MinorUnitScale, invoice.Status, "legacy-invoice-"+fmt.Sprint(invoice.ID), fingerprint,
			dbTime(invoice.IssuedAt), dbTime(testNow), dbTime(testNow)); err != nil {
			t.Fatalf("insert legacy invoice %d: %v", invoice.ID, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLegacyInvoiceRows(t *testing.T, path string) []legacyInvoiceRow {
	t.Helper()
	db, err := openRawToolStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return readLegacyInvoiceRowsFromDB(t, db)
}

func readLegacyInvoiceRowsFromDB(t *testing.T, db *sql.DB) []legacyInvoiceRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, invoice_number, seller_entity, document_kind,
		related_invoice_number, currency, amount_minor, tax_amount_minor, minor_unit_scale, status, issued_at
		FROM invoice_documents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make([]legacyInvoiceRow, 0)
	for rows.Next() {
		var item legacyInvoiceRow
		var issuedAt int64
		if err := rows.Scan(&item.ID, &item.InvoiceNumber, &item.SellerEntity, &item.DocumentKind,
			&item.RelatedInvoiceNumber, &item.Currency, &item.AmountMinor, &item.TaxAmountMinor,
			&item.MinorUnitScale, &item.Status, &issuedAt); err != nil {
			t.Fatal(err)
		}
		item.IssuedAt = fromDBTime(issuedAt)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func readTableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	if table != "invoice_documents" {
		t.Fatalf("unsupported test table %q", table)
	}
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typeName string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}
