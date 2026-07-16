package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/new-api-tools/backend/internal/toolstore"
)

// OperationReconciliationStatus is the durable audit state exposed to an
// authenticated operator. Pending means an intent exists without an outcome.
type OperationReconciliationStatus string

const (
	OperationPending   OperationReconciliationStatus = "pending"
	OperationSucceeded OperationReconciliationStatus = "succeeded"
	OperationFailed    OperationReconciliationStatus = "failed"
	OperationDenied    OperationReconciliationStatus = "denied"
	OperationCancelled OperationReconciliationStatus = "cancelled"
)

// OperationReconciliation deliberately excludes request bodies, reasons and
// audit snapshots. The endpoint needs only enough evidence to reconcile a
// browser-held idempotency key without disclosing another actor's operation.
type OperationReconciliation struct {
	Status     OperationReconciliationStatus `json:"status"`
	Action     string                        `json:"action"`
	TargetType string                        `json:"target_type"`
	TargetID   string                        `json:"target_id"`
	AuditID    int64                         `json:"audit_id"`
}

// LookupOperationReconciliation resolves the intent/outcome audit pair owned
// by actor and authMethod. Identity mismatches intentionally return
// toolstore.ErrNotFound so a caller cannot use an idempotency key as an oracle
// for another principal or authentication channel.
func LookupOperationReconciliation(
	ctx context.Context,
	store *toolstore.Store,
	idempotencyKey string,
	actor string,
	authMethod string,
) (OperationReconciliation, error) {
	key := strings.TrimSpace(idempotencyKey)
	actor = strings.TrimSpace(actor)
	authMethod = strings.TrimSpace(authMethod)
	if store == nil {
		return OperationReconciliation{}, toolstore.ErrStoreClosed
	}
	if !validReconciliationKey(key) || actor == "" || authMethod == "" {
		return OperationReconciliation{}, toolstore.ErrInvalid
	}

	intent, err := store.GetOperationAuditByIdempotencyKey(ctx, "cp:"+key+":intent")
	if err != nil {
		if errors.Is(err, toolstore.ErrNotFound) {
			_, outcomeErr := store.GetOperationAuditByIdempotencyKey(ctx, "cp:"+key+":outcome")
			if errors.Is(outcomeErr, toolstore.ErrNotFound) {
				return OperationReconciliation{}, toolstore.ErrNotFound
			}
			if outcomeErr != nil {
				return OperationReconciliation{}, fmt.Errorf("read orphaned operation outcome: %w", outcomeErr)
			}
			// An outcome without its intent is an integrity failure regardless of
			// the identity fields stored on the orphan. Mapping those fields first
			// would let corruption collapse into a misleading not-found response.
			return OperationReconciliation{}, errors.New("operation outcome is missing its intent audit")
		}
		return OperationReconciliation{}, fmt.Errorf("read operation intent: %w", err)
	}
	result, err := reconciliationFromAudit(intent, actor, authMethod, ".intent")
	if err != nil {
		return OperationReconciliation{}, err
	}

	outcome, err := store.GetOperationAuditByIdempotencyKey(ctx, "cp:"+key+":outcome")
	if err != nil {
		if errors.Is(err, toolstore.ErrNotFound) {
			result.Status = OperationPending
			return result, nil
		}
		return OperationReconciliation{}, fmt.Errorf("read operation outcome: %w", err)
	}
	if err := validateOperationAuditChain(intent, outcome); err != nil {
		return OperationReconciliation{}, err
	}
	outcomeResult, err := reconciliationFromAudit(outcome, actor, authMethod, ".outcome")
	if err != nil {
		return OperationReconciliation{}, err
	}
	return outcomeResult, nil
}

func reconciliationFromAudit(
	audit toolstore.OperationAudit,
	actor string,
	authMethod string,
	actionSuffix string,
) (OperationReconciliation, error) {
	if strings.TrimSpace(audit.Actor) != actor || strings.TrimSpace(audit.AuthMethod) != authMethod {
		return OperationReconciliation{}, toolstore.ErrNotFound
	}
	if !strings.HasSuffix(audit.Action, actionSuffix) {
		return OperationReconciliation{}, fmt.Errorf("operation audit action %q is malformed", audit.Action)
	}
	action := strings.TrimSuffix(audit.Action, actionSuffix)
	if action == "" || strings.TrimSpace(audit.TargetType) == "" || strings.TrimSpace(audit.TargetID) == "" {
		return OperationReconciliation{}, errors.New("operation audit target is incomplete")
	}

	status := OperationPending
	switch actionSuffix {
	case ".intent":
		if audit.Status != toolstore.OperationSucceeded || strings.TrimSpace(audit.ErrorCode) != "" {
			return OperationReconciliation{}, fmt.Errorf("operation intent status %q is malformed", audit.Status)
		}
		var intent operationIntent
		if len(audit.BeforeJSON) == 0 || json.Unmarshal(audit.BeforeJSON, &intent) != nil || len(intent.Request) == 0 {
			return OperationReconciliation{}, errors.New("operation intent fingerprint is malformed")
		}
	case ".outcome":
		switch audit.Status {
		case toolstore.OperationSucceeded:
			if strings.TrimSpace(audit.ErrorCode) != "" {
				return OperationReconciliation{}, errors.New("successful operation outcome contains an error code")
			}
			status = OperationSucceeded
		case toolstore.OperationFailed:
			status = OperationFailed
		case toolstore.OperationDenied:
			status = OperationDenied
		case toolstore.OperationCancelled:
			status = OperationCancelled
		default:
			return OperationReconciliation{}, fmt.Errorf("operation outcome status %q is unsupported", audit.Status)
		}
	default:
		return OperationReconciliation{}, fmt.Errorf("operation audit suffix %q is unsupported", actionSuffix)
	}

	return OperationReconciliation{
		Status:     status,
		Action:     action,
		TargetType: audit.TargetType,
		TargetID:   audit.TargetID,
		AuditID:    audit.ID,
	}, nil
}

func validateOperationAuditChain(intent, outcome toolstore.OperationAudit) error {
	intentAction := strings.TrimSuffix(intent.Action, ".intent")
	outcomeAction := strings.TrimSuffix(outcome.Action, ".outcome")
	requestID := strings.TrimSpace(intent.RequestID)
	if outcome.ID <= intent.ID || requestID == "" || requestID != strings.TrimSpace(outcome.RequestID) ||
		intentAction == "" || intentAction != outcomeAction ||
		strings.TrimSpace(intent.Actor) != strings.TrimSpace(outcome.Actor) ||
		strings.TrimSpace(intent.AuthMethod) != strings.TrimSpace(outcome.AuthMethod) ||
		strings.TrimSpace(intent.TargetType) != strings.TrimSpace(outcome.TargetType) ||
		strings.TrimSpace(intent.TargetID) != strings.TrimSpace(outcome.TargetID) ||
		strings.TrimSpace(intent.Reason) != strings.TrimSpace(outcome.Reason) ||
		len(intent.BeforeJSON) == 0 || !bytes.Equal(intent.BeforeJSON, outcome.BeforeJSON) {
		return errors.New("operation intent and outcome audit chain is inconsistent")
	}
	return nil
}

func validReconciliationKey(key string) bool {
	if len(key) < 8 || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}
