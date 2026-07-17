package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/requestmeta"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func newInvoiceTestRouter(store *toolstore.Store, authenticated bool, role auth.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api")
	if authenticated {
		api.Use(func(c *gin.Context) {
			c.Set("auth_method", "jwt")
			c.Set("user_sub", testControlPlaneActor)
			auth.SetRole(c, role)
			requestID := c.GetHeader("X-Test-Request-ID")
			if requestID == "" {
				requestID = "invoice-test-request"
			}
			c.Set("request_id", requestID)
			c.Request = c.Request.WithContext(requestmeta.WithRequestID(c.Request.Context(), requestID))
			c.Next()
		}, auth.RBACMiddleware())
	}
	RegisterInvoiceRoutes(api, store)
	NewStoreHandler(store).RegisterRoutes(api)
	return router
}

func TestInvoiceHandlerRBACExactMoneyAndPIIRedaction(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	viewer := newInvoiceTestRouter(store, true, auth.RoleViewer)
	operator := newInvoiceTestRouter(store, true, auth.RoleOperator)
	admin := newInvoiceTestRouter(store, true, auth.RoleAdmin)

	summary := performControlPlaneRequest(t, viewer, http.MethodGet, "/api/invoices/summary", nil, "invoice-summary-read")
	if summary.Code != http.StatusOK || !strings.Contains(summary.Body.String(), `"create":false`) ||
		!strings.Contains(summary.Body.String(), `"view_summary":true`) {
		t.Fatalf("viewer summary = %d: %s", summary.Code, summary.Body.String())
	}
	body := map[string]any{
		"invoice_number": "BLUE-HTTP-1", "seller_entity": "Example Seller",
		"buyer_name": "Private Buyer", "buyer_tax_id": "91310000SECRET",
		"document_kind": "blue", "related_invoice_number": "", "currency": "CNY",
		"amount_minor": "9007199254740993", "tax_amount_minor": "900719925474099",
		"minor_unit_scale": 2, "issued_at": "2026-07-16T04:00:00Z", "reason": "finance reviewed source document",
	}
	denied := performControlPlaneRequest(t, viewer, http.MethodPost, "/api/invoices", body, "invoice-viewer-create")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d: %s", denied.Code, denied.Body.String())
	}
	missingKey := performControlPlaneRequestWithMetadata(t, operator, http.MethodPost, "/api/invoices", body,
		"invoice-missing-key", "", testControlPlaneIP)
	if missingKey.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing idempotency key = %d: %s", missingKey.Code, missingKey.Body.String())
	}
	created := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices", body, "invoice-operator-create")
	if created.Code != http.StatusCreated {
		t.Fatalf("operator create = %d: %s", created.Code, created.Body.String())
	}
	createdData := decodeControlPlaneData(t, created)
	document := createdData["document"].(map[string]any)
	if document["amount_minor"] != "9007199254740993" || document["buyer_tax_id"] != "91310000SECRET" {
		t.Fatalf("created document = %#v", document)
	}
	id := int64(document["id"].(float64))
	createRecovery := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/control-plane/operations/invoice-operator-create", nil, "invoice-create-recovery")
	if createRecovery.Code != http.StatusOK || !strings.Contains(createRecovery.Body.String(), `"action":"invoice.create"`) ||
		!strings.Contains(createRecovery.Body.String(), `"target_id":"BLUE-HTTP-1"`) {
		t.Fatalf("create recovery = %d: %s", createRecovery.Code, createRecovery.Body.String())
	}

	list := performControlPlaneRequest(t, viewer, http.MethodGet, "/api/invoices", nil, "invoice-viewer-list")
	if list.Code != http.StatusOK {
		t.Fatalf("viewer list = %d: %s", list.Code, list.Body.String())
	}
	listed := decodeControlPlaneData(t, list)["items"].([]any)[0].(map[string]any)
	if listed["buyer_name"] == "Private Buyer" || listed["buyer_tax_id"] == "91310000SECRET" ||
		listed["amount_minor"] != "9007199254740993" || listed["created_by"] != nil || listed["void_reason"] != nil {
		t.Fatalf("listed document was not safely encoded: %#v", listed)
	}
	viewerDetail := performControlPlaneRequest(t, viewer, http.MethodGet,
		fmt.Sprintf("/api/invoices/%d", id), nil, "invoice-viewer-detail")
	if viewerDetail.Code != http.StatusForbidden {
		t.Fatalf("viewer detail = %d: %s", viewerDetail.Code, viewerDetail.Body.String())
	}
	operatorDetail := performControlPlaneRequest(t, operator, http.MethodGet,
		fmt.Sprintf("/api/invoices/%d", id), nil, "invoice-operator-detail")
	if operatorDetail.Code != http.StatusOK || !strings.Contains(operatorDetail.Body.String(), "91310000SECRET") {
		t.Fatalf("operator detail = %d: %s", operatorDetail.Code, operatorDetail.Body.String())
	}
	detailData := decodeControlPlaneData(t, operatorDetail)
	eventDetails := detailData["events"].([]any)[0].(map[string]any)["details"].(map[string]any)
	if eventDetails["amount_minor"] != "9007199254740993" || eventDetails["tax_amount_minor"] != "900719925474099" {
		t.Fatalf("event details lost integer precision: %#v", eventDetails)
	}

	voidBody := map[string]any{"reason": "invoice was cancelled by finance"}
	operatorVoid := performControlPlaneRequest(t, operator, http.MethodPost,
		fmt.Sprintf("/api/invoices/%d/void", id), voidBody, "invoice-operator-void")
	if operatorVoid.Code != http.StatusForbidden {
		t.Fatalf("operator void = %d: %s", operatorVoid.Code, operatorVoid.Body.String())
	}
	adminVoid := performControlPlaneRequest(t, admin, http.MethodPost,
		fmt.Sprintf("/api/invoices/%d/void", id), voidBody, "invoice-admin-void")
	if adminVoid.Code != http.StatusOK || !strings.Contains(adminVoid.Body.String(), `"status":"voided"`) {
		t.Fatalf("admin void = %d: %s", adminVoid.Code, adminVoid.Body.String())
	}
	retryVoid := performControlPlaneRequest(t, admin, http.MethodPost,
		fmt.Sprintf("/api/invoices/%d/void", id), voidBody, "invoice-admin-void")
	if retryVoid.Code != http.StatusOK {
		t.Fatalf("idempotent admin void = %d: %s", retryVoid.Code, retryVoid.Body.String())
	}
	voidRecovery := performControlPlaneRequest(t, admin, http.MethodGet,
		"/api/control-plane/operations/invoice-admin-void", nil, "invoice-void-recovery")
	if voidRecovery.Code != http.StatusOK || !strings.Contains(voidRecovery.Body.String(), `"action":"invoice.void"`) ||
		!strings.Contains(voidRecovery.Body.String(), fmt.Sprintf(`"target_id":"%d"`, id)) {
		t.Fatalf("void recovery = %d: %s", voidRecovery.Code, voidRecovery.Body.String())
	}
}

