package service

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	maxInvoiceCSVBytes = 1 << 20
	maxInvoiceCSVRows  = 500
)

type InvoiceCSVError struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type InvoiceCSVRow struct {
	RowNumber int                             `json:"row_number"`
	Valid     bool                            `json:"valid"`
	Invoice   *toolstore.InvoiceDocumentInput `json:"-"`
	Preview   *InvoiceCSVPreviewDocument      `json:"invoice,omitempty"`
	Errors    []InvoiceCSVError               `json:"errors"`
}

type InvoiceCSVPreviewDocument struct {
	InvoiceNumber        string                        `json:"invoice_number"`
	SellerEntity         string                        `json:"seller_entity"`
	DocumentKind         toolstore.InvoiceDocumentKind `json:"document_kind"`
	RelatedInvoiceNumber string                        `json:"related_invoice_number"`
	Currency             string                        `json:"currency"`
	AmountMinor          string                        `json:"amount_minor"`
	TaxAmountMinor       string                        `json:"tax_amount_minor"`
	MinorUnitScale       int                           `json:"minor_unit_scale"`
	IssuedAt             string                        `json:"issued_at"`
}

type InvoiceCSVPreview struct {
	Valid      bool              `json:"valid"`
	RowCount   int               `json:"row_count"`
	ValidCount int               `json:"valid_count"`
	ErrorCount int               `json:"error_count"`
	IssueCount int               `json:"issue_count"`
	Rows       []InvoiceCSVRow   `json:"rows"`
	Errors     []InvoiceCSVError `json:"errors,omitempty"`
}

func PreviewInvoiceCSV(contents string) InvoiceCSVPreview {
	return PreviewInvoiceCSVAt(contents, time.Now())
}

func PreviewInvoiceCSVAt(contents string, now time.Time) InvoiceCSVPreview {
	now = now.UTC().Truncate(time.Millisecond)
	preview := InvoiceCSVPreview{Rows: make([]InvoiceCSVRow, 0), Errors: make([]InvoiceCSVError, 0)}
	if len(contents) == 0 {
		preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_EMPTY", Message: "CSV content is required"})
		preview.ErrorCount, preview.IssueCount = 1, 1
		return preview
	}
	if len(contents) > maxInvoiceCSVBytes {
		preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_TOO_LARGE", Message: "CSV content exceeds 1 MiB"})
		preview.ErrorCount, preview.IssueCount = 1, 1
		return preview
	}
	if !utf8.ValidString(contents) || strings.ContainsRune(contents, '\x00') {
		preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_ENCODING", Message: "CSV must be valid UTF-8 without NUL bytes"})
		preview.ErrorCount, preview.IssueCount = 1, 1
		return preview
	}
	contents = strings.TrimPrefix(contents, "\ufeff")
	reader := csv.NewReader(strings.NewReader(contents))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	records := make([][]string, 0, maxInvoiceCSVRows+1)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_SYNTAX", Message: "CSV syntax is invalid"})
			preview.ErrorCount, preview.IssueCount = 1, 1
			return preview
		}
		records = append(records, record)
		if len(records) > maxInvoiceCSVRows+1 {
			preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_ROW_LIMIT", Message: "CSV exceeds 500 data rows"})
			preview.ErrorCount, preview.IssueCount = 1, 1
			return preview
		}
	}
	if len(records) < 2 {
		preview.Errors = append(preview.Errors, InvoiceCSVError{Code: "CSV_NO_ROWS", Message: "CSV must include a header and at least one data row"})
		preview.ErrorCount, preview.IssueCount = 1, 1
		return preview
	}
	header, headerErrors := invoiceCSVHeader(records[0])
	if len(headerErrors) > 0 {
		preview.Errors = headerErrors
		preview.ErrorCount, preview.IssueCount = 1, len(headerErrors)
		return preview
	}

	seenInvoices := make(map[string]int)
	for index, record := range records[1:] {
		row := parseInvoiceCSVRow(index+2, record, header, now)
		if row.Invoice != nil {
			key := strings.ToLower(row.Invoice.SellerEntity) + "\x00" + strings.ToLower(row.Invoice.InvoiceNumber)
			if firstRow, duplicate := seenInvoices[key]; duplicate {
				row.Errors = append(row.Errors, InvoiceCSVError{Code: "DUPLICATE_IN_FILE", Field: "invoice_number",
					Message: fmt.Sprintf("Invoice number duplicates row %d for the same seller", firstRow)})
			} else {
				seenInvoices[key] = row.RowNumber
			}
		}
		row.Valid = len(row.Errors) == 0
		if row.Valid {
			preview.ValidCount++
		} else {
			preview.ErrorCount++
			preview.IssueCount += len(row.Errors)
		}
		preview.Rows = append(preview.Rows, row)
	}
	preview.RowCount = len(preview.Rows)
	preview.Valid = preview.RowCount > 0 && preview.ValidCount == preview.RowCount && preview.ErrorCount == 0
	return preview
}

