package toolstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModelProbeRunAttemptSummaryAndReplay(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	started := testNow.Add(-10 * time.Minute)
	input := ModelProbeRunInput{
		RunKey: "manual:model-probe-test", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 2, StartedAt: started,
	}
	run, replayed, err := store.CreateModelProbeRun(ctx, input)
	if err != nil || replayed {
		t.Fatalf("CreateModelProbeRun() = run:%+v replayed:%t err:%v", run, replayed, err)
	}
	retry, replayed, err := store.CreateModelProbeRun(ctx, input)
	if err != nil || !replayed || retry.ID != run.ID {
		t.Fatalf("replayed CreateModelProbeRun() = run:%+v replayed:%t err:%v", retry, replayed, err)
	}
	conflict := input
	conflict.RequestFingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, _, err := store.CreateModelProbeRun(ctx, conflict); err != ErrConflict {
		t.Fatalf("conflicting replay error = %v, want ErrConflict", err)
	}

	semanticSuccess := true
	header := int64(120)
	first := int64(180)
	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		RunID: run.ID, ModelName: "gpt-test", Capability: "chat_completions", Endpoint: "/v1/chat/completions",
		Outcome: "success", ProtocolSuccess: true, SemanticSuccess: &semanticSuccess, HTTPStatus: 200,
		HeaderLatencyMS: &header, FirstTokenLatencyMS: &first, TotalLatencyMS: 240,
		ResponseSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		StartedAt:      started, FinishedAt: started.Add(240 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	semanticFailure := false
	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		RunID: run.ID, ModelName: "embedding-test", Capability: "embeddings", Endpoint: "/v1/embeddings",
		Outcome: "failure", ProtocolSuccess: true, SemanticSuccess: &semanticFailure, HTTPStatus: 200,
		HeaderLatencyMS: &header, TotalLatencyMS: 210, ErrorCode: "semantic_mismatch",
		ErrorMessage: "bounded failure", StartedAt: started.Add(time.Second), FinishedAt: started.Add(time.Second + 210*time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}

	finished, err := store.FinishModelProbeRun(ctx, ModelProbeRunFinish{
		ID: run.ID, Status: "partial", AttemptedCount: 2, SuccessCount: 1, FailureCount: 1,
		FinishedAt: started.Add(2 * time.Second),
	})
	if err != nil || finished.Status != "partial" || finished.AttemptedCount != 2 {
		t.Fatalf("FinishModelProbeRun() = %+v, %v", finished, err)
	}

	summary, err := store.ListModelProbeSummaries(ctx, []string{"gpt-test", "embedding-test", "never-probed"}, started.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) != 3 || summary[0].AttemptCount != 1 || summary[0].SuccessCount != 1 ||
		summary[0].Latest == nil || summary[0].Latest.ResponseSHA256 == "" {
		t.Fatalf("unexpected probe summary: %+v", summary)
	}
	if summary[1].FailureCount != 1 || summary[2].Latest != nil {
		t.Fatalf("unexpected failure/empty summary: %+v", summary)
	}
	history, err := store.ListModelProbeHistory(ctx, "gpt-test", 10)
	if err != nil || len(history) != 1 || history[0].ErrorMessage != "" {
		t.Fatalf("ListModelProbeHistory() = %+v, %v", history, err)
	}
}

func TestModelProbeSummaryForRejectsMissingOrNilEntries(t *testing.T) {
	valid := &ModelProbeSummary{ModelName: "gpt-test"}
	if item, err := modelProbeSummaryFor(map[string]*ModelProbeSummary{"gpt-test": valid}, "gpt-test"); err != nil || item != valid {
		t.Fatalf("valid summary lookup = %+v, %v", item, err)
	}
	for name, summaries := range map[string]map[string]*ModelProbeSummary{
		"missing": {},
		"nil":     {"gpt-test": nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := modelProbeSummaryFor(summaries, "gpt-test"); err == nil || !strings.Contains(err.Error(), "unexpected model") {
				t.Fatalf("modelProbeSummaryFor() error = %v, want unexpected-model error", err)
			}
		})
	}
}

