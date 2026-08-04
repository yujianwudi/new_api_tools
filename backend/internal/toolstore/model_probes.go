package toolstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrModelProbeBudgetExceeded = errors.New("toolstore: model probe daily budget exceeded")

type ModelProbeRun struct {
	ID                 int64      `json:"id"`
	RunKey             string     `json:"run_key"`
	RequestFingerprint string     `json:"-"`
	TriggerKind        string     `json:"trigger_kind"`
	Actor              string     `json:"actor"`
	Status             string     `json:"status"`
	RequestedCount     int        `json:"requested_count"`
	AttemptedCount     int        `json:"attempted_count"`
	SuccessCount       int        `json:"success_count"`
	FailureCount       int        `json:"failure_count"`
	SkippedCount       int        `json:"skipped_count"`
	ErrorCode          string     `json:"error_code,omitempty"`
	ErrorMessage       string     `json:"error_message,omitempty"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type ModelProbeRunInput struct {
	RunKey             string
	RequestFingerprint string
	TriggerKind        string
	Actor              string
	RequestedCount     int
	StartedAt          time.Time
}

type ModelProbeRunFinish struct {
	ID             int64
	Status         string
	AttemptedCount int
	SuccessCount   int
	FailureCount   int
	SkippedCount   int
	ErrorCode      string
	ErrorMessage   string
	FinishedAt     time.Time
}

type ModelProbeAttempt struct {
	ID                  int64     `json:"id"`
	RunID               int64     `json:"run_id"`
	ModelName           string    `json:"model_name"`
	Capability          string    `json:"capability"`
	Endpoint            string    `json:"endpoint"`
	Outcome             string    `json:"outcome"`
	ProtocolSuccess     bool      `json:"protocol_success"`
	SemanticSuccess     *bool     `json:"semantic_success"`
	HTTPStatus          int       `json:"http_status"`
	HeaderLatencyMS     *int64    `json:"header_latency_ms"`
	FirstTokenLatencyMS *int64    `json:"first_token_latency_ms"`
	TotalLatencyMS      int64     `json:"total_latency_ms"`
	ErrorCode           string    `json:"error_code,omitempty"`
	ErrorMessage        string    `json:"error_message,omitempty"`
	ResponseSHA256      string    `json:"response_sha256,omitempty"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	CreatedAt           time.Time `json:"created_at"`
}

type ModelProbeAttemptInput struct {
	LifecycleID         int64
	RunID               int64
	ModelName           string
	Capability          string
	Endpoint            string
	Outcome             string
	ProtocolSuccess     bool
	SemanticSuccess     *bool
	HTTPStatus          int
	HeaderLatencyMS     *int64
	FirstTokenLatencyMS *int64
	TotalLatencyMS      int64
	ErrorCode           string
	ErrorMessage        string
	ResponseSHA256      string
	StartedAt           time.Time
	FinishedAt          time.Time
}

// ModelProbeAttemptPlan is the durable pre-flight decision for one requested
// model. Supported capabilities reserve budget; unsupported capabilities are
// recorded as skipped without consuming the network budget.
type ModelProbeAttemptPlan struct {
	ModelName  string
	Capability string
}

// ModelProbeAttemptLifecycle is the durable source of truth for whether a
// probe could have consumed upstream budget. Terminal uncertain rows continue
// to consume budget because the process cannot prove that no request was sent.
type ModelProbeAttemptLifecycle struct {
	ID             int64      `json:"id"`
	RunID          int64      `json:"run_id"`
	AttemptIndex   int        `json:"attempt_index"`
	ModelName      string     `json:"model_name"`
	Capability     string     `json:"capability"`
	LifecycleState string     `json:"lifecycle_state"`
	BudgetDay      time.Time  `json:"budget_day"`
	ReservedAt     *time.Time `json:"reserved_at,omitempty"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	AttemptID      *int64     `json:"attempt_id,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type ModelProbeSummary struct {
	ModelName              string             `json:"model_name"`
	AttemptCount           int64              `json:"attempt_count_24h"`
	SuccessCount           int64              `json:"success_count_24h"`
	FailureCount           int64              `json:"failure_count_24h"`
	SkippedCount           int64              `json:"skipped_count_24h"`
	AverageHeaderLatencyMS *int64             `json:"average_header_latency_ms"`
	AverageFirstTokenMS    *int64             `json:"average_first_token_latency_ms"`
	AverageTotalLatencyMS  *int64             `json:"average_total_latency_ms"`
	Latest                 *ModelProbeAttempt `json:"latest,omitempty"`
}

