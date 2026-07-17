package toolstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type invoiceAuditRequest struct {
	Request struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"request"`
}

// CreateInvoiceAudited writes an intent, the invoice document and event, and
// the terminal outcome in one Tool Store transaction. The raw HTTP
// idempotency key remains recoverable through /control-plane/operations.
func (s *Store) CreateInvoiceAudited(ctx context.Context, input InvoiceDocumentInput, auditInput OperationAuditInput) (InvoiceDocument, OperationAudit, error) {
	rawKey, err := invoiceRawIdempotencyKey(input.IdempotencyKey, auditInput.IdempotencyKey)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	input.CreatedBy = strings.TrimSpace(auditInput.Actor)
	input.IdempotencyKey = invoiceResourceKey(auditInput.Actor, actionInvoiceCreate, rawKey)
	normalized, fingerprint, err := normalizeInvoiceInput(input)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if normalized.IssuedAt.After(s.clock()) {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: issued_at cannot be in the future", ErrInvalid)
	}
	intent, outcome, err := invoiceAuditPair(auditInput, actionInvoiceCreate,
		"invoice_document", normalized.InvoiceNumber, fingerprint)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("begin audited invoice create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, replayErr := findInvoiceOutcomeReplay(ctx, tx, intent, outcome); replayErr != nil {
		return InvoiceDocument{}, OperationAudit{}, replayErr
	} else if found {
		state, decodeErr := decodeAuditState[invoiceAuditState](existing)
		if decodeErr != nil || state.ID <= 0 || state.RequestFingerprint != fingerprint ||
			existing.TargetID != normalized.InvoiceNumber {
			return InvoiceDocument{}, OperationAudit{}, replayConflict("invoice create")
		}
		created, getErr := getInvoiceDocument(ctx, tx, state.ID)
		if getErr != nil || created.RequestFingerprint != fingerprint {
			return InvoiceDocument{}, OperationAudit{}, replayConflict("invoice create")
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("commit audited invoice create replay: %w", commitErr)
		}
		return created, existing, nil
	}
	if _, getErr := getInvoiceByIdempotencyKey(ctx, tx, normalized.IdempotencyKey); getErr == nil {
		return InvoiceDocument{}, OperationAudit{}, replayConflict("orphaned invoice")
	} else if !errors.Is(getErr, ErrNotFound) {
		return InvoiceDocument{}, OperationAudit{}, getErr
	}
	if _, err = s.appendOperationAudit(ctx, tx, intent); err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, err
	}
	created, err := s.createInvoice(ctx, tx, normalized, fingerprint)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if _, err = s.appendInvoiceEvent(ctx, tx, created.ID, "created", normalized.CreatedBy,
		invoiceEventKey(normalized.IdempotencyKey, "created"), created.IssuedAt,
		invoiceCreatedEventDetails(created)); err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	outcome.AfterJSON, err = marshalMutationState(invoiceState(created))
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, outcome)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("commit audited invoice create: %w", err)
	}
	return created, audit, nil
}

// VoidInvoiceAudited uses the Tool Store clock when no controlled historical
// timestamp is supplied. HTTP callers never choose the accounting timestamp.
func (s *Store) VoidInvoiceAudited(ctx context.Context, input InvoiceVoidInput, auditInput OperationAuditInput) (InvoiceDocument, OperationAudit, error) {
	rawKey, err := invoiceRawIdempotencyKey(input.IdempotencyKey, auditInput.IdempotencyKey)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	input.Reason = strings.TrimSpace(auditInput.Reason)
	input.Actor = strings.TrimSpace(auditInput.Actor)
	if input.ID <= 0 || !validInvoiceText(input.Reason, 1000, true, false) ||
		!validInvoiceText(input.Actor, 256, true, false) || (!input.VoidedAt.IsZero() && input.VoidedAt.UnixMilli() < 0) {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invalid invoice void input", ErrInvalid)
	}
	explicitVoidedAt := !input.VoidedAt.IsZero()
	now := s.clock().UTC().Truncate(time.Millisecond)
	if explicitVoidedAt {
		input.VoidedAt = input.VoidedAt.UTC().Truncate(time.Millisecond)
		if input.VoidedAt.After(now) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: voided_at cannot be in the future", ErrInvalid)
		}
	}
	reasonHash := sha256Hex(input.Reason)
	requestBytes, _ := json.Marshal(struct {
		ID         int64  `json:"id"`
		ReasonHash string `json:"reason_hash"`
	}{input.ID, reasonHash})
	fingerprint := sha256Hex(string(requestBytes))
	intent, outcome, err := invoiceAuditPair(auditInput, actionInvoiceVoid,
		"invoice_document", strconv.FormatInt(input.ID, 10), fingerprint)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("begin audited invoice void: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, replayErr := findInvoiceOutcomeReplay(ctx, tx, intent, outcome); replayErr != nil {
		return InvoiceDocument{}, OperationAudit{}, replayErr
	} else if found {
		state, decodeErr := decodeAuditState[invoiceAuditState](existing)
		if decodeErr != nil || state.ID != input.ID || state.Status != InvoiceVoided ||
			state.VoidReasonHash != reasonHash || state.VoidedAt == nil ||
			(explicitVoidedAt && state.VoidedAt.UnixMilli() != input.VoidedAt.UnixMilli()) {
			return InvoiceDocument{}, OperationAudit{}, replayConflict("invoice void")
		}
		updated, getErr := getInvoiceDocument(ctx, tx, input.ID)
		if getErr != nil || updated.Status != InvoiceVoided {
			return InvoiceDocument{}, OperationAudit{}, replayConflict("invoice void")
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("commit audited invoice void replay: %w", commitErr)
		}
		return updated, existing, nil
	}

	before, err := getInvoiceDocument(ctx, tx, input.ID)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if before.Status != InvoiceIssued {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice is not eligible for voiding", ErrConflict)
	}
	if !explicitVoidedAt {
		input.VoidedAt = now
	}
	if input.VoidedAt.Before(before.IssuedAt) {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice is not eligible for voiding", ErrConflict)
	}
	if _, err = s.appendOperationAudit(ctx, tx, intent); err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE invoice_documents
		SET status = 'voided', voided_at = ?, void_reason = ?, updated_at = ?
		WHERE id = ? AND status = 'issued'`, dbTime(input.VoidedAt), input.Reason, dbTime(now), input.ID)
	if err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("void invoice document: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("read invoice void result: %w", err)
	}
	if rows != 1 {
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
	}
	updated, err := getInvoiceDocument(ctx, tx, input.ID)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if _, err = s.appendInvoiceEvent(ctx, tx, updated.ID, "voided", input.Actor,
		invoiceEventKey(invoiceResourceKey(auditInput.Actor, actionInvoiceVoid, rawKey), "voided"),
		input.VoidedAt, map[string]any{"reason": input.Reason}); err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, err
	}
	afterState := invoiceState(updated)
	afterState.VoidReasonHash = reasonHash
	outcome.AfterJSON, err = marshalMutationState(afterState)
	if err != nil {
		return InvoiceDocument{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, outcome)
	if err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("%w: invoice changed concurrently", ErrConflict)
		}
		return InvoiceDocument{}, OperationAudit{}, fmt.Errorf("commit audited invoice void: %w", err)
	}
	return updated, audit, nil
}

