package toolstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInvoiceCreateIsExactIdempotentAndRedactsGenericAudit(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	input := testInvoiceInput("invoice-create-exact", "BLUE-001", InvoiceBlue, 9007199254740993)
	auditInput := testMutationAudit(actionInvoiceCreate)
	auditInput.IdempotencyKey = input.IdempotencyKey

	created, audit, err := store.CreateInvoiceAudited(ctx, input, auditInput)
	if err != nil {
		t.Fatal(err)
	}
	if created.AmountMinor != 9007199254740993 || created.DocumentKind != InvoiceBlue || created.Status != InvoiceIssued {
		t.Fatalf("created invoice = %+v", created)
	}
	if created.CreatedBy != auditInput.Actor || audit.Reason != auditInput.Reason {
		t.Fatalf("create attribution diverged from audit input: document=%+v audit=%+v", created, audit)
	}
	retry, retryAudit, err := store.CreateInvoiceAudited(ctx, input, auditInput)
	if err != nil || retry.ID != created.ID || retryAudit.ID != audit.ID {
		t.Fatalf("idempotent retry = %+v audit=%+v err=%v", retry, retryAudit, err)
	}
	conflicting := input
	conflicting.AmountMinor++
	if _, _, err := store.CreateInvoiceAudited(ctx, conflicting, auditInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrConflict", err)
	}
	detail, err := store.GetInvoice(ctx, created.ID)
	if err != nil || len(detail.Events) != 1 || detail.Events[0].EventType != "created" {
		t.Fatalf("invoice detail = %+v err=%v", detail, err)
	}
	if detail.Events[0].Actor != auditInput.Actor {
		t.Fatalf("created event actor = %q, want %q", detail.Events[0].Actor, auditInput.Actor)
	}
	for label, encoded := range map[string]string{"operation audit": string(audit.AfterJSON), "event": string(detail.Events[0].DetailsJSON)} {
		if strings.Contains(encoded, input.BuyerName) || strings.Contains(encoded, input.BuyerTaxID) {
			t.Fatalf("%s leaked buyer PII: %s", label, encoded)
		}
	}
}

func TestInvoiceSummarySubtractsEffectiveRedDocumentsAndReversesVoidedRed(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	blue := testInvoiceInput("invoice-summary-blue", "BLUE-100", InvoiceBlue, 10000)
	red := testInvoiceInput("invoice-summary-red", "RED-100", InvoiceRed, 2500)
	red.RelatedInvoiceNumber = blue.InvoiceNumber
	blueDoc := createTestInvoice(t, store, blue)
	redDoc := createTestInvoice(t, store, red)

	summary, err := store.InvoiceSummary(ctx, InvoiceSummaryFilter{Currency: "cny"})
	if err != nil || len(summary.Groups) != 1 {
		t.Fatalf("summary = %+v err=%v", summary, err)
	}
	group := summary.Groups[0]
	if group.BlueIssuedMinor != "10000" || group.RedIssuedMinor != "2500" || group.NetIssuedMinor == nil || *group.NetIssuedMinor != "7500" ||
		group.EffectiveCount != 2 || group.VoidedCount != 0 || group.AnomalyCount != 0 || summary.AnomalyCount != 0 {
		t.Fatalf("effective summary = %+v", group)
	}

	voidAudit := testMutationAudit(actionInvoiceVoid)
	voidAudit.IdempotencyKey = "invoice-summary-red-void"
	voided, _, err := store.VoidInvoiceAudited(ctx, InvoiceVoidInput{
		ID: redDoc.ID, Reason: "Original sale was reversed", IdempotencyKey: voidAudit.IdempotencyKey,
		Actor: "finance@example.com",
	}, voidAudit)
	if err != nil || voided.Status != InvoiceVoided || voided.VoidedAt == nil || !voided.VoidedAt.Equal(testNow) {
		t.Fatalf("void red invoice = %+v err=%v", voided, err)
	}
	summary, err = store.InvoiceSummary(ctx, InvoiceSummaryFilter{})
	if err != nil || len(summary.Groups) != 1 {
		t.Fatalf("summary after void = %+v err=%v", summary, err)
	}
	group = summary.Groups[0]
	if group.BlueIssuedMinor != "10000" || group.RedIssuedMinor != "0" || group.VoidedRedMinor != "2500" ||
		group.VoidedMinor != "2500" || group.NetIssuedMinor == nil || *group.NetIssuedMinor != "10000" || group.EffectiveCount != 1 || group.VoidedCount != 1 {
		t.Fatalf("summary after red void = %+v", group)
	}
	if _, _, err := store.VoidInvoiceAudited(ctx, InvoiceVoidInput{
		ID: blueDoc.ID, Reason: "time ordering test", IdempotencyKey: "invoice-invalid-void",
		VoidedAt: blueDoc.IssuedAt.Add(-time.Second), Actor: "finance@example.com",
	}, func() OperationAuditInput {
		audit := testMutationAudit(actionInvoiceVoid)
		audit.IdempotencyKey = "invoice-invalid-void"
		return audit
	}()); !errors.Is(err, ErrConflict) {
		t.Fatalf("void before issue error = %v, want ErrConflict", err)
	}
	futureVoidAudit := testMutationAudit(actionInvoiceVoid)
	futureVoidAudit.IdempotencyKey = "invoice-future-void"
	if _, _, err := store.VoidInvoiceAudited(ctx, InvoiceVoidInput{
		ID: blueDoc.ID, Reason: "spoofed reason", IdempotencyKey: futureVoidAudit.IdempotencyKey,
		VoidedAt: testNow.Add(time.Millisecond), Actor: "spoofed@example.com",
	}, futureVoidAudit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future void error = %v, want ErrInvalid", err)
	}
}

