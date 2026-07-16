package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/requestmeta"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	controlPlaneBodyLimit    = 128 << 10
	controlPlaneQueryLimit   = 4 << 10
	controlPlaneMaxPageSize  = 200
	controlPlaneDefaultLimit = 50
	controlPlaneJSONLimit    = 32 << 10
)

// StoreHandler exposes the independent Tool Store. RegisterRoutes expects the
// caller's protected API group and also installs explicit authentication and
// per-route RBAC checks as defense in depth.
type StoreHandler struct {
	store *toolstore.Store
}

func NewStoreHandler(store *toolstore.Store) *StoreHandler {
	return &StoreHandler{store: store}
}

// RegisterRoutes registers authenticated sidecar APIs below /control-plane.
func (h *StoreHandler) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/control-plane")
	authenticated := requireControlPlaneAuthentication()
	viewer := auth.RequireRole(auth.RoleViewer)
	operator := auth.RequireRole(auth.RoleOperator)
	admin := auth.RequireRole(auth.RoleAdmin)

	group.GET("/operation-audits", authenticated, viewer, h.ListOperationAudits)
	group.GET("/operation-audits/:id", authenticated, viewer, h.GetOperationAudit)

	group.POST("/risk-cases", authenticated, operator, h.CreateRiskCase)
	group.GET("/risk-cases", authenticated, viewer, h.ListRiskCases)
	group.GET("/risk-cases/:id", authenticated, viewer, h.GetRiskCase)
	group.PUT("/risk-cases/:id", authenticated, operator, h.UpdateRiskCase)
	group.POST("/risk-cases/:id/events", authenticated, operator, h.AppendRiskCaseEvent)
	group.GET("/risk-cases/:id/events", authenticated, viewer, h.ListRiskCaseEvents)

	group.POST("/support-notes", authenticated, operator, h.CreateSupportNote)
	group.GET("/support-notes", authenticated, viewer, h.ListSupportNotes)
	group.PUT("/support-notes/:id", authenticated, operator, h.UpdateSupportNote)
	group.DELETE("/support-notes/:id", authenticated, operator, h.DeleteSupportNote)

	group.POST("/price-snapshots", authenticated, admin, h.CreatePriceSnapshot)
	group.GET("/price-snapshots", authenticated, viewer, h.ListPriceSnapshots)

	group.GET("/reconciliation-runs", authenticated, viewer, h.ListReconciliationRuns)
	group.GET("/reconciliation-runs/:id", authenticated, viewer, h.GetReconciliationRun)
}

// ignoredClientIdentity accepts common spoof attempts so they can be
// explicitly overwritten instead of accidentally becoming trusted later.
type ignoredClientIdentity struct {
	Actor      string `json:"actor"`
	AuthMethod string `json:"auth_method"`
	SourceIP   string `json:"source_ip"`
	RequestID  string `json:"request_id"`
}

type riskCaseCreateRequest struct {
	ignoredClientIdentity
	CaseKey     string                   `json:"case_key"`
	Title       string                   `json:"title"`
	SubjectType string                   `json:"subject_type"`
	SubjectID   string                   `json:"subject_id"`
	Severity    toolstore.RiskSeverity   `json:"severity"`
	Status      toolstore.RiskCaseStatus `json:"status"`
	Assignee    string                   `json:"assignee"`
	Summary     string                   `json:"summary"`
	OpenedAt    *time.Time               `json:"opened_at"`
	ClosedAt    *time.Time               `json:"closed_at"`
	Reason      string                   `json:"reason"`
}

type riskCaseUpdateRequest struct {
	ignoredClientIdentity
	Title    string                   `json:"title"`
	Severity toolstore.RiskSeverity   `json:"severity"`
	Status   toolstore.RiskCaseStatus `json:"status"`
	Assignee string                   `json:"assignee"`
	Summary  string                   `json:"summary"`
	ClosedAt *time.Time               `json:"closed_at"`
	Reason   string                   `json:"reason"`
}

type riskCaseEventRequest struct {
	ignoredClientIdentity
	CaseID      int64           `json:"case_id"`
	EventType   string          `json:"event_type"`
	Actor       string          `json:"event_actor"`
	DetailsJSON json.RawMessage `json:"details"`
	OccurredAt  *time.Time      `json:"occurred_at"`
	Reason      string          `json:"reason"`
}

