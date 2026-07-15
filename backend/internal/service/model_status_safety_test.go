package service

import (
	"testing"

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
