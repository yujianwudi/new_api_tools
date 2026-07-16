package service

import (
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
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
	if status["success_rate"] != float64(0) {
		t.Fatalf("empty model reported a non-zero success rate: %+v", status)
	}
}
