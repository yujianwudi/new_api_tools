package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/requestmeta"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	testControlPlaneActor = "admin-42"
	testControlPlaneIP    = "203.0.113.44"
)

func newControlPlaneTestStore(t *testing.T) (*toolstore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-plane-secret-path.db")
	store, err := toolstore.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func newControlPlaneTestRouter(store *toolstore.Store, authenticated bool) *gin.Engine {
	return newControlPlaneTestRouterWithRole(store, authenticated, auth.RoleAdmin)
}

func newControlPlaneTestRouterWithRole(store *toolstore.Store, authenticated bool, role auth.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if authenticated {
		router.Use(func(c *gin.Context) {
			c.Set("auth_method", "jwt")
			c.Set("user_sub", testControlPlaneActor)
			if role != auth.RoleInvalid {
				auth.SetRole(c, role)
			}
			requestID := c.GetHeader("X-Test-Request-ID")
			if requestID == "" {
				requestID = "test-request-0001"
			}
			c.Set("request_id", requestID)
			c.Request = c.Request.WithContext(requestmeta.WithRequestID(c.Request.Context(), requestID))
			c.Next()
		})
	}
	NewStoreHandler(store).RegisterRoutes(router.Group("/api"))
	return router
}

func performControlPlaneRequest(t *testing.T, router http.Handler, method, path string, body any, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	idempotencyKey := ""
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		idempotencyKey = requestID
	}
	return performControlPlaneRequestWithMetadata(t, router, method, path, body, requestID, idempotencyKey, testControlPlaneIP)
}

func performControlPlaneRequestWithMetadata(
	t *testing.T,
	router http.Handler,
	method, path string,
	body any,
	requestID, idempotencyKey, sourceIP string,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	switch value := body.(type) {
	case nil:
	case []byte:
		payload = value
	case string:
		payload = []byte(value)
	default:
		var err error
		payload, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.RemoteAddr = sourceIP + ":43123"
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		request.Header.Set("X-Test-Request-ID", requestID)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeControlPlaneData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	if !envelope.Success {
		t.Fatalf("response was not successful: %s", recorder.Body.String())
	}
	return envelope.Data
}

func TestStoreHandlerRequiresAuthenticationAndEnforcesBounds(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	unauthenticated := newControlPlaneTestRouter(store, false)
	response := performControlPlaneRequest(t, unauthenticated, http.MethodGet,
		"/api/control-plane/operation-audits", nil, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d: %s", response.Code, response.Body.String())
	}

	authenticated := newControlPlaneTestRouter(store, true)
	response = performControlPlaneRequest(t, authenticated, http.MethodGet,
		"/api/control-plane/risk-cases?limit=201", nil, "bounds-request-0001")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized page status = %d: %s", response.Code, response.Body.String())
	}
	response = performControlPlaneRequest(t, authenticated, http.MethodGet,
		"/api/control-plane/risk-cases?unknown=value", nil, "bounds-request-0002")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status = %d: %s", response.Code, response.Body.String())
	}
	response = performControlPlaneRequest(t, authenticated, http.MethodPost,
		"/api/control-plane/risk-cases", map[string]any{
			"case_key": "case-unknown", "title": "test", "subject_type": "user",
			"subject_id": "42", "severity": "high", "reason": "test",
			"unexpected": true,
		}, "bounds-request-0003")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field status = %d: %s", response.Code, response.Body.String())
	}
	response = performControlPlaneRequestWithMetadata(t, authenticated, http.MethodPost,
		"/api/control-plane/risk-cases", map[string]any{
			"case_key": "missing-idempotency", "title": "test", "subject_type": "user",
			"subject_id": "42", "severity": "high", "reason": "test missing header",
		}, "bounds-request-0005", "", testControlPlaneIP)
	if response.Code != http.StatusPreconditionRequired || !strings.Contains(response.Body.String(), "IDEMPOTENCY_KEY_REQUIRED") {
		t.Fatalf("missing idempotency header status = %d: %s", response.Code, response.Body.String())
	}
	tooLarge := `{"case_key":"large","title":"` + strings.Repeat("x", controlPlaneBodyLimit) + `"}`
	response = performControlPlaneRequest(t, authenticated, http.MethodPost,
		"/api/control-plane/risk-cases", tooLarge, "bounds-request-0004")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d: %s", response.Code, response.Body.String())
	}
}

