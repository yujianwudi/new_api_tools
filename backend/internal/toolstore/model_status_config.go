package toolstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxModelStatusConfigBytes = 64 << 10

type ModelStatusConfigVersion struct {
	Version         int64
	PreviousVersion *int64
	ChangedKey      string
	ConfigJSON      json.RawMessage
	Actor           string
	RequestID       string
	Reason          string
	OperationKey    string
	CreatedAt       time.Time
}

type ModelStatusConfigWriteInput struct {
	ChangedKey      string
	ConfigJSON      json.RawMessage
	ExpectedVersion *int64
	OperationKey    string
}

func (s *Store) GetLatestModelStatusConfig(ctx context.Context) (ModelStatusConfigVersion, error) {
	if s == nil || s.db == nil {
		return ModelStatusConfigVersion{}, ErrStoreClosed
	}
	return getLatestModelStatusConfig(ctx, s.db)
}

func (s *Store) GetModelStatusConfigVersion(ctx context.Context, version int64) (ModelStatusConfigVersion, error) {
	if s == nil || s.db == nil {
		return ModelStatusConfigVersion{}, ErrStoreClosed
	}
	if version <= 0 {
		return ModelStatusConfigVersion{}, fmt.Errorf("%w: model status config version must be positive", ErrInvalid)
	}
	return getModelStatusConfig(ctx, s.db, version)
}

func (s *Store) GetModelStatusConfigByOperationKey(ctx context.Context, operationKey string) (ModelStatusConfigVersion, error) {
	if s == nil || s.db == nil {
		return ModelStatusConfigVersion{}, ErrStoreClosed
	}
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" || len(operationKey) > 256 {
		return ModelStatusConfigVersion{}, fmt.Errorf("%w: invalid model status config operation key", ErrInvalid)
	}
	return getModelStatusConfigByOperationKey(ctx, s.db, operationKey)
}

// WriteModelStatusConfigAudited appends a full immutable configuration
// snapshot and its intent/outcome audit pair in one BEGIN IMMEDIATE
// transaction. Every caller must supply the version used to construct its full
// snapshot so unrelated concurrent changes cannot be overwritten.
func (s *Store) WriteModelStatusConfigAudited(
	ctx context.Context,
	input ModelStatusConfigWriteInput,
	auditInput OperationAuditInput,
) (ModelStatusConfigVersion, OperationAudit, bool, error) {
	if input.ExpectedVersion == nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config write requires expected version", ErrInvalid)
	}
	return s.writeModelStatusConfigAudited(ctx, input, auditInput, false)
}

// ImportLegacyModelStatusConfigAudited appends the one whole-document
// bootstrap used to migrate v0.6.0 Redis settings. Normal API writes cannot use
// the bootstrap changed_key, and the importer must always supply a CAS version.
func (s *Store) ImportLegacyModelStatusConfigAudited(
	ctx context.Context,
	input ModelStatusConfigWriteInput,
	auditInput OperationAuditInput,
) (ModelStatusConfigVersion, OperationAudit, bool, error) {
	input.ChangedKey = "bootstrap"
	if input.ExpectedVersion == nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: legacy model config import requires expected version", ErrInvalid)
	}
	return s.writeModelStatusConfigAudited(ctx, input, auditInput, true)
}

