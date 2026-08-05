package modelprobe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestManagerQueuesIdempotentBudgetedProbeAndClassifiesSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		NewAPIBaseURL: server.URL, ModelProbeEnabled: true, ModelProbeAPIKey: "probe-key",
		ModelProbeModels: []string{"gpt-test"}, ModelProbeInterval: 5 * time.Minute,
		ModelProbeTimeout: time.Second, ModelProbeStaleAfter: 15 * time.Minute,
		ModelProbeMaxConcurrency: 1, ModelProbeMaxModelsPerRun: 1,
		ModelProbeDailyRequestBudget: 1, ModelProbeMaxOutputTokens: 4,
		ModelProbeRetention: 30 * 24 * time.Hour,
	}
	manager := NewManager(cfg, store)
	defer manager.Close()
	run, replayed, err := manager.QueueManual(context.Background(), "admin", "probe-test-key", []string{"gpt-test"})
	if err != nil || replayed {
		t.Fatalf("QueueManual() = run:%+v replayed:%t err:%v", run, replayed, err)
	}
	waitForRunStatus(t, store, run.RunKey, "succeeded")
	replayedRun, replayed, err := manager.QueueManual(context.Background(), "admin", "probe-test-key", []string{"gpt-test"})
	if err != nil || !replayed || replayedRun.ID != run.ID {
		t.Fatalf("idempotent QueueManual() = run:%+v replayed:%t err:%v", replayedRun, replayed, err)
	}
	summary, err := manager.Summary(context.Background(), []string{"gpt-test"})
	if err != nil || len(summary.Items) != 1 {
		t.Fatalf("Summary() = %+v, %v", summary, err)
	}
	if summary.Items[0].ProbeHealth != "healthy" || summary.Items[0].Availability == nil || *summary.Items[0].Availability != 100 {
		t.Fatalf("probe classification = %+v", summary.Items[0])
	}
	if summary.Config.RequestsUsedToday != 1 || summary.Config.State != "budget_exhausted" {
		t.Fatalf("probe budget status = %+v", summary.Config)
	}
	if _, _, err := manager.QueueManual(context.Background(), "admin", "another-probe-key", []string{"gpt-test"}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second probe error = %v, want ErrBudgetExceeded", err)
	}
	if _, _, err := manager.QueueManual(context.Background(), "admin", "forbidden-model", []string{"other-model"}); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("forbidden model error = %v, want ErrModelNotAllowed", err)
	}
}

// Run this test with -race. Startup recovery is the production writer for
// initError; queue and status readiness must use one consistent mutex snapshot.
func TestManagerQueueReadinessAccessIsSynchronized(t *testing.T) {
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "probe-readiness-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		NewAPIBaseURL: "http://127.0.0.1:1", ModelProbeEnabled: true, ModelProbeAPIKey: "probe-key",
		ModelProbeModels: []string{"gpt-test"}, ModelProbeTimeout: time.Second,
		ModelProbeMaxConcurrency: 1, ModelProbeMaxModelsPerRun: 1,
		ModelProbeDailyRequestBudget: 1, ModelProbeMaxOutputTokens: 4,
	}
	manager := NewManager(cfg, store)
	const idempotencyKey = "readiness-race"
	_, replayed, err := store.CreateModelProbeRun(context.Background(), toolstore.ModelProbeRunInput{
		RunKey: "manual:" + idempotencyKey, RequestFingerprint: fingerprint("admin", []string{"gpt-test"}),
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: time.Now().UTC(),
	})
	if err != nil || replayed {
		t.Fatalf("seed replay run = replayed:%t err:%v", replayed, err)
	}
	configuredRunner := manager.runner

	start := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			manager.mu.Lock()
			manager.runner = configuredRunner
			manager.allowed = []string{"gpt-test"}
			if i%2 == 0 {
				manager.initError = "model probe recovery failed"
			} else {
				manager.initError = ""
			}
			manager.mu.Unlock()
			runtime.Gosched()
		}
	}()
	close(start)
	defer func() {
		close(stop)
		<-done
	}()
	for i := 0; i < 1_000; i++ {
		_, replayed, queueErr := manager.QueueManual(context.Background(), "admin", idempotencyKey, []string{"gpt-test"})
		if queueErr == nil && !replayed {
			t.Fatal("readiness replay unexpectedly queued a new run")
		}
		if queueErr != nil && !errors.Is(queueErr, ErrNotConfigured) {
			t.Fatalf("QueueManual() error = %v, want replay or ErrNotConfigured", queueErr)
		}
		status, statusErr := manager.Status(context.Background())
		if statusErr != nil {
			t.Fatalf("Status() error = %v", statusErr)
		}
		if len(status.AllowedModels) != 1 || status.AllowedModels[0] != "gpt-test" {
			t.Fatalf("Status() allowed models = %v", status.AllowedModels)
		}
		if status.Configured == (status.State == "misconfigured") {
			t.Fatalf("Status() readiness snapshot is inconsistent: %+v", status)
		}
		runtime.Gosched()
	}
}