func TestModelProbeRetentionDeletesOnlyExpiredTelemetry(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	old := testNow.Add(-40 * 24 * time.Hour)
	run, _, err := store.CreateModelProbeRun(ctx, ModelProbeRunInput{
		RunKey: "scheduled:expired-probe", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "scheduled", Actor: "scheduler", RequestedCount: 1, StartedAt: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic := false
	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		RunID: run.ID, ModelName: "old-model", Capability: "chat_completions", Endpoint: "/v1/chat/completions",
		Outcome: "failure", SemanticSuccess: &semantic, TotalLatencyMS: 1, ErrorCode: "network_error",
		StartedAt: old, FinishedAt: old.Add(time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishModelProbeRun(ctx, ModelProbeRunFinish{
		ID: run.ID, Status: "failed", AttemptedCount: 1, FailureCount: 1, FinishedAt: old.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupModelProbes(ctx, testNow.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	count, err := store.CountModelProbeAttemptsSince(ctx, time.Unix(0, 0))
	if err != nil || count != 0 {
		t.Fatalf("expired attempts count = %d, err = %v", count, err)
	}
	if _, err := store.GetModelProbeRunByKey(ctx, run.RunKey); err != ErrNotFound {
		t.Fatalf("expired run lookup error = %v, want ErrNotFound", err)
	}
}

func TestCancelInterruptedModelProbeRunsPreservesAttemptCounts(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	oldStart := testNow.Add(-10 * time.Minute)
	oldRun, _, err := store.CreateModelProbeRun(ctx, ModelProbeRunInput{
		RunKey: "scheduled:interrupted-probe", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "scheduled", Actor: "scheduler", RequestedCount: 2, StartedAt: oldStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic := true
	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		RunID: oldRun.ID, ModelName: "gpt-test", Capability: "chat_completions", Endpoint: "/v1/chat/completions",
		Outcome: "success", ProtocolSuccess: true, SemanticSuccess: &semantic, HTTPStatus: 200,
		TotalLatencyMS: 10, StartedAt: oldStart, FinishedAt: oldStart.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	currentRun, _, err := store.CreateModelProbeRun(ctx, ModelProbeRunInput{
		RunKey: "scheduled:current-probe", RequestFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TriggerKind: "scheduled", Actor: "scheduler", RequestedCount: 1, StartedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := store.CancelInterruptedModelProbeRuns(ctx, testNow)
	if err != nil || cancelled != 1 {
		t.Fatalf("CancelInterruptedModelProbeRuns() = %d, %v", cancelled, err)
	}
	interrupted, err := store.GetModelProbeRunByKey(ctx, oldRun.RunKey)
	if err != nil || interrupted.Status != "cancelled" || interrupted.AttemptedCount != 1 || interrupted.SuccessCount != 1 ||
		interrupted.ErrorCode != "process_restarted" || interrupted.FinishedAt == nil {
		t.Fatalf("interrupted run = %+v, %v", interrupted, err)
	}
	current, err := store.GetModelProbeRunByKey(ctx, currentRun.RunKey)
	if err != nil || current.Status != "running" || current.FinishedAt != nil {
		t.Fatalf("current run was changed = %+v, %v", current, err)
	}
}

func TestModelProbeBudgetLifecycleSkipsUnsupportedAndSettlesNetworkAttempt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	day := startOfProbeBudgetDay(testNow)
	run, lifecycles, replayed, err := store.CreateModelProbeRunWithBudget(ctx, ModelProbeRunInput{
		RunKey: "manual:budget-lifecycle", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 2, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{
		{ModelName: "image-model", Capability: "unsupported"},
		{ModelName: "gpt-test", Capability: "chat_completions"},
	}, day, 1)
	if err != nil || replayed || len(lifecycles) != 2 {
		t.Fatalf("CreateModelProbeRunWithBudget() = run:%+v lifecycles:%+v replayed:%t err:%v", run, lifecycles, replayed, err)
	}
	if lifecycles[0].LifecycleState != "skipped" || lifecycles[0].ReservedAt != nil ||
		lifecycles[1].LifecycleState != "reserved" || lifecycles[1].ReservedAt == nil {
		t.Fatalf("initial lifecycle truth = %+v", lifecycles)
	}
	if used, err := store.CountModelProbeBudgetUsed(ctx, day); err != nil || used != 1 {
		t.Fatalf("initial budget used = %d, %v", used, err)
	}

	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		LifecycleID: lifecycles[0].ID, RunID: run.ID, ModelName: "image-model", Capability: "unsupported",
		Outcome: "skipped", ErrorCode: "unsupported_capability", ErrorMessage: "Model capability is intentionally not probed",
		StartedAt: testNow, FinishedAt: testNow,
	}); err != nil {
		t.Fatalf("persist skipped diagnostic: %v", err)
	}
	if used, err := store.CountModelProbeBudgetUsed(ctx, day); err != nil || used != 1 {
		t.Fatalf("skipped diagnostic changed budget = %d, %v", used, err)
	}

	if _, err := store.MarkModelProbeAttemptSent(ctx, lifecycles[1].ID, testNow.Add(time.Millisecond)); err != nil {
		t.Fatalf("MarkModelProbeAttemptSent() error = %v", err)
	}
	semantic := true
	if _, err := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		LifecycleID: lifecycles[1].ID, RunID: run.ID, ModelName: "gpt-test", Capability: "chat_completions",
		Endpoint: "/v1/chat/completions", Outcome: "success", ProtocolSuccess: true, SemanticSuccess: &semantic,
		HTTPStatus: 200, TotalLatencyMS: 2, StartedAt: testNow.Add(time.Millisecond), FinishedAt: testNow.Add(3 * time.Millisecond),
	}); err != nil {
		t.Fatalf("settle network attempt: %v", err)
	}
	settled, err := store.ListModelProbeAttemptLifecycles(ctx, run.ID)
	if err != nil || settled[0].LifecycleState != "skipped" || settled[0].AttemptID == nil ||
		settled[1].LifecycleState != "settled" || settled[1].AttemptID == nil {
		t.Fatalf("terminal lifecycle truth = %+v, %v", settled, err)
	}

	_, skippedOnly, _, err := store.CreateModelProbeRunWithBudget(ctx, ModelProbeRunInput{
		RunKey: "manual:skipped-does-not-consume", RequestFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{{ModelName: "audio-model", Capability: "unsupported"}}, day, 1)
	if err != nil || len(skippedOnly) != 1 || skippedOnly[0].LifecycleState != "skipped" {
		t.Fatalf("skipped-only run was budget-blocked: %+v, %v", skippedOnly, err)
	}
}

func TestModelProbeSentPersistenceFailureStaysBudgetedAsUncertain(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	day := startOfProbeBudgetDay(testNow)
	run, lifecycles, _, err := store.CreateModelProbeRunWithBudget(ctx, ModelProbeRunInput{
		RunKey: "manual:uncertain-result", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{{ModelName: "gpt-test", Capability: "chat_completions"}}, day, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkModelProbeAttemptSent(ctx, lifecycles[0].ID, testNow.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_probe_attempt_persistence
		BEFORE INSERT ON model_probe_attempts BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	semantic := true
	_, appendErr := store.AppendModelProbeAttempt(ctx, ModelProbeAttemptInput{
		LifecycleID: lifecycles[0].ID, RunID: run.ID, ModelName: "gpt-test", Capability: "chat_completions",
		Endpoint: "/v1/chat/completions", Outcome: "success", ProtocolSuccess: true, SemanticSuccess: &semantic,
		HTTPStatus: 200, TotalLatencyMS: 2, StartedAt: testNow.Add(time.Millisecond), FinishedAt: testNow.Add(3 * time.Millisecond),
	})
	if appendErr == nil {
		t.Fatal("injected result persistence failure unexpectedly succeeded")
	}
	if _, err := store.MarkModelProbeAttemptUncertain(ctx, lifecycles[0].ID,
		"result_persistence_uncertain", "network result could not settle", testNow.Add(4*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListModelProbeAttemptLifecycles(ctx, run.ID)
	if err != nil || items[0].LifecycleState != "uncertain" || items[0].AttemptID != nil {
		t.Fatalf("uncertain lifecycle = %+v, %v", items, err)
	}
	if used, err := store.CountModelProbeBudgetUsed(ctx, day); err != nil || used != 1 {
		t.Fatalf("uncertain attempt budget = %d, %v", used, err)
	}
	if _, _, _, err := store.CreateModelProbeRunWithBudget(ctx, ModelProbeRunInput{
		RunKey: "manual:blocked-after-uncertain", RequestFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{{ModelName: "gpt-next", Capability: "chat_completions"}}, day, 1); !errors.Is(err, ErrModelProbeBudgetExceeded) {
		t.Fatalf("post-uncertain budget error = %v, want ErrModelProbeBudgetExceeded", err)
	}
}

func TestModelProbeRestartMakesReservedAndSentUncertainWithoutReplay(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	started := testNow.Add(-time.Minute)
	day := startOfProbeBudgetDay(started)
	input := ModelProbeRunInput{
		RunKey: "manual:restart-recovery", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 2, StartedAt: started,
	}
	plans := []ModelProbeAttemptPlan{
		{ModelName: "gpt-one", Capability: "chat_completions"},
		{ModelName: "gpt-two", Capability: "responses"},
	}
	run, lifecycles, _, err := store.CreateModelProbeRunWithBudget(ctx, input, plans, day, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkModelProbeAttemptSent(ctx, lifecycles[1].ID, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.CancelInterruptedModelProbeRuns(ctx, testNow); err != nil || count != 1 {
		t.Fatalf("CancelInterruptedModelProbeRuns() = %d, %v", count, err)
	}
	recovered, err := store.ListModelProbeAttemptLifecycles(ctx, run.ID)
	if err != nil || recovered[0].LifecycleState != "uncertain" || recovered[0].SentAt != nil ||
		recovered[1].LifecycleState != "uncertain" || recovered[1].SentAt == nil {
		t.Fatalf("recovered lifecycle truth = %+v, %v", recovered, err)
	}
	replayedRun, replayedLifecycles, replayed, err := store.CreateModelProbeRunWithBudget(ctx, input, plans, day, 2)
	if err != nil || !replayed || replayedRun.ID != run.ID || replayedLifecycles[0].LifecycleState != "uncertain" {
		t.Fatalf("recovered replay = run:%+v lifecycles:%+v replayed:%t err:%v", replayedRun, replayedLifecycles, replayed, err)
	}
	if _, _, _, err := store.CreateModelProbeRunWithBudget(ctx, ModelProbeRunInput{
		RunKey: "manual:no-retry-after-restart", RequestFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{{ModelName: "gpt-three", Capability: "chat_completions"}}, day, 2); !errors.Is(err, ErrModelProbeBudgetExceeded) {
		t.Fatalf("restart released uncertain budget: %v", err)
	}
}

func TestModelProbeBudgetReservationSerializesIndependentStores(t *testing.T) {
	first, path := newTestStore(t)
	second, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	second.now = func() time.Time { return testNow }
	t.Cleanup(func() { _ = second.Close() })

	type outcome struct {
		run ModelProbeRun
		err error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for index, store := range []*Store{first, second} {
		wg.Add(1)
		go func(index int, store *Store) {
			defer wg.Done()
			<-start
			fingerprint := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if index == 1 {
				fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			run, _, _, err := store.CreateModelProbeRunWithBudget(context.Background(), ModelProbeRunInput{
				RunKey: "manual:concurrent-budget-" + string(rune('a'+index)), RequestFingerprint: fingerprint,
				TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
			}, []ModelProbeAttemptPlan{{ModelName: "gpt-test", Capability: "chat_completions"}}, testNow, 1)
			results <- outcome{run: run, err: err}
		}(index, store)
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, budgetBlocked := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.run.ID > 0:
			succeeded++
		case errors.Is(result.err, ErrModelProbeBudgetExceeded):
			budgetBlocked++
		default:
			t.Fatalf("unexpected concurrent reservation outcome: run=%+v err=%v", result.run, result.err)
		}
	}
	if succeeded != 1 || budgetBlocked != 1 {
		t.Fatalf("concurrent reservations succeeded=%d budgetBlocked=%d", succeeded, budgetBlocked)
	}
	if used, err := first.CountModelProbeBudgetUsed(context.Background(), testNow); err != nil || used != 1 {
		t.Fatalf("concurrent budget used = %d, %v", used, err)
	}
}

func TestModelProbeMigrationV9BackfillsSettledAndSkippedBudgetTruth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9-probes.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(migrationLedgerCreateStatement); err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations[:9] {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range item.statements {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				t.Fatalf("apply v9 fixture migration %d: %v", item.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
			VALUES (?, ?, ?, ?)`, item.version, item.name, migrationChecksum(item), dbTime(testNow)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &Store{db: db, path: path, now: func() time.Time { return testNow }}
	run, _, err := legacy.CreateModelProbeRun(context.Background(), ModelProbeRunInput{
		RunKey: "manual:v9-backfill", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		// The third requested attempt represents the v9 crash window after a
		// request may have reached the network but before its result was stored.
		TriggerKind: "manual", Actor: "admin", RequestedCount: 3, StartedAt: testNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic := true
	if _, err := legacy.AppendModelProbeAttempt(context.Background(), ModelProbeAttemptInput{
		RunID: run.ID, ModelName: "gpt-v9", Capability: "chat_completions", Endpoint: "/v1/chat/completions",
		Outcome: "success", ProtocolSuccess: true, SemanticSuccess: &semantic, HTTPStatus: 200,
		TotalLatencyMS: 1, StartedAt: testNow, FinishedAt: testNow.Add(time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AppendModelProbeAttempt(context.Background(), ModelProbeAttemptInput{
		RunID: run.ID, ModelName: "image-v9", Capability: "unsupported", Outcome: "skipped",
		ErrorCode: "unsupported_capability", StartedAt: testNow, FinishedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Init(path)
	if err != nil {
		t.Fatalf("Init(v9 fixture) error = %v", err)
	}
	defer upgraded.Close()
	items, err := upgraded.ListModelProbeAttemptLifecycles(context.Background(), run.ID)
	if err != nil || len(items) != 3 || items[0].LifecycleState != "settled" || items[0].AttemptID == nil ||
		items[1].LifecycleState != "skipped" || items[1].AttemptID == nil ||
		items[2].LifecycleState != "uncertain" || items[2].AttemptID != nil ||
		items[2].SentAt == nil || items[2].ErrorCode != "legacy_v9_unpersisted" {
		t.Fatalf("v9 lifecycle backfill = %+v, %v", items, err)
	}
	if used, err := upgraded.CountModelProbeBudgetUsed(context.Background(), testNow); err != nil || used != 2 {
		t.Fatalf("v9 budget backfill used = %d, %v", used, err)
	}
	if cancelled, err := upgraded.CancelInterruptedModelProbeRuns(context.Background(), testNow.Add(time.Millisecond)); err != nil || cancelled != 1 {
		t.Fatalf("recover migrated v9 running run = %d, %v", cancelled, err)
	}
	if used, err := upgraded.CountModelProbeBudgetUsed(context.Background(), testNow); err != nil || used != 2 {
		t.Fatalf("v9 crash recovery released migrated debt: used=%d err=%v", used, err)
	}
	health, err := upgraded.Health(context.Background())
	if err != nil || health.SchemaVersion != latestSchemaVersion {
		t.Fatalf("upgraded health = %+v, %v", health, err)
	}
}

func TestModelProbeLifecycleRejectsIllegalTransitionAndTamperedTrigger(t *testing.T) {
	store, path := newTestStore(t)
	_, lifecycles, _, err := store.CreateModelProbeRunWithBudget(context.Background(), ModelProbeRunInput{
		RunKey: "manual:lifecycle-trigger", RequestFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TriggerKind: "manual", Actor: "admin", RequestedCount: 1, StartedAt: testNow,
	}, []ModelProbeAttemptPlan{{ModelName: "gpt-test", Capability: "chat_completions"}}, testNow, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE model_probe_attempt_lifecycles SET
		lifecycle_state = 'settled', finished_at = ?, updated_at = ? WHERE id = ?`,
		dbTime(testNow.Add(time.Second)), dbTime(testNow.Add(time.Second)), lifecycles[0].ID); err == nil {
		t.Fatal("reserved-to-settled transition unexpectedly bypassed sent/attempt evidence")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = nil
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TRIGGER model_probe_attempt_lifecycles_transition`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER model_probe_attempt_lifecycles_transition
		BEFORE UPDATE ON model_probe_attempt_lifecycles BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(path); err == nil || !strings.Contains(err.Error(),
		`schema object "model_probe_attempt_lifecycles_transition" is incompatible`) {
		t.Fatalf("Init() error = %v, want v10 lifecycle trigger tamper rejection", err)
	}
}