func TestInvoiceAuditInputIsAuthoritativeForAttribution(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	createInput := testInvoiceInput("invoice-attribution-create", "ATTRIBUTION-CREATE", InvoiceBlue, 100)
	createInput.CreatedBy = "spoofed-create@example.com"
	createAudit := testMutationAudit(actionInvoiceCreate)
	createAudit.Actor = "authoritative-create@example.com"
	createAudit.Reason = "authoritative create reason"
	createAudit.IdempotencyKey = createInput.IdempotencyKey
	created, createdAudit, err := store.CreateInvoiceAudited(ctx, createInput, createAudit)
	if err != nil {
		t.Fatal(err)
	}
	createdDetail, err := store.GetInvoice(ctx, created.ID)
	if err != nil || created.CreatedBy != createAudit.Actor || createdAudit.Reason != createAudit.Reason ||
		len(createdDetail.Events) != 1 || createdDetail.Events[0].Actor != createAudit.Actor {
		t.Fatalf("create attribution = document=%+v audit=%+v detail=%+v err=%v", created, createdAudit, createdDetail, err)
	}

	importInput := testInvoiceInput("", "ATTRIBUTION-IMPORT", InvoiceBlue, 200)
	importInput.CreatedBy = "spoofed-import@example.com"
	importAudit := testMutationAudit(actionInvoiceImport)
	importAudit.Actor = "authoritative-import@example.com"
	importAudit.Reason = "authoritative import reason"
	importAudit.IdempotencyKey = "invoice-attribution-import"
	imported, importedAudit, err := store.ImportInvoicesAudited(ctx, []InvoiceDocumentInput{importInput}, importAudit)
	if err != nil || imported.Count != 1 || len(imported.Items) != 1 {
		t.Fatalf("import attribution result=%+v audit=%+v err=%v", imported, importedAudit, err)
	}
	importedDetail, err := store.GetInvoice(ctx, imported.Items[0].ID)
	if err != nil || imported.Items[0].CreatedBy != importAudit.Actor || importedAudit.Reason != importAudit.Reason ||
		len(importedDetail.Events) != 1 || importedDetail.Events[0].Actor != importAudit.Actor {
		t.Fatalf("import attribution = result=%+v audit=%+v detail=%+v err=%v", imported, importedAudit, importedDetail, err)
	}

	voidAudit := testMutationAudit(actionInvoiceVoid)
	voidAudit.Actor = "authoritative-void@example.com"
	voidAudit.Reason = "authoritative void reason"
	voidAudit.IdempotencyKey = "invoice-attribution-void"
	voided, voidedAudit, err := store.VoidInvoiceAudited(ctx, InvoiceVoidInput{
		ID: created.ID, Reason: "spoofed void reason", Actor: "spoofed-void@example.com",
		IdempotencyKey: voidAudit.IdempotencyKey,
	}, voidAudit)
	if err != nil {
		t.Fatal(err)
	}
	voidedDetail, err := store.GetInvoice(ctx, voided.ID)
	if err != nil || voided.VoidReason != voidAudit.Reason || voidedAudit.Reason != voidAudit.Reason ||
		len(voidedDetail.Events) != 2 || voidedDetail.Events[1].Actor != voidAudit.Actor ||
		!strings.Contains(string(voidedDetail.Events[1].DetailsJSON), voidAudit.Reason) ||
		strings.Contains(string(voidedDetail.Events[1].DetailsJSON), "spoofed void reason") {
		t.Fatalf("void attribution = document=%+v audit=%+v detail=%+v err=%v", voided, voidedAudit, voidedDetail, err)
	}
}