func TestStoreHandlerAppliesExplicitViewerOperatorAndAdminRoles(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	viewer := newControlPlaneTestRouterWithRole(store, true, auth.RoleViewer)
	response := performControlPlaneRequest(t, viewer, http.MethodGet,
		"/api/control-plane/operation-audits", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("viewer read status = %d: %s", response.Code, response.Body.String())
	}
	riskBody := map[string]any{
		"case_key": "role-case-viewer", "title": "Role case", "subject_type": "user", "subject_id": "7",
		"severity": "medium", "status": "open", "summary": "role enforcement", "reason": "role test",
	}
	response = performControlPlaneRequest(t, viewer, http.MethodPost,
		"/api/control-plane/risk-cases", riskBody, "role-viewer-write")
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer write status = %d: %s", response.Code, response.Body.String())
	}

	operator := newControlPlaneTestRouterWithRole(store, true, auth.RoleOperator)
	riskBody["case_key"] = "role-case-operator"
	response = performControlPlaneRequest(t, operator, http.MethodPost,
		"/api/control-plane/risk-cases", riskBody, "role-operator-write")
	if response.Code != http.StatusCreated {
		t.Fatalf("operator risk write status = %d: %s", response.Code, response.Body.String())
	}
	priceBody := map[string]any{
		"provider": "openai", "model": "gpt-role", "operation": "chat", "component": "input",
		"currency": "USD", "unit": "token", "unit_size": 1000000, "amount_decimal": "1.00",
		"amount_minor": 100, "minor_unit_scale": 2, "source": "role-test", "reason": "role test",
	}
	response = performControlPlaneRequest(t, operator, http.MethodPost,
		"/api/control-plane/price-snapshots", priceBody, "role-operator-price")
	if response.Code != http.StatusForbidden {
		t.Fatalf("operator admin-only write status = %d: %s", response.Code, response.Body.String())
	}
}

