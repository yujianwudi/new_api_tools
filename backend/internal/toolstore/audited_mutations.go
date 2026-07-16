package toolstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	actionRiskCaseCreate      = "risk_case.create"
	actionRiskCaseUpdate      = "risk_case.update"
	actionRiskCaseEventAppend = "risk_case.event.append"
	actionSupportNoteCreate   = "support_note.create"
	actionSupportNoteUpdate   = "support_note.update"
	actionSupportNoteDelete   = "support_note.delete"
	actionPriceSnapshotCreate = "price_snapshot.create"
)

// CreateRiskCaseAudited creates a case and its immutable operation audit in
// one transaction. Neither row is visible unless both writes succeed.
func (s *Store) CreateRiskCaseAudited(ctx context.Context, input RiskCaseInput, auditInput OperationAuditInput) (RiskCase, OperationAudit, error) {
	auditInput.Action = actionRiskCaseCreate
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RiskCase{}, OperationAudit{}, fmt.Errorf("begin audited risk case create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "risk_case"); err != nil {
		return RiskCase{}, OperationAudit{}, err
	} else if found {
		created, err := riskCaseAuditState(existingAudit)
		if err != nil {
			return RiskCase{}, OperationAudit{}, err
		}
		if !riskCaseMatchesInput(created, input) {
			return RiskCase{}, OperationAudit{}, replayConflict("risk case create")
		}
		if err := tx.Commit(); err != nil {
			return RiskCase{}, OperationAudit{}, fmt.Errorf("commit audited risk case create replay: %w", err)
		}
		return created, existingAudit, nil
	}

	created, err := s.createRiskCase(ctx, tx, input)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "risk_case", created.ID, nil, created)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return RiskCase{}, OperationAudit{}, fmt.Errorf("commit audited risk case create: %w", err)
	}
	return created, audit, nil
}

// UpdateRiskCaseAudited updates a case and records the exact before/after
// states atomically.
func (s *Store) UpdateRiskCaseAudited(ctx context.Context, update RiskCaseUpdate, auditInput OperationAuditInput) (RiskCase, OperationAudit, error) {
	auditInput.Action = actionRiskCaseUpdate
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RiskCase{}, OperationAudit{}, fmt.Errorf("begin audited risk case update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "risk_case"); err != nil {
		return RiskCase{}, OperationAudit{}, err
	} else if found {
		updated, err := riskCaseAuditState(existingAudit)
		if err != nil {
			return RiskCase{}, OperationAudit{}, err
		}
		if !riskCaseMatchesUpdate(updated, update) {
			return RiskCase{}, OperationAudit{}, replayConflict("risk case update")
		}
		if err := tx.Commit(); err != nil {
			return RiskCase{}, OperationAudit{}, fmt.Errorf("commit audited risk case update replay: %w", err)
		}
		return updated, existingAudit, nil
	}

	before, err := getRiskCase(ctx, tx, update.ID)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	updated, err := s.updateRiskCase(ctx, tx, update)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "risk_case", updated.ID, before, updated)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return RiskCase{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return RiskCase{}, OperationAudit{}, fmt.Errorf("commit audited risk case update: %w", err)
	}
	return updated, audit, nil
}