func TestInvoiceWritesRollbackOnAuditOrBatchConflict(t *testing.T) {
	t.Run("operation audit failure", func(t *testing.T) {
		store, _ := newTestStore(t)
		injectOperationAuditFailure(t, store)
		input := testInvoiceInput("invoice-audit-failure", "BLUE-FAIL", InvoiceBlue, 500)
		audit := testMutationAudit(actionInvoiceCreate)
		audit.IdempotencyKey = input.IdempotencyKey
		if _, _, err := store.CreateInvoiceAudited(context.Background(), input, audit); err == nil {
			t.Fatal("invoice create unexpectedly survived operation audit failure")
		}
		assertTableCount(t, store, "invoice_documents", 0)
		assertTableCount(t, store, "invoice_events", 0)
		assertTableCount(t, store, "operation_audit", 0)
	})

	t.Run("import duplicate rolls back all rows", func(t *testing.T) {
		store, _ := newTestStore(t)
		first := testInvoiceInput("", "DUPLICATE-CSV", InvoiceBlue, 100)
		second := testInvoiceInput("", "DUPLICATE-CSV", InvoiceBlue, 200)
		audit := testMutationAudit(actionInvoiceImport)
		audit.IdempotencyKey = "invoice-import-duplicate"
		if _, _, err := store.ImportInvoicesAudited(context.Background(), []InvoiceDocumentInput{first, second}, audit); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate import error = %v, want ErrConflict", err)
		}
		assertTableCount(t, store, "invoice_documents", 0)
		assertTableCount(t, store, "invoice_events", 0)
		assertTableCount(t, store, "operation_audit", 0)
	})
}

func TestInvoiceSchemaPreventsEvidenceMutationAndRejectsAmbiguousAmounts(t *testing.T) {
	store, _ := newTestStore(t)
	document := createTestInvoice(t, store, testInvoiceInput("invoice-immutable", "BLUE-IMMUTABLE", InvoiceBlue, 100))
	detail, err := store.GetInvoice(context.Background(), document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE invoice_events SET actor = 'attacker' WHERE id = ?", detail.Events[0].ID); err == nil {
		t.Fatal("invoice event update unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM invoice_events WHERE id = ?", detail.Events[0].ID); err == nil {
		t.Fatal("invoice event delete unexpectedly succeeded")
	}
	if _, err := store.db.Exec("UPDATE invoice_documents SET amount_minor = 1 WHERE id = ?", document.ID); err == nil {
		t.Fatal("invoice financial mutation unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM invoice_documents WHERE id = ?", document.ID); err == nil {
		t.Fatal("invoice document delete unexpectedly succeeded")
	}

	invalidScale := testInvoiceInput("invoice-invalid-scale", "SCALE-10", InvoiceBlue, 100)
	invalidScale.MinorUnitScale = 10
	audit := testMutationAudit(actionInvoiceCreate)
	audit.IdempotencyKey = invalidScale.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), invalidScale, audit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scale 10 error = %v, want ErrInvalid", err)
	}
	invalidRed := testInvoiceInput("invoice-invalid-red", "RED-NO-RELATION", InvoiceRed, 100)
	audit.IdempotencyKey = invalidRed.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), invalidRed, audit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unrelated red invoice error = %v, want ErrInvalid", err)
	}
	selfRed := testInvoiceInput("invoice-self-red", "Red-Self-1", InvoiceRed, 100)
	selfRed.RelatedInvoiceNumber = "red-self-1"
	audit.IdempotencyKey = selfRed.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), selfRed, audit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("case-insensitive self-related red invoice error = %v, want ErrInvalid", err)
	}
	if _, err := store.db.Exec(`INSERT INTO invoice_documents(
		invoice_number, seller_entity, buyer_name, buyer_tax_id, document_kind, related_invoice_number, currency,
		amount_minor, tax_amount_minor, minor_unit_scale, status, source, idempotency_key, request_fingerprint,
		issued_at, voided_at, void_reason, created_by, created_at, updated_at
	) VALUES (?, ?, ?, '', 'red', ?, 'CNY', 100, 0, 2, 'issued', 'manual', ?, ?, ?, NULL, '', ?, ?, ?)`,
		"  DIRECT-SELF", "Example Seller", "Private Buyer", "direct-self  ", "invoice-direct-self",
		strings.Repeat("a", 64), dbTime(testNow.Add(-time.Hour)), "admin@example.com", dbTime(testNow), dbTime(testNow)); err == nil {
		t.Fatal("database accepted a case-insensitive, whitespace-padded red self-reference")
	}
	future := testInvoiceInput("invoice-future-create", "FUTURE-CREATE", InvoiceBlue, 100)
	future.IssuedAt = testNow.Add(time.Millisecond)
	audit.IdempotencyKey = future.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), future, audit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future invoice create error = %v, want ErrInvalid", err)
	}
	importAudit := testMutationAudit(actionInvoiceImport)
	importAudit.IdempotencyKey = "invoice-future-import"
	if _, _, err := store.ImportInvoicesAudited(context.Background(), []InvoiceDocumentInput{future}, importAudit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future invoice import error = %v, want ErrInvalid", err)
	}
}