func TestStoreHandlerRiskFlowUsesTrustedMetadataAndCursorAuditAPI(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	router := newControlPlaneTestRouter(store, true)
	createResponse := performControlPlaneRequest(t, router, http.MethodPost,
		"/api/control-plane/risk-cases", map[string]any{
			"case_key": "risk-handler-1", "title": "Credential sharing",
			"subject_type": "user", "subject_id": "42", "severity": "high",
			"status": "open", "summary": "initial evidence", "reason": "triage opened",
			"actor": "attacker", "auth_method": "forged", "source_ip": "198.51.100.1",
			"request_id": "forged-request",
		}, "trusted-risk-request-0001")
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create risk case = %d: %s", createResponse.Code, createResponse.Body.String())
	}
	createdData := decodeControlPlaneData(t, createResponse)
	caseID := int64(createdData["id"].(float64))
	getCase := performControlPlaneRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/control-plane/risk-cases/%d", caseID), nil, "risk-read-request-0001")
	if getCase.Code != http.StatusOK || int64(decodeControlPlaneData(t, getCase)["id"].(float64)) != caseID {
		t.Fatalf("get risk case = %d: %s", getCase.Code, getCase.Body.String())
	}
	listCases := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/risk-cases?subject_type=user&subject_id=42", nil, "risk-read-request-0002")
	if listCases.Code != http.StatusOK || len(decodeControlPlaneData(t, listCases)["items"].([]any)) != 1 {
		t.Fatalf("list risk cases = %d: %s", listCases.Code, listCases.Body.String())
	}

	eventResponse := performControlPlaneRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/control-plane/risk-cases/%d/events", caseID), map[string]any{
			"case_id": 999999, "event_type": "evidence_attached", "actor": "attacker",
			"event_actor": "attacker-2", "details": map[string]any{"request_id": "upstream-1"},
			"reason": "attach request evidence", "auth_method": "forged",
			"source_ip": "198.51.100.2", "request_id": "forged-request-2",
		}, "trusted-risk-request-0002")
	if eventResponse.Code != http.StatusCreated {
		t.Fatalf("append risk event = %d: %s", eventResponse.Code, eventResponse.Body.String())
	}
	eventData := decodeControlPlaneData(t, eventResponse)
	if eventData["actor"] != testControlPlaneActor || int64(eventData["case_id"].(float64)) != caseID {
		t.Fatalf("event trusted metadata not enforced: %#v", eventData)
	}
	retryEvent := performControlPlaneRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/control-plane/risk-cases/%d/events", caseID), map[string]any{
			"case_id": 999999, "event_type": "evidence_attached", "actor": "attacker",
			"event_actor": "attacker-2", "details": map[string]any{"request_id": "upstream-1"},
			"reason": "attach request evidence", "auth_method": "forged",
			"source_ip": "198.51.100.2", "request_id": "forged-request-2",
		}, "trusted-risk-request-0002")
	if retryEvent.Code != http.StatusCreated {
		t.Fatalf("retry risk event = %d: %s", retryEvent.Code, retryEvent.Body.String())
	}
	if retryID := int64(decodeControlPlaneData(t, retryEvent)["id"].(float64)); retryID != int64(eventData["id"].(float64)) {
		t.Fatalf("retry risk event id = %d, want %v", retryID, eventData["id"])
	}
	conflictingEvent := performControlPlaneRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/control-plane/risk-cases/%d/events", caseID), map[string]any{
			"event_type": "evidence_attached", "details": map[string]any{"request_id": "upstream-2"},
			"reason": "attach request evidence",
		}, "trusted-risk-request-0002")
	if conflictingEvent.Code != http.StatusConflict {
		t.Fatalf("conflicting risk event = %d: %s", conflictingEvent.Code, conflictingEvent.Body.String())
	}
	eventList := performControlPlaneRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/control-plane/risk-cases/%d/events?event_type=evidence_attached", caseID),
		nil, "risk-read-request-0003")
	if eventList.Code != http.StatusOK || len(decodeControlPlaneData(t, eventList)["items"].([]any)) != 1 {
		t.Fatalf("list risk events = %d: %s", eventList.Code, eventList.Body.String())
	}
	notFoundEvents := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/risk-cases/999999/events", nil, "risk-read-request-0004")
	if notFoundEvents.Code != http.StatusNotFound {
		t.Fatalf("missing risk event list = %d: %s", notFoundEvents.Code, notFoundEvents.Body.String())
	}

	closedAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	updateResponse := performControlPlaneRequest(t, router, http.MethodPut,
		fmt.Sprintf("/api/control-plane/risk-cases/%d", caseID), map[string]any{
			"title": "Credential sharing", "severity": "critical", "status": "closed",
			"assignee": "risk-team", "summary": "confirmed", "closed_at": closedAt,
			"reason": "case confirmed by analyst", "actor": "attacker",
		}, "trusted-risk-request-0003")
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update risk case = %d: %s", updateResponse.Code, updateResponse.Body.String())
	}

	audits, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits.Items) != 3 {
		t.Fatalf("audit count = %d, want 3", len(audits.Items))
	}
	for _, audit := range audits.Items {
		if audit.Actor != testControlPlaneActor || audit.AuthMethod != "jwt" ||
			audit.SourceIP != testControlPlaneIP || strings.HasPrefix(audit.RequestID, "forged") {
			t.Fatalf("untrusted audit metadata: %+v", audit)
		}
	}
	getAudit := performControlPlaneRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/control-plane/operation-audits/%d", audits.Items[0].ID), nil,
		"audit-read-request-0000")
	if getAudit.Code != http.StatusOK || int64(decodeControlPlaneData(t, getAudit)["id"].(float64)) != audits.Items[0].ID {
		t.Fatalf("get audit = %d: %s", getAudit.Code, getAudit.Body.String())
	}

	firstPage := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operation-audits?limit=2", nil, "audit-read-request-0001")
	if firstPage.Code != http.StatusOK {
		t.Fatalf("audit first page = %d: %s", firstPage.Code, firstPage.Body.String())
	}
	pageData := decodeControlPlaneData(t, firstPage)
	items := pageData["items"].([]any)
	if len(items) != 2 || pageData["has_more"] != true || pageData["next_cursor"] == nil {
		t.Fatalf("audit first page = %#v", pageData)
	}
	cursor := int64(pageData["next_cursor"].(float64))
	secondPage := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/operation-audits?limit=2&cursor="+strconv.FormatInt(cursor, 10), nil,
		"audit-read-request-0002")
	if secondPage.Code != http.StatusOK {
		t.Fatalf("audit second page = %d: %s", secondPage.Code, secondPage.Body.String())
	}
	secondData := decodeControlPlaneData(t, secondPage)
	if len(secondData["items"].([]any)) != 1 || secondData["has_more"] != false {
		t.Fatalf("audit second page = %#v", secondData)
	}
}

