package toolstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	actionInvoiceCreate = "invoice.create"
	actionInvoiceVoid   = "invoice.void"
	actionInvoiceImport = "invoice.import"

	invoiceColumns = `id, invoice_number, seller_entity, buyer_name, buyer_tax_id,
		document_kind, related_invoice_number, currency, amount_minor, tax_amount_minor, minor_unit_scale, status, source,
		idempotency_key, request_fingerprint, issued_at, voided_at, void_reason,
		created_by, created_at, updated_at,
		(SELECT relation.related_invoice_id FROM invoice_document_relations relation
			WHERE relation.red_invoice_id = invoice_documents.id),
		COALESCE((SELECT relation.relation_state FROM invoice_document_relations relation
			WHERE relation.red_invoice_id = invoice_documents.id),
			CASE WHEN document_kind = 'blue' THEN 'not_applicable' ELSE 'unreconciled' END),
		COALESCE((SELECT relation.reason_code FROM invoice_document_relations relation
			WHERE relation.red_invoice_id = invoice_documents.id),
			CASE WHEN document_kind = 'blue' THEN '' ELSE 'missing_relation' END)`
)

type invoiceAuditState struct {
	ID                 int64               `json:"id"`
	RequestFingerprint string              `json:"request_fingerprint"`
	DocumentKind       InvoiceDocumentKind `json:"document_kind"`
	RelatedInvoiceID   *int64              `json:"related_invoice_id,omitempty"`
	Currency           string              `json:"currency"`
	AmountMinor        string              `json:"amount_minor"`
	MinorUnitScale     int                 `json:"minor_unit_scale"`
	Status             InvoiceStatus       `json:"status"`
	IssuedAt           time.Time           `json:"issued_at"`
	VoidedAt           *time.Time          `json:"voided_at,omitempty"`
	VoidReasonHash     string              `json:"void_reason_hash,omitempty"`
}

type invoiceImportAuditItem struct {
	ID                 int64  `json:"id"`
	RequestFingerprint string `json:"request_fingerprint"`
}

type invoiceImportAuditState struct {
	RequestFingerprint string                   `json:"request_fingerprint"`
	Items              []invoiceImportAuditItem `json:"items"`
}