func ValidInvoiceCSVInputs(preview InvoiceCSVPreview) ([]toolstore.InvoiceDocumentInput, bool) {
	if !preview.Valid || preview.RowCount == 0 {
		return nil, false
	}
	inputs := make([]toolstore.InvoiceDocumentInput, 0, preview.RowCount)
	for _, row := range preview.Rows {
		if !row.Valid || row.Invoice == nil {
			return nil, false
		}
		inputs = append(inputs, *row.Invoice)
	}
	return inputs, true
}

func invoiceCSVHeader(record []string) (map[string]int, []InvoiceCSVError) {
	allowed := map[string]bool{
		"invoice_number": true, "seller_entity": true, "buyer_name": true, "buyer_tax_id": true,
		"document_kind": true, "related_invoice_number": true,
		"currency": true, "amount_minor": true, "tax_amount_minor": true,
		"minor_unit_scale": true, "issued_at": true,
	}
	required := []string{"invoice_number", "seller_entity", "buyer_name", "document_kind", "currency", "amount_minor", "minor_unit_scale", "issued_at"}
	header := make(map[string]int, len(record))
	errorsFound := make([]InvoiceCSVError, 0)
	for index, raw := range record {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !allowed[name] {
			errorsFound = append(errorsFound, InvoiceCSVError{Code: "UNKNOWN_COLUMN", Field: name, Message: "CSV contains an unsupported column"})
			continue
		}
		if _, duplicate := header[name]; duplicate {
			errorsFound = append(errorsFound, InvoiceCSVError{Code: "DUPLICATE_COLUMN", Field: name, Message: "CSV contains a duplicate column"})
			continue
		}
		header[name] = index
	}
	for _, name := range required {
		if _, present := header[name]; !present {
			errorsFound = append(errorsFound, InvoiceCSVError{Code: "MISSING_COLUMN", Field: name, Message: "CSV is missing a required column"})
		}
	}
	return header, errorsFound
}

