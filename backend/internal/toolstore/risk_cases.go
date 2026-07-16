package toolstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const riskCaseColumns = `id, case_key, title, subject_type, subject_id, severity, status,
	assignee, summary, opened_at, closed_at, created_at, updated_at`

const riskCaseEventColumns = `id, case_id, event_type, actor, COALESCE(details_json, ''),
	COALESCE(idempotency_key, ''), occurred_at, created_at`

func (s *Store) CreateRiskCase(ctx context.Context, input RiskCaseInput) (RiskCase, error) {
	return s.createRiskCase(ctx, s.db, input)
}

func (s *Store) createRiskCase(ctx context.Context, executor execQueryer, input RiskCaseInput) (RiskCase, error) {
	var err error
	if input.CaseKey, err = requireText("case_key", input.CaseKey); err != nil {
		return RiskCase{}, err
	}
	if input.Title, err = requireText("title", input.Title); err != nil {
		return RiskCase{}, err
	}
	if input.SubjectType, err = requireText("subject_type", input.SubjectType); err != nil {
		return RiskCase{}, err
	}
	if input.SubjectID, err = requireText("subject_id", input.SubjectID); err != nil {
		return RiskCase{}, err
	}
	if !validSeverity(input.Severity) {
		return RiskCase{}, fmt.Errorf("%w: unsupported risk severity %q", ErrInvalid, input.Severity)
	}
	if err := validateRiskClosure(input.Status, input.ClosedAt); err != nil {
		return RiskCase{}, err
	}

	now := s.clock()
	openedAt := normalizeOccurred(input.OpenedAt, now)
	if input.ClosedAt != nil && input.ClosedAt.Before(openedAt) {
		return RiskCase{}, fmt.Errorf("%w: closed_at cannot be before opened_at", ErrInvalid)
	}
	result, err := executor.ExecContext(ctx, `INSERT INTO risk_cases(
		case_key, title, subject_type, subject_id, severity, status, assignee,
		summary, opened_at, closed_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.CaseKey, input.Title, input.SubjectType, input.SubjectID, input.Severity,
		input.Status, strings.TrimSpace(input.Assignee), strings.TrimSpace(input.Summary),
		dbTime(openedAt), nullableDBTime(input.ClosedAt), dbTime(now), dbTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return RiskCase{}, fmt.Errorf("%w: risk case key already exists", ErrConflict)
		}
		return RiskCase{}, fmt.Errorf("create risk case: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return RiskCase{}, fmt.Errorf("read risk case id: %w", err)
	}
	return getRiskCase(ctx, executor, id)
}

func (s *Store) GetRiskCase(ctx context.Context, id int64) (RiskCase, error) {
	if id <= 0 {
		return RiskCase{}, fmt.Errorf("%w: risk case id must be positive", ErrInvalid)
	}
	return getRiskCase(ctx, s.db, id)
}

func getRiskCase(ctx context.Context, queryer queryRower, id int64) (RiskCase, error) {
	item, err := scanRiskCase(queryer.QueryRowContext(ctx,
		"SELECT "+riskCaseColumns+" FROM risk_cases WHERE id = ?", id))
	if err != nil {
		if err == sql.ErrNoRows {
			return RiskCase{}, ErrNotFound
		}
		return RiskCase{}, fmt.Errorf("get risk case: %w", err)
	}
	return item, nil
}

// UpdateRiskCase replaces the mutable case fields. Evidence belongs in a
// RiskCaseEvent; callers that need atomic state+evidence should use
// UpdateRiskCaseWithEvent.
func (s *Store) UpdateRiskCase(ctx context.Context, update RiskCaseUpdate) (RiskCase, error) {
	return s.updateRiskCase(ctx, s.db, update)
}

// UpdateRiskCaseWithEvent commits the case transition and its immutable event
// in one short transaction.
func (s *Store) UpdateRiskCaseWithEvent(ctx context.Context, update RiskCaseUpdate, event RiskCaseEventInput) (RiskCase, RiskCaseEvent, error) {
	if update.ID <= 0 || event.CaseID != update.ID {
		return RiskCase{}, RiskCaseEvent{}, fmt.Errorf("%w: event case_id must match update id", ErrInvalid)
	}
	event.IdempotencyKey = strings.TrimSpace(event.IdempotencyKey)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RiskCase{}, RiskCaseEvent{}, fmt.Errorf("begin risk case transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if event.IdempotencyKey != "" {
		existingEvent, replayErr := getRiskCaseEventByIdempotencyKey(ctx, tx, event.IdempotencyKey)
		switch {
		case replayErr == nil:
			current, getErr := getRiskCase(ctx, tx, update.ID)
			if getErr != nil {
				return RiskCase{}, RiskCaseEvent{}, getErr
			}
			if !riskCaseEventMatchesInput(existingEvent, event) || !riskCaseMatchesUpdate(current, update) {
				return RiskCase{}, RiskCaseEvent{}, replayConflict("risk case transition")
			}
			if err := tx.Commit(); err != nil {
				return RiskCase{}, RiskCaseEvent{}, fmt.Errorf("commit risk case transition replay: %w", err)
			}
			return current, existingEvent, nil
		case !errors.Is(replayErr, ErrNotFound):
			return RiskCase{}, RiskCaseEvent{}, replayErr
		}
	}
	updated, err := s.updateRiskCase(ctx, tx, update)
	if err != nil {
		return RiskCase{}, RiskCaseEvent{}, err
	}
	createdEvent, err := s.appendRiskCaseEvent(ctx, tx, event)
	if err != nil {
		return RiskCase{}, RiskCaseEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return RiskCase{}, RiskCaseEvent{}, fmt.Errorf("commit risk case transition: %w", err)
	}
	return updated, createdEvent, nil
}

func (s *Store) updateRiskCase(ctx context.Context, executor execQueryer, update RiskCaseUpdate) (RiskCase, error) {
	if update.ID <= 0 {
		return RiskCase{}, fmt.Errorf("%w: risk case id must be positive", ErrInvalid)
	}
	var err error
	if update.Title, err = requireText("title", update.Title); err != nil {
		return RiskCase{}, err
	}
	if !validSeverity(update.Severity) {
		return RiskCase{}, fmt.Errorf("%w: unsupported risk severity %q", ErrInvalid, update.Severity)
	}
	if err := validateRiskClosure(update.Status, update.ClosedAt); err != nil {
		return RiskCase{}, err
	}
	persisted, err := getRiskCase(ctx, executor, update.ID)
	if err != nil {
		return RiskCase{}, err
	}
	if update.ClosedAt != nil && update.ClosedAt.UnixMilli() < persisted.OpenedAt.UnixMilli() {
		return RiskCase{}, fmt.Errorf("%w: closed_at cannot be before opened_at", ErrInvalid)
	}
	now := s.clock()
	result, err := executor.ExecContext(ctx, `UPDATE risk_cases SET
		title = ?, severity = ?, status = ?, assignee = ?, summary = ?, closed_at = ?, updated_at = ?
		WHERE id = ?`,
		update.Title, update.Severity, update.Status, strings.TrimSpace(update.Assignee),
		strings.TrimSpace(update.Summary), nullableDBTime(update.ClosedAt), dbTime(now), update.ID)
	if err != nil {
		return RiskCase{}, fmt.Errorf("update risk case: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RiskCase{}, fmt.Errorf("read risk case update result: %w", err)
	}
	if rows == 0 {
		return RiskCase{}, ErrNotFound
	}
	return getRiskCase(ctx, executor, update.ID)
}

func validateRiskClosure(status RiskCaseStatus, closedAt *time.Time) error {
	if !validRiskStatus(status) {
		return fmt.Errorf("%w: unsupported risk case status %q", ErrInvalid, status)
	}
	if status == RiskCaseClosed && closedAt == nil {
		return fmt.Errorf("%w: closed_at is required for closed risk cases", ErrInvalid)
	}
	if status != RiskCaseClosed && closedAt != nil {
		return fmt.Errorf("%w: closed_at is only valid for closed risk cases", ErrInvalid)
	}
	return nil
}

func (s *Store) ListRiskCases(ctx context.Context, filter RiskCaseFilter) (RiskCasePage, error) {
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(riskCaseColumns)
	query.WriteString(" FROM risk_cases WHERE 1=1")
	args := make([]any, 0, 8)
	add := func(column, value string) {
		if strings.TrimSpace(value) != "" {
			query.WriteString(" AND " + column + " = ?")
			args = append(args, strings.TrimSpace(value))
		}
	}
	add("subject_type", filter.SubjectType)
	add("subject_id", filter.SubjectID)
	if filter.Severity != "" {
		if !validSeverity(filter.Severity) {
			return RiskCasePage{}, fmt.Errorf("%w: unsupported risk severity %q", ErrInvalid, filter.Severity)
		}
		add("severity", string(filter.Severity))
	}
	if filter.Status != "" {
		if !validRiskStatus(filter.Status) {
			return RiskCasePage{}, fmt.Errorf("%w: unsupported risk status %q", ErrInvalid, filter.Status)
		}
		add("status", string(filter.Status))
	}
	add("assignee", filter.Assignee)
	if filter.BeforeID > 0 {
		query.WriteString(" AND id < ?")
		args = append(args, filter.BeforeID)
	}
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return RiskCasePage{}, fmt.Errorf("list risk cases: %w", err)
	}
	defer rows.Close()
	items := make([]RiskCase, 0, limit+1)
	for rows.Next() {
		item, err := scanRiskCase(rows)
		if err != nil {
			return RiskCasePage{}, fmt.Errorf("scan risk case: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return RiskCasePage{}, fmt.Errorf("iterate risk cases: %w", err)
	}
	items, cursor, more := pageResult(items, limit, func(item RiskCase) int64 { return item.ID })
	return RiskCasePage{Items: items, NextCursor: cursor, HasMore: more}, nil
}

func (s *Store) AppendRiskCaseEvent(ctx context.Context, input RiskCaseEventInput) (RiskCaseEvent, error) {
	return s.appendRiskCaseEvent(ctx, s.db, input)
}

func (s *Store) appendRiskCaseEvent(ctx context.Context, executor execQueryer, input RiskCaseEventInput) (RiskCaseEvent, error) {
	if input.CaseID <= 0 {
		return RiskCaseEvent{}, fmt.Errorf("%w: case_id must be positive", ErrInvalid)
	}
	var err error
	if input.EventType, err = requireText("event_type", input.EventType); err != nil {
		return RiskCaseEvent{}, err
	}
	if input.Actor, err = requireText("actor", input.Actor); err != nil {
		return RiskCaseEvent{}, err
	}
	details, err := normalizedJSON("details_json", input.DetailsJSON)
	if err != nil {
		return RiskCaseEvent{}, err
	}
	now := s.clock()
	occurredAt := normalizeOccurred(input.OccurredAt, now)
	idempotencyKey := strings.TrimSpace(input.IdempotencyKey)
	result, err := executor.ExecContext(ctx, `INSERT INTO risk_case_events(
		case_id, event_type, actor, details_json, idempotency_key, occurred_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING`,
		input.CaseID, input.EventType, input.Actor, details, optionalKey(idempotencyKey),
		dbTime(occurredAt), dbTime(now))
	if err != nil {
		return RiskCaseEvent{}, fmt.Errorf("append risk case event: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RiskCaseEvent{}, fmt.Errorf("read risk case event result: %w", err)
	}
	if rows == 0 {
		if idempotencyKey == "" {
			return RiskCaseEvent{}, fmt.Errorf("%w: risk case event insert was ignored", ErrConflict)
		}
		existing, getErr := getRiskCaseEventByIdempotencyKey(ctx, executor, idempotencyKey)
		if getErr != nil {
			return RiskCaseEvent{}, getErr
		}
		if existing.CaseID != input.CaseID || existing.EventType != input.EventType ||
			existing.Actor != input.Actor || string(existing.DetailsJSON) != string(details) ||
			(!input.OccurredAt.IsZero() && existing.OccurredAt.UnixMilli() != occurredAt.UnixMilli()) {
			return RiskCaseEvent{}, fmt.Errorf("%w: idempotency key belongs to a different risk case event", ErrConflict)
		}
		return existing, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return RiskCaseEvent{}, fmt.Errorf("read risk case event id: %w", err)
	}
	return getRiskCaseEvent(ctx, executor, id)
}

func (s *Store) GetRiskCaseEvent(ctx context.Context, id int64) (RiskCaseEvent, error) {
	if id <= 0 {
		return RiskCaseEvent{}, fmt.Errorf("%w: risk case event id must be positive", ErrInvalid)
	}
	return getRiskCaseEvent(ctx, s.db, id)
}

func (s *Store) GetRiskCaseEventByIdempotencyKey(ctx context.Context, key string) (RiskCaseEvent, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return RiskCaseEvent{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	return getRiskCaseEventByIdempotencyKey(ctx, s.db, key)
}

func getRiskCaseEventByIdempotencyKey(ctx context.Context, queryer queryRower, key string) (RiskCaseEvent, error) {
	item, err := scanRiskCaseEvent(queryer.QueryRowContext(ctx,
		"SELECT "+riskCaseEventColumns+" FROM risk_case_events WHERE idempotency_key = ?", key))
	if err != nil {
		if err == sql.ErrNoRows {
			return RiskCaseEvent{}, ErrNotFound
		}
		return RiskCaseEvent{}, fmt.Errorf("get risk case event by idempotency key: %w", err)
	}
	return item, nil
}

func getRiskCaseEvent(ctx context.Context, queryer queryRower, id int64) (RiskCaseEvent, error) {
	item, err := scanRiskCaseEvent(queryer.QueryRowContext(ctx,
		"SELECT "+riskCaseEventColumns+" FROM risk_case_events WHERE id = ?", id))
	if err != nil {
		if err == sql.ErrNoRows {
			return RiskCaseEvent{}, ErrNotFound
		}
		return RiskCaseEvent{}, fmt.Errorf("get risk case event: %w", err)
	}
	return item, nil
}

func (s *Store) ListRiskCaseEvents(ctx context.Context, filter RiskCaseEventFilter) (RiskCaseEventPage, error) {
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(riskCaseEventColumns)
	query.WriteString(" FROM risk_case_events WHERE 1=1")
	args := make([]any, 0, 4)
	if filter.CaseID > 0 {
		query.WriteString(" AND case_id = ?")
		args = append(args, filter.CaseID)
	}
	if strings.TrimSpace(filter.EventType) != "" {
		query.WriteString(" AND event_type = ?")
		args = append(args, strings.TrimSpace(filter.EventType))
	}
	if filter.BeforeID > 0 {
		query.WriteString(" AND id < ?")
		args = append(args, filter.BeforeID)
	}
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return RiskCaseEventPage{}, fmt.Errorf("list risk case events: %w", err)
	}
	defer rows.Close()
	items := make([]RiskCaseEvent, 0, limit+1)
	for rows.Next() {
		item, err := scanRiskCaseEvent(rows)
		if err != nil {
			return RiskCaseEventPage{}, fmt.Errorf("scan risk case event: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return RiskCaseEventPage{}, fmt.Errorf("iterate risk case events: %w", err)
	}
	items, cursor, more := pageResult(items, limit, func(item RiskCaseEvent) int64 { return item.ID })
	return RiskCaseEventPage{Items: items, NextCursor: cursor, HasMore: more}, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execQueryer interface {
	queryRower
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scanRiskCase(row rowScanner) (RiskCase, error) {
	var item RiskCase
	var openedAt, createdAt, updatedAt int64
	var closedAt sql.NullInt64
	err := row.Scan(&item.ID, &item.CaseKey, &item.Title, &item.SubjectType, &item.SubjectID,
		&item.Severity, &item.Status, &item.Assignee, &item.Summary, &openedAt,
		&closedAt, &createdAt, &updatedAt)
	if err != nil {
		return RiskCase{}, err
	}
	item.OpenedAt = fromDBTime(openedAt)
	item.CreatedAt = fromDBTime(createdAt)
	item.UpdatedAt = fromDBTime(updatedAt)
	if closedAt.Valid {
		value := fromDBTime(closedAt.Int64)
		item.ClosedAt = &value
	}
	return item, nil
}

func scanRiskCaseEvent(row rowScanner) (RiskCaseEvent, error) {
	var item RiskCaseEvent
	var details string
	var occurredAt, createdAt int64
	err := row.Scan(&item.ID, &item.CaseID, &item.EventType, &item.Actor, &details,
		&item.IdempotencyKey, &occurredAt, &createdAt)
	if err != nil {
		return RiskCaseEvent{}, err
	}
	if details != "" {
		item.DetailsJSON = json.RawMessage(details)
	}
	item.OccurredAt = fromDBTime(occurredAt)
	item.CreatedAt = fromDBTime(createdAt)
	return item, nil
}
