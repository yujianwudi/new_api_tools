package toolstore

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRiskCaseEventIdempotency(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-event-idempotency", Title: "Repeated evidence callback",
		SubjectType: "request", SubjectID: "req-1", Severity: RiskSeverityMedium,
		Status: RiskCaseOpen,
	})
	if err != nil {
		t.Fatal(err)
	}

	input := RiskCaseEventInput{
		CaseID: created.ID, EventType: "evidence_attached", Actor: "risk-engine",
		DetailsJSON:    json.RawMessage(`{"request_ids":["req-1"]}`),
		IdempotencyKey: "risk-event-1", OccurredAt: testNow.Add(-1),
	}
	first, err := store.AppendRiskCaseEvent(ctx, input)
	if err != nil {
		t.Fatalf("AppendRiskCaseEvent() error = %v", err)
	}
	retry, err := store.AppendRiskCaseEvent(ctx, input)
	if err != nil {
		t.Fatalf("idempotent AppendRiskCaseEvent() error = %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry ID = %d, want %d", retry.ID, first.ID)
	}
	if retry.IdempotencyKey != input.IdempotencyKey {
		t.Fatalf("retry idempotency key = %q, want %q", retry.IdempotencyKey, input.IdempotencyKey)
	}
	byKey, err := store.GetRiskCaseEventByIdempotencyKey(ctx, input.IdempotencyKey)
	if err != nil || byKey.ID != first.ID {
		t.Fatalf("GetRiskCaseEventByIdempotencyKey() = %+v, %v", byKey, err)
	}

	conflicting := input
	conflicting.DetailsJSON = json.RawMessage(`{"request_ids":["req-2"]}`)
	if _, err := store.AppendRiskCaseEvent(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused event idempotency key error = %v, want ErrConflict", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM risk_case_events WHERE idempotency_key = ?", input.IdempotencyKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("risk event count = %d, want 1", count)
	}
}

func TestAppendRiskCaseEventAuditedBindsEventAndAuditIdempotency(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	created, err := store.CreateRiskCase(ctx, RiskCaseInput{
		CaseKey: "risk-event-audit-binding", Title: "Bound evidence audit",
		SubjectType: "request", SubjectID: "req-bound", Severity: RiskSeverityHigh,
		Status: RiskCaseOpen,
	})
	if err != nil {
		t.Fatal(err)
	}

	mismatchedAudit := testMutationAudit(actionRiskCaseEventAppend)
	mismatchedAudit.IdempotencyKey = "audit-key"
	_, _, err = store.AppendRiskCaseEventAudited(ctx, RiskCaseEventInput{
		CaseID: created.ID, EventType: "evidence_attached", Actor: "risk-engine",
		IdempotencyKey: "event-key",
	}, mismatchedAudit)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched event/audit keys error = %v, want ErrInvalid", err)
	}
	assertTableCount(t, store, "risk_case_events", 0)
	assertTableCount(t, store, "operation_audit", 0)

	orphanInput := RiskCaseEventInput{
		CaseID: created.ID, EventType: "evidence_attached", Actor: "risk-engine",
		DetailsJSON:    json.RawMessage(`{"request_id":"req-orphan"}`),
		IdempotencyKey: "orphan-event",
	}
	if _, err := store.AppendRiskCaseEvent(ctx, orphanInput); err != nil {
		t.Fatal(err)
	}
	orphanAudit := testMutationAudit(actionRiskCaseEventAppend)
	orphanAudit.IdempotencyKey = orphanInput.IdempotencyKey
	if _, _, err := store.AppendRiskCaseEventAudited(ctx, orphanInput, orphanAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphaned event audited append error = %v, want ErrConflict", err)
	}
	assertTableCount(t, store, "risk_case_events", 1)
	assertTableCount(t, store, "operation_audit", 0)

	boundAudit := testMutationAudit(actionRiskCaseEventAppend)
	boundAudit.IdempotencyKey = "bound-event"
	boundEvent, audit, err := store.AppendRiskCaseEventAudited(ctx, RiskCaseEventInput{
		CaseID: created.ID, EventType: "case_reviewed", Actor: "risk-engine",
	}, boundAudit)
	if err != nil {
		t.Fatal(err)
	}
	if boundEvent.IdempotencyKey != boundAudit.IdempotencyKey || audit.IdempotencyKey != boundAudit.IdempotencyKey {
		t.Fatalf("bound idempotency keys event=%q audit=%q, want %q",
			boundEvent.IdempotencyKey, audit.IdempotencyKey, boundAudit.IdempotencyKey)
	}
}

