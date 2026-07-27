package modelprobe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/toolstore"
	"github.com/rs/zerolog/log"
)

var (
	ErrDisabled        = errors.New("active model probes are disabled")
	ErrNotConfigured   = errors.New("active model probes are not fully configured")
	ErrBusy            = errors.New("a model probe run is already active")
	ErrBudgetExceeded  = errors.New("daily model probe request budget would be exceeded")
	ErrNoModels        = errors.New("no eligible models were requested")
	ErrModelNotAllowed = errors.New("one or more models are not in the probe allowlist")
)

type Manager struct {
	cfg        *config.Config
	store      *toolstore.Store
	runner     *Runner
	initError  string
	allowed    []string
	allowedSet map[string]struct{}

	mu        sync.RWMutex
	running   bool
	nextRunAt time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	now       func() time.Time
}

type SystemStatus struct {
	State                  string                   `json:"state"`
	Enabled                bool                     `json:"enabled"`
	Configured             bool                     `json:"configured"`
	Running                bool                     `json:"running"`
	Message                string                   `json:"message,omitempty"`
	IntervalSeconds        int64                    `json:"interval_seconds"`
	StaleSeconds           int64                    `json:"stale_seconds"`
	MaxConcurrency         int                      `json:"max_concurrency"`
	MaxModelsPerRun        int                      `json:"max_models_per_run"`
	DailyRequestBudget     int                      `json:"daily_request_budget"`
	RequestsUsedToday      int                      `json:"requests_used_today"`
	RequestsRemainingToday int                      `json:"requests_remaining_today"`
	RetentionDays          int                      `json:"retention_days"`
	AllowedModels          []string                 `json:"allowed_models"`
	NextRunAt              *time.Time               `json:"next_run_at,omitempty"`
	LastRun                *toolstore.ModelProbeRun `json:"last_run,omitempty"`
}

type ModelStatus struct {
	toolstore.ModelProbeSummary
	Capability   string   `json:"capability"`
	ProbeHealth  string   `json:"probe_health"`
	SourceState  string   `json:"source_state"`
	Availability *float64 `json:"availability_24h"`
	ReasonCode   string   `json:"reason_code"`
	Reason       string   `json:"reason"`
	Allowed      bool     `json:"allowed"`
}

type Summary struct {
	Config SystemStatus  `json:"config"`
	Items  []ModelStatus `json:"items"`
}

func NewManager(cfg *config.Config, store *toolstore.Store) *Manager {
	manager := &Manager{
		cfg: cfg, store: store, now: time.Now,
		allowedSet: make(map[string]struct{}),
	}
	if cfg == nil || store == nil {
		manager.initError = "model probe dependencies are unavailable"
		return manager
	}
	for _, model := range cfg.ModelProbeModels {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 256 {
			continue
		}
		if _, exists := manager.allowedSet[model]; exists {
			continue
		}
		manager.allowedSet[model] = struct{}{}
		manager.allowed = append(manager.allowed, model)
	}
	if cfg.ModelProbeEnabled {
		runner, err := NewRunner(cfg.NewAPIBaseURL, cfg.ModelProbeAPIKey, cfg.ModelProbeTimeout, cfg.ModelProbeMaxOutputTokens, cfg.ModelProbeCapabilityMap)
		if err != nil {
			manager.initError = err.Error()
		} else {
			manager.runner = runner
		}
	}
	return manager
}

func (m *Manager) Start(parent context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	processStartedAt := m.now().UTC()
	m.mu.Unlock()

	if m.store != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cancelled, err := m.store.CancelInterruptedModelProbeRuns(recoveryCtx, processStartedAt)
		cancel()
		if err != nil {
			m.mu.Lock()
			m.initError = "model probe recovery failed"
			m.mu.Unlock()
			log.Error().Err(err).Msg("interrupted model probe runs could not be reconciled")
		} else if cancelled > 0 {
			log.Warn().Int64("runs", cancelled).Msg("interrupted model probe runs were marked cancelled")
		}
	}

	m.mu.Lock()
	ready := m.readyLocked()
	if ready {
		m.nextRunAt = m.now().UTC().Add(m.cfg.ModelProbeInterval)
	}
	m.mu.Unlock()
	if !ready {
		return
	}
	m.wg.Add(1)
	go m.scheduler()
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

