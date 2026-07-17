package toolstore

import "time"

type InvoiceStatus string

type InvoiceDocumentKind string

const (
	InvoiceIssued InvoiceStatus       = "issued"
	InvoiceVoided InvoiceStatus       = "voided"
	InvoiceBlue   InvoiceDocumentKind = "blue"
	InvoiceRed    InvoiceDocumentKind = "red"
)

type InvoiceDocument struct {
	ID                   int64               `json:"id"`
	InvoiceNumber        string              `json:"invoice_number"`
	SellerEntity         string              `json:"seller_entity"`
	BuyerName            string              `json:"buyer_name"`
	BuyerTaxID           string              `json:"buyer_tax_id"`
	DocumentKind         InvoiceDocumentKind `json:"document_kind"`
	RelatedInvoiceNumber string              `json:"related_invoice_number"`
	Currency             string              `json:"currency"`
	AmountMinor          int64               `json:"amount_minor"`
	TaxAmountMinor       int64               `json:"tax_amount_minor"`
	MinorUnitScale       int                 `json:"minor_unit_scale"`
	Status               InvoiceStatus       `json:"status"`
	Source               string              `json:"source"`
	IdempotencyKey       string              `json:"-"`
	RequestFingerprint   string              `json:"-"`
	IssuedAt             time.Time           `json:"issued_at"`
	VoidedAt             *time.Time          `json:"voided_at"`
	VoidReason           string              `json:"void_reason"`
	CreatedBy            string              `json:"created_by"`
	CreatedAt            time.Time           `json:"created_at"`
	UpdatedAt            time.Time           `json:"updated_at"`
}

type InvoiceDocumentInput struct {
	InvoiceNumber        string
	SellerEntity         string
	BuyerName            string
	BuyerTaxID           string
	DocumentKind         InvoiceDocumentKind
	RelatedInvoiceNumber string
	Currency             string
	AmountMinor          int64
	TaxAmountMinor       int64
	MinorUnitScale       int
	Source               string
	IdempotencyKey       string
	IssuedAt             time.Time
	CreatedBy            string
}

type InvoiceVoidInput struct {
	ID             int64
	Reason         string
	IdempotencyKey string
	VoidedAt       time.Time
	Actor          string
}

type InvoiceFilter struct {
	Status     InvoiceStatus
	Currency   string
	IssuedFrom *time.Time
	IssuedTo   *time.Time
	BeforeID   int64
	Limit      int
}

type InvoiceDocumentPage struct {
	Items      []InvoiceDocument `json:"items"`
	NextCursor int64             `json:"next_cursor,omitempty"`
	HasMore    bool              `json:"has_more"`
}

type InvoiceEvent struct {
	ID             int64     `json:"id"`
	InvoiceID      int64     `json:"invoice_id"`
	EventType      string    `json:"event_type"`
	Actor          string    `json:"actor"`
	DetailsJSON    []byte    `json:"-"`
	IdempotencyKey string    `json:"-"`
	OccurredAt     time.Time `json:"occurred_at"`
	CreatedAt      time.Time `json:"created_at"`
}

type InvoiceDetail struct {
	Document InvoiceDocument `json:"document"`
	Events   []InvoiceEvent  `json:"events"`
}

type InvoiceSummaryFilter struct {
	Currency   string
	IssuedFrom *time.Time
	IssuedTo   *time.Time
}

type InvoiceSummaryGroup struct {
	Currency        string `json:"currency"`
	MinorUnitScale  int    `json:"minor_unit_scale"`
	BlueIssuedMinor string `json:"blue_issued_minor"`
	RedIssuedMinor  string `json:"red_issued_minor"`
	VoidedBlueMinor string `json:"voided_blue_minor"`
	VoidedRedMinor  string `json:"voided_red_minor"`
	VoidedMinor     string `json:"voided_minor"`
	NetIssuedMinor  string `json:"net_issued_minor"`
	EffectiveCount  int64  `json:"effective_count"`
	VoidedCount     int64  `json:"voided_count"`
}

type InvoiceSummary struct {
	Groups      []InvoiceSummaryGroup `json:"groups"`
	GeneratedAt time.Time             `json:"generated_at"`
}

type InvoiceImportResult struct {
	Items []InvoiceDocument `json:"items"`
	Count int               `json:"count"`
}