func waitForRunStatus(t *testing.T, store *toolstore.Store, key, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, err := store.GetModelProbeRunByKey(context.Background(), key)
		if err == nil && run.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := store.GetModelProbeRunByKey(context.Background(), key)
	t.Fatalf("run did not reach %q: %+v, %v", status, run, err)
}

func waitForManagerIdle(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := manager.Status(context.Background())
		if err == nil && !status.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := manager.Status(context.Background())
	t.Fatalf("manager did not become idle: %+v, %v", status, err)
}

func TestManagerSkipsUnsupportedWithoutNetworkOrBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "probe-skip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		NewAPIBaseURL: server.URL, ModelProbeEnabled: true, ModelProbeAPIKey: "probe-key",
		ModelProbeModels: []string{"image-model", "gpt-test"}, ModelProbeInterval: 5 * time.Minute,
		ModelProbeTimeout: time.Second, ModelProbeStaleAfter: 15 * time.Minute,
		ModelProbeMaxConcurrency: 1, ModelProbeMaxModelsPerRun: 2,
		ModelProbeDailyRequestBudget: 1, ModelProbeMaxOutputTokens: 4,
		ModelProbeRetention: 30 * 24 * time.Hour,
	}
	manager := NewManager(cfg, store)
	defer manager.Close()
	skippedRun, replayed, err := manager.QueueManual(context.Background(), "admin", "unsupported-only", []string{"image-model"})
	if err != nil || replayed {
		t.Fatalf("QueueManual(unsupported) = %+v, %t, %v", skippedRun, replayed, err)
	}
	waitForRunStatus(t, store, skippedRun.RunKey, "skipped")
	waitForManagerIdle(t, manager)
	status, err := manager.Status(context.Background())
	if err != nil || status.RequestsUsedToday != 0 || requests.Load() != 0 {
		t.Fatalf("unsupported truth = status:%+v requests:%d err:%v", status, requests.Load(), err)
	}
	networkRun, replayed, err := manager.QueueManual(context.Background(), "admin", "supported-after-skip", []string{"gpt-test"})
	if err != nil || replayed {
		t.Fatalf("QueueManual(supported) = %+v, %t, %v", networkRun, replayed, err)
	}
	waitForRunStatus(t, store, networkRun.RunKey, "succeeded")
	status, err = manager.Status(context.Background())
	if err != nil || status.RequestsUsedToday != 1 || requests.Load() != 1 {
		t.Fatalf("network truth = status:%+v requests:%d err:%v", status, requests.Load(), err)
	}
}