func (m *Manager) scheduler() {
	defer m.wg.Done()
	for {
		m.mu.RLock()
		next := m.nextRunAt
		ctx := m.ctx
		m.mu.RUnlock()
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if _, _, err := m.queue(ctx, "scheduled", "scheduler", scheduledRunKey(m.now().UTC()), m.allowed); err != nil &&
			!errors.Is(err, ErrBusy) && !errors.Is(err, ErrBudgetExceeded) {
			log.Warn().Err(err).Msg("scheduled model probe was not queued")
		}
		m.mu.Lock()
		m.nextRunAt = m.now().UTC().Add(m.cfg.ModelProbeInterval)
		m.mu.Unlock()
	}
}

func (m *Manager) QueueManual(ctx context.Context, actor, idempotencyKey string, models []string) (toolstore.ModelProbeRun, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		return toolstore.ModelProbeRun{}, false, toolstore.ErrInvalid
	}
	return m.queue(ctx, "manual", actor, "manual:"+idempotencyKey, models)
}

func (m *Manager) queue(ctx context.Context, trigger, actor, runKey string, models []string) (toolstore.ModelProbeRun, bool, error) {
	if m == nil || m.cfg == nil || m.store == nil {
		return toolstore.ModelProbeRun{}, false, ErrNotConfigured
	}
	if !m.cfg.ModelProbeEnabled {
		return toolstore.ModelProbeRun{}, false, ErrDisabled
	}
	if m.runner == nil || m.initError != "" || len(m.allowed) == 0 {
		return toolstore.ModelProbeRun{}, false, ErrNotConfigured
	}
	models, err := m.validateRequestedModels(models)
	if err != nil {
		return toolstore.ModelProbeRun{}, false, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "unknown"
	}
	requestFingerprint := fingerprint(actor, models)
	if trigger == "manual" {
		existing, existingErr := m.store.GetModelProbeRunByKey(ctx, runKey)
		switch {
		case existingErr == nil:
			if existing.RequestFingerprint != requestFingerprint || existing.Actor != actor || existing.RequestedCount != len(models) {
				return toolstore.ModelProbeRun{}, false, toolstore.ErrConflict
			}
			return existing, true, nil
		case !errors.Is(existingErr, toolstore.ErrNotFound):
			return toolstore.ModelProbeRun{}, false, existingErr
		}
	}
	used, err := m.store.CountModelProbeAttemptsSince(ctx, startOfUTCDay(m.now().UTC()))
	if err != nil {
		return toolstore.ModelProbeRun{}, false, err
	}
	if used+len(models) > m.cfg.ModelProbeDailyRequestBudget {
		return toolstore.ModelProbeRun{}, false, ErrBudgetExceeded
	}

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return toolstore.ModelProbeRun{}, false, ErrBusy
	}
	m.running = true
	runCtx := m.ctx
	if runCtx == nil {
		runCtx = context.Background()
	}
	m.mu.Unlock()

	input := toolstore.ModelProbeRunInput{
		RunKey: runKey, RequestFingerprint: requestFingerprint, TriggerKind: trigger,
		Actor: actor, RequestedCount: len(models), StartedAt: m.now().UTC(),
	}
	run, replayed, err := m.store.CreateModelProbeRun(ctx, input)
	if err != nil || replayed {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		return run, replayed, err
	}

	m.wg.Add(1)
	go m.execute(runCtx, run, models)
	return run, false, nil
}

