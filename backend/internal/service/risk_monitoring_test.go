package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

func TestUsageRiskFlagsUsesFractionalFailureRate(t *testing.T) {
	flags := usageRiskFlags(0, 0, 0.75, 20)
	if !containsRiskFlag(flags, "HIGH_FAILURE_RATE") {
		t.Fatalf("HIGH_FAILURE_RATE missing from %v", flags)
	}
	flags = usageRiskFlags(0, 0, 0.25, 20)
	if containsRiskFlag(flags, "HIGH_FAILURE_RATE") {
		t.Fatalf("HIGH_FAILURE_RATE unexpectedly present in %v", flags)
	}
}

func TestAnalyzeCheckinsUsesQuotaAwardedColumn(t *testing.T) {
	stubCheckinTableExists(t, true)
	db := installSQLiteForTests(t)
	if _, err := db.Exec(`CREATE TABLE checkins (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		quota_awarded INTEGER,
		created_at INTEGER
	)`); err != nil {
		t.Fatalf("create checkins table: %v", err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO checkins(user_id, quota_awarded, created_at) VALUES (7, 123, ?)`, now); err != nil {
		t.Fatalf("insert checkin: %v", err)
	}

	analysis, err := analyzeCheckins(NewRiskMonitoringService().db, 7, now-60, now+60)
	if err != nil {
		t.Fatalf("analyzeCheckins returned error: %v", err)
	}
	if analysis == nil || analysis.CheckinCount != 1 || analysis.TotalQuotaAwarded != 123 {
		t.Fatalf("unexpected checkin analysis: %+v", analysis)
	}
}

func TestRiskLeaderboardsDoNotCacheQueryFailures(t *testing.T) {
	if _, err := cache.Get().DeleteByPrefix("risk:"); err != nil {
		t.Fatalf("clear risk cache: %v", err)
	}
	t.Cleanup(func() { _, _ = cache.Get().DeleteByPrefix("risk:") })
	installSQLiteForTests(t)

	_, err := NewRiskMonitoringService().GetLeaderboards([]string{"1h"}, 10, "requests")
	if err == nil {
		t.Fatal("leaderboard query failure was returned as an empty success")
	}

	var cached map[string]interface{}
	if found, _ := cache.Get().GetJSON("risk:leaderboards:1h:10:requests", &cached); found {
		t.Fatal("failed leaderboard result was cached")
	}
}

func TestRiskLeaderboardsDoNotCacheUserEnrichmentFailures(t *testing.T) {
	if _, err := cache.Get().DeleteByPrefix("risk:"); err != nil {
		t.Fatalf("clear risk cache: %v", err)
	}
	t.Cleanup(func() { _, _ = cache.Get().DeleteByPrefix("risk:") })
	db := installSQLiteForTests(t)
	db.MustExec(`
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			username TEXT,
			type INTEGER,
			quota INTEGER,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			ip TEXT,
			created_at INTEGER
		);
	`)
	db.MustExec(`INSERT INTO logs (user_id, username, type, quota, prompt_tokens, completion_tokens, ip, created_at)
		VALUES (7, 'alice', 2, 10, 2, 3, '203.0.113.7', ?)`, time.Now().Unix())

	_, err := NewRiskMonitoringService().GetLeaderboards([]string{"1h"}, 10, "requests")
	if err == nil {
		t.Fatal("user enrichment failure was cached as a partial leaderboard")
	}
	if !strings.Contains(err.Error(), "user enrichment") {
		t.Fatalf("unexpected leaderboard error: %v", err)
	}

	var cached map[string]interface{}
	if found, _ := cache.Get().GetJSON("risk:leaderboards:1h:10:requests", &cached); found {
		t.Fatal("partially enriched leaderboard was cached")
	}
}

func TestRiskUserAnalysisPropagatesCoreQueryFailure(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT,
			display_name TEXT,
			email TEXT,
			status INTEGER,
			"group" TEXT,
			remark TEXT,
			linux_do_id TEXT,
			request_count INTEGER,
			deleted_at INTEGER
		);
		INSERT INTO users (id, username, status) VALUES (1, 'alice', 1);
	`)

	_, err := NewRiskMonitoringService().GetUserAnalysis(1, 3600, nil)
	if err == nil {
		t.Fatal("missing logs table was returned as zero-risk analysis")
	}
	if !strings.Contains(err.Error(), "risk summary query") {
		t.Fatalf("unexpected analysis error: %v", err)
	}
}

func TestAnalyzeCheckinsPropagatesQueryFailure(t *testing.T) {
	stubCheckinTableExists(t, true)
	db := installSQLiteForTests(t)
	db.MustExec(`
		CREATE TABLE checkins (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			created_at INTEGER
		);
	`)

	if _, err := analyzeCheckins(NewRiskMonitoringService().db, 7, 0, time.Now().Unix()); err == nil {
		t.Fatal("invalid checkins schema was treated as unavailable data")
	}
}

func TestAnalyzeCheckinsRetriesTableProbeAfterError(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`
		CREATE TABLE checkins (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			quota_awarded INTEGER,
			created_at INTEGER
		);
	`)

	original := checkinTableExistsCheck
	calls := 0
	checkinTableExistsCheck = func(*database.Manager) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("temporary metadata failure")
		}
		return true, nil
	}
	t.Cleanup(func() { checkinTableExistsCheck = original })

	if _, err := analyzeCheckins(NewRiskMonitoringService().db, 7, 0, time.Now().Unix()); err == nil {
		t.Fatal("first metadata failure was ignored")
	}
	if _, err := analyzeCheckins(NewRiskMonitoringService().db, 7, 0, time.Now().Unix()); err != nil {
		t.Fatalf("table probe was not retried after recovery: %v", err)
	}
	if calls != 2 {
		t.Fatalf("table probe calls = %d, want 2", calls)
	}
}

func stubCheckinTableExists(t *testing.T, exists bool) {
	t.Helper()
	original := checkinTableExistsCheck
	checkinTableExistsCheck = func(*database.Manager) (bool, error) {
		return exists, nil
	}
	t.Cleanup(func() { checkinTableExistsCheck = original })
}

func TestSameIPRegistrationsUsesOnlyFirstObservedIP(t *testing.T) {
	db := installSQLiteForTests(t)
	if _, err := db.Exec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		ip TEXT,
		type INTEGER,
		created_at INTEGER
	)`); err != nil {
		t.Fatalf("create logs table: %v", err)
	}
	now := time.Now().Unix()
	rows := []struct {
		userID int
		ip     string
		at     int64
	}{
		{userID: 1, ip: "203.0.113.1", at: now - 40},
		{userID: 1, ip: "203.0.113.2", at: now - 30},
		{userID: 2, ip: "203.0.113.1", at: now - 20},
		{userID: 3, ip: "203.0.113.2", at: now - 10},
	}
	for _, row := range rows {
		if _, err := db.Exec("INSERT INTO logs(user_id, ip, type, created_at) VALUES (?, ?, 2, ?)", row.userID, row.ip, row.at); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}

	result, err := NewRiskMonitoringService().GetSameIPRegistrations("1h", 2, 10)
	if err != nil {
		t.Fatalf("GetSameIPRegistrations returned error: %v", err)
	}
	items, ok := result["items"].([]map[string]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected items: %#v", result["items"])
	}
	if items[0]["first_ip"] != "203.0.113.1" || toInt64(items[0]["user_count"]) != 2 {
		t.Fatalf("unexpected first-IP aggregate: %#v", items[0])
	}
}

func containsRiskFlag(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
