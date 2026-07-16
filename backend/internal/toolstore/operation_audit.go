package toolstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const operationAuditColumns = `id, request_id, actor, source_ip, auth_method, action,
	target_type, target_id, reason, COALESCE(before_json, ''), COALESCE(after_json, ''),
	status, error_code, COALESCE(idempotency_key, ''), occurred_at, created_at`

// AppendOperationAudit writes a final, immutable operation record. A supplied
// idempotency key makes retries return the original row without duplicating it.
func (s *Store) AppendOperationAudit(ctx context.Context, input OperationAuditInput) (OperationAudit, error) {
	return s.appendOperationAudit(ctx, s.db, input)
}

// AppendOperationAuditWithReconciliationRun atomically persists an immutable
// operation audit and its reconciliation fallback. Neither record is visible
// unless both writes and the transaction commit succeed.
func (s *Store) AppendOperationAuditWithReconciliationRun(
	ctx context.Context,
	auditInput OperationAuditInput,
	runInput ReconciliationRunInput,
) (OperationAudit, ReconciliationRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationAudit{}, ReconciliationRun{}, fmt.Errorf("begin operation audit and reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	audit, err := s.appendOperationAudit(ctx, tx, auditInput)
	if err != nil {
		return OperationAudit{}, ReconciliationRun{}, err
	}
	run, err := s.createReconciliationRun(ctx, tx, runInput)
	if err != nil {
		return OperationAudit{}, ReconciliationRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationAudit{}, ReconciliationRun{}, fmt.Errorf("commit operation audit and reconciliation: %w", err)
	}
	return audit, run, nil
}

// ClaimOperationAudit atomically inserts an immutable operation record and
// reports whether this caller inserted it. Callers must continue the protected
// side effect only when claimed is true; an existing matching row is returned
// with claimed=false.
func (s *Store) ClaimOperationAudit(ctx context.Context, input OperationAuditInput) (OperationAudit, bool, error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return OperationAudit{}, false, fmt.Errorf("%w: idempotency key is required for an operation claim", ErrInvalid)
	}
	return s.appendOperationAuditClaim(ctx, s.db, input)
}

func (s *Store) appendOperationAudit(ctx context.Context, executor execQueryer, input OperationAuditInput) (OperationAudit, error) {
	audit, _, err := s.appendOperationAuditClaim(ctx, executor, input)
	return audit, err
}