func TestInvoiceNumberUniquenessIsCaseInsensitiveWithinSeller(t *testing.T) {
	store, _ := newTestStore(t)
	first := testInvoiceInput("invoice-case-first", "Case-Number-1", InvoiceBlue, 100)
	createTestInvoice(t, store, first)
	duplicate := testInvoiceInput("invoice-case-duplicate", "case-number-1", InvoiceBlue, 100)
	duplicate.SellerEntity = strings.ToLower(first.SellerEntity)
	audit := testMutationAudit(actionInvoiceCreate)
	audit.IdempotencyKey = duplicate.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), duplicate, audit); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-insensitive duplicate error = %v, want ErrConflict", err)
	}
}

func TestInvoiceSummaryUsesExactBigIntegersBeyondSQLiteSumRange(t *testing.T) {
	store, _ := newTestStore(t)
	const maximum = int64(^uint64(0) >> 1)
	blueOne := testInvoiceInput("invoice-big-blue-one", "BIG-BLUE-1", InvoiceBlue, maximum)
	audit := testMutationAudit(actionInvoiceCreate)
	audit.IdempotencyKey = blueOne.IdempotencyKey
	document, outcome, err := store.CreateInvoiceAudited(context.Background(), blueOne, audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(outcome.AfterJSON), `"amount_minor":"9223372036854775807"`) {
		t.Fatalf("large audit amount was not a decimal string: %s", outcome.AfterJSON)
	}
	detail, err := store.GetInvoice(context.Background(), document.ID)
	if err != nil || len(detail.Events) != 1 ||
		!strings.Contains(string(detail.Events[0].DetailsJSON), `"amount_minor":"9223372036854775807"`) {
		t.Fatalf("large event amount was not exact: detail=%+v err=%v", detail, err)
	}
	createTestInvoice(t, store, testInvoiceInput("invoice-big-blue-two", "BIG-BLUE-2", InvoiceBlue, maximum))
	red := testInvoiceInput("invoice-big-red", "BIG-RED-1", InvoiceRed, maximum)
	red.RelatedInvoiceNumber = blueOne.InvoiceNumber
	createTestInvoice(t, store, red)
	voidedBlue := createTestInvoice(t, store, testInvoiceInput("invoice-big-voided", "BIG-BLUE-VOID", InvoiceBlue, maximum))
	voidAudit := testMutationAudit(actionInvoiceVoid)
	voidAudit.IdempotencyKey = "invoice-big-void-operation"
	if _, _, err := store.VoidInvoiceAudited(context.Background(), InvoiceVoidInput{
		ID: voidedBlue.ID, Reason: "void large invoice", IdempotencyKey: voidAudit.IdempotencyKey,
		Actor: "finance@example.com",
	}, voidAudit); err != nil {
		t.Fatal(err)
	}
	summary, err := store.InvoiceSummary(context.Background(), InvoiceSummaryFilter{})
	if err != nil || len(summary.Groups) != 1 {
		t.Fatalf("large summary = %+v err=%v", summary, err)
	}
	group := summary.Groups[0]
	if group.BlueIssuedMinor != "18446744073709551614" || group.RedIssuedMinor != "9223372036854775807" ||
		group.VoidedBlueMinor != "9223372036854775807" || group.VoidedMinor != "9223372036854775807" ||
		group.NetIssuedMinor == nil || *group.NetIssuedMinor != "9223372036854775807" || group.EffectiveCount != 3 || group.VoidedCount != 1 {
		t.Fatalf("large exact summary = %+v", group)
	}
}