func TestStoreHandlerWritesReplayByActorActionAndHeaderKey(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	router := newControlPlaneTestRouter(store, true)
	const key = "shared-control-plane-key-0001"

	writeAndReplay := func(method, path string, firstBody, retryBody any, wantStatus int, requestPrefix string) map[string]any {
		t.Helper()
		first := performControlPlaneRequestWithMetadata(t, router, method, path, firstBody,
			requestPrefix+"-first", key, "198.51.100.10")
		if first.Code != wantStatus {
			t.Fatalf("first %s %s = %d: %s", method, path, first.Code, first.Body.String())
		}
		firstData := decodeControlPlaneData(t, first)

		retry := performControlPlaneRequestWithMetadata(t, router, method, path, retryBody,
			requestPrefix+"-retry", key, "198.51.100.11")
		if retry.Code != wantStatus {
			t.Fatalf("retry %s %s = %d: %s", method, path, retry.Code, retry.Body.String())
		}
		retryData := decodeControlPlaneData(t, retry)
		if !reflect.DeepEqual(retryData, firstData) {
			t.Fatalf("retry %s %s returned a different result:\nfirst=%#v\nretry=%#v", method, path, firstData, retryData)
		}
		return firstData
	}

	riskBody := map[string]any{
		"case_key": "header-idempotency-risk", "title": "Header replay",
		"subject_type": "user", "subject_id": "42", "severity": "high",
		"status": "open", "summary": "initial", "reason": "open replay test case",
	}
	risk := writeAndReplay(http.MethodPost, "/api/control-plane/risk-cases",
		riskBody, riskBody, http.StatusCreated, "risk-create-request")
	riskID := int64(risk["id"].(float64))

	riskUpdate := map[string]any{
		"title": "Header replay", "severity": "critical", "status": "investigating",
		"assignee": "risk-team", "summary": "confirmed", "reason": "advance replay test case",
	}
	writeAndReplay(http.MethodPut, fmt.Sprintf("/api/control-plane/risk-cases/%d", riskID),
		riskUpdate, riskUpdate, http.StatusOK, "risk-update-request")

	eventBody := map[string]any{
		"event_type": "evidence_attached", "details": map[string]any{"request_id": "upstream-42"},
		"reason": "attach replay evidence",
	}
	writeAndReplay(http.MethodPost, fmt.Sprintf("/api/control-plane/risk-cases/%d/events", riskID),
		eventBody, eventBody, http.StatusCreated, "risk-event-request")

	noteFirstBody := map[string]any{
		"subject_type": "request", "subject_id": "req-header-replay",
		"body": "Customer reports a retry.", "visibility": "internal",
		"idempotency_key": "untrusted-body-key-one", "reason": "record retry report",
	}
	noteRetryBody := map[string]any{
		"subject_type": "request", "subject_id": "req-header-replay",
		"body": "Customer reports a retry.", "visibility": "internal",
		"idempotency_key": "untrusted-body-key-two", "reason": "record retry report",
	}
	note := writeAndReplay(http.MethodPost, "/api/control-plane/support-notes",
		noteFirstBody, noteRetryBody, http.StatusCreated, "note-create-request")
	if note["idempotency_key"] == "untrusted-body-key-one" || note["idempotency_key"] == "untrusted-body-key-two" {
		t.Fatalf("body idempotency key was trusted: %#v", note)
	}
	noteID := int64(note["id"].(float64))

	noteUpdate := map[string]any{
		"body": "Retry confirmed upstream.", "visibility": "customer", "reason": "publish retry finding",
	}
	writeAndReplay(http.MethodPut, fmt.Sprintf("/api/control-plane/support-notes/%d", noteID),
		noteUpdate, noteUpdate, http.StatusOK, "note-update-request")
	noteDelete := map[string]any{"reason": "retry case resolved"}
	writeAndReplay(http.MethodDelete, fmt.Sprintf("/api/control-plane/support-notes/%d", noteID),
		noteDelete, noteDelete, http.StatusOK, "note-delete-request")

	priceFirstBody := map[string]any{
		"provider": "openai", "model": "gpt-header-replay", "operation": "responses",
		"component": "input", "currency": "USD", "unit": "token", "unit_size": 1,
		"amount_decimal": "0.000001", "amount_minor": 1, "minor_unit_scale": 6,
		"source": "provider sheet", "idempotency_key": "untrusted-price-one",
		"reason": "record provider price",
	}
	priceRetryBody := map[string]any{
		"provider": "openai", "model": "gpt-header-replay", "operation": "responses",
		"component": "input", "currency": "USD", "unit": "token", "unit_size": 1,
		"amount_decimal": "0.000001", "amount_minor": 1, "minor_unit_scale": 6,
		"source": "provider sheet", "idempotency_key": "untrusted-price-two",
		"reason": "record provider price",
	}
	price := writeAndReplay(http.MethodPost, "/api/control-plane/price-snapshots",
		priceFirstBody, priceRetryBody, http.StatusCreated, "price-create-request")
	if price["idempotency_key"] == "untrusted-price-one" || price["idempotency_key"] == "untrusted-price-two" {
		t.Fatalf("price body idempotency key was trusted: %#v", price)
	}

	audits, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits.Items) != 7 {
		t.Fatalf("audit count = %d, want one per distinct action: %+v", len(audits.Items), audits.Items)
	}
}