func (s *Store) appendOperationAuditClaim(ctx context.Context, executor execQueryer, input OperationAuditInput) (OperationAudit, bool, error) {
	var err error
	if input.RequestID, err = requireText("request_id", input.RequestID); err != nil {
		return OperationAudit{}, false, err
	}
	if input.Actor, err = requireText("actor", input.Actor); err != nil {
		return OperationAudit{}, false, err
	}
	if input.SourceIP, err = requireText("source_ip", input.SourceIP); err != nil {
		return OperationAudit{}, false, err
	}
	if input.AuthMethod, err = requireText("auth_method", input.AuthMethod); err != nil {
		return OperationAudit{}, false, err
	}
	if input.Action, err = requireText("action", input.Action); err != nil {
		return OperationAudit{}, false, err
	}
	if input.TargetType, err = requireText("target_type", input.TargetType); err != nil {
		return OperationAudit{}, false, err
	}
	if input.TargetID, err = requireText("target_id", input.TargetID); err != nil {
		return OperationAudit{}, false, err
	}
	if !validOperationStatus(input.Status) {
		return OperationAudit{}, false, fmt.Errorf("%w: unsupported operation status %q", ErrInvalid, input.Status)
	}
	beforeJSON, err := normalizedJSON("before_json", input.BeforeJSON)
	if err != nil {
		return OperationAudit{}, false, err
	}
	afterJSON, err := normalizedJSON("after_json", input.AfterJSON)
	if err != nil {
		return OperationAudit{}, false, err
	}

	now := s.clock()
	occurredAt := normalizeOccurred(input.OccurredAt, now)
	idempotencyKey := strings.TrimSpace(input.IdempotencyKey)
	result, err := executor.ExecContext(ctx, `INSERT INTO operation_audit(
		request_id, actor, source_ip, auth_method, action, target_type, target_id,
		reason, before_json, after_json, status, error_code, idempotency_key,
		occurred_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING`,
		input.RequestID, input.Actor, input.SourceIP, input.AuthMethod, input.Action,
		input.TargetType, input.TargetID, strings.TrimSpace(input.Reason),
		beforeJSON, afterJSON, input.Status, strings.TrimSpace(input.ErrorCode),
		optionalKey(idempotencyKey), dbTime(occurredAt), dbTime(now))
	if err != nil {
		return OperationAudit{}, false, fmt.Errorf("append operation audit: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return OperationAudit{}, false, fmt.Errorf("read operation audit result: %w", err)
	}
	if rows == 0 {
		if idempotencyKey == "" {
			return OperationAudit{}, false, fmt.Errorf("%w: operation audit insert was ignored", ErrConflict)
		}
		existing, getErr := getOperationAuditByIdempotencyKey(ctx, executor, idempotencyKey)
		if getErr != nil {
			return OperationAudit{}, false, getErr
		}
		if existing.Actor != input.Actor || existing.AuthMethod != input.AuthMethod ||
			existing.Action != input.Action || existing.TargetType != input.TargetType ||
			existing.TargetID != input.TargetID || existing.Reason != strings.TrimSpace(input.Reason) ||
			string(existing.BeforeJSON) != string(beforeJSON) || string(existing.AfterJSON) != string(afterJSON) ||
			existing.Status != input.Status || existing.ErrorCode != strings.TrimSpace(input.ErrorCode) {
			return OperationAudit{}, false, fmt.Errorf("%w: idempotency key belongs to a different operation audit", ErrConflict)
		}
		return existing, false, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return OperationAudit{}, true, fmt.Errorf("read operation audit id: %w", err)
	}
	audit, err := getOperationAudit(ctx, executor, id)
	return audit, true, err
}

func (s *Store) GetOperationAudit(ctx context.Context, id int64) (OperationAudit, error) {
	if id <= 0 {
		return OperationAudit{}, fmt.Errorf("%w: operation audit id must be positive", ErrInvalid)
	}
	return getOperationAudit(ctx, s.db, id)
}

func getOperationAudit(ctx context.Context, queryer queryRower, id int64) (OperationAudit, error) {
	item, err := scanOperationAudit(queryer.QueryRowContext(ctx,
		"SELECT "+operationAuditColumns+" FROM operation_audit WHERE id = ?", id))
	if err != nil {
		if err == sql.ErrNoRows {
			return OperationAudit{}, ErrNotFound
		}
		return OperationAudit{}, fmt.Errorf("get operation audit: %w", err)
	}
	return item, nil
}

func (s *Store) GetOperationAuditByIdempotencyKey(ctx context.Context, key string) (OperationAudit, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return OperationAudit{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	return getOperationAuditByIdempotencyKey(ctx, s.db, key)
}

func getOperationAuditByIdempotencyKey(ctx context.Context, queryer queryRower, key string) (OperationAudit, error) {
	item, err := scanOperationAudit(queryer.QueryRowContext(ctx,
		"SELECT "+operationAuditColumns+" FROM operation_audit WHERE idempotency_key = ?", key))
	if err != nil {
		if err == sql.ErrNoRows {
			return OperationAudit{}, ErrNotFound
		}
		return OperationAudit{}, fmt.Errorf("get operation audit by idempotency key: %w", err)
	}
	return item, nil
}

func (s *Store) ListOperationAudits(ctx context.Context, filter OperationAuditFilter) (OperationAuditPage, error) {
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(operationAuditColumns)
	query.WriteString(" FROM operation_audit WHERE 1=1")
	args := make([]any, 0, 9)
	addStringFilter := func(column, value string) {
		if value != "" {
			query.WriteString(" AND ")
			query.WriteString(column)
			query.WriteString(" = ?")
			args = append(args, strings.TrimSpace(value))
		}
	}
	addStringFilter("request_id", filter.RequestID)
	addStringFilter("actor", filter.Actor)
	addStringFilter("action", filter.Action)
	addStringFilter("target_type", filter.TargetType)
	addStringFilter("target_id", filter.TargetID)
	if filter.Status != "" {
		if !validOperationStatus(filter.Status) {
			return OperationAuditPage{}, fmt.Errorf("%w: unsupported operation status %q", ErrInvalid, filter.Status)
		}
		addStringFilter("status", string(filter.Status))
	}
	if filter.OrderByCreatedAt {
		if filter.BeforeID < 0 {
			return OperationAuditPage{}, fmt.Errorf("%w: before_id cannot be negative", ErrInvalid)
		}
		if filter.BeforeCreatedAt.IsZero() != (filter.BeforeID == 0) ||
			(!filter.BeforeCreatedAt.IsZero() && filter.BeforeCreatedAt.UnixMilli() < 0) {
			return OperationAuditPage{}, fmt.Errorf("%w: created_at cursor requires a non-negative timestamp and positive id", ErrInvalid)
		}
		if filter.BeforeID > 0 {
			before := filter.BeforeCreatedAt.UTC().Truncate(time.Millisecond)
			query.WriteString(" AND (created_at < ? OR (created_at = ? AND id < ?))")
			args = append(args, dbTime(before), dbTime(before), filter.BeforeID)
		}
		query.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ?")
	} else {
		if filter.BeforeID < 0 {
			return OperationAuditPage{}, fmt.Errorf("%w: before_id cannot be negative", ErrInvalid)
		}
		if !filter.BeforeCreatedAt.IsZero() {
			return OperationAuditPage{}, fmt.Errorf("%w: created_at cursor requires created_at ordering", ErrInvalid)
		}
		if filter.BeforeID > 0 {
			query.WriteString(" AND id < ?")
			args = append(args, filter.BeforeID)
		}
		query.WriteString(" ORDER BY id DESC LIMIT ?")
	}
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return OperationAuditPage{}, fmt.Errorf("list operation audits: %w", err)
	}
	defer rows.Close()
	items := make([]OperationAudit, 0, limit+1)
	for rows.Next() {
		item, err := scanOperationAudit(rows)
		if err != nil {
			return OperationAuditPage{}, fmt.Errorf("scan operation audit: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return OperationAuditPage{}, fmt.Errorf("iterate operation audits: %w", err)
	}
	items, cursor, more := pageResult(items, limit, func(item OperationAudit) int64 { return item.ID })
	return OperationAuditPage{Items: items, NextCursor: cursor, HasMore: more}, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperationAudit(row rowScanner) (OperationAudit, error) {
	var item OperationAudit
	var beforeJSON, afterJSON string
	var occurredAt, createdAt int64
	err := row.Scan(
		&item.ID, &item.RequestID, &item.Actor, &item.SourceIP, &item.AuthMethod,
		&item.Action, &item.TargetType, &item.TargetID, &item.Reason, &beforeJSON,
		&afterJSON, &item.Status, &item.ErrorCode, &item.IdempotencyKey,
		&occurredAt, &createdAt,
	)
	if err != nil {
		return OperationAudit{}, err
	}
	if beforeJSON != "" {
		item.BeforeJSON = json.RawMessage(beforeJSON)
	}
	if afterJSON != "" {
		item.AfterJSON = json.RawMessage(afterJSON)
	}
	item.OccurredAt = fromDBTime(occurredAt)
	item.CreatedAt = fromDBTime(createdAt)
	return item, nil
}