func (s *Store) CreateModelProbeRun(ctx context.Context, input ModelProbeRunInput) (ModelProbeRun, bool, error) {
	if s == nil || s.db == nil {
		return ModelProbeRun{}, false, ErrStoreClosed
	}
	input.RunKey = strings.TrimSpace(input.RunKey)
	input.RequestFingerprint = strings.ToLower(strings.TrimSpace(input.RequestFingerprint))
	input.TriggerKind = strings.TrimSpace(input.TriggerKind)
	input.Actor = strings.TrimSpace(input.Actor)
	if len(input.RunKey) < 8 || len(input.RunKey) > 256 || !validSHA256(input.RequestFingerprint) ||
		(input.TriggerKind != "scheduled" && input.TriggerKind != "manual") ||
		input.Actor == "" || len(input.Actor) > 256 || input.RequestedCount < 0 {
		return ModelProbeRun{}, false, ErrInvalid
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = s.clock()
	}
	now := s.clock()
	result, err := s.db.ExecContext(ctx, `INSERT INTO model_probe_runs(
		run_key, request_fingerprint, trigger_kind, actor, status, requested_count,
		started_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?)`,
		input.RunKey, input.RequestFingerprint, input.TriggerKind, input.Actor,
		input.RequestedCount, dbTime(input.StartedAt), dbTime(now), dbTime(now))
	if err != nil {
		existing, getErr := s.GetModelProbeRunByKey(ctx, input.RunKey)
		if getErr == nil {
			if existing.RequestFingerprint != input.RequestFingerprint || existing.TriggerKind != input.TriggerKind ||
				existing.Actor != input.Actor || existing.RequestedCount != input.RequestedCount {
				return ModelProbeRun{}, false, ErrConflict
			}
			return existing, true, nil
		}
		return ModelProbeRun{}, false, fmt.Errorf("create model probe run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelProbeRun{}, false, fmt.Errorf("read model probe run id: %w", err)
	}
	created, err := s.getModelProbeRun(ctx, id)
	return created, false, err
}

// CreateModelProbeRunWithBudget atomically checks the UTC-day budget, creates
// the run, and persists one lifecycle row per requested model. BEGIN IMMEDIATE
// is intentional: independent Store instances must serialize the budget read
// with the reservation writes instead of racing through deferred transactions.
func (s *Store) CreateModelProbeRunWithBudget(
	ctx context.Context,
	input ModelProbeRunInput,
	plans []ModelProbeAttemptPlan,
	budgetDay time.Time,
	dailyBudget int,
) (ModelProbeRun, []ModelProbeAttemptLifecycle, bool, error) {
	if s == nil || s.db == nil {
		return ModelProbeRun{}, nil, false, ErrStoreClosed
	}
	input.RunKey = strings.TrimSpace(input.RunKey)
	input.RequestFingerprint = strings.ToLower(strings.TrimSpace(input.RequestFingerprint))
	input.TriggerKind = strings.TrimSpace(input.TriggerKind)
	input.Actor = strings.TrimSpace(input.Actor)
	if input.StartedAt.IsZero() {
		input.StartedAt = s.clock()
	}
	if len(input.RunKey) < 8 || len(input.RunKey) > 256 || !validSHA256(input.RequestFingerprint) ||
		(input.TriggerKind != "scheduled" && input.TriggerKind != "manual") ||
		input.Actor == "" || len(input.Actor) > 256 || input.RequestedCount < 0 ||
		len(plans) != input.RequestedCount || dailyBudget < 0 {
		return ModelProbeRun{}, nil, false, ErrInvalid
	}
	for index := range plans {
		plans[index].ModelName = strings.TrimSpace(plans[index].ModelName)
		plans[index].Capability = strings.TrimSpace(plans[index].Capability)
		if plans[index].ModelName == "" || len(plans[index].ModelName) > 256 ||
			!validProbeCapability(plans[index].Capability) {
			return ModelProbeRun{}, nil, false, ErrInvalid
		}
	}
	startedBudgetDay := startOfProbeBudgetDay(input.StartedAt)
	budgetDay = startOfProbeBudgetDay(budgetDay)
	if budgetDay.IsZero() {
		budgetDay = startedBudgetDay
	}
	if !budgetDay.Equal(startedBudgetDay) {
		return ModelProbeRun{}, nil, false, ErrInvalid
	}
	networkBound := 0
	for _, plan := range plans {
		if plan.Capability != "unsupported" {
			networkBound++
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("acquire model probe budget connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("begin immediate model probe budget reservation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var existingID int64
	var existingFingerprint, existingTrigger, existingActor string
	var existingRequested int
	err = conn.QueryRowContext(ctx, `SELECT id, request_fingerprint, trigger_kind, actor, requested_count
		FROM model_probe_runs WHERE run_key = ?`, input.RunKey).
		Scan(&existingID, &existingFingerprint, &existingTrigger, &existingActor, &existingRequested)
	if err == nil {
		if existingFingerprint != input.RequestFingerprint || existingTrigger != input.TriggerKind ||
			existingActor != input.Actor || existingRequested != input.RequestedCount {
			return ModelProbeRun{}, nil, false, ErrConflict
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return ModelProbeRun{}, nil, false, fmt.Errorf("commit replayed model probe budget reservation: %w", err)
		}
		committed = true
		if err := conn.Close(); err != nil {
			return ModelProbeRun{}, nil, false, fmt.Errorf("release replayed model probe budget connection: %w", err)
		}
		run, getErr := s.getModelProbeRun(ctx, existingID)
		if getErr != nil {
			return ModelProbeRun{}, nil, false, getErr
		}
		lifecycles, getErr := s.ListModelProbeAttemptLifecycles(ctx, existingID)
		return run, lifecycles, true, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ModelProbeRun{}, nil, false, fmt.Errorf("inspect model probe budget replay: %w", err)
	}

	var used int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_probe_attempt_lifecycles
		WHERE budget_day = ? AND lifecycle_state IN ('reserved', 'sent', 'settled', 'uncertain')`,
		dbTime(budgetDay)).Scan(&used); err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("count model probe budget reservations: %w", err)
	}
	if used+networkBound > dailyBudget {
		return ModelProbeRun{}, nil, false, ErrModelProbeBudgetExceeded
	}

	now := s.clock()
	result, err := conn.ExecContext(ctx, `INSERT INTO model_probe_runs(
		run_key, request_fingerprint, trigger_kind, actor, status, requested_count,
		started_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?)`,
		input.RunKey, input.RequestFingerprint, input.TriggerKind, input.Actor,
		input.RequestedCount, dbTime(input.StartedAt), dbTime(now), dbTime(now))
	if err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("create budgeted model probe run: %w", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("read budgeted model probe run id: %w", err)
	}
	for index, plan := range plans {
		state := "reserved"
		var reservedAt, finishedAt any = dbTime(input.StartedAt), nil
		errorCode, errorMessage := "", ""
		if plan.Capability == "unsupported" {
			state = "skipped"
			reservedAt = nil
			finishedAt = dbTime(input.StartedAt)
			errorCode = "unsupported_capability"
			errorMessage = "Model capability is intentionally not probed"
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_probe_attempt_lifecycles(
			run_id, attempt_index, model_name, capability, lifecycle_state, budget_day,
			reserved_at, sent_at, finished_at, error_code, error_message, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
			runID, index, plan.ModelName, plan.Capability, state, dbTime(budgetDay),
			reservedAt, finishedAt, errorCode, errorMessage, dbTime(now), dbTime(now)); err != nil {
			return ModelProbeRun{}, nil, false, fmt.Errorf("reserve model probe attempt %d: %w", index, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("commit model probe budget reservation: %w", err)
	}
	committed = true
	if err := conn.Close(); err != nil {
		return ModelProbeRun{}, nil, false, fmt.Errorf("release model probe budget connection: %w", err)
	}
	run, err := s.getModelProbeRun(ctx, runID)
	if err != nil {
		return ModelProbeRun{}, nil, false, err
	}
	lifecycles, err := s.ListModelProbeAttemptLifecycles(ctx, runID)
	return run, lifecycles, false, err
}

func (s *Store) GetModelProbeRunByKey(ctx context.Context, runKey string) (ModelProbeRun, error) {
	if s == nil || s.db == nil {
		return ModelProbeRun{}, ErrStoreClosed
	}
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM model_probe_runs WHERE run_key = ?", strings.TrimSpace(runKey)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProbeRun{}, ErrNotFound
	}
	if err != nil {
		return ModelProbeRun{}, fmt.Errorf("find model probe run: %w", err)
	}
	return s.getModelProbeRun(ctx, id)
}

func (s *Store) LatestModelProbeRun(ctx context.Context) (*ModelProbeRun, error) {
	if s == nil || s.db == nil {
		return nil, ErrStoreClosed
	}
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM model_probe_runs ORDER BY id DESC LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find latest model probe run: %w", err)
	}
	run, err := s.getModelProbeRun(ctx, id)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) getModelProbeRun(ctx context.Context, id int64) (ModelProbeRun, error) {
	var run ModelProbeRun
	var startedAt, createdAt, updatedAt int64
	var finishedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, run_key, request_fingerprint, trigger_kind, actor, status,
		requested_count, attempted_count, success_count, failure_count, skipped_count,
		error_code, error_message, started_at, finished_at, created_at, updated_at
		FROM model_probe_runs WHERE id = ?`, id).Scan(
		&run.ID, &run.RunKey, &run.RequestFingerprint, &run.TriggerKind, &run.Actor, &run.Status,
		&run.RequestedCount, &run.AttemptedCount, &run.SuccessCount, &run.FailureCount, &run.SkippedCount,
		&run.ErrorCode, &run.ErrorMessage, &startedAt, &finishedAt, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProbeRun{}, ErrNotFound
	}
	if err != nil {
		return ModelProbeRun{}, fmt.Errorf("read model probe run: %w", err)
	}
	run.StartedAt = fromDBTime(startedAt)
	run.CreatedAt = fromDBTime(createdAt)
	run.UpdatedAt = fromDBTime(updatedAt)
	if finishedAt.Valid {
		value := fromDBTime(finishedAt.Int64)
		run.FinishedAt = &value
	}
	return run, nil
}

func (s *Store) ListModelProbeAttemptLifecycles(ctx context.Context, runID int64) ([]ModelProbeAttemptLifecycle, error) {
	if s == nil || s.db == nil {
		return nil, ErrStoreClosed
	}
	if runID <= 0 {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, attempt_index, model_name, capability,
		lifecycle_state, budget_day, reserved_at, sent_at, finished_at, attempt_id,
		error_code, error_message, created_at, updated_at
		FROM model_probe_attempt_lifecycles WHERE run_id = ? ORDER BY attempt_index`, runID)
	if err != nil {
		return nil, fmt.Errorf("list model probe attempt lifecycles: %w", err)
	}
	defer rows.Close()
	result := make([]ModelProbeAttemptLifecycle, 0)
	for rows.Next() {
		item, err := scanModelProbeAttemptLifecycle(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model probe attempt lifecycles: %w", err)
	}
	return result, nil
}

func (s *Store) getModelProbeAttemptLifecycle(ctx context.Context, id int64) (ModelProbeAttemptLifecycle, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, run_id, attempt_index, model_name, capability,
		lifecycle_state, budget_day, reserved_at, sent_at, finished_at, attempt_id,
		error_code, error_message, created_at, updated_at
		FROM model_probe_attempt_lifecycles WHERE id = ?`, id)
	item, err := scanModelProbeAttemptLifecycle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProbeAttemptLifecycle{}, ErrNotFound
	}
	return item, err
}

// MarkModelProbeAttemptSent is the final durable gate before network I/O. A
// caller must not send when this transition fails.
func (s *Store) MarkModelProbeAttemptSent(ctx context.Context, id int64, sentAt time.Time) (ModelProbeAttemptLifecycle, error) {
	if s == nil || s.db == nil {
		return ModelProbeAttemptLifecycle{}, ErrStoreClosed
	}
	if id <= 0 {
		return ModelProbeAttemptLifecycle{}, ErrInvalid
	}
	if sentAt.IsZero() {
		sentAt = s.clock()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
		lifecycle_state = 'sent', sent_at = MAX(?, reserved_at), updated_at = MAX(?, updated_at)
		WHERE id = ? AND lifecycle_state = 'reserved'`, dbTime(sentAt), dbTime(s.clock()), id)
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("mark model probe attempt sent: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("read sent model probe attempt count: %w", err)
	}
	if rows != 1 {
		return ModelProbeAttemptLifecycle{}, ErrConflict
	}
	return s.getModelProbeAttemptLifecycle(ctx, id)
}

// SkipReservedModelProbeAttempt releases budget only while the attempt is
// provably pre-send. A sent attempt can never transition to skipped.
func (s *Store) SkipReservedModelProbeAttempt(ctx context.Context, id int64, code, message string, finishedAt time.Time) (ModelProbeAttemptLifecycle, error) {
	if s == nil || s.db == nil {
		return ModelProbeAttemptLifecycle{}, ErrStoreClosed
	}
	code = strings.TrimSpace(code)
	message = truncateText(message, 512)
	if id <= 0 || len(code) > 64 {
		return ModelProbeAttemptLifecycle{}, ErrInvalid
	}
	if finishedAt.IsZero() {
		finishedAt = s.clock()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
		lifecycle_state = 'skipped', finished_at = MAX(?, reserved_at),
		error_code = ?, error_message = ?, updated_at = MAX(?, updated_at)
		WHERE id = ? AND lifecycle_state = 'reserved'`,
		dbTime(finishedAt), code, message, dbTime(s.clock()), id)
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("skip reserved model probe attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("read skipped model probe attempt count: %w", err)
	}
	if rows != 1 {
		return ModelProbeAttemptLifecycle{}, ErrConflict
	}
	return s.getModelProbeAttemptLifecycle(ctx, id)
}

// MarkModelProbeAttemptUncertain conservatively consumes budget when the
// caller cannot prove whether a reserved/sent request completed.
func (s *Store) MarkModelProbeAttemptUncertain(ctx context.Context, id int64, code, message string, finishedAt time.Time) (ModelProbeAttemptLifecycle, error) {
	if s == nil || s.db == nil {
		return ModelProbeAttemptLifecycle{}, ErrStoreClosed
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = "probe_result_uncertain"
	}
	message = truncateText(message, 512)
	if id <= 0 || len(code) > 64 {
		return ModelProbeAttemptLifecycle{}, ErrInvalid
	}
	if finishedAt.IsZero() {
		finishedAt = s.clock()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
		lifecycle_state = 'uncertain', finished_at = MAX(?, COALESCE(sent_at, reserved_at)),
		error_code = ?, error_message = ?, updated_at = MAX(?, updated_at)
		WHERE id = ? AND lifecycle_state IN ('reserved', 'sent')`,
		dbTime(finishedAt), code, message, dbTime(s.clock()), id)
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("mark model probe attempt uncertain: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("read uncertain model probe attempt count: %w", err)
	}
	if rows != 1 {
		return ModelProbeAttemptLifecycle{}, ErrConflict
	}
	return s.getModelProbeAttemptLifecycle(ctx, id)
}

func (s *Store) CountModelProbeBudgetUsed(ctx context.Context, budgetDay time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrStoreClosed
	}
	budgetDay = startOfProbeBudgetDay(budgetDay)
	if budgetDay.IsZero() {
		return 0, ErrInvalid
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_probe_attempt_lifecycles
		WHERE budget_day = ? AND lifecycle_state IN ('reserved', 'sent', 'settled', 'uncertain')`,
		dbTime(budgetDay)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count model probe budget used: %w", err)
	}
	return count, nil
}

func (s *Store) FinishModelProbeRun(ctx context.Context, input ModelProbeRunFinish) (ModelProbeRun, error) {
	if s == nil || s.db == nil {
		return ModelProbeRun{}, ErrStoreClosed
	}
	if input.ID <= 0 || !validProbeRunStatus(input.Status) || input.Status == "running" ||
		input.AttemptedCount < 0 || input.SuccessCount < 0 || input.FailureCount < 0 || input.SkippedCount < 0 ||
		input.AttemptedCount != input.SuccessCount+input.FailureCount+input.SkippedCount ||
		len(input.ErrorCode) > 64 || len(input.ErrorMessage) > 512 {
		return ModelProbeRun{}, ErrInvalid
	}
	if input.FinishedAt.IsZero() {
		input.FinishedAt = s.clock()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE model_probe_runs SET status = ?, attempted_count = ?,
		success_count = ?, failure_count = ?, skipped_count = ?, error_code = ?, error_message = ?,
		finished_at = ?, updated_at = ? WHERE id = ? AND status = 'running'`,
		input.Status, input.AttemptedCount, input.SuccessCount, input.FailureCount, input.SkippedCount,
		strings.TrimSpace(input.ErrorCode), truncateText(input.ErrorMessage, 512),
		dbTime(input.FinishedAt), dbTime(s.clock()), input.ID)
	if err != nil {
		return ModelProbeRun{}, fmt.Errorf("finish model probe run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ModelProbeRun{}, fmt.Errorf("read finished model probe run count: %w", err)
	}
	if rows != 1 {
		return ModelProbeRun{}, ErrConflict
	}
	return s.getModelProbeRun(ctx, input.ID)
}

// CancelInterruptedModelProbeRuns reconciles runs older than staleBefore. The
// caller must choose a cutoff that exceeds the maximum legitimate run duration
// so starting a second live instance cannot cancel the first instance's work.
// Every reserved/sent row becomes terminal uncertain before the run is
// cancelled, so delayed crash recovery never releases possibly consumed budget.
func (s *Store) CancelInterruptedModelProbeRuns(ctx context.Context, staleBefore time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, ErrStoreClosed
	}
	if staleBefore.IsZero() {
		return 0, ErrInvalid
	}
	finishedAt := s.clock()
	if finishedAt.Before(staleBefore) {
		finishedAt = staleBefore
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin interrupted model probe recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
		lifecycle_state = 'uncertain',
		finished_at = MAX(?, COALESCE(sent_at, reserved_at)),
		error_code = 'process_restarted',
		error_message = 'Probe attempt state was uncertain after process restart',
		updated_at = MAX(?, updated_at)
		WHERE lifecycle_state IN ('reserved', 'sent') AND run_id IN (
			SELECT id FROM model_probe_runs WHERE status = 'running' AND started_at < ?
		)`, dbTime(finishedAt), dbTime(finishedAt), dbTime(staleBefore)); err != nil {
		return 0, fmt.Errorf("reconcile interrupted model probe attempts: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_probe_runs SET
		status = 'cancelled',
		attempted_count = (SELECT COUNT(*) FROM model_probe_attempts WHERE run_id = model_probe_runs.id),
		success_count = (SELECT COUNT(*) FROM model_probe_attempts WHERE run_id = model_probe_runs.id AND outcome = 'success'),
		failure_count = (SELECT COUNT(*) FROM model_probe_attempts WHERE run_id = model_probe_runs.id AND outcome = 'failure'),
		skipped_count = (SELECT COUNT(*) FROM model_probe_attempts WHERE run_id = model_probe_runs.id AND outcome = 'skipped'),
		error_code = 'process_restarted',
		error_message = 'Probe run was interrupted before the current process started',
		finished_at = ?, updated_at = ?
		WHERE status = 'running' AND started_at < ?`,
		dbTime(finishedAt), dbTime(finishedAt), dbTime(staleBefore))
	if err != nil {
		return 0, fmt.Errorf("cancel interrupted model probe runs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read interrupted model probe run count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit interrupted model probe recovery: %w", err)
	}
	return count, nil
}

func (s *Store) AppendModelProbeAttempt(ctx context.Context, input ModelProbeAttemptInput) (ModelProbeAttempt, error) {
	if s == nil || s.db == nil {
		return ModelProbeAttempt{}, ErrStoreClosed
	}
	input.ModelName = strings.TrimSpace(input.ModelName)
	input.Capability = strings.TrimSpace(input.Capability)
	input.Endpoint = strings.TrimSpace(input.Endpoint)
	input.Outcome = strings.TrimSpace(input.Outcome)
	input.ErrorCode = strings.TrimSpace(input.ErrorCode)
	input.ErrorMessage = truncateText(input.ErrorMessage, 512)
	input.ResponseSHA256 = strings.ToLower(strings.TrimSpace(input.ResponseSHA256))
	if input.LifecycleID < 0 || input.RunID <= 0 || input.ModelName == "" || len(input.ModelName) > 256 ||
		!validProbeCapability(input.Capability) || len(input.Endpoint) > 128 ||
		!validProbeOutcome(input.Outcome) || input.HTTPStatus < 0 || input.HTTPStatus > 599 ||
		input.TotalLatencyMS < 0 || len(input.ErrorCode) > 64 ||
		(input.ResponseSHA256 != "" && !validSHA256(input.ResponseSHA256)) ||
		input.StartedAt.IsZero() || input.FinishedAt.Before(input.StartedAt) {
		return ModelProbeAttempt{}, ErrInvalid
	}
	if input.Outcome == "success" && (!input.ProtocolSuccess || input.SemanticSuccess == nil || !*input.SemanticSuccess || input.ErrorCode != "") {
		return ModelProbeAttempt{}, ErrInvalid
	}
	if input.Outcome == "skipped" && input.ProtocolSuccess {
		return ModelProbeAttempt{}, ErrInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("begin model probe attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if input.LifecycleID > 0 {
		var lifecycleRunID int64
		var lifecycleModel, lifecycleCapability, lifecycleState string
		var existingAttempt sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT run_id, model_name, capability, lifecycle_state, attempt_id
			FROM model_probe_attempt_lifecycles WHERE id = ?`, input.LifecycleID).
			Scan(&lifecycleRunID, &lifecycleModel, &lifecycleCapability, &lifecycleState, &existingAttempt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ModelProbeAttempt{}, ErrNotFound
			}
			return ModelProbeAttempt{}, fmt.Errorf("read model probe attempt lifecycle: %w", err)
		}
		wantedState := "sent"
		if input.Outcome == "skipped" {
			wantedState = "skipped"
		}
		if lifecycleRunID != input.RunID || lifecycleModel != input.ModelName ||
			lifecycleCapability != input.Capability || lifecycleState != wantedState || existingAttempt.Valid {
			return ModelProbeAttempt{}, ErrConflict
		}
	}
	now := s.clock()
	result, err := tx.ExecContext(ctx, `INSERT INTO model_probe_attempts(
		run_id, model_name, capability, endpoint, outcome, protocol_success, semantic_success,
		http_status, header_latency_ms, first_token_latency_ms, total_latency_ms,
		error_code, error_message, response_sha256, started_at, finished_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.RunID, input.ModelName, input.Capability, input.Endpoint, input.Outcome,
		boolInt(input.ProtocolSuccess), nullableBoolInt(input.SemanticSuccess), input.HTTPStatus,
		nullableInt64(input.HeaderLatencyMS), nullableInt64(input.FirstTokenLatencyMS), input.TotalLatencyMS,
		input.ErrorCode, input.ErrorMessage, input.ResponseSHA256, dbTime(input.StartedAt),
		dbTime(input.FinishedAt), dbTime(now))
	if err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("insert model probe attempt: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("read model probe attempt id: %w", err)
	}

	bucket := input.StartedAt.UTC().Truncate(time.Hour)
	success, failure, skipped := 0, 0, 0
	switch input.Outcome {
	case "success":
		success = 1
	case "failure":
		failure = 1
	case "skipped":
		skipped = 1
	}
	headerValue, headerSamples := int64(0), 0
	if input.HeaderLatencyMS != nil {
		headerValue, headerSamples = *input.HeaderLatencyMS, 1
	}
	firstValue, firstSamples := int64(0), 0
	if input.FirstTokenLatencyMS != nil {
		firstValue, firstSamples = *input.FirstTokenLatencyMS, 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO model_probe_rollups(
		bucket_start, model_name, capability, attempt_count, success_count, failure_count, skipped_count,
		header_latency_sum_ms, header_latency_samples, first_token_latency_sum_ms,
		first_token_latency_samples, total_latency_sum_ms, last_attempt_at
	) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(bucket_start, model_name, capability) DO UPDATE SET
		attempt_count = attempt_count + 1,
		success_count = success_count + excluded.success_count,
		failure_count = failure_count + excluded.failure_count,
		skipped_count = skipped_count + excluded.skipped_count,
		header_latency_sum_ms = header_latency_sum_ms + excluded.header_latency_sum_ms,
		header_latency_samples = header_latency_samples + excluded.header_latency_samples,
		first_token_latency_sum_ms = first_token_latency_sum_ms + excluded.first_token_latency_sum_ms,
		first_token_latency_samples = first_token_latency_samples + excluded.first_token_latency_samples,
		total_latency_sum_ms = total_latency_sum_ms + excluded.total_latency_sum_ms,
		last_attempt_at = MAX(last_attempt_at, excluded.last_attempt_at)`,
		dbTime(bucket), input.ModelName, input.Capability, success, failure, skipped,
		headerValue, headerSamples, firstValue, firstSamples, input.TotalLatencyMS, dbTime(input.FinishedAt))
	if err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("roll up model probe attempt: %w", err)
	}
	if input.LifecycleID > 0 {
		var lifecycleResult sql.Result
		if input.Outcome == "skipped" {
			lifecycleResult, err = tx.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
				attempt_id = ?, updated_at = MAX(?, updated_at)
				WHERE id = ? AND lifecycle_state = 'skipped' AND attempt_id IS NULL`,
				id, dbTime(now), input.LifecycleID)
		} else {
			lifecycleResult, err = tx.ExecContext(ctx, `UPDATE model_probe_attempt_lifecycles SET
				lifecycle_state = 'settled', finished_at = ?, attempt_id = ?,
				error_code = ?, error_message = ?, updated_at = MAX(?, updated_at)
				WHERE id = ? AND lifecycle_state = 'sent' AND attempt_id IS NULL`,
				dbTime(input.FinishedAt), id, input.ErrorCode, input.ErrorMessage,
				dbTime(now), input.LifecycleID)
		}
		if err != nil {
			return ModelProbeAttempt{}, fmt.Errorf("settle model probe attempt lifecycle: %w", err)
		}
		rows, err := lifecycleResult.RowsAffected()
		if err != nil {
			return ModelProbeAttempt{}, fmt.Errorf("read settled model probe attempt lifecycle count: %w", err)
		}
		if rows != 1 {
			return ModelProbeAttempt{}, ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("commit model probe attempt: %w", err)
	}
	return s.getModelProbeAttempt(ctx, id)
}

func (s *Store) CountModelProbeAttemptsSince(ctx context.Context, since time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrStoreClosed
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM model_probe_attempts WHERE started_at >= ?", dbTime(since)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count model probe attempts: %w", err)
	}
	return count, nil
}

func (s *Store) ListModelProbeSummaries(ctx context.Context, models []string, since time.Time) ([]ModelProbeSummary, error) {
	if s == nil || s.db == nil {
		return nil, ErrStoreClosed
	}
	models = uniqueProbeModels(models)
	if len(models) == 0 {
		return []ModelProbeSummary{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(models)), ",")
	args := make([]any, 0, len(models)+1)
	args = append(args, dbTime(since.UTC().Truncate(time.Hour)))
	for _, model := range models {
		args = append(args, model)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT model_name,
		SUM(attempt_count), SUM(success_count), SUM(failure_count), SUM(skipped_count),
		SUM(header_latency_sum_ms), SUM(header_latency_samples),
		SUM(first_token_latency_sum_ms), SUM(first_token_latency_samples), SUM(total_latency_sum_ms)
		FROM model_probe_rollups WHERE bucket_start >= ? AND model_name IN (`+placeholders+`)
		GROUP BY model_name`, args...)
	if err != nil {
		return nil, fmt.Errorf("summarize model probes: %w", err)
	}
	summaries := make(map[string]*ModelProbeSummary, len(models))
	for _, model := range models {
		summaries[model] = &ModelProbeSummary{ModelName: model}
	}
	for rows.Next() {
		var model string
		var attempts, successes, failures, skipped, headerSum, headerSamples, firstSum, firstSamples, totalSum int64
		if err := rows.Scan(&model, &attempts, &successes, &failures, &skipped, &headerSum, &headerSamples, &firstSum, &firstSamples, &totalSum); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan model probe summary: %w", err)
		}
		item := summaries[model]
		item.AttemptCount, item.SuccessCount, item.FailureCount, item.SkippedCount = attempts, successes, failures, skipped
		item.AverageHeaderLatencyMS = averageInt64(headerSum, headerSamples)
		item.AverageFirstTokenMS = averageInt64(firstSum, firstSamples)
		item.AverageTotalLatencyMS = averageInt64(totalSum, attempts)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close model probe summary rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model probe summary: %w", err)
	}

	latestArgs := make([]any, 0, len(models))
	for _, model := range models {
		latestArgs = append(latestArgs, model)
	}
	latestRows, err := s.db.QueryContext(ctx, `SELECT a.id, a.run_id, a.model_name, a.capability, a.endpoint,
		a.outcome, a.protocol_success, a.semantic_success, a.http_status, a.header_latency_ms,
		a.first_token_latency_ms, a.total_latency_ms, a.error_code, a.error_message,
		a.response_sha256, a.started_at, a.finished_at, a.created_at
		FROM model_probe_attempts a
		INNER JOIN (SELECT model_name, MAX(id) AS id FROM model_probe_attempts
			WHERE model_name IN (`+placeholders+`) GROUP BY model_name) latest ON latest.id = a.id`, latestArgs...)
	if err != nil {
		return nil, fmt.Errorf("read latest model probes: %w", err)
	}
	for latestRows.Next() {
		attempt, err := scanModelProbeAttempt(latestRows)
		if err != nil {
			_ = latestRows.Close()
			return nil, err
		}
		summaries[attempt.ModelName].Latest = &attempt
	}
	if err := latestRows.Close(); err != nil {
		return nil, fmt.Errorf("close latest model probe rows: %w", err)
	}
	if err := latestRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest model probes: %w", err)
	}

	result := make([]ModelProbeSummary, 0, len(models))
	for _, model := range models {
		result = append(result, *summaries[model])
	}
	return result, nil
}

