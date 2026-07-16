package toolstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const supportNoteColumns = `id, subject_type, subject_id, author, body, visibility,
	COALESCE(idempotency_key, ''), created_at, updated_at, deleted_at`

func (s *Store) CreateSupportNote(ctx context.Context, input SupportNoteInput) (SupportNote, error) {
	return s.createSupportNote(ctx, s.db, input)
}

func (s *Store) createSupportNote(ctx context.Context, executor execQueryer, input SupportNoteInput) (SupportNote, error) {
	var err error
	if input.SubjectType, err = requireText("subject_type", input.SubjectType); err != nil {
		return SupportNote{}, err
	}
	if input.SubjectID, err = requireText("subject_id", input.SubjectID); err != nil {
		return SupportNote{}, err
	}
	if input.Author, err = requireText("author", input.Author); err != nil {
		return SupportNote{}, err
	}
	if input.Body, err = requireText("body", input.Body); err != nil {
		return SupportNote{}, err
	}
	if !validVisibility(input.Visibility) {
		return SupportNote{}, fmt.Errorf("%w: unsupported note visibility %q", ErrInvalid, input.Visibility)
	}
	now := s.clock()
	key := strings.TrimSpace(input.IdempotencyKey)
	result, err := executor.ExecContext(ctx, `INSERT INTO support_notes(
		subject_type, subject_id, author, body, visibility, idempotency_key,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING`,
		input.SubjectType, input.SubjectID, input.Author, input.Body, input.Visibility,
		optionalKey(key), dbTime(now), dbTime(now))
	if err != nil {
		return SupportNote{}, fmt.Errorf("create support note: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SupportNote{}, fmt.Errorf("read support note result: %w", err)
	}
	if rows == 0 {
		if key == "" {
			return SupportNote{}, fmt.Errorf("%w: support note insert was ignored", ErrConflict)
		}
		existing, getErr := getSupportNoteByIdempotencyKey(ctx, executor, key)
		if getErr != nil {
			return SupportNote{}, getErr
		}
		if !supportNoteMatchesInput(existing, input) {
			return SupportNote{}, fmt.Errorf("%w: idempotency key belongs to a different support note", ErrConflict)
		}
		return existing, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return SupportNote{}, fmt.Errorf("read support note id: %w", err)
	}
	return getSupportNote(ctx, executor, id)
}

func supportNoteMatchesInput(item SupportNote, input SupportNoteInput) bool {
	return item.DeletedAt == nil &&
		item.SubjectType == strings.TrimSpace(input.SubjectType) &&
		item.SubjectID == strings.TrimSpace(input.SubjectID) &&
		item.Author == strings.TrimSpace(input.Author) &&
		item.Body == strings.TrimSpace(input.Body) &&
		item.Visibility == input.Visibility &&
		item.IdempotencyKey == strings.TrimSpace(input.IdempotencyKey)
}

func (s *Store) GetSupportNote(ctx context.Context, id int64) (SupportNote, error) {
	if id <= 0 {
		return SupportNote{}, fmt.Errorf("%w: support note id must be positive", ErrInvalid)
	}
	return getSupportNote(ctx, s.db, id)
}

func getSupportNote(ctx context.Context, queryer queryRower, id int64) (SupportNote, error) {
	item, err := scanSupportNote(queryer.QueryRowContext(ctx,
		"SELECT "+supportNoteColumns+" FROM support_notes WHERE id = ?", id))
	if err != nil {
		if err == sql.ErrNoRows {
			return SupportNote{}, ErrNotFound
		}
		return SupportNote{}, fmt.Errorf("get support note: %w", err)
	}
	return item, nil
}

func (s *Store) GetSupportNoteByIdempotencyKey(ctx context.Context, key string) (SupportNote, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return SupportNote{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	return getSupportNoteByIdempotencyKey(ctx, s.db, key)
}

func getSupportNoteByIdempotencyKey(ctx context.Context, queryer queryRower, key string) (SupportNote, error) {
	item, err := scanSupportNote(queryer.QueryRowContext(ctx,
		"SELECT "+supportNoteColumns+" FROM support_notes WHERE idempotency_key = ?", key))
	if err != nil {
		if err == sql.ErrNoRows {
			return SupportNote{}, ErrNotFound
		}
		return SupportNote{}, fmt.Errorf("get support note by idempotency key: %w", err)
	}
	return item, nil
}

// UpdateSupportNote edits an active note without removing its original
// creation metadata. The surrounding handler should append an operation audit.
func (s *Store) UpdateSupportNote(ctx context.Context, update SupportNoteUpdate) (SupportNote, error) {
	return s.updateSupportNote(ctx, s.db, update)
}

func (s *Store) updateSupportNote(ctx context.Context, executor execQueryer, update SupportNoteUpdate) (SupportNote, error) {
	if update.ID <= 0 {
		return SupportNote{}, fmt.Errorf("%w: support note id must be positive", ErrInvalid)
	}
	var err error
	if update.Body, err = requireText("body", update.Body); err != nil {
		return SupportNote{}, err
	}
	if !validVisibility(update.Visibility) {
		return SupportNote{}, fmt.Errorf("%w: unsupported note visibility %q", ErrInvalid, update.Visibility)
	}
	result, err := executor.ExecContext(ctx, `UPDATE support_notes
		SET body = ?, visibility = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, update.Body, update.Visibility, dbTime(s.clock()), update.ID)
	if err != nil {
		return SupportNote{}, fmt.Errorf("update support note: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SupportNote{}, fmt.Errorf("read support note update result: %w", err)
	}
	if rows == 0 {
		return SupportNote{}, ErrNotFound
	}
	return getSupportNote(ctx, executor, update.ID)
}

// DeleteSupportNote is intentionally a soft delete so support history remains
// available to auditors.
func (s *Store) DeleteSupportNote(ctx context.Context, id int64) (SupportNote, error) {
	return s.deleteSupportNote(ctx, s.db, id)
}

func (s *Store) deleteSupportNote(ctx context.Context, executor execQueryer, id int64) (SupportNote, error) {
	if id <= 0 {
		return SupportNote{}, fmt.Errorf("%w: support note id must be positive", ErrInvalid)
	}
	now := dbTime(s.clock())
	result, err := executor.ExecContext(ctx, `UPDATE support_notes
		SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return SupportNote{}, fmt.Errorf("delete support note: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SupportNote{}, fmt.Errorf("read support note delete result: %w", err)
	}
	if rows == 0 {
		return SupportNote{}, ErrNotFound
	}
	return getSupportNote(ctx, executor, id)
}

func (s *Store) ListSupportNotes(ctx context.Context, filter SupportNoteFilter) (SupportNotePage, error) {
	limit := pageLimit(filter.Limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(supportNoteColumns)
	query.WriteString(" FROM support_notes WHERE 1=1")
	args := make([]any, 0, 7)
	add := func(column, value string) {
		if strings.TrimSpace(value) != "" {
			query.WriteString(" AND " + column + " = ?")
			args = append(args, strings.TrimSpace(value))
		}
	}
	add("subject_type", filter.SubjectType)
	add("subject_id", filter.SubjectID)
	add("author", filter.Author)
	if filter.Visibility != "" {
		if !validVisibility(filter.Visibility) {
			return SupportNotePage{}, fmt.Errorf("%w: unsupported note visibility %q", ErrInvalid, filter.Visibility)
		}
		add("visibility", string(filter.Visibility))
	}
	if !filter.IncludeDeleted {
		query.WriteString(" AND deleted_at IS NULL")
	}
	if filter.BeforeID > 0 {
		query.WriteString(" AND id < ?")
		args = append(args, filter.BeforeID)
	}
	query.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return SupportNotePage{}, fmt.Errorf("list support notes: %w", err)
	}
	defer rows.Close()
	items := make([]SupportNote, 0, limit+1)
	for rows.Next() {
		item, err := scanSupportNote(rows)
		if err != nil {
			return SupportNotePage{}, fmt.Errorf("scan support note: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return SupportNotePage{}, fmt.Errorf("iterate support notes: %w", err)
	}
	items, cursor, more := pageResult(items, limit, func(item SupportNote) int64 { return item.ID })
	return SupportNotePage{Items: items, NextCursor: cursor, HasMore: more}, nil
}

func scanSupportNote(row rowScanner) (SupportNote, error) {
	var item SupportNote
	var createdAt, updatedAt int64
	var deletedAt sql.NullInt64
	err := row.Scan(&item.ID, &item.SubjectType, &item.SubjectID, &item.Author, &item.Body,
		&item.Visibility, &item.IdempotencyKey, &createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return SupportNote{}, err
	}
	item.CreatedAt = fromDBTime(createdAt)
	item.UpdatedAt = fromDBTime(updatedAt)
	if deletedAt.Valid {
		value := fromDBTime(deletedAt.Int64)
		item.DeletedAt = &value
	}
	return item, nil
}