func TestAuditedCreatesRejectOrphanedIdempotentResources(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	noteInput := SupportNoteInput{
		SubjectType: "request", SubjectID: "req-orphan", Author: "support-1",
		Body: "Created outside audited wrapper", Visibility: NoteInternal,
		IdempotencyKey: "orphan-note",
	}
	if _, err := store.CreateSupportNote(ctx, noteInput); err != nil {
		t.Fatal(err)
	}
	noteAudit := testMutationAudit(actionSupportNoteCreate)
	noteAudit.IdempotencyKey = noteInput.IdempotencyKey
	if _, _, err := store.CreateSupportNoteAudited(ctx, noteInput, noteAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphaned support note audit error = %v, want ErrConflict", err)
	}

	priceInput := PriceSnapshotInput{
		Provider: "openai", Model: "gpt-test", Operation: "responses", Component: "input",
		Currency: "USD", Unit: "token", UnitSize: 1, AmountDecimal: "0.000001",
		AmountMinor: 1, MinorUnitScale: 6, Source: "provider-price-sheet",
		IdempotencyKey: "orphan-price", EffectiveAt: testNow,
	}
	if _, err := store.CreatePriceSnapshot(ctx, priceInput); err != nil {
		t.Fatal(err)
	}
	priceAudit := testMutationAudit(actionPriceSnapshotCreate)
	priceAudit.IdempotencyKey = priceInput.IdempotencyKey
	if _, _, err := store.CreatePriceSnapshotAudited(ctx, priceInput, priceAudit); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphaned price snapshot audit error = %v, want ErrConflict", err)
	}
	assertTableCount(t, store, "operation_audit", 0)
}

func TestAuditedMutationPopulatesCanonicalAudit(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	auditTemplate := testMutationAudit("risk_case.create")
	auditTemplate.Action = "client-controlled.action"
	auditTemplate.TargetType = "client-controlled"
	auditTemplate.TargetID = "client-controlled"
	auditTemplate.BeforeJSON = json.RawMessage(`{"spoofed":true}`)
	auditTemplate.AfterJSON = json.RawMessage(`{"spoofed":true}`)
	auditTemplate.Status = OperationFailed
	auditTemplate.ErrorCode = "spoofed"

	created, audit, err := store.CreateRiskCaseAudited(ctx, RiskCaseInput{
		CaseKey: "risk-audited-success", Title: "Audited create",
		SubjectType: "user", SubjectID: "42", Severity: RiskSeverityHigh,
		Status: RiskCaseOpen,
	}, auditTemplate)
	if err != nil {
		t.Fatalf("CreateRiskCaseAudited() error = %v", err)
	}
	if audit.Action != actionRiskCaseCreate || audit.TargetType != "risk_case" || audit.TargetID != strconv.FormatInt(created.ID, 10) {
		t.Fatalf("audit target = %s/%s, created ID = %d", audit.TargetType, audit.TargetID, created.ID)
	}
	if audit.Status != OperationSucceeded || audit.ErrorCode != "" || len(audit.BeforeJSON) != 0 {
		t.Fatalf("audit completion fields = %+v", audit)
	}
	if !strings.Contains(string(audit.AfterJSON), `"case_key"`) || strings.Contains(string(audit.AfterJSON), `"CaseKey"`) {
		t.Fatalf("audit after state does not use stable lower_snake_case fields: %s", audit.AfterJSON)
	}
	var after RiskCase
	if err := json.Unmarshal(audit.AfterJSON, &after); err != nil {
		t.Fatalf("unmarshal audit after state: %v", err)
	}
	if after.ID != created.ID || after.CaseKey != created.CaseKey {
		t.Fatalf("audit after state = %+v, created = %+v", after, created)
	}
}

