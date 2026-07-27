package toolstore

import (
	"context"
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