// AppendRiskCaseEventAudited appends immutable case evidence and the matching
// operation audit atomically.
func (s *Store) AppendRiskCaseEventAudited(ctx context.Context, input RiskCaseEventInput, auditInput OperationAuditInput) (RiskCaseEvent, OperationAudit, error) {
	auditInput.Action = actionRiskCaseEventAppend
	key, err := auditedIdempotencyKey(input.IdempotencyKey, auditInput.IdempotencyKey, "risk case event")
	if err != nil {
		return RiskCaseEvent{}, OperationAudit{}, err
	}
	input.IdempotencyKey = key
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RiskCaseEvent{}, OperationAudit{}, fmt.Errorf("begin audited risk case event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "risk_case_event"); err != nil {
		return RiskCaseEvent{}, OperationAudit{}, err
	} else if found {
		event, err := riskCaseEventAuditState(existingAudit)
		if err != nil {
			return RiskCaseEvent{}, OperationAudit{}, err
		}
		storedEvent, err := getRiskCaseEvent(ctx, tx, event.ID)
		if err != nil {
			return RiskCaseEvent{}, OperationAudit{}, err
		}
		if !riskCaseEventMatchesInput(storedEvent, input) {
			return RiskCaseEvent{}, OperationAudit{}, replayConflict("risk case event append")
		}
		if err := tx.Commit(); err != nil {
			return RiskCaseEvent{}, OperationAudit{}, fmt.Errorf("commit audited risk case event append replay: %w", err)
		}
		return storedEvent, existingAudit, nil
	}
	if _, err := getRiskCaseEventByIdempotencyKey(ctx, tx, key); err == nil {
		return RiskCaseEvent{}, OperationAudit{}, replayConflict("orphaned risk case event")
	} else if !errors.Is(err, ErrNotFound) {
		return RiskCaseEvent{}, OperationAudit{}, err
	}

	event, err := s.appendRiskCaseEvent(ctx, tx, input)
	if err != nil {
		return RiskCaseEvent{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "risk_case_event", event.ID, nil, event)
	if err != nil {
		return RiskCaseEvent{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return RiskCaseEvent{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return RiskCaseEvent{}, OperationAudit{}, fmt.Errorf("commit audited risk case event append: %w", err)
	}
	return event, audit, nil
}

// CreateSupportNoteAudited creates a note and its operation audit atomically.
func (s *Store) CreateSupportNoteAudited(ctx context.Context, input SupportNoteInput, auditInput OperationAuditInput) (SupportNote, OperationAudit, error) {
	auditInput.Action = actionSupportNoteCreate
	key, err := auditedIdempotencyKey(input.IdempotencyKey, auditInput.IdempotencyKey, "support note")
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	input.IdempotencyKey = key
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("begin audited support note create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "support_note"); err != nil {
		return SupportNote{}, OperationAudit{}, err
	} else if found {
		created, err := supportNoteAuditState(existingAudit)
		if err != nil {
			return SupportNote{}, OperationAudit{}, err
		}
		if !supportNoteMatchesInput(created, input) {
			return SupportNote{}, OperationAudit{}, replayConflict("support note create")
		}
		if err := tx.Commit(); err != nil {
			return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note create replay: %w", err)
		}
		return created, existingAudit, nil
	}
	if _, err := getSupportNoteByIdempotencyKey(ctx, tx, key); err == nil {
		return SupportNote{}, OperationAudit{}, replayConflict("orphaned support note")
	} else if !errors.Is(err, ErrNotFound) {
		return SupportNote{}, OperationAudit{}, err
	}

	created, err := s.createSupportNote(ctx, tx, input)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "support_note", created.ID, nil, created)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note create: %w", err)
	}
	return created, audit, nil
}

// UpdateSupportNoteAudited updates a live note and records its before/after
// states atomically.
func (s *Store) UpdateSupportNoteAudited(ctx context.Context, update SupportNoteUpdate, auditInput OperationAuditInput) (SupportNote, OperationAudit, error) {
	auditInput.Action = actionSupportNoteUpdate
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("begin audited support note update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "support_note"); err != nil {
		return SupportNote{}, OperationAudit{}, err
	} else if found {
		updated, err := supportNoteAuditState(existingAudit)
		if err != nil {
			return SupportNote{}, OperationAudit{}, err
		}
		if !supportNoteMatchesUpdate(updated, update) {
			return SupportNote{}, OperationAudit{}, replayConflict("support note update")
		}
		if err := tx.Commit(); err != nil {
			return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note update replay: %w", err)
		}
		return updated, existingAudit, nil
	}

	before, err := getSupportNote(ctx, tx, update.ID)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	updated, err := s.updateSupportNote(ctx, tx, update)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "support_note", updated.ID, before, updated)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note update: %w", err)
	}
	return updated, audit, nil
}

