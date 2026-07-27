package modelprobe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
