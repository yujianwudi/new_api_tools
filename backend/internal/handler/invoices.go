package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	invoiceBodyLimit = (2 << 20) + (64 << 10)
)

type InvoiceHandler struct {
	store  *toolstore.Store
	common *StoreHandler
}

type invoiceCreateRequest struct {
	ignoredClientIdentity
	InvoiceNumber        string                        `json:"invoice_number"`
	SellerEntity         string                        `json:"seller_entity"`
	BuyerName            string                        `json:"buyer_name"`
	BuyerTaxID           string                        `json:"buyer_tax_id"`
	DocumentKind         toolstore.InvoiceDocumentKind `json:"document_kind"`
	RelatedInvoiceNumber string                        `json:"related_invoice_number"`
	Currency             string                        `json:"currency"`
	AmountMinor          invoiceMinorAmount            `json:"amount_minor"`
	TaxAmountMinor       invoiceMinorAmount            `json:"tax_amount_minor"`
	MinorUnitScale       int                           `json:"minor_unit_scale"`
	IssuedAt             *time.Time                    `json:"issued_at"`
	Reason               string                        `json:"reason"`
}

type invoiceVoidRequest struct {
	ignoredClientIdentity
	Reason string `json:"reason"`
}

type invoiceCSVRequest struct {
	ignoredClientIdentity
	CSV    string `json:"csv"`
	Reason string `json:"reason"`
}

type invoiceMinorAmount struct {
	value int64
	set   bool
}

func (amount *invoiceMinorAmount) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		raw = text
	}
	if raw == "" || strings.HasPrefix(raw, "+") || strings.ContainsAny(raw, ".eE \t\r\n") {
		return errors.New("minor amount must be a base-10 integer")
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errors.New("minor amount is outside the signed 64-bit range")
	}
	amount.value = value
	amount.set = true
	return nil
}

func RegisterInvoiceRoutes(api *gin.RouterGroup, store *toolstore.Store) {
	handler := &InvoiceHandler{store: store, common: NewStoreHandler(store)}
	group := api.Group("/invoices")
	authenticated := requireControlPlaneAuthentication()
	viewer := auth.RequireRole(auth.RoleViewer)
	operator := auth.RequireRole(auth.RoleOperator)
	admin := auth.RequireRole(auth.RoleAdmin)

	group.GET("/summary", authenticated, viewer, handler.Summary)
	group.GET("", authenticated, viewer, handler.List)
	group.POST("", authenticated, operator, handler.Create)
	group.POST("/import/preview", authenticated, operator, handler.PreviewCSV)
	group.POST("/import/confirm", authenticated, operator, handler.ConfirmCSV)
	group.GET("/:id", authenticated, operator, handler.Detail)
	group.POST("/:id/void", authenticated, admin, handler.Void)
}

