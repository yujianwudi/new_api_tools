package service

import (
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
)

func TestGetModelStatusPropagatesLogQueryErrors(t *testing.T) {
	installSQLiteForTests(t)
	cache.Get().ClearLocal()

	svc := NewModelStatusService()
	if _, err := svc.GetModelStatus("missing-logs-table", "1h"); err == nil {
		t.Fatal("expected missing logs table error to be propagated")
	}
}

func TestGetModelStatusTreatsZeroCompletionConsumeAsSuccessfulEmbedding(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE logs (
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		completion_tokens INTEGER
	)`)
	db.MustExec(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES ('text-embedding-3-small', ?, 2, 0)`, time.Now().Unix()-1)
	cache.Get().ClearLocal()

	status, err := NewModelStatusService().GetModelStatus("text-embedding-3-small", "1h")
	if err != nil {
		t.Fatalf("embedding model status returned error: %v", err)
	}
	if status["total_requests"] != int64(1) || status["success_count"] != int64(1) || status["failure_count"] != int64(0) {
		t.Fatalf("zero-completion consume was not counted as success: %+v", status)
	}
	if status["empty_count"] != int64(1) || status["success_rate"] != float64(100) || status["current_status"] != "green" {
		t.Fatalf("empty diagnostic changed embedding availability: %+v", status)
	}
}

func TestGetModelStatusUsesTypeFiveAsFailureForNonGenerativeModel(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE logs (
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		completion_tokens INTEGER
	)`)
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES ('rerank-model', ?, 2, 0), ('rerank-model', ?, 5, 0)`, now-2, now-1)
	cache.Get().ClearLocal()

	status, err := NewModelStatusService().GetModelStatus("rerank-model", "1h")
	if err != nil {
		t.Fatalf("rerank model status returned error: %v", err)
	}
	if status["total_requests"] != int64(2) || status["success_count"] != int64(1) || status["failure_count"] != int64(1) {
		t.Fatalf("consume/failure log semantics are incorrect: %+v", status)
	}
	if status["empty_count"] != int64(1) || status["success_rate"] != float64(50) {
		t.Fatalf("empty diagnostic was treated as an additional failure: %+v", status)
	}
}

func TestGetModelStatusMarksSuccessfulEmptyQueryUnknown(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE logs (
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		completion_tokens INTEGER
	)`)
	cache.Get().ClearLocal()

	svc := NewModelStatusService()
	status, err := svc.GetModelStatus("unused-model", "1h")
	if err != nil {
		t.Fatalf("empty model query returned error: %v", err)
	}
	if status["current_status"] != "unknown" {
		t.Fatalf("empty model was not marked unknown: %+v", status)
	}
	if status["success_rate"] != nil || status["source_state"] != "empty" || status["traffic_health"] != "unknown" {
		t.Fatalf("empty model reported numeric or authoritative health: %+v", status)
	}
}

func TestGetModelStatusMakesStaleHistoryNonAuthoritative(t *testing.T) {
	db := installSQLiteForTests(t)
	if cfg := config.GetOptional(); cfg != nil {
		previous := cfg.LogFreshnessMaxAge
		cfg.LogFreshnessMaxAge = 15 * time.Minute
		t.Cleanup(func() { cfg.LogFreshnessMaxAge = previous })
	}
	db.MustExec(`CREATE TABLE logs (
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		completion_tokens INTEGER
	)`)
	db.MustExec(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES ('stale-green-model', ?, 2, 1)`, time.Now().Add(-20*time.Minute).Unix())
	cache.Get().ClearLocal()

	status, err := NewModelStatusService().GetModelStatus("stale-green-model", "1h")
	if err != nil {
		t.Fatalf("stale model status returned error: %v", err)
	}
	if status["source_state"] != "stale" || status["current_status"] != "unknown" ||
		status["traffic_health"] != "unknown" || status["success_rate"] != nil {
		t.Fatalf("stale history remained authoritative: %+v", status)
	}
	if status["observed_status"] != "green" || status["observed_rate"] != float64(100) {
		t.Fatalf("stale diagnostic evidence was not preserved: %+v", status)
	}
	if _, ok := status["fetched_at"].(string); !ok {
		t.Fatalf("status snapshot is missing fetched_at: %+v", status)
	}
}

func TestGetAvailableModelsIncludesCatalogModelsWithoutTraffic(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE logs (
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		completion_tokens INTEGER
	)`)
	db.MustExec(`CREATE TABLE abilities (model TEXT)`)
	db.MustExec(`INSERT INTO abilities(model) VALUES ('gpt-catalog-only'), ('gpt-observed')`)
	db.MustExec(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES ('gpt-observed', ?, 2, 1)`, time.Now().Unix()-1)
	cache.Get().ClearLocal()

	models, err := NewModelStatusService().GetAvailableModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0]["model_name"] != "gpt-observed" || models[0]["catalog_state"] != "catalog_and_observed" {
		t.Fatalf("observed catalog merge = %+v", models)
	}
	if models[1]["model_name"] != "gpt-catalog-only" || models[1]["request_count_24h"] != int64(0) || models[1]["catalog_state"] != "catalog_only" {
		t.Fatalf("catalog-only model missing = %+v", models)
	}
}