func (m *Manager) execute(ctx context.Context, run toolstore.ModelProbeRun, models []string) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	type result struct {
		attempt toolstore.ModelProbeAttemptInput
		err     error
	}
	jobs := make(chan string)
	results := make(chan result, len(models))
	workers := m.cfg.ModelProbeMaxConcurrency
	if workers > len(models) {
		workers = len(models)
	}
	var workersWG sync.WaitGroup
	workersWG.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer workersWG.Done()
			for model := range jobs {
				attempt := m.runner.Probe(ctx, run.ID, model)
				_, err := m.store.AppendModelProbeAttempt(context.Background(), attempt)
				results <- result{attempt: attempt, err: err}
			}
		}()
	}
	go func() {
		for _, model := range models {
			jobs <- model
		}
		close(jobs)
		workersWG.Wait()
		close(results)
	}()

	attempted, successes, failures, skipped := 0, 0, 0, 0
	storeFailure := false
	for item := range results {
		if item.err != nil {
			storeFailure = true
			continue
		}
		attempted++
		switch item.attempt.Outcome {
		case "success":
			successes++
		case "failure":
			failures++
		case "skipped":
			skipped++
		}
	}

	status, errorCode, errorMessage := "succeeded", "", ""
	switch {
	case storeFailure:
		status, errorCode, errorMessage = "failed", "store_write_failed", "One or more probe results could not be persisted"
	case ctx.Err() != nil:
		status, errorCode, errorMessage = "cancelled", "cancelled", "Probe run was cancelled during shutdown"
	case successes == 0 && failures > 0:
		status = "failed"
	case successes == 0 && skipped > 0:
		status = "skipped"
	case failures > 0 || skipped > 0:
		status = "partial"
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, finishErr := m.store.FinishModelProbeRun(finishCtx, toolstore.ModelProbeRunFinish{
		ID: run.ID, Status: status, AttemptedCount: attempted, SuccessCount: successes,
		FailureCount: failures, SkippedCount: skipped, ErrorCode: errorCode,
		ErrorMessage: errorMessage, FinishedAt: m.now().UTC(),
	})
	if finishErr != nil {
		log.Error().Err(finishErr).Int64("run_id", run.ID).Msg("model probe run could not be finalized")
	}
	cleanupBefore := m.now().UTC().Add(-m.cfg.ModelProbeRetention)
	if cleanupErr := m.store.CleanupModelProbes(finishCtx, cleanupBefore); cleanupErr != nil {
		log.Warn().Err(cleanupErr).Msg("model probe retention cleanup failed")
	}
	cancel()
}

func (m *Manager) Summary(ctx context.Context, models []string) (Summary, error) {
	status, err := m.Status(ctx)
	if err != nil {
		return Summary{}, err
	}
	models = uniqueModels(models)
	if len(models) == 0 {
		models = append([]string(nil), m.allowed...)
	}
	if len(models) > 200 {
		return Summary{}, toolstore.ErrInvalid
	}
	stored, err := m.store.ListModelProbeSummaries(ctx, models, m.now().UTC().Add(-24*time.Hour))
	if err != nil {
		return Summary{}, err
	}
	items := make([]ModelStatus, 0, len(stored))
	for _, item := range stored {
		_, allowed := m.allowedSet[item.ModelName]
		capability := capabilityFor(item.ModelName, m.cfg.ModelProbeCapabilityMap)
		if item.Latest != nil {
			capability = item.Latest.Capability
		}
		view := ModelStatus{ModelProbeSummary: item, Capability: capability, Allowed: allowed}
		view.Availability = availability(item)
		view.ProbeHealth, view.SourceState, view.ReasonCode, view.Reason = m.classify(item, allowed, status)
		items = append(items, view)
	}
	return Summary{Config: status, Items: items}, nil
}

func (m *Manager) History(ctx context.Context, model string, limit int) ([]toolstore.ModelProbeAttempt, error) {
	return m.store.ListModelProbeHistory(ctx, model, limit)
}

func (m *Manager) Status(ctx context.Context) (SystemStatus, error) {
	if m == nil || m.cfg == nil || m.store == nil {
		return SystemStatus{State: "unavailable", Message: "Model probe dependencies are unavailable"}, nil
	}
	used, err := m.store.CountModelProbeAttemptsSince(ctx, startOfUTCDay(m.now().UTC()))
	if err != nil {
		return SystemStatus{}, err
	}
	lastRun, err := m.store.LatestModelProbeRun(ctx)
	if err != nil {
		return SystemStatus{}, err
	}
	m.mu.RLock()
	running := m.running
	next := m.nextRunAt
	configured := m.runner != nil && m.initError == "" && len(m.allowed) > 0
	m.mu.RUnlock()
	remaining := m.cfg.ModelProbeDailyRequestBudget - used
	if remaining < 0 {
		remaining = 0
	}
	state, message := "ready", ""
	switch {
	case !m.cfg.ModelProbeEnabled:
		state, message = "disabled", "主动探测默认关闭；配置专用模型令牌和白名单后再启用"
	case !configured:
		state, message = "misconfigured", "主动探测缺少专用令牌、有效 NewAPI 地址或模型白名单"
		if m.initError != "" {
			message = "主动探测配置无效"
		}
	case remaining == 0:
		state, message = "budget_exhausted", "今日主动探测请求预算已用尽"
	case running:
		state = "running"
	}
	status := SystemStatus{
		State: state, Enabled: m.cfg.ModelProbeEnabled, Configured: configured, Running: running,
		Message: message, IntervalSeconds: int64(m.cfg.ModelProbeInterval / time.Second),
		StaleSeconds: int64(m.cfg.ModelProbeStaleAfter / time.Second), MaxConcurrency: m.cfg.ModelProbeMaxConcurrency,
		MaxModelsPerRun: m.cfg.ModelProbeMaxModelsPerRun, DailyRequestBudget: m.cfg.ModelProbeDailyRequestBudget,
		RequestsUsedToday: used, RequestsRemainingToday: remaining,
		RetentionDays: int(m.cfg.ModelProbeRetention / (24 * time.Hour)),
		AllowedModels: append([]string(nil), m.allowed...), LastRun: lastRun,
	}
	if !next.IsZero() && configured {
		value := next
		status.NextRunAt = &value
	}
	return status, nil
}