func (h *InvoiceHandler) Summary(c *gin.Context) {
	if !h.common.begin(c) {
		return
	}
	if err := validateControlPlaneQuery(c, querySpec{"currency": 3, "issued_from": 64, "issued_to": 64}); err != nil {
		writeControlPlaneInputError(c, "Invalid invoice summary query")
		return
	}
	from, to, ok := parseInvoiceRange(c)
	if !ok {
		return
	}
	summary, err := h.store.InvoiceSummary(c.Request.Context(), toolstore.InvoiceSummaryFilter{
		Currency: c.Query("currency"), IssuedFrom: from, IssuedTo: to,
	})
	if err != nil {
		h.common.writeStoreError(c, "summarize invoices", err)
		return
	}
	groups := make([]gin.H, 0, len(summary.Groups))
	for _, group := range summary.Groups {
		groups = append(groups, invoiceSummaryJSON(group))
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"groups": groups, "generated_at": formatControlPlaneTime(summary.GeneratedAt),
		"source_health": gin.H{"status": "ok"}, "capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) List(c *gin.Context) {
	if !h.common.begin(c) {
		return
	}
	if err := validateControlPlaneQuery(c, querySpec{
		"status": 16, "currency": 3, "issued_from": 64, "issued_to": 64,
		"cursor": 19, "limit": 3,
	}); err != nil {
		writeControlPlaneInputError(c, "Invalid invoice list query")
		return
	}
	cursor, limit, err := parseControlPlanePage(c)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid invoice pagination")
		return
	}
	from, to, ok := parseInvoiceRange(c)
	if !ok {
		return
	}
	page, err := h.store.ListInvoices(c.Request.Context(), toolstore.InvoiceFilter{
		Status: toolstore.InvoiceStatus(c.Query("status")), Currency: c.Query("currency"),
		IssuedFrom: from, IssuedTo: to, BeforeID: cursor, Limit: limit,
	})
	if err != nil {
		h.common.writeStoreError(c, "list invoices", err)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, invoiceDocumentJSON(item, false))
	}
	var nextCursor any
	if page.HasMore {
		nextCursor = page.NextCursor
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"items": items, "next_cursor": nextCursor, "has_more": page.HasMore,
		"capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) Detail(c *gin.Context) {
	if !h.common.begin(c) {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	detail, err := h.store.GetInvoice(c.Request.Context(), id)
	if err != nil {
		h.common.writeStoreError(c, "get invoice", err)
		return
	}
	events := make([]gin.H, 0, len(detail.Events))
	for _, event := range detail.Events {
		events = append(events, invoiceEventJSON(event))
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"document": invoiceDocumentJSON(detail.Document, true), "events": events,
		"capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) Create(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	var request invoiceCreateRequest
	if !decodeInvoiceJSON(c, &request) || request.IssuedAt == nil ||
		!request.AmountMinor.set || !request.TaxAmountMinor.set || !validReason(request.Reason) {
		writeControlPlaneInputError(c, "Invalid invoice payload")
		return
	}
	audit := invoiceAuditInput(identity, request.Reason)
	created, _, err := h.store.CreateInvoiceAudited(c.Request.Context(), toolstore.InvoiceDocumentInput{
		InvoiceNumber: request.InvoiceNumber, SellerEntity: request.SellerEntity,
		BuyerName: request.BuyerName, BuyerTaxID: request.BuyerTaxID,
		DocumentKind: request.DocumentKind, RelatedInvoiceNumber: request.RelatedInvoiceNumber,
		Currency: request.Currency, AmountMinor: request.AmountMinor.value,
		TaxAmountMinor: request.TaxAmountMinor.value, MinorUnitScale: request.MinorUnitScale,
		Source: "manual", IdempotencyKey: audit.IdempotencyKey,
		IssuedAt: request.IssuedAt.UTC(), CreatedBy: identity.Actor,
	}, audit)
	if err != nil {
		h.common.writeStoreError(c, "create invoice", err)
		return
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(gin.H{
		"document": invoiceDocumentJSON(created, true), "capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) Void(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	id, ok := parseControlPlaneID(c)
	if !ok {
		return
	}
	var request invoiceVoidRequest
	if !decodeInvoiceJSON(c, &request) || !validReason(request.Reason) {
		writeControlPlaneInputError(c, "Invalid invoice void payload")
		return
	}
	audit := invoiceAuditInput(identity, request.Reason)
	updated, _, err := h.store.VoidInvoiceAudited(c.Request.Context(), toolstore.InvoiceVoidInput{
		ID: id, Reason: request.Reason, IdempotencyKey: audit.IdempotencyKey,
		Actor: identity.Actor,
	}, audit)
	if err != nil {
		h.common.writeStoreError(c, "void invoice", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"document": invoiceDocumentJSON(updated, true), "capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) PreviewCSV(c *gin.Context) {
	if !h.common.begin(c) {
		return
	}
	var request invoiceCSVRequest
	if !decodeInvoiceJSON(c, &request) {
		return
	}
	preview := service.PreviewInvoiceCSV(request.CSV)
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"preview": preview, "capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) ConfirmCSV(c *gin.Context) {
	identity, ok := h.beginWrite(c)
	if !ok {
		return
	}
	var request invoiceCSVRequest
	if !decodeInvoiceJSON(c, &request) || !validReason(request.Reason) {
		writeControlPlaneInputError(c, "Invalid invoice import payload")
		return
	}
	preview := service.PreviewInvoiceCSV(request.CSV)
	inputs, valid := service.ValidInvoiceCSVInputs(preview)
	if !valid {
		c.JSON(http.StatusBadRequest, models.NewErrorResponse(
			"CSV_INVALID", "Invoice CSV contains validation errors", preview))
		return
	}
	audit := invoiceAuditInput(identity, request.Reason)
	for index := range inputs {
		inputs[index].CreatedBy = identity.Actor
	}
	result, _, err := h.store.ImportInvoicesAudited(c.Request.Context(), inputs, audit)
	if err != nil {
		h.common.writeStoreError(c, "import invoices", err)
		return
	}
	items := make([]gin.H, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, invoiceDocumentJSON(item, true))
	}
	c.JSON(http.StatusCreated, models.NewSuccessResponse(gin.H{
		"items": items, "count": result.Count, "capabilities": invoiceCapabilities(c),
	}))
}

func (h *InvoiceHandler) beginWrite(c *gin.Context) (controlPlaneIdentity, bool) {
	identity, ok := h.common.beginWrite(c)
	if !ok {
		return controlPlaneIdentity{}, false
	}
	if actor, authMethod := controlPlaneMutationIdentity(c); actor != "" && authMethod == identity.AuthMethod {
		identity.Actor = actor
	}
	return identity, true
}

func invoiceAuditInput(identity controlPlaneIdentity, reason string) toolstore.OperationAuditInput {
	return toolstore.OperationAuditInput{
		RequestID: identity.RequestID, Actor: identity.Actor, SourceIP: identity.SourceIP,
		AuthMethod: identity.AuthMethod, Reason: strings.TrimSpace(reason),
		Status: toolstore.OperationSucceeded, IdempotencyKey: identity.IdempotencyKey,
	}
}

func decodeInvoiceJSON(c *gin.Context, destination any) bool {
	if c.ContentType() != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, models.NewErrorResponse(
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json"))
		return false
	}
	if c.Request.ContentLength > invoiceBodyLimit {
		writeControlPlaneBodyTooLarge(c)
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, invoiceBodyLimit)
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			writeControlPlaneBodyTooLarge(c)
		} else {
			writeControlPlaneInputError(c, "Invalid JSON request body")
		}
		return false
	}
	if !utf8.Valid(payload) {
		c.JSON(http.StatusBadRequest, models.NewErrorResponse(
			"INVALID_UTF8", "Request body must be valid UTF-8"))
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			writeControlPlaneBodyTooLarge(c)
		} else {
			writeControlPlaneInputError(c, "Invalid JSON request body")
		}
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeControlPlaneInputError(c, "Request body must contain one JSON object")
		return false
	}
	return true
}

func parseInvoiceRange(c *gin.Context) (*time.Time, *time.Time, bool) {
	parse := func(name string) (*time.Time, bool) {
		raw, present := c.GetQuery(name)
		if !present {
			return nil, true
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil || parsed.UnixMilli() < 0 {
			return nil, false
		}
		parsed = parsed.UTC().Truncate(time.Millisecond)
		return &parsed, true
	}
	from, ok := parse("issued_from")
	if !ok {
		writeControlPlaneInputError(c, "issued_from must be RFC3339")
		return nil, nil, false
	}
	to, ok := parse("issued_to")
	if !ok || (from != nil && to != nil && !from.Before(*to)) {
		writeControlPlaneInputError(c, "issued_to must be RFC3339 and later than issued_from")
		return nil, nil, false
	}
	return from, to, true
}

func invoiceCapabilities(c *gin.Context) gin.H {
	return gin.H{
		"view_summary": auth.HasRole(c, auth.RoleViewer),
		"view_list":    auth.HasRole(c, auth.RoleViewer),
		"view_detail":  auth.HasRole(c, auth.RoleOperator),
		"create":       auth.HasRole(c, auth.RoleOperator),
		"import":       auth.HasRole(c, auth.RoleOperator),
		"void":         auth.HasRole(c, auth.RoleAdmin),
	}
}

func invoiceDocumentJSON(item toolstore.InvoiceDocument, revealPII bool) gin.H {
	buyerName, buyerTaxID := item.BuyerName, item.BuyerTaxID
	if !revealPII {
		buyerName = maskInvoiceName(buyerName)
		buyerTaxID = maskInvoiceTaxID(buyerTaxID)
	}
	result := gin.H{
		"id": item.ID, "invoice_number": item.InvoiceNumber, "seller_entity": item.SellerEntity,
		"buyer_name": buyerName, "buyer_tax_id": buyerTaxID,
		"document_kind": item.DocumentKind, "related_invoice_number": item.RelatedInvoiceNumber,
		"currency": item.Currency, "amount_minor": strconv.FormatInt(item.AmountMinor, 10),
		"tax_amount_minor": strconv.FormatInt(item.TaxAmountMinor, 10),
		"minor_unit_scale": item.MinorUnitScale, "status": item.Status, "source": item.Source,
		"issued_at":  formatControlPlaneTime(item.IssuedAt),
		"voided_at":  formatOptionalControlPlaneTime(item.VoidedAt),
		"created_at": formatControlPlaneTime(item.CreatedAt),
		"updated_at": formatControlPlaneTime(item.UpdatedAt),
	}
	if revealPII {
		result["void_reason"] = item.VoidReason
		result["created_by"] = item.CreatedBy
	}
	return result
}

func invoiceSummaryJSON(group toolstore.InvoiceSummaryGroup) gin.H {
	return gin.H{
		"currency": group.Currency, "minor_unit_scale": group.MinorUnitScale,
		"blue_issued_minor": group.BlueIssuedMinor,
		"red_issued_minor":  group.RedIssuedMinor,
		"voided_blue_minor": group.VoidedBlueMinor,
		"voided_red_minor":  group.VoidedRedMinor,
		"voided_minor":      group.VoidedMinor,
		"net_issued_minor":  group.NetIssuedMinor,
		"effective_count":   group.EffectiveCount, "voided_count": group.VoidedCount,
	}
}

func invoiceEventJSON(item toolstore.InvoiceEvent) gin.H {
	var details any
	if len(bytes.TrimSpace(item.DetailsJSON)) > 0 && json.Valid(item.DetailsJSON) {
		_ = json.Unmarshal(item.DetailsJSON, &details)
	}
	return gin.H{
		"id": item.ID, "invoice_id": item.InvoiceID, "event_type": item.EventType,
		"actor": item.Actor, "details": details,
		"occurred_at": formatControlPlaneTime(item.OccurredAt), "created_at": formatControlPlaneTime(item.CreatedAt),
	}
}

func maskInvoiceName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	runeValue, _ := utf8.DecodeRuneInString(value)
	return string(runeValue) + "***"
}

func maskInvoiceTaxID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return "****"
	}
	return "****" + value[len(value)-4:]
}
