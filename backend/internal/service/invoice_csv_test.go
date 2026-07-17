package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestInvoiceCSVRejectsFormulaInjectionWithoutEchoingBuyerPII(t *testing.T) {
	csvText := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,buyer_tax_id,document_kind,currency,amount_minor,tax_amount_minor,minor_unit_scale,issued_at",
		"INV-1,Example Seller,=1+1,91310000SECRET,blue,CNY,100,10,2,2026-07-16T04:00:00Z",
	}, "\n")
	preview := PreviewInvoiceCSV(csvText)
	if preview.Valid || preview.ErrorCount != 1 || preview.IssueCount == 0 || len(preview.Rows) != 1 {
		t.Fatalf("formula preview = %+v", preview)
	}
	foundFormula := false
	for _, issue := range preview.Rows[0].Errors {
		if issue.Code == "FORMULA_INJECTION" && issue.Field == "buyer_name" {
			foundFormula = true
		}
	}
	if !foundFormula {
		t.Fatalf("formula issue missing: %+v", preview.Rows[0].Errors)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "91310000SECRET") || strings.Contains(string(encoded), "=1+1") {
		t.Fatalf("preview echoed buyer PII or formula: %s", encoded)
	}
}

func TestInvoiceCSVReturnsPerRowErrorsAndPreservesLargeMinorUnits(t *testing.T) {
	csvText := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,related_invoice_number,currency,amount_minor,tax_amount_minor,minor_unit_scale,issued_at",
		"BLUE-1,Example Seller,Buyer One,blue,,CNY,9007199254740993,0,2,2026-07-16T04:00:00Z",
		"RED-1,Example Seller,Buyer Two,red,,CNY,100,0,2,2026-07-16T05:00:00Z",
		"BLUE-2,Example Seller,Buyer Three,blue,,CNY,1.25,0,10,not-a-time",
	}, "\n")
	preview := PreviewInvoiceCSV(csvText)
	if preview.Valid || preview.RowCount != 3 || preview.ValidCount != 1 || preview.ErrorCount != 2 || preview.IssueCount < 4 {
		t.Fatalf("mixed preview = %+v", preview)
	}
	if preview.Rows[0].Invoice == nil || preview.Rows[0].Invoice.AmountMinor != 9007199254740993 ||
		preview.Rows[0].Preview == nil || preview.Rows[0].Preview.AmountMinor != "9007199254740993" {
		t.Fatalf("large exact row = %+v", preview.Rows[0])
	}
	if _, valid := ValidInvoiceCSVInputs(preview); valid {
		t.Fatal("invalid preview unexpectedly produced confirmable inputs")
	}
}

func TestInvoiceCSVAcceptsRelatedRedInvoiceAndDetectsFileDuplicate(t *testing.T) {
	validCSV := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,related_invoice_number,currency,amount_minor,minor_unit_scale,issued_at",
		"RED-2,Example Seller,Buyer,red,BLUE-2,CNY,125,2,2026-07-16T04:00:00+08:00",
	}, "\n")
	preview := PreviewInvoiceCSV(validCSV)
	inputs, valid := ValidInvoiceCSVInputs(preview)
	if !valid || len(inputs) != 1 || inputs[0].DocumentKind != toolstore.InvoiceRed ||
		inputs[0].RelatedInvoiceNumber != "BLUE-2" || inputs[0].IssuedAt.Location().String() != "UTC" {
		t.Fatalf("valid red preview = %+v inputs=%+v", preview, inputs)
	}

	duplicateCSV := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,currency,amount_minor,minor_unit_scale,issued_at",
		"DUP-1,Example Seller,Buyer One,blue,CNY,100,2,2026-07-16T04:00:00Z",
		"DUP-1,example seller,Buyer Two,blue,CNY,200,2,2026-07-16T05:00:00Z",
	}, "\n")
	duplicate := PreviewInvoiceCSV(duplicateCSV)
	if duplicate.Valid || duplicate.ErrorCount != 1 || duplicate.Rows[1].Valid {
		t.Fatalf("duplicate preview = %+v", duplicate)
	}
	found := false
	for _, issue := range duplicate.Rows[1].Errors {
		found = found || issue.Code == "DUPLICATE_IN_FILE"
	}
	if !found {
		t.Fatalf("duplicate issue missing: %+v", duplicate.Rows[1].Errors)
	}
}

func TestInvoiceCSVRejectsFutureIssuedAtAndCaseInsensitiveRedSelfReference(t *testing.T) {
	now := time.Date(2026, time.July, 16, 4, 0, 0, 0, time.UTC)
	csvText := strings.Join([]string{
		"invoice_number,seller_entity,buyer_name,document_kind,related_invoice_number,currency,amount_minor,minor_unit_scale,issued_at",
		"FUTURE-1,Example Seller,Buyer,blue,,CNY,100,2,2026-07-16T04:00:00.001Z",
		"Red-Self-1,Example Seller,Buyer,red,red-self-1,CNY,100,2,2026-07-16T03:00:00Z",
	}, "\n")
	preview := PreviewInvoiceCSVAt(csvText, now)
	if preview.Valid || preview.ErrorCount != 2 {
		t.Fatalf("future/self-reference preview = %+v", preview)
	}
	want := []string{"FUTURE_ISSUED_AT", "INVALID_RELATED_INVOICE"}
	for index, code := range want {
		found := false
		for _, issue := range preview.Rows[index].Errors {
			found = found || issue.Code == code
		}
		if !found {
			t.Fatalf("row %d missing %s: %+v", index+2, code, preview.Rows[index].Errors)
		}
	}
}