// ImportInvoicesAudited atomically records the intent, every validated CSV
// row and event, and one terminal batch outcome.
func (s *Store) ImportInvoicesAudited(ctx context.Context, inputs []InvoiceDocumentInput, auditInput OperationAuditInput) (InvoiceImportResult, OperationAudit, error) {
	rawKey := strings.TrimSpace(auditInput.IdempotencyKey)
	if !validInvoiceRawKey(rawKey) {
		return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("%w: invoice import requires a valid idempotency key", ErrInvalid)
	}
	if len(inputs) == 0 || len(inputs) > 500 {
		return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("%w: invoice import must contain 1 to 500 rows", ErrInvalid)
	}
	resourceRoot := invoiceResourceKey(auditInput.Actor, actionInvoiceImport, rawKey)
	now := s.clock()
	normalized := make([]InvoiceDocumentInput, len(inputs))
	fingerprints := make([]string, len(inputs))
	for index, input := range inputs {
		input.CreatedBy = strings.TrimSpace(auditInput.Actor)
		input.Source = "csv"
		input.IdempotencyKey = invoiceImportRowKey(resourceRoot, index)
		var err error
		normalized[index], fingerprints[index], err = normalizeInvoiceInput(input)
		if err != nil {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("%w: invoice import row %d: %v", ErrInvalid, index+1, err)
		}
		if normalized[index].IssuedAt.After(now) {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("%w: invoice import row %d issued_at cannot be in the future", ErrInvalid, index+1)
		}
	}
	batchFingerprint := fingerprintStrings(fingerprints)
	intent, outcome, err := invoiceAuditPair(auditInput, actionInvoiceImport,
		"invoice_import", "batch", batchFingerprint)
	if err != nil {
		return InvoiceImportResult{}, OperationAudit{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("begin audited invoice import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, replayErr := findInvoiceOutcomeReplay(ctx, tx, intent, outcome); replayErr != nil {
		return InvoiceImportResult{}, OperationAudit{}, replayErr
	} else if found {
		state, decodeErr := decodeAuditState[invoiceImportAuditState](existing)
		if decodeErr != nil || state.RequestFingerprint != batchFingerprint || len(state.Items) != len(inputs) {
			return InvoiceImportResult{}, OperationAudit{}, replayConflict("invoice import")
		}
		items := make([]InvoiceDocument, len(state.Items))
		for index, auditItem := range state.Items {
			item, getErr := getInvoiceDocument(ctx, tx, auditItem.ID)
			if getErr != nil || item.RequestFingerprint != fingerprints[index] || auditItem.RequestFingerprint != fingerprints[index] {
				return InvoiceImportResult{}, OperationAudit{}, replayConflict("invoice import")
			}
			items[index] = item
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("commit audited invoice import replay: %w", commitErr)
		}
		return InvoiceImportResult{Items: items, Count: len(items)}, existing, nil
	}
	for _, input := range normalized {
		if _, getErr := getInvoiceByIdempotencyKey(ctx, tx, input.IdempotencyKey); getErr == nil {
			return InvoiceImportResult{}, OperationAudit{}, replayConflict("orphaned invoice import row")
		} else if !errors.Is(getErr, ErrNotFound) {
			return InvoiceImportResult{}, OperationAudit{}, getErr
		}
	}
	if _, err = s.appendOperationAudit(ctx, tx, intent); err != nil {
		if invoiceSQLiteContention(err) {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("%w: invoice import changed concurrently", ErrConflict)
		}
		return InvoiceImportResult{}, OperationAudit{}, err
	}
	items := make([]InvoiceDocument, 0, len(normalized))
	auditItems := make([]invoiceImportAuditItem, 0, len(normalized))
	for index, input := range normalized {
		created, createErr := s.createInvoice(ctx, tx, input, fingerprints[index])
		if createErr != nil {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("invoice import row %d: %w", index+1, createErr)
		}
		if _, createErr = s.appendInvoiceEvent(ctx, tx, created.ID, "imported", input.CreatedBy,
			invoiceEventKey(input.IdempotencyKey, "imported"), created.IssuedAt,
			invoiceCreatedEventDetails(created)); createErr != nil {
			return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("invoice import row %d event: %w", index+1, createErr)
		}
		items = append(items, created)
		auditItems = append(auditItems, invoiceImportAuditItem{ID: created.ID, RequestFingerprint: fingerprints[index]})
	}
	outcome.AfterJSON, err = marshalMutationState(invoiceImportAuditState{
		RequestFingerprint: batchFingerprint, Items: auditItems,
	})
	if err != nil {
		return InvoiceImportResult{}, OperationAudit{}, err
	}
	audit, err := s.appendOperationAudit(ctx, tx, outcome)
	if err != nil {
		return InvoiceImportResult{}, OperationAudit{}, err
	}
	if err := tx.Commit(); err != nil {
		return InvoiceImportResult{}, OperationAudit{}, fmt.Errorf("commit audited invoice import: %w", err)
	}
	return InvoiceImportResult{Items: items, Count: len(items)}, audit, nil
}

func invoiceAuditPair(base OperationAuditInput, action, targetType, targetID, fingerprint string) (OperationAuditInput, OperationAuditInput, error) {
	rawKey := strings.TrimSpace(base.IdempotencyKey)
	if !validInvoiceRawKey(rawKey) || strings.TrimSpace(targetID) == "" || strings.TrimSpace(fingerprint) == "" {
		return OperationAuditInput{}, OperationAuditInput{}, fmt.Errorf("%w: invalid invoice audit identity", ErrInvalid)
	}
	request := invoiceAuditRequest{}
	request.Request.Fingerprint = fingerprint
	beforeJSON, err := marshalMutationState(request)
	if err != nil {
		return OperationAuditInput{}, OperationAuditInput{}, err
	}
	intent := base
	intent.Action = action + ".intent"
	intent.TargetType = targetType
	intent.TargetID = targetID
	intent.BeforeJSON = beforeJSON
	intent.AfterJSON = nil
	intent.Status = OperationSucceeded
	intent.ErrorCode = ""
	intent.IdempotencyKey = "cp:" + rawKey + ":intent"
	outcome := intent
	outcome.Action = action + ".outcome"
	outcome.IdempotencyKey = "cp:" + rawKey + ":outcome"
	return intent, outcome, nil
}

func findInvoiceOutcomeReplay(ctx context.Context, queryer queryRower, expectedIntent, expectedOutcome OperationAuditInput) (OperationAudit, bool, error) {
	outcome, err := getOperationAuditByIdempotencyKey(ctx, queryer, expectedOutcome.IdempotencyKey)
	if errors.Is(err, ErrNotFound) {
		if _, intentErr := getOperationAuditByIdempotencyKey(ctx, queryer, expectedIntent.IdempotencyKey); intentErr == nil {
			return OperationAudit{}, false, replayConflict("orphaned invoice operation intent")
		} else if !errors.Is(intentErr, ErrNotFound) {
			return OperationAudit{}, false, intentErr
		}
		return OperationAudit{}, false, nil
	}
	if err != nil {
		return OperationAudit{}, false, err
	}
	intent, err := getOperationAuditByIdempotencyKey(ctx, queryer, expectedIntent.IdempotencyKey)
	if err != nil {
		return OperationAudit{}, false, replayConflict("invoice operation audit chain")
	}
	if intent.ID >= outcome.ID || intent.RequestID != outcome.RequestID || intent.SourceIP != outcome.SourceIP ||
		intent.Actor != outcome.Actor || intent.AuthMethod != outcome.AuthMethod || intent.Reason != outcome.Reason ||
		intent.TargetType != outcome.TargetType || intent.TargetID != outcome.TargetID ||
		intent.Action != expectedIntent.Action || outcome.Action != expectedOutcome.Action ||
		intent.Actor != strings.TrimSpace(expectedIntent.Actor) || intent.AuthMethod != strings.TrimSpace(expectedIntent.AuthMethod) ||
		intent.Reason != strings.TrimSpace(expectedIntent.Reason) || intent.TargetType != expectedIntent.TargetType ||
		intent.TargetID != expectedIntent.TargetID || !bytes.Equal(intent.BeforeJSON, expectedIntent.BeforeJSON) ||
		!bytes.Equal(outcome.BeforeJSON, expectedOutcome.BeforeJSON) || !bytes.Equal(intent.BeforeJSON, outcome.BeforeJSON) ||
		intent.Status != OperationSucceeded || outcome.Status != OperationSucceeded || intent.ErrorCode != "" || outcome.ErrorCode != "" {
		return OperationAudit{}, false, replayConflict("invoice operation audit chain")
	}
	return outcome, true, nil
}

func invoiceRawIdempotencyKey(resourceKey, auditKey string) (string, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	auditKey = strings.TrimSpace(auditKey)
	if resourceKey != auditKey || !validInvoiceRawKey(auditKey) {
		return "", fmt.Errorf("%w: invoice idempotency keys must match", ErrInvalid)
	}
	return auditKey, nil
}

func validInvoiceRawKey(key string) bool {
	if len(key) < 8 || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' ||
			character == '.' || character == ':') {
			return false
		}
	}
	return true
}

func invoiceResourceKey(actor, action, rawKey string) string {
	return "invoice-resource:" + sha256Hex(strings.TrimSpace(actor)+"\x00"+action+"\x00"+strings.TrimSpace(rawKey))
}

func invoiceSQLiteContention(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "locked") || strings.Contains(lower, "busy") || strings.Contains(lower, "snapshot")
}