func (s *Store) writeModelStatusConfigAudited(
	ctx context.Context,
	input ModelStatusConfigWriteInput,
	auditInput OperationAuditInput,
	allowBootstrap bool,
) (ModelStatusConfigVersion, OperationAudit, bool, error) {
	if s == nil || s.db == nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, ErrStoreClosed
	}
	input.ChangedKey = strings.TrimSpace(input.ChangedKey)
	input.OperationKey = strings.TrimSpace(input.OperationKey)
	if !validModelStatusConfigKey(input.ChangedKey) || (input.ChangedKey == "bootstrap" && !allowBootstrap) ||
		input.OperationKey == "" || len(input.OperationKey) > 256 {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: invalid model status config write identity", ErrInvalid)
	}
	configJSON, err := normalizedJSON("model status config", input.ConfigJSON)
	if err != nil || len(configJSON) < 2 || len(configJSON) > maxModelStatusConfigBytes || configJSON[0] != '{' {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: invalid model status config document", ErrInvalid)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(configJSON, &object); err != nil || object == nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config must be a JSON object", ErrInvalid)
	}
	auditInput.Actor = strings.TrimSpace(auditInput.Actor)
	auditInput.RequestID = strings.TrimSpace(auditInput.RequestID)
	auditInput.Reason = strings.TrimSpace(auditInput.Reason)
	if auditInput.Actor == "" || len(auditInput.Actor) > 256 || auditInput.RequestID == "" ||
		len(auditInput.RequestID) > 128 || auditInput.Reason == "" || len(auditInput.Reason) > 512 {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: invalid model status config audit metadata", ErrInvalid)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if invoiceSQLiteContention(err) {
			return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config changed concurrently", ErrConflict)
		}
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("begin model status config write: %w", err)
	}

	if existing, replayErr := getModelStatusConfigByOperationKey(ctx, conn, input.OperationKey); replayErr == nil {
		if existing.ChangedKey != input.ChangedKey || existing.Actor != auditInput.Actor ||
			existing.RequestID != auditInput.RequestID || existing.Reason != auditInput.Reason ||
			!bytes.Equal(existing.ConfigJSON, configJSON) {
			return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config replay payload changed", ErrConflict)
		}
		outcome, auditErr := getOperationAuditByIdempotencyKey(ctx, conn, input.OperationKey+":outcome")
		if auditErr != nil {
			return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config replay audit is incomplete", ErrConflict)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return ModelStatusConfigVersion{}, OperationAudit{}, false, err
		}
		done = true
		return existing, outcome, true, nil
	} else if !errors.Is(replayErr, ErrNotFound) {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, replayErr
	}

	current, err := getLatestModelStatusConfig(ctx, conn)
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	if input.ExpectedVersion != nil && *input.ExpectedVersion != current.Version {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: model status config expected version %d, current version %d",
			ErrConflict, *input.ExpectedVersion, current.Version)
	}

	beforeJSON, err := json.Marshal(struct {
		Version int64           `json:"version"`
		Config  json.RawMessage `json:"config"`
	}{current.Version, current.ConfigJSON})
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	intent := auditInput
	actionPrefix := "model_status.config"
	if allowBootstrap {
		actionPrefix = "model_status.config.legacy_import"
	}
	intent.Action = actionPrefix + ".intent"
	intent.TargetType = "model_status_config"
	intent.TargetID = input.ChangedKey
	intent.BeforeJSON = beforeJSON
	intent.AfterJSON = nil
	intent.Status = OperationSucceeded
	intent.ErrorCode = ""
	intent.IdempotencyKey = input.OperationKey + ":intent"
	if _, err := s.appendOperationAudit(ctx, conn, intent); err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}

	now := s.clock().UTC().Truncate(time.Millisecond)
	result, err := conn.ExecContext(ctx, `INSERT INTO model_status_config_versions(
		previous_version, changed_key, config_json, actor, request_id, reason, operation_key, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, current.Version, input.ChangedKey, string(configJSON),
		auditInput.Actor, auditInput.RequestID, auditInput.Reason, input.OperationKey, dbTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("%w: duplicate model status config operation", ErrConflict)
		}
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("append model status config version: %w", err)
	}
	version, err := result.LastInsertId()
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	written, err := getModelStatusConfig(ctx, conn, version)
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	afterJSON, err := json.Marshal(struct {
		Version int64           `json:"version"`
		Config  json.RawMessage `json:"config"`
	}{written.Version, written.ConfigJSON})
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	outcomeInput := intent
	outcomeInput.Action = actionPrefix + ".outcome"
	outcomeInput.AfterJSON = afterJSON
	outcomeInput.IdempotencyKey = input.OperationKey + ":outcome"
	outcome, err := s.appendOperationAudit(ctx, conn, outcomeInput)
	if err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return ModelStatusConfigVersion{}, OperationAudit{}, false, fmt.Errorf("commit model status config write: %w", err)
	}
	done = true
	return written, outcome, false, nil
}

func validModelStatusConfigKey(value string) bool {
	switch value {
	case "bootstrap", "selected_models", "time_window", "theme", "refresh_interval",
		"sort_mode", "custom_order", "custom_groups", "site_title":
		return true
	default:
		return false
	}
}

func getLatestModelStatusConfig(ctx context.Context, queryer queryRower) (ModelStatusConfigVersion, error) {
	item, err := scanModelStatusConfig(queryer.QueryRowContext(ctx, `SELECT version, previous_version, changed_key,
		config_json, actor, request_id, reason, operation_key, created_at
		FROM model_status_config_versions ORDER BY version DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelStatusConfigVersion{}, ErrNotFound
	}
	return item, err
}

func getModelStatusConfig(ctx context.Context, queryer queryRower, version int64) (ModelStatusConfigVersion, error) {
	item, err := scanModelStatusConfig(queryer.QueryRowContext(ctx, `SELECT version, previous_version, changed_key,
		config_json, actor, request_id, reason, operation_key, created_at
		FROM model_status_config_versions WHERE version = ?`, version))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelStatusConfigVersion{}, ErrNotFound
	}
	return item, err
}

func getModelStatusConfigByOperationKey(ctx context.Context, queryer queryRower, key string) (ModelStatusConfigVersion, error) {
	item, err := scanModelStatusConfig(queryer.QueryRowContext(ctx, `SELECT version, previous_version, changed_key,
		config_json, actor, request_id, reason, operation_key, created_at
		FROM model_status_config_versions WHERE operation_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelStatusConfigVersion{}, ErrNotFound
	}
	return item, err
}

func scanModelStatusConfig(row rowScanner) (ModelStatusConfigVersion, error) {
	var item ModelStatusConfigVersion
	var previous sql.NullInt64
	var raw string
	var createdAt int64
	if err := row.Scan(&item.Version, &previous, &item.ChangedKey, &raw, &item.Actor,
		&item.RequestID, &item.Reason, &item.OperationKey, &createdAt); err != nil {
		return ModelStatusConfigVersion{}, err
	}
	if previous.Valid {
		value := previous.Int64
		item.PreviousVersion = &value
	}
	item.ConfigJSON = json.RawMessage(raw)
	item.CreatedAt = fromDBTime(createdAt)
	return item, nil
}