func (s *Store) GetInvoice(ctx context.Context, id int64) (InvoiceDetail, error) {
	if id <= 0 {
		return InvoiceDetail{}, fmt.Errorf("%w: invoice id must be positive", ErrInvalid)
	}
	document, err := getInvoiceDocument(ctx, s.db, id)
	if err != nil {
		return InvoiceDetail{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, invoice_id, event_type, actor, details_json,
		idempotency_key, occurred_at, created_at FROM invoice_events WHERE invoice_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return InvoiceDetail{}, fmt.Errorf("list invoice events: %w", err)
	}
	defer rows.Close()
	events := make([]InvoiceEvent, 0)
	for rows.Next() {
		event, scanErr := scanInvoiceEvent(rows)
		if scanErr != nil {
			return InvoiceDetail{}, fmt.Errorf("scan invoice event: %w", scanErr)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return InvoiceDetail{}, fmt.Errorf("iterate invoice events: %w", err)
	}
	return InvoiceDetail{Document: document, Events: events}, nil
}

func (s *Store) ListInvoices(ctx context.Context, filter InvoiceFilter) (InvoiceDocumentPage, error) {
	if err := validateInvoiceFilter(filter.Status, filter.Currency, filter.IssuedFrom, filter.IssuedTo, filter.BeforeID); err != nil {
		return InvoiceDocumentPage{}, err
	}
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(invoiceColumns)
	query.WriteString(" FROM invoice_documents WHERE 1=1")
	args := make([]any, 0, 6)
	if filter.Status != "" {
		query.WriteString(" AND status = ?")
		args = append(args, filter.Status)
	}
	if currency := strings.ToUpper(strings.TrimSpace(filter.Currency)); currency != "" {
		query.WriteString(" AND currency = ?")
		args = append(args, currency)
	}
	appendInvoiceTimeFilters(&query, &args, filter.IssuedFrom, filter.IssuedTo)
	if filter.BeforeID > 0 {
		query.WriteString(" AND id < ?")
		args = append(args, filter.BeforeID)
	}
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return InvoiceDocumentPage{}, fmt.Errorf("list invoices: %w", err)
	}
	defer rows.Close()
	items := make([]InvoiceDocument, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanInvoiceDocument(rows)
		if scanErr != nil {
			return InvoiceDocumentPage{}, fmt.Errorf("scan invoice: %w", scanErr)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return InvoiceDocumentPage{}, fmt.Errorf("iterate invoices: %w", err)
	}
	page := InvoiceDocumentPage{Items: items}
	if len(items) > limit {
		page.HasMore = true
		page.Items = items[:limit]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

func (s *Store) InvoiceSummary(ctx context.Context, filter InvoiceSummaryFilter) (InvoiceSummary, error) {
	if err := validateInvoiceFilter("", filter.Currency, filter.IssuedFrom, filter.IssuedTo, 0); err != nil {
		return InvoiceSummary{}, err
	}
	var query strings.Builder
	query.WriteString(`SELECT currency, minor_unit_scale, status, document_kind, amount_minor,
		CASE WHEN document_kind = 'blue' THEN 'not_applicable' ELSE COALESCE((
			SELECT relation.relation_state FROM invoice_document_relations relation
			WHERE relation.red_invoice_id = invoice_documents.id
		), 'unreconciled') END AS relation_state,
		CASE WHEN document_kind = 'blue' THEN 1 WHEN EXISTS (
			SELECT 1 FROM invoice_document_relations relation
			JOIN invoice_documents original ON original.id = relation.related_invoice_id
			WHERE relation.red_invoice_id = invoice_documents.id
				AND relation.relation_state = 'verified'
				AND original.document_kind = 'blue' AND original.status = 'issued'
				AND trim(original.invoice_number) = trim(invoice_documents.related_invoice_number) COLLATE NOCASE
				AND trim(original.seller_entity) = trim(invoice_documents.seller_entity) COLLATE NOCASE
				AND original.currency = invoice_documents.currency
				AND original.minor_unit_scale = invoice_documents.minor_unit_scale
		) THEN 1 ELSE 0 END AS relation_valid
		FROM invoice_documents WHERE 1=1`)
	args := make([]any, 0, 3)
	if currency := strings.ToUpper(strings.TrimSpace(filter.Currency)); currency != "" {
		query.WriteString(" AND currency = ?")
		args = append(args, currency)
	}
	appendInvoiceTimeFilters(&query, &args, filter.IssuedFrom, filter.IssuedTo)
	query.WriteString(" ORDER BY currency, minor_unit_scale, id")
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return InvoiceSummary{}, fmt.Errorf("summarize invoices: %w", err)
	}
	defer rows.Close()
	type accumulator struct {
		currency                                             string
		scale                                                int
		blue, red, voidedBlue, voidedRed, voided, net        big.Int
		effectiveCount, voidedCount, unreconciled, anomalies int64
	}
	groups := make([]InvoiceSummaryGroup, 0)
	totalUnreconciled := int64(0)
	totalAnomalies := int64(0)
	var current *accumulator
	flush := func() {
		if current == nil {
			return
		}
		var net *string
		sourceHealth := "ok"
		if current.unreconciled > 0 {
			sourceHealth = "unreconciled"
		} else {
			value := current.net.String()
			net = &value
		}
		groups = append(groups, InvoiceSummaryGroup{
			Currency: current.currency, MinorUnitScale: current.scale,
			BlueIssuedMinor: current.blue.String(), RedIssuedMinor: current.red.String(),
			VoidedBlueMinor: current.voidedBlue.String(), VoidedRedMinor: current.voidedRed.String(),
			VoidedMinor: current.voided.String(), NetIssuedMinor: net,
			EffectiveCount: current.effectiveCount, VoidedCount: current.voidedCount,
			SourceHealth: sourceHealth, UnreconciledCount: current.unreconciled,
			AnomalyCount: current.anomalies,
		})
	}
	for rows.Next() {
		var currency string
		var scale int
		var status InvoiceStatus
		var kind InvoiceDocumentKind
		var amount int64
		var relationState InvoiceRelationState
		var relationValid int
		if err := rows.Scan(&currency, &scale, &status, &kind, &amount, &relationState, &relationValid); err != nil {
			return InvoiceSummary{}, fmt.Errorf("scan invoice summary: %w", err)
		}
		if current == nil || current.currency != currency || current.scale != scale {
			flush()
			current = &accumulator{currency: currency, scale: scale}
		}
		value := new(big.Int).SetInt64(amount)
		trustedRed := kind != InvoiceRed || (relationState == InvoiceRelationVerified && relationValid == 1)
		if kind == InvoiceRed && !trustedRed {
			current.unreconciled++
			current.anomalies++
			totalUnreconciled++
			totalAnomalies++
		}
		switch status {
		case InvoiceIssued:
			current.effectiveCount++
			if kind == InvoiceBlue {
				current.blue.Add(&current.blue, value)
				current.net.Add(&current.net, value)
			} else {
				current.red.Add(&current.red, value)
				if trustedRed {
					current.net.Sub(&current.net, value)
				}
			}
		case InvoiceVoided:
			current.voidedCount++
			current.voided.Add(&current.voided, value)
			if kind == InvoiceBlue {
				current.voidedBlue.Add(&current.voidedBlue, value)
			} else {
				current.voidedRed.Add(&current.voidedRed, value)
			}
		default:
			return InvoiceSummary{}, fmt.Errorf("summarize invoices: unsupported status %q", status)
		}
	}
	if err := rows.Err(); err != nil {
		return InvoiceSummary{}, fmt.Errorf("iterate invoice summary: %w", err)
	}
	flush()
	sourceHealth := "ok"
	if totalUnreconciled > 0 {
		sourceHealth = "unreconciled"
	}
	return InvoiceSummary{
		Groups: groups, GeneratedAt: s.clock(), SourceHealth: sourceHealth,
		UnreconciledCount: totalUnreconciled, AnomalyCount: totalAnomalies,
	}, nil
}

func (s *Store) createInvoice(ctx context.Context, executor execQueryer, input InvoiceDocumentInput, fingerprint string) (InvoiceDocument, error) {
	resolved, err := s.resolveInvoiceRelation(ctx, executor, input)
	if err != nil {
		return InvoiceDocument{}, err
	}
	input = resolved
	now := s.clock()
	result, err := executor.ExecContext(ctx, `INSERT INTO invoice_documents(
		invoice_number, seller_entity, buyer_name, buyer_tax_id, document_kind, related_invoice_number, currency,
		amount_minor, tax_amount_minor, minor_unit_scale, status, source,
		idempotency_key, request_fingerprint, issued_at, voided_at, void_reason,
		created_by, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'issued', ?, ?, ?, ?, NULL, '', ?, ?, ?)`,
		input.InvoiceNumber, input.SellerEntity, input.BuyerName, input.BuyerTaxID,
		input.DocumentKind, input.RelatedInvoiceNumber, input.Currency,
		input.AmountMinor, input.TaxAmountMinor, input.MinorUnitScale, input.Source,
		input.IdempotencyKey, fingerprint, dbTime(input.IssuedAt), input.CreatedBy, dbTime(now), dbTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return InvoiceDocument{}, fmt.Errorf("%w: invoice number or idempotency key already exists", ErrConflict)
		}
		return InvoiceDocument{}, fmt.Errorf("create invoice document: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return InvoiceDocument{}, fmt.Errorf("read invoice id: %w", err)
	}
	if input.DocumentKind == InvoiceRed {
		if input.RelatedInvoiceID == nil {
			return InvoiceDocument{}, fmt.Errorf("%w: red invoice relation was not resolved", ErrConflict)
		}
		if _, err := executor.ExecContext(ctx, `INSERT INTO invoice_document_relations(
			red_invoice_id, related_invoice_id, relation_state, reason_code,
			related_invoice_number_snapshot, seller_entity_snapshot, currency_snapshot,
			minor_unit_scale_snapshot, created_at, updated_at
		) VALUES (?, ?, 'verified', '', ?, ?, ?, ?, ?, ?)`,
			id, *input.RelatedInvoiceID, input.RelatedInvoiceNumber, input.SellerEntity,
			input.Currency, input.MinorUnitScale, dbTime(now), dbTime(now)); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "exceeds original invoice") {
				return InvoiceDocument{}, fmt.Errorf("%w: cumulative red invoice amount exceeds original invoice", ErrConflict)
			}
			return InvoiceDocument{}, fmt.Errorf("create invoice document relation: %w", err)
		}
	}
	return getInvoiceDocument(ctx, executor, id)
}

func (s *Store) resolveInvoiceRelation(ctx context.Context, queryer queryRower, input InvoiceDocumentInput) (InvoiceDocumentInput, error) {
	if input.DocumentKind == InvoiceBlue {
		if input.RelatedInvoiceID != nil || input.RelatedInvoiceNumber != "" {
			return InvoiceDocumentInput{}, fmt.Errorf("%w: blue invoice cannot reference another invoice", ErrInvalid)
		}
		return input, nil
	}
	if input.DocumentKind != InvoiceRed {
		return InvoiceDocumentInput{}, fmt.Errorf("%w: unsupported invoice document kind", ErrInvalid)
	}

	var relatedID int64
	if input.RelatedInvoiceID != nil {
		relatedID = *input.RelatedInvoiceID
	} else {
		var count int
		if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(id), 0)
			FROM invoice_documents WHERE document_kind = 'blue'
				AND trim(seller_entity) = trim(?) COLLATE NOCASE
				AND trim(invoice_number) = trim(?) COLLATE NOCASE`,
			input.SellerEntity, input.RelatedInvoiceNumber).Scan(&count, &relatedID); err != nil {
			return InvoiceDocumentInput{}, fmt.Errorf("resolve related invoice number: %w", err)
		}
		if count != 1 || relatedID <= 0 {
			return InvoiceDocumentInput{}, fmt.Errorf("%w: related invoice number is missing or ambiguous", ErrConflict)
		}
	}
	original, err := getInvoiceDocument(ctx, queryer, relatedID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return InvoiceDocumentInput{}, fmt.Errorf("%w: related invoice does not exist", ErrConflict)
		}
		return InvoiceDocumentInput{}, err
	}
	if original.DocumentKind != InvoiceBlue || original.Status != InvoiceIssued {
		return InvoiceDocumentInput{}, fmt.Errorf("%w: related invoice must be an issued blue invoice", ErrConflict)
	}
	if !strings.EqualFold(strings.TrimSpace(original.SellerEntity), strings.TrimSpace(input.SellerEntity)) ||
		original.Currency != input.Currency || original.MinorUnitScale != input.MinorUnitScale {
		return InvoiceDocumentInput{}, fmt.Errorf("%w: related invoice seller, currency, or precision does not match", ErrConflict)
	}
	if input.RelatedInvoiceNumber != "" && !strings.EqualFold(strings.TrimSpace(original.InvoiceNumber), strings.TrimSpace(input.RelatedInvoiceNumber)) {
		return InvoiceDocumentInput{}, fmt.Errorf("%w: related invoice id and number do not match", ErrConflict)
	}
	var credited int64
	if err := queryer.QueryRowContext(ctx, `SELECT COALESCE(SUM(red.amount_minor), 0)
		FROM invoice_document_relations relation
		JOIN invoice_documents red ON red.id = relation.red_invoice_id
		WHERE relation.related_invoice_id = ? AND relation.relation_state = 'verified'
			AND red.status = 'issued'`, relatedID).Scan(&credited); err != nil {
		return InvoiceDocumentInput{}, fmt.Errorf("sum related red invoices: %w", err)
	}
	if credited < 0 || credited > original.AmountMinor || input.AmountMinor > original.AmountMinor-credited {
		return InvoiceDocumentInput{}, fmt.Errorf("%w: cumulative red invoice amount exceeds original invoice", ErrConflict)
	}
	input.RelatedInvoiceID = &relatedID
	input.RelatedInvoiceNumber = original.InvoiceNumber
	return input, nil
}

func (s *Store) appendInvoiceEvent(ctx context.Context, executor execQueryer, invoiceID int64, eventType, actor, key string, occurredAt time.Time, details any) (InvoiceEvent, error) {
	encoded, err := json.Marshal(details)
	if err != nil {
		return InvoiceEvent{}, fmt.Errorf("marshal invoice event details: %w", err)
	}
	now := s.clock()
	result, err := executor.ExecContext(ctx, `INSERT INTO invoice_events(
		invoice_id, event_type, actor, details_json, idempotency_key, occurred_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, invoiceID, eventType, actor, encoded, key, dbTime(occurredAt), dbTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return InvoiceEvent{}, fmt.Errorf("%w: invoice event idempotency key already exists", ErrConflict)
		}
		return InvoiceEvent{}, fmt.Errorf("append invoice event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return InvoiceEvent{}, fmt.Errorf("read invoice event id: %w", err)
	}
	return getInvoiceEvent(ctx, executor, id)
}

func getInvoiceDocument(ctx context.Context, queryer queryRower, id int64) (InvoiceDocument, error) {
	item, err := scanInvoiceDocument(queryer.QueryRowContext(ctx,
		"SELECT "+invoiceColumns+" FROM invoice_documents WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return InvoiceDocument{}, ErrNotFound
	}
	if err != nil {
		return InvoiceDocument{}, fmt.Errorf("get invoice document: %w", err)
	}
	return item, nil
}

func getInvoiceByIdempotencyKey(ctx context.Context, queryer queryRower, key string) (InvoiceDocument, error) {
	item, err := scanInvoiceDocument(queryer.QueryRowContext(ctx,
		"SELECT "+invoiceColumns+" FROM invoice_documents WHERE idempotency_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return InvoiceDocument{}, ErrNotFound
	}
	if err != nil {
		return InvoiceDocument{}, fmt.Errorf("get invoice by idempotency key: %w", err)
	}
	return item, nil
}

func getInvoiceEvent(ctx context.Context, queryer queryRower, id int64) (InvoiceEvent, error) {
	event, err := scanInvoiceEvent(queryer.QueryRowContext(ctx, `SELECT id, invoice_id, event_type, actor,
		details_json, idempotency_key, occurred_at, created_at FROM invoice_events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return InvoiceEvent{}, ErrNotFound
	}
	if err != nil {
		return InvoiceEvent{}, fmt.Errorf("get invoice event: %w", err)
	}
	return event, nil
}

func scanInvoiceDocument(row rowScanner) (InvoiceDocument, error) {
	var item InvoiceDocument
	var issuedAt, createdAt, updatedAt int64
	var voidedAt, relatedInvoiceID sql.NullInt64
	err := row.Scan(&item.ID, &item.InvoiceNumber, &item.SellerEntity, &item.BuyerName,
		&item.BuyerTaxID, &item.DocumentKind, &item.RelatedInvoiceNumber,
		&item.Currency, &item.AmountMinor, &item.TaxAmountMinor,
		&item.MinorUnitScale, &item.Status, &item.Source, &item.IdempotencyKey,
		&item.RequestFingerprint, &issuedAt, &voidedAt, &item.VoidReason,
		&item.CreatedBy, &createdAt, &updatedAt, &relatedInvoiceID, &item.RelationState, &item.RelationReason)
	if err != nil {
		return InvoiceDocument{}, err
	}
	item.IssuedAt = fromDBTime(issuedAt)
	item.CreatedAt = fromDBTime(createdAt)
	item.UpdatedAt = fromDBTime(updatedAt)
	if voidedAt.Valid {
		value := fromDBTime(voidedAt.Int64)
		item.VoidedAt = &value
	}
	if relatedInvoiceID.Valid {
		value := relatedInvoiceID.Int64
		item.RelatedInvoiceID = &value
	}
	return item, nil
}

func scanInvoiceEvent(row rowScanner) (InvoiceEvent, error) {
	var item InvoiceEvent
	var details string
	var occurredAt, createdAt int64
	if err := row.Scan(&item.ID, &item.InvoiceID, &item.EventType, &item.Actor, &details,
		&item.IdempotencyKey, &occurredAt, &createdAt); err != nil {
		return InvoiceEvent{}, err
	}
	item.DetailsJSON = []byte(details)
	item.OccurredAt = fromDBTime(occurredAt)
	item.CreatedAt = fromDBTime(createdAt)
	return item, nil
}

func normalizeInvoiceInput(input InvoiceDocumentInput) (InvoiceDocumentInput, string, error) {
	input.InvoiceNumber = strings.TrimSpace(input.InvoiceNumber)
	input.SellerEntity = strings.TrimSpace(input.SellerEntity)
	input.BuyerName = strings.TrimSpace(input.BuyerName)
	input.BuyerTaxID = strings.TrimSpace(input.BuyerTaxID)
	input.DocumentKind = InvoiceDocumentKind(strings.ToLower(strings.TrimSpace(string(input.DocumentKind))))
	input.RelatedInvoiceNumber = strings.TrimSpace(input.RelatedInvoiceNumber)
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	input.Source = strings.ToLower(strings.TrimSpace(input.Source))
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.CreatedBy = strings.TrimSpace(input.CreatedBy)
	input.IssuedAt = input.IssuedAt.UTC().Truncate(time.Millisecond)
	if !validInvoiceText(input.InvoiceNumber, 128, true, true) ||
		!validInvoiceText(input.SellerEntity, 128, true, true) ||
		!validInvoiceText(input.BuyerName, 256, true, true) ||
		!validInvoiceText(input.BuyerTaxID, 128, false, true) ||
		(input.DocumentKind != InvoiceBlue && input.DocumentKind != InvoiceRed) ||
		!validInvoiceText(input.RelatedInvoiceNumber, 128, input.DocumentKind == InvoiceRed && input.RelatedInvoiceID == nil, true) ||
		(input.RelatedInvoiceID != nil && *input.RelatedInvoiceID <= 0) ||
		(input.DocumentKind == InvoiceBlue && (input.RelatedInvoiceNumber != "" || input.RelatedInvoiceID != nil)) ||
		(input.DocumentKind == InvoiceRed && input.RelatedInvoiceNumber == "" && input.RelatedInvoiceID == nil) ||
		(input.DocumentKind == InvoiceRed && strings.EqualFold(input.RelatedInvoiceNumber, input.InvoiceNumber)) ||
		!validCurrency(input.Currency) || input.AmountMinor <= 0 || input.TaxAmountMinor < 0 ||
		input.TaxAmountMinor > input.AmountMinor || input.MinorUnitScale < 0 || input.MinorUnitScale > 9 ||
		(input.Source != "manual" && input.Source != "csv") ||
		!validInvoiceText(input.IdempotencyKey, 256, true, false) ||
		!validInvoiceText(input.CreatedBy, 256, true, false) || input.IssuedAt.IsZero() || input.IssuedAt.UnixMilli() < 0 {
		return InvoiceDocumentInput{}, "", fmt.Errorf("%w: invalid invoice document input", ErrInvalid)
	}
	fingerprintPayload := struct {
		InvoiceNumber, SellerEntity, BuyerName, BuyerTaxID, RelatedInvoiceNumber, Currency string
		RelatedInvoiceID                                                                   *int64 `json:",omitempty"`
		DocumentKind                                                                       InvoiceDocumentKind
		AmountMinor, TaxAmountMinor                                                        int64
		MinorUnitScale                                                                     int
		Source, CreatedBy                                                                  string
		IssuedAt                                                                           int64
	}{input.InvoiceNumber, input.SellerEntity, input.BuyerName, input.BuyerTaxID, input.RelatedInvoiceNumber, input.Currency,
		input.RelatedInvoiceID,
		input.DocumentKind,
		input.AmountMinor, input.TaxAmountMinor, input.MinorUnitScale, input.Source, input.CreatedBy,
		input.IssuedAt.UnixMilli()}
	encoded, _ := json.Marshal(fingerprintPayload)
	return input, sha256Hex(string(encoded)), nil
}

func validateInvoiceFilter(status InvoiceStatus, currency string, from, to *time.Time, beforeID int64) error {
	if status != "" && status != InvoiceIssued && status != InvoiceVoided {
		return fmt.Errorf("%w: unsupported invoice status", ErrInvalid)
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != "" && !validCurrency(currency) {
		return fmt.Errorf("%w: invalid invoice currency", ErrInvalid)
	}
	if beforeID < 0 || (from != nil && from.UnixMilli() < 0) || (to != nil && to.UnixMilli() < 0) ||
		(from != nil && to != nil && !from.Before(*to)) {
		return fmt.Errorf("%w: invalid invoice filter", ErrInvalid)
	}
	return nil
}

func appendInvoiceTimeFilters(query *strings.Builder, args *[]any, from, to *time.Time) {
	if from != nil {
		query.WriteString(" AND issued_at >= ?")
		*args = append(*args, dbTime(*from))
	}
	if to != nil {
		query.WriteString(" AND issued_at < ?")
		*args = append(*args, dbTime(*to))
	}
}

func validCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func validInvoiceText(value string, maximum int, required, spreadsheetSafe bool) bool {
	if !utf8.ValidString(value) || len(value) > maximum || (required && strings.TrimSpace(value) == "") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	if spreadsheetSafe {
		trimmed := strings.TrimLeft(value, " \t\r\n")
		if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
			return false
		}
	}
	return true
}

func invoiceState(item InvoiceDocument) invoiceAuditState {
	return invoiceAuditState{ID: item.ID, RequestFingerprint: item.RequestFingerprint, DocumentKind: item.DocumentKind,
		RelatedInvoiceID: item.RelatedInvoiceID,
		Currency:         item.Currency, AmountMinor: strconv.FormatInt(item.AmountMinor, 10), MinorUnitScale: item.MinorUnitScale,
		Status: item.Status, IssuedAt: item.IssuedAt, VoidedAt: item.VoidedAt}
}

func invoiceCreatedEventDetails(item InvoiceDocument) map[string]any {
	return map[string]any{"invoice_number": item.InvoiceNumber, "document_kind": item.DocumentKind,
		"related_invoice_number": item.RelatedInvoiceNumber, "related_invoice_id": item.RelatedInvoiceID,
		"currency":         item.Currency,
		"amount_minor":     strconv.FormatInt(item.AmountMinor, 10),
		"tax_amount_minor": strconv.FormatInt(item.TaxAmountMinor, 10),
		"minor_unit_scale": item.MinorUnitScale, "source": item.Source}
}

func invoiceEventKey(key, eventType string) string {
	return "invoice-event:" + sha256Hex(strings.TrimSpace(key)+"\x00"+eventType)
}

func invoiceImportRowKey(key string, index int) string {
	return "invoice-row:" + sha256Hex(strings.TrimSpace(key)+"\x00"+strconv.Itoa(index))
}

func fingerprintStrings(values []string) string {
	encoded, _ := json.Marshal(values)
	return sha256Hex(string(encoded))
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
