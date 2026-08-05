package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/new-api-tools/backend/internal/auth"
)

func TestInvoiceHandlerExposesVerifiedRelationAndNullableUnreconciledNet(t *testing.T) {
	store, _ := newControlPlaneTestStore(t)
	operator := newInvoiceTestRouter(store, true, auth.RoleOperator)
	admin := newInvoiceTestRouter(store, true, auth.RoleAdmin)

	blueResponse := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices", map[string]any{
		"invoice_number": "BLUE-HTTP-RELATION", "seller_entity": "Example Seller",
		"buyer_name": "Private Buyer", "buyer_tax_id": "91310000PRIVATE",
		"document_kind": "blue", "currency": "CNY", "amount_minor": "100",
		"tax_amount_minor": "10", "minor_unit_scale": 2,
		"issued_at": "2026-07-16T04:00:00Z", "reason": "finance verified original invoice",
	}, "invoice-http-relation-blue")
	if blueResponse.Code != http.StatusCreated {
		t.Fatalf("blue create = %d: %s", blueResponse.Code, blueResponse.Body.String())
	}
	blueDocument := decodeControlPlaneData(t, blueResponse)["document"].(map[string]any)
	blueID := int64(blueDocument["id"].(float64))

	redResponse := performControlPlaneRequest(t, operator, http.MethodPost, "/api/invoices", map[string]any{
		"invoice_number": "RED-HTTP-RELATION", "seller_entity": "Example Seller",
		"buyer_name": "Private Buyer", "buyer_tax_id": "91310000PRIVATE",
		"document_kind": "red", "related_invoice_id": blueID, "currency": "CNY",
		"amount_minor": "25", "tax_amount_minor": "2", "minor_unit_scale": 2,
		"issued_at": "2026-07-16T04:00:00Z", "reason": "finance verified red invoice relation",
	}, "invoice-http-relation-red")
	if redResponse.Code != http.StatusCreated {
		t.Fatalf("red create = %d: %s", redResponse.Code, redResponse.Body.String())
	}
	redDocument := decodeControlPlaneData(t, redResponse)["document"].(map[string]any)
	if int64(redDocument["related_invoice_id"].(float64)) != blueID ||
		redDocument["related_invoice_number"] != "BLUE-HTTP-RELATION" ||
		redDocument["relation_state"] != "verified" || redDocument["relation_reason"] != "" {
		t.Fatalf("red relation response = %#v", redDocument)
	}

	trustedSummary := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/invoices/summary", nil, "invoice-http-relation-summary-ok")
	if trustedSummary.Code != http.StatusOK {
		t.Fatalf("trusted summary = %d: %s", trustedSummary.Code, trustedSummary.Body.String())
	}
	trustedData := decodeControlPlaneData(t, trustedSummary)
	trustedGroup := trustedData["groups"].([]any)[0].(map[string]any)
	trustedHealth := trustedData["source_health"].(map[string]any)
	if trustedGroup["net_issued_minor"] != "75" || trustedGroup["source_health"] != "ok" ||
		trustedGroup["anomaly_count"].(float64) != 0 || trustedHealth["status"] != "ok" ||
		trustedHealth["unreconciled_count"].(float64) != 0 || trustedHealth["anomaly_count"].(float64) != 0 {
		t.Fatalf("trusted summary contract = %#v health=%#v", trustedGroup, trustedHealth)
	}

	voidResponse := performControlPlaneRequest(t, admin, http.MethodPost,
		fmt.Sprintf("/api/invoices/%d/void", blueID),
		map[string]any{"reason": "original invoice was voided after verification"},
		"invoice-http-relation-void-blue")
	if voidResponse.Code != http.StatusOK {
		t.Fatalf("blue void = %d: %s", voidResponse.Code, voidResponse.Body.String())
	}

	unreconciledSummary := performControlPlaneRequest(t, operator, http.MethodGet,
		"/api/invoices/summary", nil, "invoice-http-relation-summary-unreconciled")
	if unreconciledSummary.Code != http.StatusOK {
		t.Fatalf("unreconciled summary = %d: %s", unreconciledSummary.Code, unreconciledSummary.Body.String())
	}
	unreconciledData := decodeControlPlaneData(t, unreconciledSummary)
	unreconciledGroup := unreconciledData["groups"].([]any)[0].(map[string]any)
	unreconciledHealth := unreconciledData["source_health"].(map[string]any)
	if unreconciledGroup["net_issued_minor"] != nil || unreconciledGroup["source_health"] != "unreconciled" ||
		unreconciledGroup["unreconciled_count"].(float64) != 1 ||
		unreconciledGroup["anomaly_count"].(float64) != 1 ||
		unreconciledHealth["status"] != "unreconciled" ||
		unreconciledHealth["unreconciled_count"].(float64) != 1 ||
		unreconciledHealth["anomaly_count"].(float64) != 1 {
		t.Fatalf("nullable unreconciled summary contract = %#v health=%#v", unreconciledGroup, unreconciledHealth)
	}
}