func TestConcurrentInvoiceVoidProducesOneEventAndOneAuditChain(t *testing.T) {
	first, path := newTestStore(t)
	second, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	document := createTestInvoice(t, first, testInvoiceInput("invoice-concurrent-create", "BLUE-CONCURRENT", InvoiceBlue, 100))
	stores := []*Store{first, second}
	keys := []string{"invoice-concurrent-void-a", "invoice-concurrent-void-b"}
	start := make(chan struct{})
	errorsFound := make(chan error, len(stores))
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			audit := testMutationAudit(actionInvoiceVoid)
			audit.IdempotencyKey = keys[index]
			_, _, callErr := stores[index].VoidInvoiceAudited(context.Background(), InvoiceVoidInput{
				ID: document.ID, Reason: "concurrent finance void", IdempotencyKey: keys[index],
				Actor: "finance@example.com",
			}, audit)
			errorsFound <- callErr
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	succeeded, conflicted := 0, 0
	for callErr := range errorsFound {
		switch {
		case callErr == nil:
			succeeded++
		case errors.Is(callErr, ErrConflict):
			conflicted++
		default:
			t.Fatalf("concurrent void error = %v", callErr)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent void succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	thirdAudit := testMutationAudit(actionInvoiceVoid)
	thirdAudit.IdempotencyKey = "invoice-concurrent-void-c"
	if _, _, err := first.VoidInvoiceAudited(context.Background(), InvoiceVoidInput{
		ID: document.ID, Reason: "different retry key", IdempotencyKey: thirdAudit.IdempotencyKey,
		Actor: "finance@example.com",
	}, thirdAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeated void error = %v, want ErrConflict", err)
	}
	assertTableCount(t, first, "invoice_events", 2)
	assertTableCount(t, first, "operation_audit", 4)
}

func TestInvoiceCreateAndImportMapCrossStoreContentionToConflict(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Store) error
	}{
		{
			name: "create",
			call: func(store *Store) error {
				input := testInvoiceInput("invoice-locked-create", "LOCKED-CREATE", InvoiceBlue, 100)
				audit := testMutationAudit(actionInvoiceCreate)
				audit.IdempotencyKey = input.IdempotencyKey
				_, _, err := store.CreateInvoiceAudited(context.Background(), input, audit)
				return err
			},
		},
		{
			name: "import",
			call: func(store *Store) error {
				input := testInvoiceInput("", "LOCKED-IMPORT", InvoiceBlue, 100)
				audit := testMutationAudit(actionInvoiceImport)
				audit.IdempotencyKey = "invoice-locked-import"
				_, _, err := store.ImportInvoicesAudited(context.Background(), []InvoiceDocumentInput{input}, audit)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			locker, path := newTestStore(t)
			contender, err := Init(path)
			if err != nil {
				t.Fatal(err)
			}
			defer contender.Close()
			if _, err := contender.db.Exec("PRAGMA busy_timeout = 1"); err != nil {
				t.Fatal(err)
			}
			lockTx, err := locker.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lockTx.Rollback()
			if _, err := lockTx.Exec(`INSERT INTO support_notes(
				subject_type, subject_id, author, body, visibility, idempotency_key,
				created_at, updated_at, deleted_at
			) VALUES ('lock', 'invoice', 'test', 'hold write lock', 'internal', NULL, ?, ?, NULL)`,
				dbTime(testNow), dbTime(testNow)); err != nil {
				t.Fatal(err)
			}
			if err := test.call(contender); !errors.Is(err, ErrConflict) {
				t.Fatalf("cross-store contention error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestRedInvoiceRequiresVerifiedOriginalAndEnforcesExactCumulativeLimit(t *testing.T) {
	store, _ := newTestStore(t)
	blue := createTestInvoice(t, store, testInvoiceInput("red-rules-blue", "BLUE-RULES", InvoiceBlue, 100))
	blueID := blue.ID

	tests := []struct {
		name  string
		input InvoiceDocumentInput
	}{
		{name: "orphan number", input: func() InvoiceDocumentInput {
			item := testInvoiceInput("red-orphan", "RED-ORPHAN", InvoiceRed, 10)
			item.RelatedInvoiceNumber = "MISSING-BLUE"
			return item
		}()},
		{name: "cross seller", input: func() InvoiceDocumentInput {
			item := testInvoiceInput("red-cross-seller", "RED-SELLER", InvoiceRed, 10)
			item.SellerEntity = "Different Seller"
			item.RelatedInvoiceID = &blueID
			return item
		}()},
		{name: "cross currency", input: func() InvoiceDocumentInput {
			item := testInvoiceInput("red-cross-currency", "RED-CURRENCY", InvoiceRed, 10)
			item.Currency = "USD"
			item.RelatedInvoiceID = &blueID
			return item
		}()},
		{name: "cross precision", input: func() InvoiceDocumentInput {
			item := testInvoiceInput("red-cross-scale", "RED-SCALE", InvoiceRed, 10)
			item.MinorUnitScale = 3
			item.RelatedInvoiceID = &blueID
			return item
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audit := testMutationAudit(actionInvoiceCreate)
			audit.IdempotencyKey = tt.input.IdempotencyKey
			if _, _, err := store.CreateInvoiceAudited(context.Background(), tt.input, audit); !errors.Is(err, ErrConflict) {
				t.Fatalf("CreateInvoiceAudited() error = %v, want ErrConflict", err)
			}
		})
	}

	first := testInvoiceInput("red-exact-first", "RED-EXACT-60", InvoiceRed, 60)
	first.RelatedInvoiceNumber = blue.InvoiceNumber
	firstDoc := createTestInvoice(t, store, first)
	if firstDoc.RelatedInvoiceID == nil || *firstDoc.RelatedInvoiceID != blue.ID ||
		firstDoc.RelationState != InvoiceRelationVerified {
		t.Fatalf("first red relation = %+v", firstDoc)
	}
	second := testInvoiceInput("red-exact-second", "RED-EXACT-40", InvoiceRed, 40)
	second.RelatedInvoiceID = &blueID
	secondDoc := createTestInvoice(t, store, second)
	if secondDoc.RelatedInvoiceID == nil || *secondDoc.RelatedInvoiceID != blue.ID {
		t.Fatalf("second red relation = %+v", secondDoc)
	}

	redID := firstDoc.ID
	redToRed := testInvoiceInput("red-points-red", "RED-POINTS-RED", InvoiceRed, 1)
	redToRed.RelatedInvoiceID = &redID
	redAudit := testMutationAudit(actionInvoiceCreate)
	redAudit.IdempotencyKey = redToRed.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), redToRed, redAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("red-to-red error = %v, want ErrConflict", err)
	}

	overage := testInvoiceInput("red-over-one", "RED-OVER-ONE", InvoiceRed, 1)
	overage.RelatedInvoiceID = &blueID
	overageAudit := testMutationAudit(actionInvoiceCreate)
	overageAudit.IdempotencyKey = overage.IdempotencyKey
	if _, _, err := store.CreateInvoiceAudited(context.Background(), overage, overageAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("one-minor-unit overage error = %v, want ErrConflict", err)
	}

	summary, err := store.InvoiceSummary(context.Background(), InvoiceSummaryFilter{})
	if err != nil || len(summary.Groups) != 1 {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	group := summary.Groups[0]
	if group.NetIssuedMinor == nil || *group.NetIssuedMinor != "0" || group.SourceHealth != "ok" ||
		group.RedIssuedMinor != "100" || group.AnomalyCount != 0 || summary.SourceHealth != "ok" || summary.AnomalyCount != 0 {
		t.Fatalf("exact cumulative summary = %+v overall=%+v", group, summary)
	}
}

func TestVoidingOriginalBlueInvalidatesRelationsAndMakesNetUnreconciled(t *testing.T) {
	store, _ := newTestStore(t)
	blue := createTestInvoice(t, store, testInvoiceInput("void-original-blue", "BLUE-VOID-REL", InvoiceBlue, 100))
	red := testInvoiceInput("void-original-red", "RED-VOID-REL", InvoiceRed, 20)
	red.RelatedInvoiceNumber = blue.InvoiceNumber
	redDoc := createTestInvoice(t, store, red)

	voidAudit := testMutationAudit(actionInvoiceVoid)
	voidAudit.IdempotencyKey = "void-original-operation"
	if _, _, err := store.VoidInvoiceAudited(context.Background(), InvoiceVoidInput{
		ID: blue.ID, Reason: "original blue invoice was voided", IdempotencyKey: voidAudit.IdempotencyKey,
		Actor: "finance@example.com",
	}, voidAudit); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.GetInvoice(context.Background(), redDoc.ID)
	if err != nil || reloaded.Document.RelationState != InvoiceRelationUnreconciled ||
		reloaded.Document.RelationReason != "original_voided" {
		t.Fatalf("invalidated red relation = %+v, %v", reloaded, err)
	}
	summary, err := store.InvoiceSummary(context.Background(), InvoiceSummaryFilter{})
	if err != nil || len(summary.Groups) != 1 {
		t.Fatalf("summary after original void = %+v, %v", summary, err)
	}
	group := summary.Groups[0]
	if group.NetIssuedMinor != nil || group.SourceHealth != "unreconciled" || group.UnreconciledCount != 1 ||
		group.AnomalyCount != 1 || group.VoidedBlueMinor != "100" || group.RedIssuedMinor != "20" ||
		summary.SourceHealth != "unreconciled" || summary.AnomalyCount != 1 {
		t.Fatalf("unreconciled original-void summary = %+v overall=%+v", group, summary)
	}
}

func TestConcurrentRedInvoicesAcrossStoresCannotExceedOriginal(t *testing.T) {
	first, path := newTestStore(t)
	second, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	blue := createTestInvoice(t, first, testInvoiceInput("concurrent-red-blue", "BLUE-CONCURRENT-RED", InvoiceBlue, 100))

	stores := []*Store{first, second}
	keys := []string{"concurrent-red-a", "concurrent-red-b"}
	numbers := []string{"RED-CONCURRENT-A", "RED-CONCURRENT-B"}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			input := testInvoiceInput(keys[index], numbers[index], InvoiceRed, 60)
			input.RelatedInvoiceNumber = blue.InvoiceNumber
			audit := testMutationAudit(actionInvoiceCreate)
			audit.IdempotencyKey = input.IdempotencyKey
			_, _, callErr := stores[index].CreateInvoiceAudited(context.Background(), input, audit)
			results <- callErr
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, rejected := 0, 0
	for callErr := range results {
		switch {
		case callErr == nil:
			succeeded++
		case errors.Is(callErr, ErrConflict):
			rejected++
		default:
			t.Fatalf("concurrent red error = %v", callErr)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent red succeeded=%d rejected=%d", succeeded, rejected)
	}
	summary, err := first.InvoiceSummary(context.Background(), InvoiceSummaryFilter{})
	if err != nil || len(summary.Groups) != 1 || summary.Groups[0].NetIssuedMinor == nil ||
		*summary.Groups[0].NetIssuedMinor != "40" || summary.Groups[0].RedIssuedMinor != "60" {
		t.Fatalf("concurrent red summary = %+v, %v", summary, err)
	}
}

func testInvoiceInput(key, number string, kind InvoiceDocumentKind, amount int64) InvoiceDocumentInput {
	return InvoiceDocumentInput{
		InvoiceNumber: number, SellerEntity: "Example Seller", BuyerName: "Private Buyer",
		BuyerTaxID: "91310000PRIVATE", DocumentKind: kind, Currency: "CNY",
		AmountMinor: amount, TaxAmountMinor: amount / 10, MinorUnitScale: 2,
		Source: "manual", IdempotencyKey: key, IssuedAt: testNow.Add(-time.Hour),
		CreatedBy: "finance@example.com",
	}
}

func createTestInvoice(t *testing.T, store *Store, input InvoiceDocumentInput) InvoiceDocument {
	t.Helper()
	audit := testMutationAudit(actionInvoiceCreate)
	audit.IdempotencyKey = input.IdempotencyKey
	document, _, err := store.CreateInvoiceAudited(context.Background(), input, audit)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