func parseInvoiceCSVRow(rowNumber int, record []string, header map[string]int, now time.Time) InvoiceCSVRow {
	row := InvoiceCSVRow{RowNumber: rowNumber, Errors: make([]InvoiceCSVError, 0)}
	if len(record) != len(header) {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "COLUMN_COUNT", Message: "Row has a different column count than the header"})
		return row
	}
	value := func(field string) string {
		index, present := header[field]
		if !present || index >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[index])
	}
	for field := range header {
		if hasSpreadsheetFormulaPrefix(value(field)) {
			row.Errors = append(row.Errors, InvoiceCSVError{Code: "FORMULA_INJECTION", Field: field,
				Message: "Spreadsheet formula prefixes are not allowed"})
		}
	}
	textFields := []struct {
		name     string
		required bool
		maximum  int
	}{
		{"invoice_number", true, 128}, {"seller_entity", true, 128},
		{"buyer_name", true, 256}, {"buyer_tax_id", false, 128}, {"currency", true, 3},
		{"document_kind", true, 4}, {"related_invoice_number", false, 128},
	}
	for _, field := range textFields {
		raw := value(field.name)
		if (field.required && raw == "") || len(raw) > field.maximum || containsInvoiceCSVControl(raw) {
			row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_TEXT", Field: field.name, Message: "Text value is missing or invalid"})
		}
	}
	currency := strings.ToUpper(value("currency"))
	if len(currency) != 3 || currency[0] < 'A' || currency[0] > 'Z' || currency[1] < 'A' || currency[1] > 'Z' || currency[2] < 'A' || currency[2] > 'Z' {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_CURRENCY", Field: "currency", Message: "Currency must be three uppercase letters"})
	}
	documentKind := toolstore.InvoiceDocumentKind(strings.ToLower(value("document_kind")))
	relatedInvoiceNumber := value("related_invoice_number")
	if documentKind != toolstore.InvoiceBlue && documentKind != toolstore.InvoiceRed {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_DOCUMENT_KIND", Field: "document_kind", Message: "document_kind must be blue or red"})
	} else if (documentKind == toolstore.InvoiceBlue && relatedInvoiceNumber != "") ||
		(documentKind == toolstore.InvoiceRed && (relatedInvoiceNumber == "" || strings.EqualFold(relatedInvoiceNumber, value("invoice_number")))) {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_RELATED_INVOICE", Field: "related_invoice_number", Message: "Red invoices require a different related invoice number; blue invoices must leave it empty"})
	}
	amountMinor, amountOK := parseInvoiceCSVInt(value("amount_minor"), 1, int64(^uint64(0)>>1))
	if !amountOK {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_AMOUNT", Field: "amount_minor", Message: "Amount must be a positive integer in minor units"})
	}
	taxAmountMinor := int64(0)
	if rawTax := value("tax_amount_minor"); rawTax != "" {
		var taxOK bool
		taxAmountMinor, taxOK = parseInvoiceCSVInt(rawTax, 0, amountMinor)
		if !taxOK {
			row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_TAX_AMOUNT", Field: "tax_amount_minor", Message: "Tax amount must be an integer between zero and amount_minor"})
		}
	}
	scale64, scaleOK := parseInvoiceCSVInt(value("minor_unit_scale"), 0, 9)
	if !scaleOK {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_SCALE", Field: "minor_unit_scale", Message: "Minor unit scale must be an integer from 0 to 9"})
	}
	issuedAt, timeErr := time.Parse(time.RFC3339, value("issued_at"))
	if timeErr != nil || issuedAt.UnixMilli() < 0 {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "INVALID_TIMESTAMP", Field: "issued_at", Message: "issued_at must be an RFC3339 timestamp"})
	} else if issuedAt.After(now) {
		row.Errors = append(row.Errors, InvoiceCSVError{Code: "FUTURE_ISSUED_AT", Field: "issued_at", Message: "issued_at cannot be in the future"})
	}
	row.Invoice = &toolstore.InvoiceDocumentInput{
		InvoiceNumber: value("invoice_number"), SellerEntity: value("seller_entity"),
		BuyerName: value("buyer_name"), BuyerTaxID: value("buyer_tax_id"),
		DocumentKind: documentKind, RelatedInvoiceNumber: relatedInvoiceNumber, Currency: currency,
		AmountMinor: amountMinor, TaxAmountMinor: taxAmountMinor, MinorUnitScale: int(scale64),
		Source: "csv", IssuedAt: issuedAt.UTC(),
	}
	row.Preview = &InvoiceCSVPreviewDocument{
		InvoiceNumber: value("invoice_number"), SellerEntity: value("seller_entity"),
		DocumentKind: documentKind, RelatedInvoiceNumber: relatedInvoiceNumber, Currency: currency,
		AmountMinor: strconv.FormatInt(amountMinor, 10), TaxAmountMinor: strconv.FormatInt(taxAmountMinor, 10),
		MinorUnitScale: int(scale64), IssuedAt: issuedAt.UTC().Format(time.RFC3339),
	}
	return row
}

func parseInvoiceCSVInt(value string, minimum, maximum int64) (int64, bool) {
	if hasSpreadsheetFormulaPrefix(value) || strings.ContainsAny(value, ".,eE") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed >= minimum && parsed <= maximum
}

func hasSpreadsheetFormulaPrefix(value string) bool {
	value = strings.TrimLeft(value, " \t\r\n")
	return value != "" && strings.ContainsRune("=+-@", rune(value[0]))
}

func containsInvoiceCSVControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
