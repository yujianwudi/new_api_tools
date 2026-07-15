package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/database"
)

func installRedemptionSchema(t *testing.T) {
	t.Helper()
	db := installSQLiteForTests(t)
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`); err != nil {
		t.Fatalf("create users table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE redemptions (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		key TEXT,
		status INTEGER DEFAULT 1,
		name TEXT,
		quota INTEGER,
		created_time INTEGER,
		redeemed_time INTEGER,
		used_user_id INTEGER,
		deleted_at TEXT,
		expired_time INTEGER
	)`); err != nil {
		t.Fatalf("create redemptions table: %v", err)
	}
}

func TestListCodesUsesDatabaseStatusAsPrimaryState(t *testing.T) {
	installRedemptionSchema(t)
	db := database.Get().DB
	now := time.Now().Unix()
	rows := []struct {
		id           int
		status       int
		redeemedTime int64
		expiredTime  int64
	}{
		{id: 1, status: 1},
		{id: 2, status: 2},
		{id: 3, status: 3},
		{id: 4, status: 1, expiredTime: now - 60},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO redemptions
			(id, user_id, key, status, name, quota, created_time, redeemed_time, used_user_id, expired_time)
			VALUES (?, 1, ?, ?, 'test', 10, ?, ?, 0, ?)`, row.id, "key", row.status, row.id, row.redeemedTime, row.expiredTime); err != nil {
			t.Fatalf("insert redemption: %v", err)
		}
	}

	result, err := ListCodes(ListRedemptionParams{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListCodes returned error: %v", err)
	}
	got := make(map[int64]string, len(result.Items))
	for _, item := range result.Items {
		got[item.ID] = item.Status
	}
	want := map[int64]string{1: "unused", 2: "disabled", 3: "used", 4: "expired"}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("redemption %d status = %q, want %q", id, got[id], status)
		}
	}
}

func TestRedemptionStatisticsKeepDisabledSeparate(t *testing.T) {
	installRedemptionSchema(t)
	db := database.Get().DB
	now := time.Now().Unix()
	statements := []string{
		`INSERT INTO redemptions(id,status,quota,created_time,redeemed_time,expired_time) VALUES (1,1,10,1,0,0)`,
		`INSERT INTO redemptions(id,status,quota,created_time,redeemed_time,expired_time) VALUES (2,2,20,2,0,0)`,
		`INSERT INTO redemptions(id,status,quota,created_time,redeemed_time,expired_time) VALUES (3,3,30,3,1,0)`,
		`INSERT INTO redemptions(id,status,quota,created_time,redeemed_time,expired_time) VALUES (4,1,40,4,0,` + fmt.Sprint(now-60) + `)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("insert redemption: %v", err)
		}
	}

	stats, err := GetRedemptionStatistics("", "")
	if err != nil {
		t.Fatalf("GetRedemptionStatistics returned error: %v", err)
	}
	if stats.TotalCount != 4 || stats.UnusedCount != 1 || stats.DisabledCount != 1 || stats.UsedCount != 1 || stats.ExpiredCount != 1 {
		t.Fatalf("unexpected counts: %+v", stats)
	}
	if stats.TotalQuota != 100 || stats.UnusedQuota != 10 || stats.DisabledQuota != 20 || stats.UsedQuota != 30 || stats.ExpiredQuota != 40 {
		t.Fatalf("unexpected quotas: %+v", stats)
	}
}

func TestRedemptionStatisticsReturnZerosForEmptySelection(t *testing.T) {
	installRedemptionSchema(t)
	stats, err := GetRedemptionStatistics("", "")
	if err != nil {
		t.Fatalf("GetRedemptionStatistics returned error for empty table: %v", err)
	}
	if *stats != (RedemptionStatistics{}) {
		t.Fatalf("empty statistics = %+v, want all zeros", stats)
	}
}

func TestBuildInsertSQLUsesSafeHexTextLiterals(t *testing.T) {
	value := "abc\\'); DROP TABLE users; --"
	mysqlSQL := buildInsertSQL([]string{value}, value, []int64{1}, 2, 3, "`key`", false)
	if strings.Contains(mysqlSQL, value) || !strings.Contains(mysqlSQL, "CONVERT(X'") {
		t.Fatalf("MySQL SQL did not encode text literals: %s", mysqlSQL)
	}
	postgresSQL := buildInsertSQL([]string{value}, value, []int64{1}, 2, 3, `"key"`, true)
	if strings.Contains(postgresSQL, value) || !strings.Contains(postgresSQL, "convert_from(decode('") {
		t.Fatalf("PostgreSQL SQL did not encode text literals: %s", postgresSQL)
	}
}

func TestGenerateCodesRejectsUnsafeKeyPrefixBeforeDatabaseAccess(t *testing.T) {
	_, err := GenerateCodes(GenerateParams{
		Name:      "test",
		Count:     1,
		KeyPrefix: "VIP",
	})
	if err == nil {
		t.Fatal("GenerateCodes accepted an uppercase key prefix")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("GenerateCodes error = %q, want prefix validation error", err)
	}
}