func TestManagerSentResultPersistenceFailureBecomesUncertain(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "probe-uncertain.db")
	store, err := toolstore.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER fail_manager_probe_attempt
		BEFORE INSERT ON model_probe_attempts BEGIN SELECT RAISE(ABORT, 'injected manager persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		NewAPIBaseURL: server.URL, ModelProbeEnabled: true, ModelProbeAPIKey: "probe-key",
		ModelProbeModels: []string{"gpt-test"}, ModelProbeInterval: 5 * time.Minute,
		ModelProbeTimeout: time.Second, ModelProbeStaleAfter: 15 * time.Minute,
		ModelProbeMaxConcurrency: 1, ModelProbeMaxModelsPerRun: 1,
		ModelProbeDailyRequestBudget: 1, ModelProbeMaxOutputTokens: 4,
		ModelProbeRetention: 30 * 24 * time.Hour,
	}
	manager := NewManager(cfg, store)
	defer manager.Close()
	run, _, err := manager.QueueManual(context.Background(), "admin", "uncertain-write", []string{"gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, store, run.RunKey, "failed")
	lifecycles, err := store.ListModelProbeAttemptLifecycles(context.Background(), run.ID)
	if err != nil || len(lifecycles) != 1 || lifecycles[0].LifecycleState != "uncertain" || requests.Load() != 1 {
		t.Fatalf("persistence failure truth = lifecycles:%+v requests:%d err:%v", lifecycles, requests.Load(), err)
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.RequestsUsedToday != 1 || status.RequestsRemainingToday != 0 {
		t.Fatalf("uncertain budget status = %+v, %v", status, err)
	}
}

func TestManagerShutdownBeforeSendFinalizesCountsAndReleasesOnlyUnsentBudget(t *testing.T) {
	var requests atomic.Int32
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		select {
		case <-request.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()
	defer close(releaseRequest)

	store, err := toolstore.Init(filepath.Join(t.TempDir(), "probe-shutdown-before-send.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	models := []string{"gpt-first", "gpt-second", "gpt-third"}
	cfg := &config.Config{
		NewAPIBaseURL: server.URL, ModelProbeEnabled: true, ModelProbeAPIKey: "probe-key",
		ModelProbeModels: models, ModelProbeInterval: 5 * time.Minute,
		ModelProbeTimeout: time.Minute, ModelProbeStaleAfter: 15 * time.Minute,
		ModelProbeMaxConcurrency: 1, ModelProbeMaxModelsPerRun: len(models),
		ModelProbeDailyRequestBudget: len(models), ModelProbeMaxOutputTokens: 4,
		ModelProbeRetention: 30 * 24 * time.Hour,
	}
	manager := NewManager(cfg, store)
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	manager.Start(lifecycleCtx)
	defer manager.Close()

	run, replayed, err := manager.QueueManual(context.Background(), "admin", "shutdown-before-send", models)
	if err != nil || replayed {
		t.Fatalf("QueueManual() = run:%+v replayed:%t err:%v", run, replayed, err)
	}
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first probe request did not reach the network")
	}

	var before []toolstore.ModelProbeAttemptLifecycle
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		before, err = store.ListModelProbeAttemptLifecycles(context.Background(), run.ID)
		if err == nil && len(before) == len(models) && before[0].LifecycleState == "sent" &&
			before[1].LifecycleState == "reserved" && before[2].LifecycleState == "reserved" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(before) != len(models) || before[0].LifecycleState != "sent" ||
		before[1].LifecycleState != "reserved" || before[2].LifecycleState != "reserved" {
		t.Fatalf("pre-shutdown lifecycles = %+v, %v", before, err)
	}
	if used, countErr := store.CountModelProbeBudgetUsed(context.Background(), before[0].BudgetDay); countErr != nil || used != len(models) {
		t.Fatalf("pre-shutdown reservations = %d, %v; want %d", used, countErr, len(models))
	}

	cancelLifecycle()
	waitForRunStatus(t, store, run.RunKey, "cancelled")
	finished, err := store.GetModelProbeRunByKey(context.Background(), run.RunKey)
	if err != nil {
		t.Fatal(err)
	}
	if finished.AttemptedCount != len(models) || finished.SuccessCount != 0 ||
		finished.FailureCount != 1 || finished.SkippedCount != len(models)-1 ||
		finished.AttemptedCount != finished.SuccessCount+finished.FailureCount+finished.SkippedCount {
		t.Fatalf("shutdown run counts are inconsistent: %+v", finished)
	}
	after, err := store.ListModelProbeAttemptLifecycles(context.Background(), run.ID)
	if err != nil || len(after) != len(models) || after[0].LifecycleState != "settled" ||
		after[1].LifecycleState != "skipped" || after[2].LifecycleState != "skipped" {
		t.Fatalf("post-shutdown lifecycles = %+v, %v", after, err)
	}
	if used, countErr := store.CountModelProbeBudgetUsed(context.Background(), before[0].BudgetDay); countErr != nil || used != 1 {
		t.Fatalf("post-shutdown budget = %d, %v; want only the sent attempt to remain charged", used, countErr)
	}
	if requests.Load() != 1 {
		t.Fatalf("network requests = %d, want 1", requests.Load())
	}
}

func TestManagerStartupRecoversOnlyStaleRunsFromAnotherStoreInstance(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "probe-multi-instance-recovery.db")
	ownerStore, err := toolstore.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	recoveryStore, err := toolstore.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryStore.Close()

	createReservedRun := func(key string, startedAt time.Time) (toolstore.ModelProbeRun, []toolstore.ModelProbeAttemptLifecycle) {
		t.Helper()
		run, lifecycles, replayed, createErr := ownerStore.CreateModelProbeRunWithBudget(
			context.Background(),
			toolstore.ModelProbeRunInput{
				RunKey: key, RequestFingerprint: fingerprint(key, []string{"gpt-test"}),
				TriggerKind: "manual", Actor: "instance-a", RequestedCount: 1, StartedAt: startedAt,
			},
			[]toolstore.ModelProbeAttemptPlan{{ModelName: "gpt-test", Capability: CapabilityChatCompletions}},
			startOfUTCDay(startedAt), 10,
		)
		if createErr != nil || replayed || len(lifecycles) != 1 {
			t.Fatalf("create %s = run:%+v lifecycles:%+v replayed:%t err:%v", key, run, lifecycles, replayed, createErr)
		}
		return run, lifecycles
	}

	staleRun, _ := createReservedRun("manual:stale-instance-run", now.Add(-modelProbeRecoveryStaleWindow-time.Minute))
	liveRun, liveBefore := createReservedRun("manual:live-instance-run", now.Add(-time.Hour))

	manager := NewManager(&config.Config{ModelProbeEnabled: false}, recoveryStore)
	manager.now = func() time.Time { return now }
	manager.Start(context.Background())
	defer manager.Close()

	staleAfter, err := recoveryStore.GetModelProbeRunByKey(context.Background(), staleRun.RunKey)
	if err != nil || staleAfter.Status != "cancelled" {
		t.Fatalf("stale other-instance run was not recovered: %+v, %v", staleAfter, err)
	}
	staleLifecycle, err := recoveryStore.ListModelProbeAttemptLifecycles(context.Background(), staleRun.ID)
	if err != nil || len(staleLifecycle) != 1 || staleLifecycle[0].LifecycleState != "uncertain" {
		t.Fatalf("stale lifecycle = %+v, %v", staleLifecycle, err)
	}

	liveAfter, err := recoveryStore.GetModelProbeRunByKey(context.Background(), liveRun.RunKey)
	if err != nil || liveAfter.Status != "running" || liveAfter.FinishedAt != nil {
		t.Fatalf("live other-instance run was cancelled: %+v, %v", liveAfter, err)
	}
	liveLifecycle, err := recoveryStore.ListModelProbeAttemptLifecycles(context.Background(), liveRun.ID)
	if err != nil || len(liveLifecycle) != 1 || liveLifecycle[0].LifecycleState != "reserved" ||
		liveLifecycle[0].ID != liveBefore[0].ID {
		t.Fatalf("live lifecycle changed = %+v, %v", liveLifecycle, err)
	}
	if used, err := recoveryStore.CountModelProbeBudgetUsed(context.Background(), startOfUTCDay(now)); err != nil || used != 2 {
		t.Fatalf("stale/live recovery released budget: used=%d err=%v", used, err)
	}
}

func TestManagerClassifyFailClosedTruthTable(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := &Manager{
		cfg: &config.Config{ModelProbeStaleAfter: 15 * time.Minute},
		now: func() time.Time { return now },
	}
	freshFinished := now.Add(-time.Minute)
	item := toolstore.ModelProbeSummary{
		ModelName: "gpt-test", AttemptCount: 1, SuccessCount: 1,
		Latest: &toolstore.ModelProbeAttempt{
			ModelName: "gpt-test", Capability: CapabilityChatCompletions,
			Outcome: "success", FinishedAt: freshFinished,
		},
	}
	tests := []struct {
		name       string
		system     SystemStatus
		finishedAt time.Time
		wantHealth string
		wantSource string
		wantCode   string
	}{
		{name: "disabled overrides fresh success", system: SystemStatus{State: "disabled", Enabled: false, Configured: true}, finishedAt: freshFinished, wantHealth: "unavailable", wantSource: "unavailable", wantCode: "disabled"},
		{name: "misconfigured overrides fresh success", system: SystemStatus{State: "misconfigured", Enabled: true, Configured: false}, finishedAt: freshFinished, wantHealth: "unavailable", wantSource: "unavailable", wantCode: "misconfigured"},
		{name: "unavailable overrides fresh success", system: SystemStatus{State: "unavailable", Enabled: true, Configured: true}, finishedAt: freshFinished, wantHealth: "unavailable", wantSource: "unavailable", wantCode: "unavailable"},
		{name: "stale success is not healthy", system: SystemStatus{State: "ready", Enabled: true, Configured: true}, finishedAt: now.Add(-time.Hour), wantHealth: "stale", wantSource: "stale", wantCode: "probe_stale"},
		{name: "only fresh configured success is healthy", system: SystemStatus{State: "ready", Enabled: true, Configured: true}, finishedAt: freshFinished, wantHealth: "healthy", wantSource: "fresh", wantCode: "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item.Latest.FinishedAt = tt.finishedAt
			health, source, code, _ := manager.classify(item, true, tt.system)
			if health != tt.wantHealth || source != tt.wantSource || code != tt.wantCode {
				t.Fatalf("classify() = health:%q source:%q code:%q", health, source, code)
			}
		})
	}
}