func (m *Manager) classify(item toolstore.ModelProbeSummary, allowed bool, system SystemStatus) (health, source, code, reason string) {
	if !allowed {
		return "unavailable", "unavailable", "not_allowed", "模型不在主动探测白名单中"
	}
	if item.Latest == nil {
		if system.Running {
			return "pending", "pending", "probe_pending", "主动探测正在执行，尚未产生结果"
		}
		if !system.Enabled || !system.Configured {
			return "unavailable", "unavailable", system.State, system.Message
		}
		return "unavailable", "unavailable", "no_probe_data", "尚无主动探测数据"
	}
	if item.Latest.Outcome == "skipped" || item.Latest.Capability == CapabilityUnsupported {
		return "unsupported", "unsupported", item.Latest.ErrorCode, "该模型能力未配置安全的探测适配器"
	}
	if m.now().UTC().Sub(item.Latest.FinishedAt) > m.cfg.ModelProbeStaleAfter {
		return "stale", "stale", "probe_stale", "最近主动探测结果已过期"
	}
	if item.Latest.Outcome == "failure" {
		return "unhealthy", "fresh", item.Latest.ErrorCode, item.Latest.ErrorMessage
	}
	if value := availability(item); value != nil && *value < 95 {
		return "degraded", "fresh", "availability_below_slo", "24 小时主动探测可用率低于 95%"
	}
	return "healthy", "fresh", "ok", "最近主动探测成功"
}

func (m *Manager) validateRequestedModels(models []string) ([]string, error) {
	models = uniqueModels(models)
	if len(models) == 0 {
		return nil, ErrNoModels
	}
	if len(models) > m.cfg.ModelProbeMaxModelsPerRun {
		return nil, toolstore.ErrInvalid
	}
	for _, model := range models {
		if _, allowed := m.allowedSet[model]; !allowed {
			return nil, ErrModelNotAllowed
		}
	}
	return models, nil
}

func (m *Manager) readyLocked() bool {
	return m.cfg != nil && m.cfg.ModelProbeEnabled && m.runner != nil && m.initError == "" && len(m.allowed) > 0
}

func capabilityFor(model string, mapping map[string]string) string {
	if value, ok := mapping[model]; ok {
		return normalizeCapability(value)
	}
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "embedding") || strings.Contains(lower, "embed-"):
		return CapabilityEmbeddings
	case strings.Contains(lower, "rerank"):
		return CapabilityRerank
	case containsAny(lower, "dall-e", "image", "flux", "stable-diffusion", "midjourney", "sdxl", "whisper", "audio", "speech", "tts", "transcri"):
		return CapabilityUnsupported
	default:
		return CapabilityChatCompletions
	}
}

func availability(item toolstore.ModelProbeSummary) *float64 {
	measured := item.SuccessCount + item.FailureCount
	if measured <= 0 {
		return nil
	}
	value := float64(item.SuccessCount) / float64(measured) * 100
	value = float64(int(value*100+0.5)) / 100
	return &value
}

func uniqueModels(values []string) []string {
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

func fingerprint(actor string, models []string) string {
	ordered := append([]string(nil), models...)
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.TrimSpace(actor) + "\x00" + strings.Join(ordered, "\x00")))
	return hex.EncodeToString(sum[:])
}

func scheduledRunKey(now time.Time) string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("scheduled:%d", now.UnixNano())
	}
	return fmt.Sprintf("scheduled:%d:%s", now.Unix(), hex.EncodeToString(random))
}

func startOfUTCDay(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