// DeleteSupportNoteAudited soft-deletes a note and records the resulting
// tombstone state atomically.
func (s *Store) DeleteSupportNoteAudited(ctx context.Context, id int64, auditInput OperationAuditInput) (SupportNote, OperationAudit, error) {
	auditInput.Action = actionSupportNoteDelete
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("begin audited support note delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "support_note"); err != nil {
		return SupportNote{}, OperationAudit{}, err
	} else if found {
		deleted, err := supportNoteAuditState(existingAudit)
		if err != nil {
			return SupportNote{}, OperationAudit{}, err
		}
		if deleted.ID != id || deleted.DeletedAt == nil {
			return SupportNote{}, OperationAudit{}, replayConflict("support note delete")
		}
		if err := tx.Commit(); err != nil {
			return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note delete replay: %w", err)
		}
		return deleted, existingAudit, nil
	}

	before, err := getSupportNote(ctx, tx, id)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	deleted, err := s.deleteSupportNote(ctx, tx, id)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "support_note", deleted.ID, before, deleted)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return SupportNote{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return SupportNote{}, OperationAudit{}, fmt.Errorf("commit audited support note delete: %w", err)
	}
	return deleted, audit, nil
}

// CreatePriceSnapshotAudited appends an immutable price observation and its
// matching operation audit atomically.
func (s *Store) CreatePriceSnapshotAudited(ctx context.Context, input PriceSnapshotInput, auditInput OperationAuditInput) (PriceSnapshot, OperationAudit, error) {
	auditInput.Action = actionPriceSnapshotCreate
	key, err := auditedIdempotencyKey(input.IdempotencyKey, auditInput.IdempotencyKey, "price snapshot")
	if err != nil {
		return PriceSnapshot{}, OperationAudit{}, err
	}
	input.IdempotencyKey = key
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PriceSnapshot{}, OperationAudit{}, fmt.Errorf("begin audited price snapshot create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existingAudit, found, err := findCompletedAuditReplay(ctx, tx, auditInput, "price_snapshot"); err != nil {
		return PriceSnapshot{}, OperationAudit{}, err
	} else if found {
		created, err := priceSnapshotAuditState(existingAudit)
		if err != nil {
			return PriceSnapshot{}, OperationAudit{}, err
		}
		if !priceSnapshotMatchesInput(created, input) {
			return PriceSnapshot{}, OperationAudit{}, replayConflict("price snapshot create")
		}
		if err := tx.Commit(); err != nil {
			return PriceSnapshot{}, OperationAudit{}, fmt.Errorf("commit audited price snapshot create replay: %w", err)
		}
		return created, existingAudit, nil
	}
	if _, err := getPriceSnapshotByIdempotencyKey(ctx, tx, key); err == nil {
		return PriceSnapshot{}, OperationAudit{}, replayConflict("orphaned price snapshot")
	} else if !errors.Is(err, ErrNotFound) {
		return PriceSnapshot{}, OperationAudit{}, err
	}

	created, err := s.createPriceSnapshot(ctx, tx, input)
	if err != nil {
		return PriceSnapshot{}, OperationAudit{}, err
	}
	auditInput, err = completedMutationAudit(auditInput, "price_snapshot", created.ID, nil, created)
	if err != nil {
		return PriceSnapshot{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return PriceSnapshot{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return PriceSnapshot{}, OperationAudit{}, fmt.Errorf("commit audited price snapshot create: %w", err)
	}
	return created, audit, nil
}

func auditedIdempotencyKey(resourceKey, auditKey, operation string) (string, error) {
	auditKey = strings.TrimSpace(auditKey)
	if auditKey == "" {
		return "", fmt.Errorf("%w: audited %s requires an idempotency key", ErrInvalid, operation)
	}
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey != "" && resourceKey != auditKey {
		return "", fmt.Errorf("%w: %s idempotency key must match its operation audit", ErrInvalid, operation)
	}
	return auditKey, nil
}

func completedMutationAudit(input OperationAuditInput, targetType string, targetID int64, before, after any) (OperationAuditInput, error) {
	beforeJSON, err := marshalMutationState(before)
	if err != nil {
		return OperationAuditInput{}, err
	}
	afterJSON, err := marshalMutationState(after)
	if err != nil {
		return OperationAuditInput{}, err
	}
	input.TargetType = targetType
	input.TargetID = strconv.FormatInt(targetID, 10)
	input.BeforeJSON = beforeJSON
	input.AfterJSON = afterJSON
	input.Status = OperationSucceeded
	input.ErrorCode = ""
	return input, nil
}

func marshalMutationState(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal operation audit state: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func findCompletedAuditReplay(ctx context.Context, queryer queryRower, input OperationAuditInput, targetType string) (OperationAudit, bool, error) {
	key := strings.TrimSpace(input.IdempotencyKey)
	if key == "" {
		return OperationAudit{}, false, nil
	}
	existing, err := getOperationAuditByIdempotencyKey(ctx, queryer, key)
	if errors.Is(err, ErrNotFound) {
		return OperationAudit{}, false, nil
	}
	if err != nil {
		return OperationAudit{}, false, err
	}
	if existing.Actor != strings.TrimSpace(input.Actor) ||
		existing.AuthMethod != strings.TrimSpace(input.AuthMethod) ||
		existing.Action != strings.TrimSpace(input.Action) ||
		existing.Reason != strings.TrimSpace(input.Reason) ||
		existing.TargetType != targetType || existing.Status != OperationSucceeded ||
		existing.ErrorCode != "" {
		return OperationAudit{}, false, replayConflict("operation audit")
	}
	return existing, true, nil
}

func riskCaseAuditState(audit OperationAudit) (RiskCase, error) {
	state, err := decodeAuditState[RiskCase](audit)
	if err != nil {
		return RiskCase{}, err
	}
	if err := verifyAuditStateID(audit, state.ID); err != nil {
		return RiskCase{}, err
	}
	return state, nil
}

func riskCaseEventAuditState(audit OperationAudit) (RiskCaseEvent, error) {
	state, err := decodeAuditState[RiskCaseEvent](audit)
	if err != nil {
		return RiskCaseEvent{}, err
	}
	if strings.TrimSpace(string(state.DetailsJSON)) == "null" {
		state.DetailsJSON = nil
	}
	if err := verifyAuditStateID(audit, state.ID); err != nil {
		return RiskCaseEvent{}, err
	}
	return state, nil
}

func supportNoteAuditState(audit OperationAudit) (SupportNote, error) {
	state, err := decodeAuditState[SupportNote](audit)
	if err != nil {
		return SupportNote{}, err
	}
	if err := verifyAuditStateID(audit, state.ID); err != nil {
		return SupportNote{}, err
	}
	return state, nil
}

func priceSnapshotAuditState(audit OperationAudit) (PriceSnapshot, error) {
	state, err := decodeAuditState[PriceSnapshot](audit)
	if err != nil {
		return PriceSnapshot{}, err
	}
	if strings.TrimSpace(string(state.MetadataJSON)) == "null" {
		state.MetadataJSON = nil
	}
	if err := verifyAuditStateID(audit, state.ID); err != nil {
		return PriceSnapshot{}, err
	}
	return state, nil
}

func decodeAuditState[T any](audit OperationAudit) (T, error) {
	var state T
	if len(audit.AfterJSON) == 0 {
		return state, replayConflict("operation audit state")
	}
	if err := json.Unmarshal(audit.AfterJSON, &state); err != nil {
		return state, fmt.Errorf("decode operation audit state: %w", err)
	}
	return state, nil
}

func verifyAuditStateID(audit OperationAudit, stateID int64) error {
	targetID, err := strconv.ParseInt(audit.TargetID, 10, 64)
	if err != nil || targetID <= 0 || targetID != stateID {
		return replayConflict("operation audit target")
	}
	return nil
}

func riskCaseMatchesInput(item RiskCase, input RiskCaseInput) bool {
	return item.CaseKey == strings.TrimSpace(input.CaseKey) &&
		item.Title == strings.TrimSpace(input.Title) &&
		item.SubjectType == strings.TrimSpace(input.SubjectType) &&
		item.SubjectID == strings.TrimSpace(input.SubjectID) &&
		item.Severity == input.Severity && item.Status == input.Status &&
		item.Assignee == strings.TrimSpace(input.Assignee) &&
		item.Summary == strings.TrimSpace(input.Summary) &&
		(input.OpenedAt.IsZero() || item.OpenedAt.UnixMilli() == input.OpenedAt.UnixMilli()) &&
		sameOptionalTime(item.ClosedAt, input.ClosedAt)
}

func riskCaseMatchesUpdate(item RiskCase, update RiskCaseUpdate) bool {
	return item.ID == update.ID && item.Title == strings.TrimSpace(update.Title) &&
		item.Severity == update.Severity && item.Status == update.Status &&
		item.Assignee == strings.TrimSpace(update.Assignee) &&
		item.Summary == strings.TrimSpace(update.Summary) &&
		sameOptionalTime(item.ClosedAt, update.ClosedAt)
}

func riskCaseEventMatchesInput(item RiskCaseEvent, input RiskCaseEventInput) bool {
	details, err := normalizedJSON("details_json", input.DetailsJSON)
	return err == nil && item.CaseID == input.CaseID &&
		item.EventType == strings.TrimSpace(input.EventType) &&
		item.Actor == strings.TrimSpace(input.Actor) &&
		string(item.DetailsJSON) == string(details) &&
		item.IdempotencyKey == strings.TrimSpace(input.IdempotencyKey) &&
		(input.OccurredAt.IsZero() || item.OccurredAt.UnixMilli() == input.OccurredAt.UnixMilli())
}

func supportNoteMatchesUpdate(item SupportNote, update SupportNoteUpdate) bool {
	return item.ID == update.ID && item.Body == strings.TrimSpace(update.Body) &&
		item.Visibility == update.Visibility
}

func priceSnapshotMatchesInput(item PriceSnapshot, input PriceSnapshotInput) bool {
	metadata, err := normalizedJSON("metadata_json", input.MetadataJSON)
	return err == nil && item.Provider == strings.TrimSpace(input.Provider) &&
		item.Model == strings.TrimSpace(input.Model) &&
		item.Operation == strings.TrimSpace(input.Operation) &&
		item.Component == strings.TrimSpace(input.Component) &&
		item.Currency == strings.ToUpper(strings.TrimSpace(input.Currency)) &&
		item.Unit == strings.TrimSpace(input.Unit) && item.UnitSize == input.UnitSize &&
		item.AmountDecimal == strings.TrimSpace(input.AmountDecimal) &&
		item.AmountMinor == input.AmountMinor && item.MinorUnitScale == input.MinorUnitScale &&
		item.Source == strings.TrimSpace(input.Source) &&
		string(item.MetadataJSON) == string(metadata) &&
		item.IdempotencyKey == strings.TrimSpace(input.IdempotencyKey) &&
		(input.EffectiveAt.IsZero() || item.EffectiveAt.UnixMilli() == input.EffectiveAt.UnixMilli()) &&
		sameOptionalTime(item.ExpiresAt, input.ExpiresAt)
}

func replayConflict(operation string) error {
	return fmt.Errorf("%w: idempotency key belongs to a different %s", ErrConflict, operation)
}