type supportNoteCreateRequest struct {
	ignoredClientIdentity
	SubjectType    string                   `json:"subject_type"`
	SubjectID      string                   `json:"subject_id"`
	Author         string                   `json:"author"`
	Body           string                   `json:"body"`
	Visibility     toolstore.NoteVisibility `json:"visibility"`
	IdempotencyKey string                   `json:"idempotency_key"`
	Reason         string                   `json:"reason"`
}

type supportNoteUpdateRequest struct {
	ignoredClientIdentity
	Body       string                   `json:"body"`
	Visibility toolstore.NoteVisibility `json:"visibility"`
	Reason     string                   `json:"reason"`
}

type supportNoteDeleteRequest struct {
	ignoredClientIdentity
	Reason string `json:"reason"`
}

type priceSnapshotCreateRequest struct {
	ignoredClientIdentity
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Operation      string          `json:"operation"`
	Component      string          `json:"component"`
	Currency       string          `json:"currency"`
	Unit           string          `json:"unit"`
	UnitSize       int64           `json:"unit_size"`
	AmountDecimal  string          `json:"amount_decimal"`
	AmountMinor    int64           `json:"amount_minor"`
	MinorUnitScale int             `json:"minor_unit_scale"`
	Source         string          `json:"source"`
	MetadataJSON   json.RawMessage `json:"metadata"`
	IdempotencyKey string          `json:"idempotency_key"`
	EffectiveAt    *time.Time      `json:"effective_at"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	Reason         string          `json:"reason"`
}

type controlPlaneIdentity struct {
	Actor          string
	AuthMethod     string
	SourceIP       string
	RequestID      string
	IdempotencyKey string
}

func (h *StoreHandler) ListOperationAudits(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	allowed := querySpec{
		"cursor": 20, "limit": 3, "request_id": 128, "actor": 256,
		"action": 128, "target_type": 64, "target_id": 256, "status": 16,
	}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid operation audit query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	page, err := h.store.ListOperationAudits(c.Request.Context(), toolstore.OperationAuditFilter{
		RequestID: c.Query("request_id"), Actor: c.Query("actor"), Action: c.Query("action"),
		TargetType: c.Query("target_type"), TargetID: c.Query("target_id"),
		Status: toolstore.OperationStatus(c.Query("status")), BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list operation audits", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, operationAuditJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) GetOperationAudit(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	item, err := h.store.GetOperationAudit(c.Request.Context(), id)
	if err != nil {
		h.writeStoreError(c, "get operation audit", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(operationAuditJSON(item)))
}

func (h *StoreHandler) CreateRiskCase(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	var request riskCaseCreateRequest
	if !decodeControlPlaneJSON(c, &request) || !validRiskCaseCreateRequest(request) {
		writeControlPlaneInputError(c, "Invalid risk case payload")
		return
	}
	status := request.Status
	if status == "" {
		status = toolstore.RiskCaseOpen
	}
	var openedAt time.Time
	if request.OpenedAt != nil {
		openedAt = *request.OpenedAt
	}
	audit := mutationAuditInput(identity, "risk_case.create", request.Reason)
	created, _, err := h.store.CreateRiskCaseAudited(c.Request.Context(), toolstore.RiskCaseInput{
		CaseKey: request.CaseKey, Title: request.Title, SubjectType: request.SubjectType,
		SubjectID: request.SubjectID, Severity: request.Severity, Status: status,
		Assignee: request.Assignee, Summary: request.Summary, OpenedAt: openedAt,
		ClosedAt: request.ClosedAt,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "create risk case", err)
		return
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(riskCaseJSON(created)))
}

func (h *StoreHandler) ListRiskCases(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	allowed := querySpec{
		"cursor": 20, "limit": 3, "subject_type": 64, "subject_id": 256,
		"severity": 16, "status": 16, "assignee": 256,
	}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid risk case query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	page, err := h.store.ListRiskCases(c.Request.Context(), toolstore.RiskCaseFilter{
		SubjectType: c.Query("subject_type"), SubjectID: c.Query("subject_id"),
		Severity: toolstore.RiskSeverity(c.Query("severity")), Status: toolstore.RiskCaseStatus(c.Query("status")),
		Assignee: c.Query("assignee"), BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list risk cases", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, riskCaseJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) GetRiskCase(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	item, err := h.store.GetRiskCase(c.Request.Context(), id)
	if err != nil {
		h.writeStoreError(c, "get risk case", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(riskCaseJSON(item)))
}

func (h *StoreHandler) UpdateRiskCase(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	var request riskCaseUpdateRequest
	if !decodeControlPlaneJSON(c, &request) || !validRiskCaseUpdateRequest(request) {
		writeControlPlaneInputError(c, "Invalid risk case payload")
		return
	}
	audit := mutationAuditInput(identity, "risk_case.update", request.Reason)
	updated, _, err := h.store.UpdateRiskCaseAudited(c.Request.Context(), toolstore.RiskCaseUpdate{
		ID: id, Title: request.Title, Severity: request.Severity, Status: request.Status,
		Assignee: request.Assignee, Summary: request.Summary, ClosedAt: request.ClosedAt,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "update risk case", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(riskCaseJSON(updated)))
}

func (h *StoreHandler) AppendRiskCaseEvent(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	caseID, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	var request riskCaseEventRequest
	if !decodeControlPlaneJSON(c, &request) || !validRiskCaseEventRequest(request) {
		writeControlPlaneInputError(c, "Invalid risk case event payload")
		return
	}
	var occurredAt time.Time
	if request.OccurredAt != nil {
		occurredAt = *request.OccurredAt
	}
	audit := mutationAuditInput(identity, "risk_case.event.append", request.Reason)
	event, _, err := h.store.AppendRiskCaseEventAudited(c.Request.Context(), toolstore.RiskCaseEventInput{
		CaseID: caseID, EventType: request.EventType, Actor: identity.Actor,
		DetailsJSON: request.DetailsJSON, OccurredAt: occurredAt,
		IdempotencyKey: audit.IdempotencyKey,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "append risk case event", err)
		return
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(riskCaseEventJSON(event)))
}

func (h *StoreHandler) ListRiskCaseEvents(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	caseID, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	if _, err := h.store.GetRiskCase(c.Request.Context(), caseID); err != nil {
		h.writeStoreError(c, "get risk case before listing events", err)
		return
	}
	allowed := querySpec{"cursor": 20, "limit": 3, "event_type": 128}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid risk event query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	page, err := h.store.ListRiskCaseEvents(c.Request.Context(), toolstore.RiskCaseEventFilter{
		CaseID: caseID, EventType: c.Query("event_type"), BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list risk case events", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, riskCaseEventJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) CreateSupportNote(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	var request supportNoteCreateRequest
	if !decodeControlPlaneJSON(c, &request) || !validSupportNoteCreateRequest(request) {
		writeControlPlaneInputError(c, "Invalid support note payload")
		return
	}
	audit := mutationAuditInput(identity, "support_note.create", request.Reason)
	created, _, err := h.store.CreateSupportNoteAudited(c.Request.Context(), toolstore.SupportNoteInput{
		SubjectType: request.SubjectType, SubjectID: request.SubjectID, Author: identity.Actor,
		Body: request.Body, Visibility: request.Visibility, IdempotencyKey: audit.IdempotencyKey,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "create support note", err)
		return
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(supportNoteJSON(created)))
}

func (h *StoreHandler) ListSupportNotes(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	allowed := querySpec{
		"cursor": 20, "limit": 3, "subject_type": 64, "subject_id": 256,
		"author": 256, "visibility": 16, "include_deleted": 5,
	}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid support note query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	includeDeleted := false
	if raw, present := c.GetQuery("include_deleted"); present {
		switch raw {
		case "true":
			includeDeleted = true
		case "false":
		default:
			writeControlPlaneInputError(c, "Invalid include_deleted value")
			return
		}
	}
	page, err := h.store.ListSupportNotes(c.Request.Context(), toolstore.SupportNoteFilter{
		SubjectType: c.Query("subject_type"), SubjectID: c.Query("subject_id"),
		Author: c.Query("author"), Visibility: toolstore.NoteVisibility(c.Query("visibility")),
		IncludeDeleted: includeDeleted, BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list support notes", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, supportNoteJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) UpdateSupportNote(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	var request supportNoteUpdateRequest
	if !decodeControlPlaneJSON(c, &request) || !validSupportNoteUpdateRequest(request) {
		writeControlPlaneInputError(c, "Invalid support note payload")
		return
	}
	audit := mutationAuditInput(identity, "support_note.update", request.Reason)
	updated, _, err := h.store.UpdateSupportNoteAudited(c.Request.Context(), toolstore.SupportNoteUpdate{
		ID: id, Body: request.Body, Visibility: request.Visibility,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "update support note", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(supportNoteJSON(updated)))
}

func (h *StoreHandler) DeleteSupportNote(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	var request supportNoteDeleteRequest
	if !decodeControlPlaneJSON(c, &request) || !validReason(request.Reason) {
		writeControlPlaneInputError(c, "A bounded deletion reason is required")
		return
	}
	audit := mutationAuditInput(identity, "support_note.delete", request.Reason)
	deleted, _, err := h.store.DeleteSupportNoteAudited(c.Request.Context(), id, audit)
	if err != nil {
		h.writeStoreError(c, "delete support note", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(supportNoteJSON(deleted)))
}

func (h *StoreHandler) CreatePriceSnapshot(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	var request priceSnapshotCreateRequest
	if !decodeControlPlaneJSON(c, &request) || !validPriceSnapshotCreateRequest(request) {
		writeControlPlaneInputError(c, "Invalid price snapshot payload")
		return
	}
	var effectiveAt time.Time
	if request.EffectiveAt != nil {
		effectiveAt = *request.EffectiveAt
	}
	audit := mutationAuditInput(identity, "price_snapshot.create", request.Reason)
	created, _, err := h.store.CreatePriceSnapshotAudited(c.Request.Context(), toolstore.PriceSnapshotInput{
		Provider: request.Provider, Model: request.Model, Operation: request.Operation,
		Component: request.Component, Currency: request.Currency, Unit: request.Unit,
		UnitSize: request.UnitSize, AmountDecimal: request.AmountDecimal,
		AmountMinor: request.AmountMinor, MinorUnitScale: request.MinorUnitScale,
		Source: request.Source, MetadataJSON: request.MetadataJSON,
		IdempotencyKey: audit.IdempotencyKey, EffectiveAt: effectiveAt,
		ExpiresAt: request.ExpiresAt,
	}, audit)
	if err != nil {
		h.writeStoreError(c, "create price snapshot", err)
		return
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(priceSnapshotJSON(created)))
}

func (h *StoreHandler) ListPriceSnapshots(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	allowed := querySpec{
		"cursor": 20, "limit": 3, "provider": 128, "model": 256,
		"operation": 128, "component": 128, "currency": 3, "active_at": 64,
	}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid price snapshot query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	var activeAt *time.Time
	if raw, present := c.GetQuery("active_at"); present {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeControlPlaneInputError(c, "Invalid active_at timestamp")
			return
		}
		activeAt = &parsed
	}
	page, err := h.store.ListPriceSnapshots(c.Request.Context(), toolstore.PriceSnapshotFilter{
		Provider: c.Query("provider"), Model: c.Query("model"), Operation: c.Query("operation"),
		Component: c.Query("component"), Currency: c.Query("currency"), ActiveAt: activeAt,
		BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list price snapshots", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, priceSnapshotJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) ListReconciliationRuns(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	allowed := querySpec{"cursor": 20, "limit": 3, "kind": 128, "status": 16}
	if err := validateControlPlaneQuery(c, allowed); err != nil {
		writeControlPlaneInputError(c, "Invalid reconciliation query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid cursor or limit")
		return
	}
	page, err := h.store.ListReconciliationRuns(c.Request.Context(), toolstore.ReconciliationRunFilter{
		Kind: c.Query("kind"), Status: toolstore.ReconciliationStatus(c.Query("status")),
		BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.writeStoreError(c, "list reconciliation runs", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, reconciliationRunJSON(item))
	}
	writeControlPlanePage(c, items, page.NextCursor, page.HasMore)
}

func (h *StoreHandler) GetReconciliationRun(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	item, err := h.store.GetReconciliationRun(c.Request.Context(), id)
	if err != nil {
		h.writeStoreError(c, "get reconciliation run", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(reconciliationRunJSON(item)))
}

func (h *StoreHandler) begin(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	_, ok := authenticatedControlPlaneActor(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, models.NewErrorResponse(
			"UNAUTHORIZED", "Authenticated control-plane access is required"))
		return false
	}
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"TOOL_STORE_UNAVAILABLE", "Control-plane store is temporarily unavailable"))
		return false
	}
	return true
}

func requireControlPlaneAuthentication() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if _, ok := authenticatedControlPlaneActor(c); !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, models.NewErrorResponse(
				"UNAUTHORIZED", "Authenticated control-plane access is required"))
			return
		}
		c.Next()
	}
}

func (h *StoreHandler) beginWrite(c *gin.Context) (controlPlaneIdentity, bool) {
	if !h.begin(c) {
		return controlPlaneIdentity{}, false
	}
	actor, _ := authenticatedControlPlaneActor(c)
	authMethod := strings.TrimSpace(c.GetString("auth_method"))
	requestID := strings.TrimSpace(requestmeta.RequestID(c.Request.Context()))
	if requestID == "" {
		requestID = strings.TrimSpace(c.GetString("request_id"))
	}
	sourceIP := strings.TrimSpace(c.ClientIP())
	idempotencyKey, validIdempotencyKey := controlPlaneIdempotencyKey(c)
	if !validIdempotencyKey {
		c.JSON(http.StatusPreconditionRequired, models.NewErrorResponse(
			"IDEMPOTENCY_KEY_REQUIRED", "A valid Idempotency-Key header is required"))
		return controlPlaneIdentity{}, false
	}
	if !validAuditScalar(actor, 256) || !validAuditScalar(authMethod, 32) ||
		!validRequestIDValue(requestID) || !validAuditScalar(sourceIP, 64) {
		c.JSON(http.StatusInternalServerError, models.NewErrorResponse(
			"REQUEST_METADATA_UNAVAILABLE", "Trusted request metadata is unavailable"))
		return controlPlaneIdentity{}, false
	}
	return controlPlaneIdentity{
		Actor: actor, AuthMethod: authMethod, SourceIP: sourceIP, RequestID: requestID,
		IdempotencyKey: idempotencyKey,
	}, true
}

func controlPlaneIdempotencyKey(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", false
	}
	key := strings.TrimSpace(values[0])
	if len(key) < 8 || len(key) > 128 {
		return "", false
	}
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':') {
			return "", false
		}
	}
	return key, true
}

func authenticatedControlPlaneActor(c *gin.Context) (string, bool) {
	method := strings.TrimSpace(c.GetString("auth_method"))
	if method != "jwt" && method != "api_key" {
		return "", false
	}
	if actor := strings.TrimSpace(c.GetString("actor")); validAuditScalar(actor, 256) {
		return actor, true
	}
	if subject := strings.TrimSpace(c.GetString("user_sub")); validAuditScalar(subject, 256) {
		return subject, true
	}
	if method == "api_key" {
		if keyID := strings.TrimSpace(c.GetString("api_key_id")); validAuditScalar(keyID, 200) {
			return "api-key:" + keyID, true
		}
		return "api-key", true
	}
	return "", false
}

func mutationAuditInput(identity controlPlaneIdentity, action, reason string) toolstore.OperationAuditInput {
	action = strings.ToLower(strings.TrimSpace(action))
	keyMaterial := strings.TrimSpace(identity.Actor) + "\x00" + action + "\x00" + strings.TrimSpace(identity.IdempotencyKey)
	idempotencyHash := sha256.Sum256([]byte(keyMaterial))
	return toolstore.OperationAuditInput{
		RequestID: identity.RequestID, Actor: identity.Actor, SourceIP: identity.SourceIP,
		AuthMethod: identity.AuthMethod, Action: action, Reason: strings.TrimSpace(reason),
		Status:         toolstore.OperationSucceeded,
		IdempotencyKey: fmt.Sprintf("http:%x", idempotencyHash[:]),
	}
}

func (h *StoreHandler) writeStoreError(c *gin.Context, operation string, err error) {
	switch {
	case errors.Is(err, toolstore.ErrInvalid):
		writeControlPlaneInputError(c, "Invalid control-plane parameters")
	case errors.Is(err, toolstore.ErrNotFound):
		c.JSON(http.StatusNotFound, models.NewErrorResponse("NOT_FOUND", "Control-plane record was not found"))
	case errors.Is(err, toolstore.ErrConflict):
		c.JSON(http.StatusConflict, models.NewErrorResponse("CONFLICT", "Control-plane record conflicts with existing state"))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"TOOL_STORE_UNAVAILABLE", "Control-plane store is temporarily unavailable"))
	default:
		respondHandlerError(c, http.StatusServiceUnavailable, "TOOL_STORE_UNAVAILABLE",
			"Control-plane store is temporarily unavailable", operation,
			errors.New("tool store operation failed"))
	}
}

func decodeControlPlaneJSON(c *gin.Context, destination any) bool {
	if c.ContentType() != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, models.NewErrorResponse(
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json"))
		return false
	}
	if c.Request.ContentLength > controlPlaneBodyLimit {
		writeControlPlaneBodyTooLarge(c)
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, controlPlaneBodyLimit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeControlPlaneBodyTooLarge(c)
			return false
		}
		writeControlPlaneInputError(c, "Invalid JSON request body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeControlPlaneBodyTooLarge(c)
			return false
		}
		writeControlPlaneInputError(c, "Request body must contain one JSON object")
		return false
	}
	return true
}

func writeControlPlaneBodyTooLarge(c *gin.Context) {
	c.JSON(http.StatusRequestEntityTooLarge, models.NewErrorResponse(
		"BODY_TOO_LARGE", "Request body exceeds the allowed size"))
}

func writeControlPlaneInputError(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, models.NewErrorResponse("INVALID_PARAMS", message))
}

type querySpec map[string]int

func validateControlPlaneQuery(c *gin.Context, allowed querySpec) error {
	if len(c.Request.URL.RawQuery) > controlPlaneQueryLimit {
		return errors.New("query string exceeds maximum length")
	}
	for key, values := range c.Request.URL.Query() {
		maximum, ok := allowed[key]
		if !ok {
			return fmt.Errorf("unknown query parameter %q", key)
		}
		if len(values) != 1 || len(values[0]) > maximum || containsUnsafeControl(values[0], false) {
			return fmt.Errorf("invalid query parameter %q", key)
		}
	}
	return nil
}

func parseControlPlanePage(c *gin.Context) (int64, int, error) {
	cursor := int64(0)
	if raw, present := c.GetQuery("cursor"); present {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, 0, errors.New("invalid cursor")
		}
		cursor = parsed
	}
	limit := controlPlaneDefaultLimit
	if raw, present := c.GetQuery("limit"); present {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > controlPlaneMaxPageSize {
			return 0, 0, errors.New("invalid limit")
		}
		limit = parsed
	}
	return cursor, limit, nil
}

func parseControlPlaneID(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	if len(raw) == 0 || len(raw) > 19 {
		writeControlPlaneInputError(c, "Invalid record ID")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeControlPlaneInputError(c, "Invalid record ID")
		return 0, false
	}
	return id, true
}

func writeControlPlanePage(c *gin.Context, items []gin.H, nextCursor int64, hasMore bool) {
	var cursor any
	if hasMore {
		cursor = nextCursor
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"items": items, "next_cursor": cursor, "has_more": hasMore,
	}))
}

func validRiskCaseCreateRequest(request riskCaseCreateRequest) bool {
	status := request.Status
	if status == "" {
		status = toolstore.RiskCaseOpen
	}
	return validScalar(request.CaseKey, 128, true) &&
		validScalar(request.Title, 256, true) &&
		validScalar(request.SubjectType, 64, true) &&
		validScalar(request.SubjectID, 256, true) &&
		validRiskSeverity(request.Severity) && validRiskCaseStatus(status) &&
		validScalar(request.Assignee, 256, false) && validNarrative(request.Summary, 4096, false) &&
		validReason(request.Reason) && validOptionalTimestamp(request.OpenedAt) &&
		validOptionalTimestamp(request.ClosedAt) && validClosure(status, request.ClosedAt)
}

func validRiskCaseUpdateRequest(request riskCaseUpdateRequest) bool {
	return validScalar(request.Title, 256, true) && validRiskSeverity(request.Severity) &&
		validRiskCaseStatus(request.Status) && validScalar(request.Assignee, 256, false) &&
		validNarrative(request.Summary, 4096, false) && validReason(request.Reason) &&
		validOptionalTimestamp(request.ClosedAt) && validClosure(request.Status, request.ClosedAt)
}

func validRiskCaseEventRequest(request riskCaseEventRequest) bool {
	return validScalar(request.EventType, 128, true) && validReason(request.Reason) &&
		len(request.DetailsJSON) <= controlPlaneJSONLimit && validOptionalTimestamp(request.OccurredAt)
}

func validSupportNoteCreateRequest(request supportNoteCreateRequest) bool {
	return validScalar(request.SubjectType, 64, true) && validScalar(request.SubjectID, 256, true) &&
		validNarrative(request.Body, 16<<10, true) && validNoteVisibility(request.Visibility) &&
		validScalar(request.IdempotencyKey, 128, false) && validReason(request.Reason)
}

func validSupportNoteUpdateRequest(request supportNoteUpdateRequest) bool {
	return validNarrative(request.Body, 16<<10, true) && validNoteVisibility(request.Visibility) &&
		validReason(request.Reason)
}

func validPriceSnapshotCreateRequest(request priceSnapshotCreateRequest) bool {
	return validScalar(request.Provider, 128, true) && validScalar(request.Model, 256, true) &&
		validScalar(request.Operation, 128, true) && validScalar(request.Component, 128, true) &&
		validScalar(request.Currency, 3, true) && validScalar(request.Unit, 64, true) &&
		request.UnitSize > 0 && request.UnitSize <= 1_000_000_000_000 &&
		validScalar(request.AmountDecimal, 64, true) && request.AmountMinor >= 0 &&
		request.MinorUnitScale >= 0 && request.MinorUnitScale <= 18 &&
		validScalar(request.Source, 256, true) && len(request.MetadataJSON) <= controlPlaneJSONLimit &&
		validScalar(request.IdempotencyKey, 128, false) && validOptionalTimestamp(request.EffectiveAt) &&
		validOptionalTimestamp(request.ExpiresAt) && validReason(request.Reason)
}

func validRiskSeverity(value toolstore.RiskSeverity) bool {
	switch value {
	case toolstore.RiskSeverityLow, toolstore.RiskSeverityMedium,
		toolstore.RiskSeverityHigh, toolstore.RiskSeverityCritical:
		return true
	default:
		return false
	}
}

func validRiskCaseStatus(value toolstore.RiskCaseStatus) bool {
	switch value {
	case toolstore.RiskCaseOpen, toolstore.RiskCaseInvestigating,
		toolstore.RiskCaseMitigated, toolstore.RiskCaseClosed:
		return true
	default:
		return false
	}
}

func validNoteVisibility(value toolstore.NoteVisibility) bool {
	return value == toolstore.NoteInternal || value == toolstore.NoteCustomer
}

func validClosure(status toolstore.RiskCaseStatus, closedAt *time.Time) bool {
	return (status == toolstore.RiskCaseClosed && closedAt != nil) ||
		(status != toolstore.RiskCaseClosed && closedAt == nil)
}

func validReason(value string) bool {
	return validNarrative(value, 1000, true)
}

func validScalar(value string, maximum int, required bool) bool {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return false
	}
	if len(trimmed) > maximum {
		return false
	}
	return !containsUnsafeControl(trimmed, false)
}

func validNarrative(value string, maximum int, required bool) bool {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return false
	}
	if len(value) > maximum {
		return false
	}
	return !containsUnsafeControl(value, true)
}

func validAuditScalar(value string, maximum int) bool {
	return validScalar(value, maximum, true)
}

func containsUnsafeControl(value string, allowWhitespace bool) bool {
	for _, character := range value {
		if !unicode.IsControl(character) {
			continue
		}
		if allowWhitespace && (character == '\n' || character == '\r' || character == '\t') {
			continue
		}
		return true
	}
	return false
}

func validRequestIDValue(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			return false
		}
	}
	return true
}

func validOptionalTimestamp(value *time.Time) bool {
	if value == nil {
		return true
	}
	return !value.IsZero() && value.UnixMilli() >= 0 && value.Year() <= 3000
}

func operationAuditJSON(item toolstore.OperationAudit) gin.H {
	return gin.H{
		"id": item.ID, "request_id": item.RequestID, "actor": item.Actor,
		"source_ip": item.SourceIP, "auth_method": item.AuthMethod, "action": item.Action,
		"target_type": item.TargetType, "target_id": item.TargetID, "reason": item.Reason,
		"before": rawJSONValue(item.BeforeJSON), "after": rawJSONValue(item.AfterJSON),
		"status": item.Status, "error_code": item.ErrorCode,
		"idempotency_key": item.IdempotencyKey, "occurred_at": formatControlPlaneTime(item.OccurredAt),
		"created_at": formatControlPlaneTime(item.CreatedAt),
	}
}

func riskCaseJSON(item toolstore.RiskCase) gin.H {
	return gin.H{
		"id": item.ID, "case_key": item.CaseKey, "title": item.Title,
		"subject_type": item.SubjectType, "subject_id": item.SubjectID,
		"severity": item.Severity, "status": item.Status, "assignee": item.Assignee,
		"summary": item.Summary, "opened_at": formatControlPlaneTime(item.OpenedAt),
		"closed_at":  formatOptionalControlPlaneTime(item.ClosedAt),
		"created_at": formatControlPlaneTime(item.CreatedAt), "updated_at": formatControlPlaneTime(item.UpdatedAt),
	}
}

func riskCaseEventJSON(item toolstore.RiskCaseEvent) gin.H {
	return gin.H{
		"id": item.ID, "case_id": item.CaseID, "event_type": item.EventType,
		"actor": item.Actor, "details": rawJSONValue(item.DetailsJSON),
		"occurred_at": formatControlPlaneTime(item.OccurredAt), "created_at": formatControlPlaneTime(item.CreatedAt),
	}
}

func supportNoteJSON(item toolstore.SupportNote) gin.H {
	return gin.H{
		"id": item.ID, "subject_type": item.SubjectType, "subject_id": item.SubjectID,
		"author": item.Author, "body": item.Body, "visibility": item.Visibility,
		"idempotency_key": item.IdempotencyKey, "created_at": formatControlPlaneTime(item.CreatedAt),
		"updated_at": formatControlPlaneTime(item.UpdatedAt), "deleted_at": formatOptionalControlPlaneTime(item.DeletedAt),
	}
}

func priceSnapshotJSON(item toolstore.PriceSnapshot) gin.H {
	return gin.H{
		"id": item.ID, "provider": item.Provider, "model": item.Model,
		"operation": item.Operation, "component": item.Component, "currency": item.Currency,
		"unit": item.Unit, "unit_size": item.UnitSize, "amount_decimal": item.AmountDecimal,
		"amount_minor": item.AmountMinor, "minor_unit_scale": item.MinorUnitScale,
		"source": item.Source, "metadata": rawJSONValue(item.MetadataJSON),
		"idempotency_key": item.IdempotencyKey, "effective_at": formatControlPlaneTime(item.EffectiveAt),
		"expires_at": formatOptionalControlPlaneTime(item.ExpiresAt), "created_at": formatControlPlaneTime(item.CreatedAt),
	}
}

func reconciliationRunJSON(item toolstore.ReconciliationRun) gin.H {
	return gin.H{
		"id": item.ID, "run_key": item.RunKey, "kind": item.Kind, "status": item.Status,
		"window_start": formatControlPlaneTime(item.WindowStart), "window_end": formatControlPlaneTime(item.WindowEnd),
		"started_at": formatControlPlaneTime(item.StartedAt), "finished_at": formatOptionalControlPlaneTime(item.FinishedAt),
		"scanned_count": item.ScannedCount, "matched_count": item.MatchedCount,
		"discrepancy_count": item.DiscrepancyCount, "discrepancy_minor": item.DiscrepancyMinor,
		"currency": item.Currency, "summary": rawJSONValue(item.SummaryJSON),
		"error_code": item.ErrorCode, "error_message": item.ErrorMessage,
		"created_at": formatControlPlaneTime(item.CreatedAt), "updated_at": formatControlPlaneTime(item.UpdatedAt),
	}
}

func rawJSONValue(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func formatControlPlaneTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func formatOptionalControlPlaneTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatControlPlaneTime(*value)
}
