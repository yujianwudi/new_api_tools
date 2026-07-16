package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestOperationReconciliationEndpointIsActorScoped(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	const key = "handler-operation-reconciliation"
	beforeJSON := []byte(`{"request":{"action":"user.enable"}}`)
	_, err := store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-operation-request", Actor: testControlPlaneActor, SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.enable.intent", TargetType: "user", TargetID: "9", Reason: "appeal accepted",
		BeforeJSON: beforeJSON, Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:" + key + ":intent", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-operation-request", Actor: testControlPlaneActor, SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.enable.outcome", TargetType: "user", TargetID: "9", Reason: "appeal accepted",
		BeforeJSON: beforeJSON, AfterJSON: []byte(`{}`), Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	router := newControlPlaneTestRouter(store, true)
	response := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operations/"+key, nil, "operation-reconcile-read")
	if response.Code != http.StatusOK {
		t.Fatalf("operation reconciliation status = %d: %s", response.Code, response.Body.String())
	}
	body := decodeControlPlaneData(t, response)
	if body["status"] != "succeeded" || body["action"] != "user.enable" || body["target_id"] != "9" {
		t.Fatalf("operation reconciliation body = %#v", body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("operation reconciliation response was cacheable: %#v", response.Header())
	}

	otherActorStore, _ := newControlPlaneTestStore(t)
	_, err = otherActorStore.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "other-operation-request", Actor: "different-actor", SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.disable.intent", TargetType: "user", TargetID: "10", Reason: "risk review",
		BeforeJSON: []byte(`{}`), AfterJSON: []byte(`{}`), Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:other-actor-operation:intent", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRouter := newControlPlaneTestRouter(otherActorStore, true)
	denied := performControlPlaneRequest(t, otherRouter, http.MethodGet,
		"/api/control-plane/operations/other-actor-operation", nil, "operation-reconcile-other")
	if denied.Code != http.StatusNotFound || strings.Contains(denied.Body.String(), "different-actor") {
		t.Fatalf("actor-scoped reconciliation = %d: %s", denied.Code, denied.Body.String())
	}

	otherAuthStore, _ := newControlPlaneTestStore(t)
	_, err = otherAuthStore.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "other-auth-request", Actor: testControlPlaneActor, SourceIP: testControlPlaneIP, AuthMethod: "api_key",
		Action: "user.disable.intent", TargetType: "user", TargetID: "11", Reason: "risk review",
		BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:other-auth-operation:intent", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherAuthRouter := newControlPlaneTestRouter(otherAuthStore, true)
	authDenied := performControlPlaneRequest(t, otherAuthRouter, http.MethodGet,
		"/api/control-plane/operations/other-auth-operation", nil, "operation-reconcile-auth")
	if authDenied.Code != http.StatusNotFound || strings.Contains(authDenied.Body.String(), "api_key") {
		t.Fatalf("auth-method-scoped reconciliation = %d: %s", authDenied.Code, authDenied.Body.String())
	}
}

func TestOperationReconciliationEndpointKeepsOwnedBrokenChainLocked(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	const key = "handler-broken-operation-chain"
	beforeJSON := []byte(`{"request":{"action":"user.disable"}}`)
	_, err := store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-broken-request", Actor: testControlPlaneActor, SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.disable.intent", TargetType: "user", TargetID: "77", Reason: "risk review",
		BeforeJSON: beforeJSON, Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:" + key + ":intent", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-broken-request", Actor: "forged-outcome-actor", SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.disable.outcome", TargetType: "user", TargetID: "77", Reason: "risk review",
		BeforeJSON: beforeJSON, AfterJSON: []byte(`{}`), Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	router := newControlPlaneTestRouter(store, true)
	response := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operations/"+key, nil, "operation-reconcile-broken")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken-chain reconciliation status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "forged-outcome-actor") {
		t.Fatalf("broken-chain reconciliation leaked audit details: %s", response.Body.String())
	}
}

func TestOperationReconciliationEndpointKeepsOwnedSourceIPMismatchLocked(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	const key = "handler-source-ip-mismatch-chain"
	beforeJSON := []byte(`{"request":{"action":"user.disable"}}`)
	base := toolstore.OperationAuditInput{
		RequestID: "handler-source-ip-mismatch-request", Actor: testControlPlaneActor,
		SourceIP: testControlPlaneIP, AuthMethod: "jwt", TargetType: "user", TargetID: "78",
		Reason: "risk review", BeforeJSON: beforeJSON, Status: toolstore.OperationSucceeded,
		OccurredAt: time.Now(),
	}
	intent := base
	intent.Action = "user.disable.intent"
	intent.IdempotencyKey = "cp:" + key + ":intent"
	if _, err := store.AppendOperationAudit(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	outcome := base
	outcome.SourceIP = "198.51.100.23"
	outcome.Action = "user.disable.outcome"
	outcome.AfterJSON = []byte(`{}`)
	outcome.IdempotencyKey = "cp:" + key + ":outcome"
	if _, err := store.AppendOperationAudit(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}

	router := newControlPlaneTestRouter(store, true)
	response := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operations/"+key, nil, "operation-reconcile-source-ip-mismatch")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("source-IP-mismatched chain reconciliation status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), outcome.SourceIP) {
		t.Fatalf("source-IP-mismatched chain leaked audit details: %s", response.Body.String())
	}
}