func (s *Store) ListModelProbeHistory(ctx context.Context, model string, limit int) ([]ModelProbeAttempt, error) {
	if s == nil || s.db == nil {
		return nil, ErrStoreClosed
	}
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 256 {
		return nil, ErrInvalid
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, model_name, capability, endpoint,
		outcome, protocol_success, semantic_success, http_status, header_latency_ms,
		first_token_latency_ms, total_latency_ms, error_code, error_message,
		response_sha256, started_at, finished_at, created_at
		FROM model_probe_attempts WHERE model_name = ? ORDER BY id DESC LIMIT ?`, model, limit)
	if err != nil {
		return nil, fmt.Errorf("list model probe history: %w", err)
	}
	defer rows.Close()
	result := make([]ModelProbeAttempt, 0, limit)
	for rows.Next() {
		attempt, err := scanModelProbeAttempt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model probe history: %w", err)
	}
	return result, nil
}

func (s *Store) CleanupModelProbes(ctx context.Context, before time.Time) error {
	if s == nil || s.db == nil {
		return ErrStoreClosed
	}
	if before.IsZero() {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin model probe retention cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM model_probe_attempts WHERE started_at < ?", dbTime(before)); err != nil {
		return fmt.Errorf("delete expired model probe attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM model_probe_rollups WHERE bucket_start < ?", dbTime(before.UTC().Truncate(time.Hour))); err != nil {
		return fmt.Errorf("delete expired model probe rollups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_probe_runs
		WHERE finished_at IS NOT NULL AND finished_at < ?`, dbTime(before)); err != nil {
		return fmt.Errorf("delete expired model probe runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model probe retention cleanup: %w", err)
	}
	return nil
}

