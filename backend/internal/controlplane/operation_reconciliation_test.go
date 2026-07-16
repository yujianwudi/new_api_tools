package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestLookupOperationReconciliation(t *testing.T) {
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-reconciliation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const key = "operation-reconciliation-1234"

	intent, err := store.AppendOperationAudit(ctx, toolstore.OperationAuditInput{
		RequestID: "operation-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
		Action: "user.disable.intent", TargetType: "user", TargetID: "42", Reason: "risk review",
		BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), AfterJSON: []byte(`{}`),
		Status: toolstore.OperationSucceeded, IdempotencyKey: "cp:" + key + ":intent", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := LookupOperationReconciliation(ctx, store, key, "admin", "jwt")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != OperationPending || pending.Action != "user.disable" || pending.TargetType != "user" ||
		pending.TargetID != "42" || pending.AuditID != intent.ID {
		t.Fatalf("pending reconciliation = %+v", pending)
	}
	if _, err := LookupOperationReconciliation(ctx, store, key, "other-actor", "jwt"); !errors.Is(err, toolstore.ErrNotFound) {
		t.Fatalf("actor mismatch error = %v, want ErrNotFound", err)
	}
	if _, err := LookupOperationReconciliation(ctx, store, key, "admin", "api_key"); !errors.Is(err, toolstore.ErrNotFound) {
		t.Fatalf("auth method mismatch error = %v, want ErrNotFound", err)
	}

	outcome, err := store.AppendOperationAudit(ctx, toolstore.OperationAuditInput{
		RequestID: "operation-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
		Action: "user.disable.outcome", TargetType: "user", TargetID: "42", Reason: "risk review",
		BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), AfterJSON: []byte(`{"after":{"status":2}}`),
		Status: toolstore.OperationSucceeded, IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	succeeded, err := LookupOperationReconciliation(ctx, store, key, "admin", "jwt")
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.Status != OperationSucceeded || succeeded.AuditID != outcome.ID {
		t.Fatalf("succeeded reconciliation = %+v", succeeded)
	}
}

func TestLookupOperationReconciliationOutcomeStatuses(t *testing.T) {
	statuses := []struct {
		storeStatus toolstore.OperationStatus
		want        OperationReconciliationStatus
	}{
		{toolstore.OperationFailed, OperationFailed},
		{toolstore.OperationDenied, OperationDenied},
		{toolstore.OperationCancelled, OperationCancelled},
	}
	for _, test := range statuses {
		t.Run(string(test.storeStatus), func(t *testing.T) {
			store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-status.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			key := "operation-status-" + string(test.storeStatus)
			beforeJSON := []byte(`{"request":{"action":"user.delete"}}`)
			_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
				RequestID: "operation-status-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
				Action: "user.delete.intent", TargetType: "user", TargetID: "7", Reason: "operator review",
				BeforeJSON: beforeJSON, Status: toolstore.OperationSucceeded,
				IdempotencyKey: "cp:" + key + ":intent", OccurredAt: time.Now(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
				RequestID: "operation-status-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
				Action: "user.delete.outcome", TargetType: "user", TargetID: "7", Reason: "operator review",
				BeforeJSON: beforeJSON, AfterJSON: []byte(`{}`), Status: test.storeStatus,
				IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt")
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.want {
				t.Fatalf("status = %q, want %q", result.Status, test.want)
			}
		})
	}
}

func TestLookupOperationReconciliationRejectsIncompleteOrInconsistentChains(t *testing.T) {
	t.Run("orphaned outcome is externally indistinguishable from missing", func(t *testing.T) {
		store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-orphan.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		const key = "operation-orphan-outcome"
		_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
			RequestID: "operation-orphan-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
			Action: "user.disable.outcome", TargetType: "user", TargetID: "42", Reason: "risk review",
			BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), AfterJSON: []byte(`{}`),
			Status: toolstore.OperationSucceeded, IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		previousLogger := logger.L
		logger.L = nil
		t.Cleanup(func() { logger.L = previousLogger })
		_, err = LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt")
		if !errors.Is(err, toolstore.ErrNotFound) {
			t.Fatalf("orphaned outcome error = %v, want ErrNotFound", err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*toolstore.OperationAuditInput)
	}{
		{
			name: "actor tamper",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.Actor = "different-actor"
			},
		},
		{
			name: "auth method tamper",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.AuthMethod = "api_key"
			},
		},
	} {
		t.Run("orphaned outcome "+test.name+" is externally indistinguishable from missing", func(t *testing.T) {
			store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-orphan-identity.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			key := "operation-orphan-" + strings.ReplaceAll(test.name, " ", "-")
			outcome := toolstore.OperationAuditInput{
				RequestID: "operation-orphan-identity-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
				Action: "user.disable.outcome", TargetType: "user", TargetID: "42", Reason: "risk review",
				BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), AfterJSON: []byte(`{}`),
				Status: toolstore.OperationSucceeded, IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
			}
			test.mutate(&outcome)
			if _, err := store.AppendOperationAudit(context.Background(), outcome); err != nil {
				t.Fatal(err)
			}
			_, err = LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt")
			if !errors.Is(err, toolstore.ErrNotFound) {
				t.Fatalf("orphaned tampered outcome error = %v, want ErrNotFound", err)
			}
		})
	}

	t.Run("mismatched immutable fingerprint", func(t *testing.T) {
		store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-mismatch.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		const key = "operation-mismatched-chain"
		base := toolstore.OperationAuditInput{
			Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt", TargetType: "user", TargetID: "42",
			Reason: "risk review", Status: toolstore.OperationSucceeded, OccurredAt: time.Now(),
		}
		intent := base
		intent.RequestID = "operation-mismatch-request"
		intent.Action = "user.disable.intent"
		intent.BeforeJSON = []byte(`{"request":{"action":"user.disable"}}`)
		intent.IdempotencyKey = "cp:" + key + ":intent"
		if _, err := store.AppendOperationAudit(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
		outcome := base
		outcome.RequestID = "operation-mismatch-request"
		outcome.Action = "user.disable.outcome"
		outcome.BeforeJSON = []byte(`{"request":{"action":"user.enable"}}`)
		outcome.AfterJSON = []byte(`{}`)
		outcome.IdempotencyKey = "cp:" + key + ":outcome"
		if _, err := store.AppendOperationAudit(context.Background(), outcome); err != nil {
			t.Fatal(err)
		}
		if _, err := LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt"); err == nil {
			t.Fatal("mismatched operation audit chain was accepted")
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*toolstore.OperationAuditInput)
	}{
		{
			name: "outcome request id mismatch",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.RequestID = "different-operation-request"
			},
		},
		{
			name: "outcome actor mismatch",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.Actor = "different-actor"
			},
		},
		{
			name: "outcome source ip mismatch",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.SourceIP = "198.51.100.23"
			},
		},
		{
			name: "outcome auth method mismatch",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.AuthMethod = "api_key"
			},
		},
		{
			name: "successful outcome with error code",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.ErrorCode = "UNEXPECTED_ERROR"
			},
		},
	} {
		t.Run(test.name+" remains locked", func(t *testing.T) {
			store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-chain-mismatch.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			key := "operation-chain-" + strings.ReplaceAll(test.name, " ", "-")
			beforeJSON := []byte(`{"request":{"action":"user.disable"}}`)
			base := toolstore.OperationAuditInput{
				RequestID: "operation-chain-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
				TargetType: "user", TargetID: "42", Reason: "risk review", BeforeJSON: beforeJSON,
				Status: toolstore.OperationSucceeded, OccurredAt: time.Now(),
			}
			intent := base
			intent.Action = "user.disable.intent"
			intent.IdempotencyKey = "cp:" + key + ":intent"
			if _, err := store.AppendOperationAudit(context.Background(), intent); err != nil {
				t.Fatal(err)
			}
			outcome := base
			outcome.Action = "user.disable.outcome"
			outcome.AfterJSON = []byte(`{}`)
			outcome.IdempotencyKey = "cp:" + key + ":outcome"
			test.mutate(&outcome)
			if _, err := store.AppendOperationAudit(context.Background(), outcome); err != nil {
				t.Fatal(err)
			}
			_, err = LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt")
			if err == nil || errors.Is(err, toolstore.ErrNotFound) {
				t.Fatalf("chain mismatch error = %v, want non-not-found audit-chain error", err)
			}
		})
	}

	t.Run("malformed intent fingerprint", func(t *testing.T) {
		store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-malformed-intent.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		const key = "operation-malformed-intent"
		_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
			RequestID: "operation-malformed-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
			Action: "user.disable.intent", TargetType: "user", TargetID: "42", Reason: "risk review",
			BeforeJSON: []byte(`{}`), Status: toolstore.OperationSucceeded,
			IdempotencyKey: "cp:" + key + ":intent", OccurredAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LookupOperationReconciliation(context.Background(), store, key, "admin", "jwt"); err == nil {
			t.Fatal("malformed operation intent fingerprint was accepted")
		}
	})
}

func TestLookupOperationReconciliationRejectsInvalidKey(t *testing.T) {
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "operation-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := LookupOperationReconciliation(context.Background(), store, "bad/key", "admin", "jwt"); !errors.Is(err, toolstore.ErrInvalid) {
		t.Fatalf("invalid key error = %v, want ErrInvalid", err)
	}
}