func TestAuditedMutationReplaysReturnOriginalResults(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := testNow
	store.now = func() time.Time { return now }

	riskInput := RiskCaseInput{
		CaseKey: "audited-replay-risk", Title: "Replay risk create", SubjectType: "user",
		SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
	}
	riskAuditInput := testMutationAudit("risk_case.create")
	riskAuditInput.Action = "spoofed"
	risk, riskAudit, err := store.CreateRiskCaseAudited(ctx, riskInput, riskAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	riskReplayAuditInput := riskAuditInput
	riskReplayAuditInput.RequestID = "req-risk-retry"
	riskReplayAuditInput.SourceIP = "198.51.100.8"
	riskReplayAuditInput.OccurredAt = now.Add(time.Hour)
	riskReplay, riskAuditReplay, err := store.CreateRiskCaseAudited(ctx, riskInput, riskReplayAuditInput)
	if err != nil || riskReplay.ID != risk.ID || riskAuditReplay.ID != riskAudit.ID {
		t.Fatalf("risk create replay = %+v/%+v, %v", riskReplay, riskAuditReplay, err)
	}
	if riskAuditReplay.RequestID != riskAudit.RequestID || riskAuditReplay.SourceIP != riskAudit.SourceIP ||
		!riskAuditReplay.OccurredAt.Equal(riskAudit.OccurredAt) {
		t.Fatalf("risk replay did not return original audit metadata: first=%+v retry=%+v", riskAudit, riskAuditReplay)
	}

	riskUpdate := RiskCaseUpdate{
		ID: risk.ID, Title: "Replay risk update", Severity: RiskSeverityCritical,
		Status: RiskCaseInvestigating, Assignee: "analyst-1", Summary: "investigating",
	}
	riskUpdateAuditInput := testMutationAudit("risk_case.update")
	riskUpdateAuditInput.Action = "spoofed"
	updatedRisk, riskUpdateAudit, err := store.UpdateRiskCaseAudited(ctx, riskUpdate, riskUpdateAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	updatedRiskReplay, riskUpdateAuditReplay, err := store.UpdateRiskCaseAudited(ctx, riskUpdate, riskUpdateAuditInput)
	if err != nil || updatedRiskReplay.UpdatedAt != updatedRisk.UpdatedAt || riskUpdateAuditReplay.ID != riskUpdateAudit.ID {
		t.Fatalf("risk update replay = %+v/%+v, %v", updatedRiskReplay, riskUpdateAuditReplay, err)
	}

	eventAuditInput := testMutationAudit("risk_case.event.append")
	eventAuditInput.Action = "spoofed"
	eventInput := RiskCaseEventInput{
		CaseID: risk.ID, EventType: "evidence_attached", Actor: "risk-engine",
		DetailsJSON:    json.RawMessage(`{"request_id":"req-1"}`),
		IdempotencyKey: eventAuditInput.IdempotencyKey,
	}
	event, eventAudit, err := store.AppendRiskCaseEventAudited(ctx, eventInput, eventAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	eventReplay, eventAuditReplay, err := store.AppendRiskCaseEventAudited(ctx, eventInput, eventAuditInput)
	if err != nil || eventReplay.ID != event.ID || eventAuditReplay.ID != eventAudit.ID {
		t.Fatalf("event replay = %+v/%+v, %v", eventReplay, eventAuditReplay, err)
	}

	noteInput := SupportNoteInput{
		SubjectType: "request", SubjectID: "req-1", Author: "support-1",
		Body: "Replay note create", Visibility: NoteInternal, IdempotencyKey: "audit-support_note.create",
	}
	noteAuditInput := testMutationAudit("support_note.create")
	noteAuditInput.Action = "spoofed"
	note, noteAudit, err := store.CreateSupportNoteAudited(ctx, noteInput, noteAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	noteReplay, noteAuditReplay, err := store.CreateSupportNoteAudited(ctx, noteInput, noteAuditInput)
	if err != nil || noteReplay.ID != note.ID || noteAuditReplay.ID != noteAudit.ID {
		t.Fatalf("note create replay = %+v/%+v, %v", noteReplay, noteAuditReplay, err)
	}
	conflictingNote := noteInput
	conflictingNote.Visibility = NoteCustomer
	if _, _, err := store.CreateSupportNoteAudited(ctx, conflictingNote, noteAuditInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting audited note replay error = %v, want ErrConflict", err)
	}

	noteUpdate := SupportNoteUpdate{ID: note.ID, Body: "Replay note update", Visibility: NoteCustomer}
	noteUpdateAuditInput := testMutationAudit("support_note.update")
	noteUpdateAuditInput.Action = "spoofed"
	updatedNote, noteUpdateAudit, err := store.UpdateSupportNoteAudited(ctx, noteUpdate, noteUpdateAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	updatedNoteReplay, noteUpdateAuditReplay, err := store.UpdateSupportNoteAudited(ctx, noteUpdate, noteUpdateAuditInput)
	if err != nil || updatedNoteReplay.UpdatedAt != updatedNote.UpdatedAt || noteUpdateAuditReplay.ID != noteUpdateAudit.ID {
		t.Fatalf("note update replay = %+v/%+v, %v", updatedNoteReplay, noteUpdateAuditReplay, err)
	}

	noteDeleteAuditInput := testMutationAudit("support_note.delete")
	noteDeleteAuditInput.Action = "spoofed"
	deletedNote, noteDeleteAudit, err := store.DeleteSupportNoteAudited(ctx, note.ID, noteDeleteAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	deletedNoteReplay, noteDeleteAuditReplay, err := store.DeleteSupportNoteAudited(ctx, note.ID, noteDeleteAuditInput)
	if err != nil || deletedNoteReplay.DeletedAt == nil ||
		deletedNoteReplay.DeletedAt.UnixMilli() != deletedNote.DeletedAt.UnixMilli() ||
		noteDeleteAuditReplay.ID != noteDeleteAudit.ID {
		t.Fatalf("note delete replay = %+v/%+v, %v", deletedNoteReplay, noteDeleteAuditReplay, err)
	}

	priceInput := PriceSnapshotInput{
		Provider: "openai", Model: "gpt-test", Operation: "responses", Component: "input",
		Currency: "usd", Unit: "token", UnitSize: 1, AmountDecimal: "0.000001",
		AmountMinor: 1, MinorUnitScale: 6, Source: "provider-price-sheet",
		MetadataJSON: json.RawMessage(`{"version":"2026-07"}`), IdempotencyKey: "audit-price_snapshot.create",
	}
	priceAuditInput := testMutationAudit("price_snapshot.create")
	priceAuditInput.Action = "spoofed"
	price, priceAudit, err := store.CreatePriceSnapshotAudited(ctx, priceInput, priceAuditInput)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	priceReplay, priceAuditReplay, err := store.CreatePriceSnapshotAudited(ctx, priceInput, priceAuditInput)
	if err != nil || priceReplay.ID != price.ID || priceAuditReplay.ID != priceAudit.ID {
		t.Fatalf("price create replay = %+v/%+v, %v", priceReplay, priceAuditReplay, err)
	}

	conflictingUpdate := riskUpdate
	conflictingUpdate.Summary = "different payload"
	if _, _, err := store.UpdateRiskCaseAudited(ctx, conflictingUpdate, riskUpdateAuditInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting audited replay error = %v, want ErrConflict", err)
	}
	for _, tt := range []struct{ name, got, want string }{
		{"risk create", riskAudit.Action, actionRiskCaseCreate},
		{"risk update", riskUpdateAudit.Action, actionRiskCaseUpdate},
		{"risk event", eventAudit.Action, actionRiskCaseEventAppend},
		{"note create", noteAudit.Action, actionSupportNoteCreate},
		{"note update", noteUpdateAudit.Action, actionSupportNoteUpdate},
		{"note delete", noteDeleteAudit.Action, actionSupportNoteDelete},
		{"price create", priceAudit.Action, actionPriceSnapshotCreate},
	} {
		if tt.got != tt.want {
			t.Fatalf("%s audit action = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	assertTableCount(t, store, "operation_audit", 7)
}

func TestCreateSupportNoteAuditedReplayReturnsImmutableSnapshotAfterLaterChanges(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	input := SupportNoteInput{
		SubjectType: "request", SubjectID: "req-deleted", Author: "support-1",
		Body: "Replay must remain live", Visibility: NoteInternal, IdempotencyKey: "audit-support_note.create",
	}
	auditInput := testMutationAudit("support_note.create")
	created, audit, err := store.CreateSupportNoteAudited(ctx, input, auditInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSupportNote(ctx, SupportNoteUpdate{
		ID: created.ID, Body: "Later mutable body", Visibility: NoteCustomer,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSupportNote(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	replayed, replayAudit, err := store.CreateSupportNoteAudited(ctx, input, auditInput)
	if err != nil {
		t.Fatalf("immutable create replay error = %v", err)
	}
	if replayAudit.ID != audit.ID || replayed.ID != created.ID || replayed.Body != created.Body ||
		replayed.Visibility != created.Visibility || replayed.DeletedAt != nil || replayed.UpdatedAt != created.UpdatedAt {
		t.Fatalf("immutable create replay = %+v/%+v, want original %+v/%+v", replayed, replayAudit, created, audit)
	}
	assertTableCount(t, store, "operation_audit", 1)
}

func TestAuditedMutationsRollbackWhenAuditInsertFails(t *testing.T) {
	t.Run("risk case create", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		injectOperationAuditFailure(t, store)
		_, _, err := store.CreateRiskCaseAudited(ctx, RiskCaseInput{
			CaseKey: "rollback-risk-create", Title: "Rollback create", SubjectType: "user",
			SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
		}, testMutationAudit("risk_case.create"))
		requireInjectedAuditFailure(t, err)
		assertTableCount(t, store, "risk_cases", 0)
	})

	t.Run("risk case update", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		before, err := store.CreateRiskCase(ctx, RiskCaseInput{
			CaseKey: "rollback-risk-update", Title: "Original title", SubjectType: "user",
			SubjectID: "42", Severity: RiskSeverityMedium, Status: RiskCaseOpen,
		})
		if err != nil {
			t.Fatal(err)
		}
		injectOperationAuditFailure(t, store)
		_, _, err = store.UpdateRiskCaseAudited(ctx, RiskCaseUpdate{
			ID: before.ID, Title: "Changed title", Severity: RiskSeverityCritical,
			Status: RiskCaseInvestigating, Assignee: "analyst-1", Summary: "changed",
		}, testMutationAudit("risk_case.update"))
		requireInjectedAuditFailure(t, err)
		after, getErr := store.GetRiskCase(ctx, before.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.Title != before.Title || after.Severity != before.Severity || after.Status != before.Status ||
			after.Assignee != before.Assignee || after.Summary != before.Summary {
			t.Fatalf("risk case changed despite rollback: before=%+v after=%+v", before, after)
		}
		assertTableCount(t, store, "operation_audit", 0)
	})

	t.Run("risk case event append", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		created, err := store.CreateRiskCase(ctx, RiskCaseInput{
			CaseKey: "rollback-risk-event", Title: "Rollback event", SubjectType: "user",
			SubjectID: "42", Severity: RiskSeverityHigh, Status: RiskCaseOpen,
		})
		if err != nil {
			t.Fatal(err)
		}
		injectOperationAuditFailure(t, store)
		_, _, err = store.AppendRiskCaseEventAudited(ctx, RiskCaseEventInput{
			CaseID: created.ID, EventType: "evidence_attached", Actor: "risk-engine",
			DetailsJSON: json.RawMessage(`{"request_id":"req-1"}`), IdempotencyKey: "audit-risk_case.event.append",
		}, testMutationAudit("risk_case.event.append"))
		requireInjectedAuditFailure(t, err)
		assertTableCount(t, store, "risk_case_events", 0)
	})

	t.Run("support note create", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		injectOperationAuditFailure(t, store)
		_, _, err := store.CreateSupportNoteAudited(ctx, SupportNoteInput{
			SubjectType: "request", SubjectID: "req-1", Author: "support-1",
			Body: "Must roll back", Visibility: NoteInternal, IdempotencyKey: "audit-support_note.create",
		}, testMutationAudit("support_note.create"))
		requireInjectedAuditFailure(t, err)
		assertTableCount(t, store, "support_notes", 0)
	})

	t.Run("support note update", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		before, err := store.CreateSupportNote(ctx, SupportNoteInput{
			SubjectType: "request", SubjectID: "req-1", Author: "support-1",
			Body: "Original note", Visibility: NoteInternal,
		})
		if err != nil {
			t.Fatal(err)
		}
		injectOperationAuditFailure(t, store)
		_, _, err = store.UpdateSupportNoteAudited(ctx, SupportNoteUpdate{
			ID: before.ID, Body: "Changed note", Visibility: NoteCustomer,
		}, testMutationAudit("support_note.update"))
		requireInjectedAuditFailure(t, err)
		after, getErr := store.GetSupportNote(ctx, before.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.Body != before.Body || after.Visibility != before.Visibility {
			t.Fatalf("support note changed despite rollback: before=%+v after=%+v", before, after)
		}
	})

	t.Run("support note delete", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		before, err := store.CreateSupportNote(ctx, SupportNoteInput{
			SubjectType: "request", SubjectID: "req-1", Author: "support-1",
			Body: "Keep note", Visibility: NoteInternal,
		})
		if err != nil {
			t.Fatal(err)
		}
		injectOperationAuditFailure(t, store)
		_, _, err = store.DeleteSupportNoteAudited(ctx, before.ID, testMutationAudit("support_note.delete"))
		requireInjectedAuditFailure(t, err)
		after, getErr := store.GetSupportNote(ctx, before.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.DeletedAt != nil {
			t.Fatalf("support note was deleted despite rollback: %+v", after)
		}
	})

	t.Run("price snapshot create", func(t *testing.T) {
		store, _ := newTestStore(t)
		ctx := context.Background()
		injectOperationAuditFailure(t, store)
		_, _, err := store.CreatePriceSnapshotAudited(ctx, PriceSnapshotInput{
			Provider: "openai", Model: "gpt-test", Operation: "responses", Component: "input",
			Currency: "USD", Unit: "token", UnitSize: 1, AmountDecimal: "0.000001",
			AmountMinor: 1, MinorUnitScale: 6, Source: "provider-price-sheet",
			IdempotencyKey: "audit-price_snapshot.create", EffectiveAt: testNow,
		}, testMutationAudit("price_snapshot.create"))
		requireInjectedAuditFailure(t, err)
		assertTableCount(t, store, "price_snapshots", 0)
	})
}

func testMutationAudit(action string) OperationAuditInput {
	return OperationAuditInput{
		RequestID: "req-" + action, Actor: "admin@example.com", SourceIP: "203.0.113.7",
		AuthMethod: "jwt", Action: action, Reason: "operator approved",
		IdempotencyKey: "audit-" + action,
	}
}

func injectOperationAuditFailure(t *testing.T, store *Store) {
	t.Helper()
	_, err := store.db.Exec(`CREATE TRIGGER fail_operation_audit
		BEFORE INSERT ON operation_audit
		BEGIN
			SELECT RAISE(ABORT, 'injected audit failure');
		END`)
	if err != nil {
		t.Fatalf("create audit failure trigger: %v", err)
	}
}

func requireInjectedAuditFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "injected audit failure") {
		t.Fatalf("audited mutation error = %v, want injected audit failure", err)
	}
}

func assertTableCount(t *testing.T, store *Store, table string, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
