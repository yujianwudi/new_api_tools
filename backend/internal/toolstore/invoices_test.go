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
	if group.BlueIssuedMinor != "10000" || group.RedIssuedMinor != "2500" || group.NetIssuedMinor != "7500" ||
		group.EffectiveCount != 2 || group.VoidedCount != 0 {
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
		group.VoidedMinor != "2500" || group.NetIssuedMinor != "10000" || group.EffectiveCount != 1 || group.VoidedCount != 1 {
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
		group.NetIssuedMinor != "9223372036854775807" || group.EffectiveCount != 3 || group.VoidedCount != 1 {
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
