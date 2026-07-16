package toolstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const priceSnapshotColumns = `id, provider, model, operation, component, currency,
	unit, unit_size, amount_decimal, amount_minor, minor_unit_scale, source,
	COALESCE(metadata_json, ''), COALESCE(idempotency_key, ''), effective_at,
	expires_at, created_at`

// CreatePriceSnapshot appends an immutable price observation. Decimal and
// scaled-minor representations must agree exactly before any SQL is executed.
func (s *Store) CreatePriceSnapshot(ctx context.Context, input PriceSnapshotInput) (PriceSnapshot, error) {
	return s.createPriceSnapshot(ctx, s.db, input)
}

func (s *Store) createPriceSnapshot(ctx context.Context, executor execQueryer, input PriceSnapshotInput) (PriceSnapshot, error) {
	var err error
	if input.Provider, err = requireText("provider", input.Provider); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Model, err = requireText("model", input.Model); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Operation, err = requireText("operation", input.Operation); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Component, err = requireText("component", input.Component); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Unit, err = requireText("unit", input.Unit); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Source, err = requireText("source", input.Source); err != nil {
		return PriceSnapshot{}, err
	}
	if input.Currency, err = validateCurrency(input.Currency); err != nil {
		return PriceSnapshot{}, err
	}
	if input.UnitSize <= 0 {
		return PriceSnapshot{}, fmt.Errorf("%w: unit_size must be positive", ErrInvalid)
	}
	input.AmountDecimal = strings.TrimSpace(input.AmountDecimal)
	if err := validateExactAmount(input.AmountDecimal, input.AmountMinor, input.MinorUnitScale); err != nil {
		return PriceSnapshot{}, err
	}
	metadata, err := normalizedJSON("metadata_json", input.MetadataJSON)
	if err != nil {
		return PriceSnapshot{}, err
	}
	now := s.clock()
	effectiveAt := normalizeOccurred(input.EffectiveAt, now)
	expiresAt := normalizeOptionalTime(input.ExpiresAt)
	if expiresAt != nil && !expiresAt.After(effectiveAt) {
		return PriceSnapshot{}, fmt.Errorf("%w: expires_at must be after effective_at", ErrInvalid)
	}
	key := strings.TrimSpace(input.IdempotencyKey)
	result, err := executor.ExecContext(ctx, `INSERT INTO price_snapshots(
		provider, model, operation, component, currency, unit, unit_size,
		amount_decimal, amount_minor, minor_unit_scale, source, metadata_json,
		idempotency_key, effective_at, expires_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING`,
		input.Provider, input.Model, input.Operation, input.Component, input.Currency,
		input.Unit, input.UnitSize, input.AmountDecimal, input.AmountMinor,
		input.MinorUnitScale, input.Source, metadata, optionalKey(key),
		dbTime(effectiveAt), nullableDBTime(expiresAt), dbTime(now))
	if err != nil {
		return PriceSnapshot{}, fmt.Errorf("create price snapshot: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return PriceSnapshot{}, fmt.Errorf("read price snapshot result: %w", err)
	}
	if rows == 0 {
		if key == "" {
			return PriceSnapshot{}, fmt.Errorf("%w: price snapshot insert was ignored", ErrConflict)
		}
		existing, getErr := getPriceSnapshotByIdempotencyKey(ctx, executor, key)
		if getErr != nil {
			return PriceSnapshot{}, getErr
		}
		if existing.Provider != input.Provider || existing.Model != input.Model ||
			existing.Operation != input.Operation || existing.Component != input.Component ||
			existing.Currency != input.Currency || existing.Unit != input.Unit ||
			existing.UnitSize != input.UnitSize || existing.AmountDecimal != input.AmountDecimal ||
			existing.AmountMinor != input.AmountMinor || existing.MinorUnitScale != input.MinorUnitScale ||
			existing.Source != input.Source || string(existing.MetadataJSON) != string(metadata) ||
			(!input.EffectiveAt.IsZero() && existing.EffectiveAt.UnixMilli() != effectiveAt.UnixMilli()) ||
			!sameOptionalTime(existing.ExpiresAt, expiresAt) {
			return PriceSnapshot{}, fmt.Errorf("%w: idempotency key belongs to a different price snapshot", ErrConflict)
		}
		return existing, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return PriceSnapshot{}, fmt.Errorf("read price snapshot id: %w", err)
	}
	return getPriceSnapshot(ctx, executor, id)
}

func (s *Store) GetPriceSnapshot(ctx context.Context, id int64) (PriceSnapshot, error) {
	if id <= 0 {
		return PriceSnapshot{}, fmt.Errorf("%w: price snapshot id must be positive", ErrInvalid)
	}
	return getPriceSnapshot(ctx, s.db, id)
}

func getPriceSnapshot(ctx context.Context, queryer queryRower, id int64) (PriceSnapshot, error) {
	item, err := scanPriceSnapshot(queryer.QueryRowContext(ctx,
		"SELECT "+priceSnapshotColumns+" FROM price_snapshots WHERE id = ?", id))
	if err != nil {
		if err == sql.ErrNoRows {
			return PriceSnapshot{}, ErrNotFound
		}
		return PriceSnapshot{}, fmt.Errorf("get price snapshot: %w", err)
	}
	return item, nil
}

func (s *Store) GetPriceSnapshotByIdempotencyKey(ctx context.Context, key string) (PriceSnapshot, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return PriceSnapshot{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	return getPriceSnapshotByIdempotencyKey(ctx, s.db, key)
}

func getPriceSnapshotByIdempotencyKey(ctx context.Context, queryer queryRower, key string) (PriceSnapshot, error) {
	item, err := scanPriceSnapshot(queryer.QueryRowContext(ctx,
		"SELECT "+priceSnapshotColumns+" FROM price_snapshots WHERE idempotency_key = ?", key))
	if err != nil {
		if err == sql.ErrNoRows {
			return PriceSnapshot{}, ErrNotFound
		}
		return PriceSnapshot{}, fmt.Errorf("get price snapshot by idempotency key: %w", err)
	}
	return item, nil
}

func (s *Store) ListPriceSnapshots(ctx context.Context, filter PriceSnapshotFilter) (PriceSnapshotPage, error) {
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(priceSnapshotColumns)
	query.WriteString(" FROM price_snapshots WHERE 1=1")
	args := make([]any, 0, 9)
	add := func(column, value string) {
		if strings.TrimSpace(value) != "" {
			query.WriteString(" AND " + column + " = ?")
			args = append(args, strings.TrimSpace(value))
		}
	}
	add("provider", filter.Provider)
	add("model", filter.Model)
	add("operation", filter.Operation)
	add("component", filter.Component)
	if strings.TrimSpace(filter.Currency) != "" {
		currency, err := validateCurrency(filter.Currency)
		if err != nil {
			return PriceSnapshotPage{}, err
		}
		add("currency", currency)
	}
	if filter.ActiveAt != nil {
		at := dbTime(*filter.ActiveAt)
		query.WriteString(" AND effective_at <= ? AND (expires_at IS NULL OR expires_at > ?)")
		args = append(args, at, at)
	}
	if filter.BeforeID > 0 {
		query.WriteString(" AND id < ?")
		args = append(args, filter.BeforeID)
	}
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return PriceSnapshotPage{}, fmt.Errorf("list price snapshots: %w", err)
	}
	defer rows.Close()
	items := make([]PriceSnapshot, 0, limit+1)
	for rows.Next() {
		item, err := scanPriceSnapshot(rows)
		if err != nil {
			return PriceSnapshotPage{}, fmt.Errorf("scan price snapshot: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return PriceSnapshotPage{}, fmt.Errorf("iterate price snapshots: %w", err)
	}
	items, cursor, more := pageResult(items, limit, func(item PriceSnapshot) int64 { return item.ID })
	return PriceSnapshotPage{Items: items, NextCursor: cursor, HasMore: more}, nil
}

func scanPriceSnapshot(row rowScanner) (PriceSnapshot, error) {
	var item PriceSnapshot
	var metadata string
	var effectiveAt, createdAt int64
	var expiresAt sql.NullInt64
	err := row.Scan(&item.ID, &item.Provider, &item.Model, &item.Operation,
		&item.Component, &item.Currency, &item.Unit, &item.UnitSize,
		&item.AmountDecimal, &item.AmountMinor, &item.MinorUnitScale, &item.Source,
		&metadata, &item.IdempotencyKey, &effectiveAt, &expiresAt, &createdAt)
	if err != nil {
		return PriceSnapshot{}, err
	}
	if metadata != "" {
		item.MetadataJSON = json.RawMessage(metadata)
	}
	item.EffectiveAt = fromDBTime(effectiveAt)
	item.CreatedAt = fromDBTime(createdAt)
	if expiresAt.Valid {
		value := fromDBTime(expiresAt.Int64)
		item.ExpiresAt = &value
	}
	return item, nil
}