func TestStoreHandlerRollsBackMutationWhenAuditInsertFails(t *testing.T) {
	store, path := newControlPlaneTestStore(t)
	router := newControlPlaneTestRouter(store, true)

	faultDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = faultDB.Close() })
	if _, err := faultDB.Exec(`CREATE TRIGGER fail_operation_audit
		BEFORE INSERT ON operation_audit BEGIN
			SELECT RAISE(ABORT, 'injected audit failure');
		END`); err != nil {
		t.Fatal(err)
	}

	response := performControlPlaneRequest(t, router, http.MethodPost,
		"/api/control-plane/risk-cases", map[string]any{
			"case_key": "risk-audit-rollback", "title": "Must roll back",
			"subject_type": "user", "subject_id": "rollback-user", "severity": "high",
			"status": "open", "summary": "injected failure", "reason": "test transaction rollback",
		}, "risk-audit-rollback-request")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit failure status = %d: %s", response.Code, response.Body.String())
	}

	cases, err := store.ListRiskCases(context.Background(), toolstore.RiskCaseFilter{
		SubjectType: "user", SubjectID: "rollback-user", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases.Items) != 0 {
		t.Fatalf("risk case survived failed audit: %+v", cases.Items)
	}
	audits, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits.Items) != 0 {
		t.Fatalf("audit row survived failed transaction: %+v", audits.Items)
	}
}