type modelProbeScanner interface {
	Scan(dest ...any) error
}

func (s *Store) getModelProbeAttempt(ctx context.Context, id int64) (ModelProbeAttempt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, run_id, model_name, capability, endpoint,
		outcome, protocol_success, semantic_success, http_status, header_latency_ms,
		first_token_latency_ms, total_latency_ms, error_code, error_message,
		response_sha256, started_at, finished_at, created_at
		FROM model_probe_attempts WHERE id = ?`, id)
	attempt, err := scanModelProbeAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProbeAttempt{}, ErrNotFound
	}
	return attempt, err
}

func scanModelProbeAttempt(scanner modelProbeScanner) (ModelProbeAttempt, error) {
	var attempt ModelProbeAttempt
	var protocol int
	var semantic, header, first sql.NullInt64
	var started, finished, created int64
	if err := scanner.Scan(&attempt.ID, &attempt.RunID, &attempt.ModelName, &attempt.Capability, &attempt.Endpoint,
		&attempt.Outcome, &protocol, &semantic, &attempt.HTTPStatus, &header, &first,
		&attempt.TotalLatencyMS, &attempt.ErrorCode, &attempt.ErrorMessage, &attempt.ResponseSHA256,
		&started, &finished, &created); err != nil {
		return ModelProbeAttempt{}, fmt.Errorf("scan model probe attempt: %w", err)
	}
	attempt.ProtocolSuccess = protocol == 1
	if semantic.Valid {
		value := semantic.Int64 == 1
		attempt.SemanticSuccess = &value
	}
	if header.Valid {
		value := header.Int64
		attempt.HeaderLatencyMS = &value
	}
	if first.Valid {
		value := first.Int64
		attempt.FirstTokenLatencyMS = &value
	}
	attempt.StartedAt = fromDBTime(started)
	attempt.FinishedAt = fromDBTime(finished)
	attempt.CreatedAt = fromDBTime(created)
	return attempt, nil
}

func scanModelProbeAttemptLifecycle(scanner modelProbeScanner) (ModelProbeAttemptLifecycle, error) {
	var item ModelProbeAttemptLifecycle
	var budgetDay, createdAt, updatedAt int64
	var reservedAt, sentAt, finishedAt, attemptID sql.NullInt64
	if err := scanner.Scan(
		&item.ID, &item.RunID, &item.AttemptIndex, &item.ModelName, &item.Capability,
		&item.LifecycleState, &budgetDay, &reservedAt, &sentAt, &finishedAt, &attemptID,
		&item.ErrorCode, &item.ErrorMessage, &createdAt, &updatedAt,
	); err != nil {
		return ModelProbeAttemptLifecycle{}, fmt.Errorf("scan model probe attempt lifecycle: %w", err)
	}
	item.BudgetDay = fromDBTime(budgetDay)
	item.CreatedAt = fromDBTime(createdAt)
	item.UpdatedAt = fromDBTime(updatedAt)
	if reservedAt.Valid {
		value := fromDBTime(reservedAt.Int64)
		item.ReservedAt = &value
	}
	if sentAt.Valid {
		value := fromDBTime(sentAt.Int64)
		item.SentAt = &value
	}
	if finishedAt.Valid {
		value := fromDBTime(finishedAt.Int64)
		item.FinishedAt = &value
	}
	if attemptID.Valid {
		value := attemptID.Int64
		item.AttemptID = &value
	}
	return item, nil
}

func validProbeRunStatus(value string) bool {
	switch value {
	case "running", "succeeded", "partial", "failed", "skipped", "cancelled":
		return true
	default:
		return false
	}
}

func validProbeCapability(value string) bool {
	switch value {
	case "chat_completions", "responses", "embeddings", "rerank", "unsupported":
		return true
	default:
		return false
	}
}

func validProbeOutcome(value string) bool {
	switch value {
	case "success", "failure", "skipped":
		return true
	default:
		return false
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableBoolInt(value *bool) any {
	if value == nil {
		return nil
	}
	return boolInt(*value)
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func averageInt64(sum, count int64) *int64 {
	if count <= 0 {
		return nil
	}
	value := sum / count
	return &value
}

func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func uniqueProbeModels(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func startOfProbeBudgetDay(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	year, month, day := value.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
