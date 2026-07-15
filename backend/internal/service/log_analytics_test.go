package service

import (
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
)

func TestQuotaDataRankingsSumAggregatedRequestCounts(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT
	)`)
	db.MustExec(`CREATE TABLE quota_data (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		created_at INTEGER,
		count INTEGER,
		quota INTEGER
	)`)
	db.MustExec("INSERT INTO users(id, username) VALUES (1, 'alice'), (2, 'bob')")
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO quota_data(user_id, created_at, count, quota) VALUES
		(1, ?, 1000, 5000),
		(2, ?, 1, 9000),
		(2, ?, 1, 9000)`, now, now, now)

	quotaDataMu.Lock()
	quotaDataCheckedAt = time.Now()
	quotaDataAvailable = true
	quotaDataRefreshing = false
	quotaDataMu.Unlock()
	t.Cleanup(resetQuotaDataAvailabilityForTesting)
	cache.Get().ClearLocal()

	svc := NewLogAnalyticsService()
	requestRanking, err := svc.GetUserRequestRanking(10)
	if err != nil {
		t.Fatalf("GetUserRequestRanking returned error: %v", err)
	}
	if len(requestRanking) != 2 || toInt64(requestRanking[0]["user_id"]) != 1 || toInt64(requestRanking[0]["request_count"]) != 1000 {
		t.Fatalf("request ranking did not sum quota_data.count: %+v", requestRanking)
	}

	quotaRanking, err := svc.GetUserQuotaRanking(10)
	if err != nil {
		t.Fatalf("GetUserQuotaRanking returned error: %v", err)
	}
	if len(quotaRanking) != 2 || toInt64(quotaRanking[0]["user_id"]) != 2 || toInt64(quotaRanking[0]["request_count"]) != 2 {
		t.Fatalf("quota ranking did not preserve aggregated request count: %+v", quotaRanking)
	}
}