func TestStoreHandlerSupportPriceAndReconciliationAPIs(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	router := newControlPlaneTestRouter(store, true)
	noteResponse := performControlPlaneRequest(t, router, http.MethodPost,
		"/api/control-plane/support-notes", map[string]any{
			"subject_type": "request", "subject_id": "req-123", "author": "attacker",
			"body": "Customer reports a stream interruption.", "visibility": "internal",
			"idempotency_key": "note-handler-1", "reason": "support investigation",
		}, "support-request-0001")
	if noteResponse.Code != http.StatusCreated {
		t.Fatalf("create note = %d: %s", noteResponse.Code, noteResponse.Body.String())
	}
	noteData := decodeControlPlaneData(t, noteResponse)
	if noteData["author"] != testControlPlaneActor {
		t.Fatalf("note author = %#v", noteData["author"])
	}
	noteID := int64(noteData["id"].(float64))
	updateResponse := performControlPlaneRequest(t, router, http.MethodPut,
		fmt.Sprintf("/api/control-plane/support-notes/%d", noteID), map[string]any{
			"body": "Upstream interruption confirmed.", "visibility": "customer",
			"reason": "publish verified finding",
		}, "support-request-0002")
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update note = %d: %s", updateResponse.Code, updateResponse.Body.String())
	}
	deleteResponse := performControlPlaneRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/control-plane/support-notes/%d", noteID),
		map[string]any{"reason": "case resolved"}, "support-request-0003")
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete note = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	activeNotes := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/support-notes?subject_type=request&subject_id=req-123", nil,
		"support-read-0001")
	if got := len(decodeControlPlaneData(t, activeNotes)["items"].([]any)); got != 0 {
		t.Fatalf("active note count = %d", got)
	}
	allNotes := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/support-notes?include_deleted=true", nil, "support-read-0002")
	if got := len(decodeControlPlaneData(t, allNotes)["items"].([]any)); got != 1 {
		t.Fatalf("all note count = %d", got)
	}

	priceResponse := performControlPlaneRequest(t, router, http.MethodPost,
		"/api/control-plane/price-snapshots", map[string]any{
			"provider": "openai", "model": "gpt-test", "operation": "responses",
			"component": "input", "currency": "USD", "unit": "token", "unit_size": 1,
			"amount_decimal": "0.000001", "amount_minor": 1, "minor_unit_scale": 6,
			"source": "provider sheet", "metadata": map[string]any{"version": "2026-07"},
			"idempotency_key": "price-handler-1", "reason": "monthly provider update",
			"actor": "attacker", "source_ip": "198.51.100.9",
		}, "price-request-0001")
	if priceResponse.Code != http.StatusCreated {
		t.Fatalf("create price = %d: %s", priceResponse.Code, priceResponse.Body.String())
	}
	priceData := decodeControlPlaneData(t, priceResponse)
	if priceData["amount_decimal"] != "0.000001" || int64(priceData["amount_minor"].(float64)) != 1 {
		t.Fatalf("price data = %#v", priceData)
	}
	priceList := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/price-snapshots?provider=openai&limit=1", nil, "price-read-0001")
	if priceList.Code != http.StatusOK || len(decodeControlPlaneData(t, priceList)["items"].([]any)) != 1 {
		t.Fatalf("price list = %d: %s", priceList.Code, priceList.Body.String())
	}

	windowEnd := time.Now().UTC().Truncate(time.Second)
	run, err := store.CreateReconciliationRun(context.Background(), toolstore.ReconciliationRunInput{
		RunKey: "handler-recon-1", Kind: "daily_usage", Status: toolstore.ReconciliationRunning,
		WindowStart: windowEnd.Add(-24 * time.Hour), WindowEnd: windowEnd, Currency: "CNY",
	})
	if err != nil {
		t.Fatal(err)
	}
	runList := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/reconciliation-runs?kind=daily_usage", nil, "recon-read-0001")
	if runList.Code != http.StatusOK || len(decodeControlPlaneData(t, runList)["items"].([]any)) != 1 {
		t.Fatalf("run list = %d: %s", runList.Code, runList.Body.String())
	}
	runGet := performControlPlaneRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/control-plane/reconciliation-runs/%d", run.ID), nil, "recon-read-0002")
	if runGet.Code != http.StatusOK || int64(decodeControlPlaneData(t, runGet)["id"].(float64)) != run.ID {
		t.Fatalf("run get = %d: %s", runGet.Code, runGet.Body.String())
	}
}

func TestStoreHandlerDoesNotLeakSQLiteErrorsOrPaths(t *testing.T) {
	store, path := newControlPlaneTestStore(t)
	router := newControlPlaneTestRouter(store, true)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response := performControlPlaneRequest(t, router, http.MethodGet,
		"/api/control-plane/risk-cases", nil, "closed-store-request-0001")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store status = %d: %s", response.Code, response.Body.String())
	}
	body := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{"sqlite", "database is closed", strings.ToLower(path), "control-plane-secret-path"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}