func TestInvoiceHandlerRejectsInvalidDatesAndAuditsCSVImport(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	operator := newInvoiceTestRouter(store, true, auth.RoleOperator)

	invalidDate := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/invoices/summary?issued_from=not-a-date", nil, "invoice-invalid-date")
	if invalidDate.Code != http.StatusBadRequest {
		t.Fatalf("invalid date = %d: %s", invalidDate.Code, invalidDate.Body.String())
	}
	reversedRange := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/invoices?issued_from=2026-07-17T00:00:00%2B08:00&issued_to=2026-07-16T00:00:00%2B08:00",
		nil, "invoice-reversed-range")
	if reversedRange.Code != http.StatusBadRequest {
		t.Fatalf("reversed range = %d: %s", reversedRange.Code, reversedRange.Body.String())
	}
	invalidUTF8 := append([]byte(`{"csv":"`), byte(0xff))
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	utf8Response := performControlPlaneRequest(t, operator, http.MethodPost,
		"/api/invoices/import/preview", invalidUTF8, "invoice-invalid-utf8")
	if utf8Response.Code != http.StatusBadRequest || !strings.Contains(utf8Response.Body.String(), "INVALID_UTF8") {
		t.Fatalf("invalid UTF-8 = %d: %s", utf8Response.Code, utf8Response.Body.String())
	}
	futureCreate := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices", map[string]any{
		"invoice_number": "FUTURE-HTTP", "seller_entity": "Example Seller", "buyer_name": "Buyer",
		"document_kind": "blue", "related_invoice_number": "", "currency": "CNY",
		"amount_minor": "100", "tax_amount_minor": "0", "minor_unit_scale": 2,
		"issued_at": "2999-01-01T00:00:00Z", "reason": "future timestamp regression",
	}, "invoice-future-http")
	if futureCreate.Code != http.StatusBadRequest {
		t.Fatalf("future invoice create = %d: %s", futureCreate.Code, futureCreate.Body.String())
	}

	formulaCSV := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,currency,amount_minor,tax_amount_minor,minor_unit_scale,issued_at",
		"CSV-1,Example Seller,=1+1,blue,CNY,100,10,2,2026-07-16T04:00:00Z",
	}, "\n")
	preview := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices/import/preview",
		map[string]any{"csv": formulaCSV}, "invoice-preview-formula")
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "FORMULA_INJECTION") ||
		strings.Contains(preview.Body.String(), "=1+1") {
		t.Fatalf("formula preview = %d: %s", preview.Code, preview.Body.String())
	}
	invalidConfirm := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices/import/confirm",
		map[string]any{"csv": formulaCSV, "reason": "reviewed invalid import"}, "invoice-confirm-formula")
	if invalidConfirm.Code != http.StatusBadRequest || !strings.Contains(invalidConfirm.Body.String(), "CSV_INVALID") {
		t.Fatalf("invalid confirm = %d: %s", invalidConfirm.Code, invalidConfirm.Body.String())
	}

	validCSV := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,currency,amount_minor,tax_amount_minor,minor_unit_scale,issued_at",
		"CSV-2,Example Seller,Buyer,blue,CNY,12345678901234567,10,2,2026-07-16T04:00:00Z",
	}, "\n")
	confirmed := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices/import/confirm",
		map[string]any{"csv": validCSV, "reason": "finance approved import"}, "invoice-confirm-valid")
	if confirmed.Code != http.StatusCreated || !strings.Contains(confirmed.Body.String(), `"amount_minor":"12345678901234567"`) {
		t.Fatalf("valid confirm = %d: %s", confirmed.Code, confirmed.Body.String())
	}
	retry := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices/import/confirm",
		map[string]any{"csv": validCSV, "reason": "finance approved import"}, "invoice-confirm-valid")
	if retry.Code != http.StatusCreated {
		t.Fatalf("import replay = %d: %s", retry.Code, retry.Body.String())
	}
	importRecovery := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/control-plane/operations/invoice-confirm-valid", nil, "invoice-import-recovery")
	if importRecovery.Code != http.StatusOK || !strings.Contains(importRecovery.Body.String(), `"action":"invoice.import"`) ||
		!strings.Contains(importRecovery.Body.String(), `"target_id":"batch"`) {
		t.Fatalf("import recovery = %d: %s", importRecovery.Code, importRecovery.Body.String())
	}
	audits, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "invoice.import.outcome"})
	if err != nil || len(audits.Items) != 1 || strings.Contains(string(audits.Items[0].AfterJSON), "Buyer") {
		t.Fatalf("import audits = %+v err=%v", audits, err)
	}
}

func TestInvoiceMinorAmountRejectsFractionAndExponent(t *testing.T) {
	for _, payload := range []string{`{"amount_minor":1.1}`, `{"amount_minor":1e3}`, `{"amount_minor":"+1"}`} {
		var decoded struct {
			Amount invoiceMinorAmount `json:"amount_minor"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err == nil {
			t.Fatalf("payload %s unexpectedly parsed", payload)
		}
	}
}
