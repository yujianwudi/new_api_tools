package service

import (
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
)

func TestGetUsageStatisticsConvertsNewAPIUseTimeSecondsToMilliseconds(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE logs (
		created_at INTEGER,
		type INTEGER,
		quota INTEGER,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		use_time REAL
	)`)
	db.MustExec(`INSERT INTO logs
		(created_at, type, quota, prompt_tokens, completion_tokens, use_time)
		VALUES (?, 2, 10, 2, 0, 1), (?, 2, 20, 3, 0, 2)`,
		time.Now().Unix()-2, time.Now().Unix()-1)
	cache.Get().ClearLocal()

	usage, err := NewDashboardService().GetUsageStatistics("24h", true)
	if err != nil {
		t.Fatalf("GetUsageStatistics returned error: %v", err)
	}
	if got := usage["average_response_time_ms"]; got != float64(1500) {
		t.Fatalf("average_response_time_ms = %#v, want 1500", got)
	}
	if got := usage["average_response_time"]; got != float64(1500) {
		t.Fatalf("backward-compatible average_response_time = %#v, want 1500 milliseconds", got)
	}
}