func TestOperationReconciliationEndpointHidesOrphanedOutcomeAsMissing(t *testing.T) {
	store, path := newControlPlaneTestStore(t)
	const key = "handler-orphaned-operation"
	_, err := store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-orphan-request", Actor: testControlPlaneActor, SourceIP: testControlPlaneIP, AuthMethod: "jwt",
		Action: "user.hard_delete.outcome", TargetType: "user", TargetID: "sensitive-target-77",
		Reason: "sensitive operator reason", BeforeJSON: []byte(`{"request":{"action":"user.hard_delete"}}`),
		AfterJSON: []byte(`{"after":{"exists":false}}`), Status: toolstore.OperationSucceeded,
		IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	router := newControlPlaneTestRouter(store, true)
	response := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operations/"+key, nil, "operation-reconcile-orphan")
	if response.Code != http.StatusNotFound {
		t.Fatalf("orphaned reconciliation status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("orphaned reconciliation response was cacheable: %#v", response.Header())
	}
	body := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{
		"hard_delete", "sensitive-target", "sensitive operator", "missing its intent",
		"sqlite", strings.ToLower(path), key,
	} {
		if strings.Contains(body, strings.ToLower(forbidden)) {
			t.Fatalf("orphaned reconciliation leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestOperationReconciliationEndpointHidesOrphanIdentityTamperingAsMissing(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*toolstore.OperationAuditInput)
	}{
		{
			name: "actor",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.Actor = "forged-orphan-actor"
			},
		},
		{
			name: "auth_method",
			mutate: func(outcome *toolstore.OperationAuditInput) {
				outcome.AuthMethod = "api_key"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newControlPlaneTestStore(t)
			key := "handler-orphan-tampered-" + test.name
			outcome := toolstore.OperationAuditInput{
				RequestID: "handler-orphan-tampered-request", Actor: testControlPlaneActor,
				SourceIP: testControlPlaneIP, AuthMethod: "jwt", Action: "user.disable.outcome",
				TargetType: "user", TargetID: "sensitive-target-88", Reason: "sensitive orphan reason",
				BeforeJSON: []byte(`{"request":{"action":"user.disable"}}`), AfterJSON: []byte(`{}`),
				Status: toolstore.OperationSucceeded, IdempotencyKey: "cp:" + key + ":outcome", OccurredAt: time.Now(),
			}
			test.mutate(&outcome)
			if _, err := store.AppendOperationAudit(context.Background(), outcome); err != nil {
				t.Fatal(err)
			}

			router := newControlPlaneTestRouter(store, true)
			response := performControlPlaneRequest(t, router, http.MethodGet,
				"/api/control-plane/operations/"+key, nil, "operation-reconcile-orphan-tampered")
			if response.Code != http.StatusNotFound {
				t.Fatalf("orphan identity tamper status = %d: %s", response.Code, response.Body.String())
			}
			body := strings.ToLower(response.Body.String())
			for _, forbidden := range []string{"forged-orphan", "api_key", "sensitive-target", "sensitive orphan", key} {
				if strings.Contains(body, strings.ToLower(forbidden)) {
					t.Fatalf("orphan identity tamper leaked %q: %s", forbidden, response.Body.String())
				}
			}
		})
	}
}

func TestControlPlaneMutationIdentityMatchesAuditedAuthenticationIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	jwtRecorder := httptest.NewRecorder()
	jwtContext, _ := gin.CreateTestContext(jwtRecorder)
	jwtContext.Set("auth_method", "jwt")
	jwtContext.Set("user_sub", "operator-42")
	if actor, authMethod := controlPlaneMutationIdentity(jwtContext); actor != "operator-42" || authMethod != "jwt" {
		t.Fatalf("JWT reconciliation identity = %q/%q", actor, authMethod)
	}

	apiRecorder := httptest.NewRecorder()
	apiContext, _ := gin.CreateTestContext(apiRecorder)
	apiContext.Set("auth_method", "api_key")
	auth.SetRole(apiContext, auth.RoleOperator)
	if actor, authMethod := controlPlaneMutationIdentity(apiContext); actor != "api-key-operator" || authMethod != "api_key" {
		t.Fatalf("API-key reconciliation identity = %q/%q", actor, authMethod)
	}
}
