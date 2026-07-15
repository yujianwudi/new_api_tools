package service

import (
	"testing"
	"time"
)

func TestMaskTokenKeyFullyRedactsShortKeys(t *testing.T) {
	if got := MaskTokenKey("short"); got != "****" {
		t.Fatalf("MaskTokenKey(short) = %q, want full redaction", got)
	}
	if got := MaskTokenKey("1234567890"); got != "12345678****" {
		t.Fatalf("MaskTokenKey(long) = %q, want prefix mask", got)
	}
}

func TestGetTokenStatisticsUsesMutuallyExclusiveStatuses(t *testing.T) {
	db := installSQLiteForTests(t)
	if _, err := db.Exec(`CREATE TABLE tokens (
		id INTEGER PRIMARY KEY,
		status INTEGER NOT NULL,
		expired_time INTEGER,
		remain_quota INTEGER,
		unlimited_quota INTEGER,
		deleted_at TEXT
	)`); err != nil {
		t.Fatalf("create tokens table: %v", err)
	}

	now := time.Now().Unix()
	rows := []struct {
		status         int
		expiredTime    int64
		remainQuota    int64
		unlimitedQuota bool
	}{
		{status: 1, expiredTime: 0, remainQuota: 100},
		{status: 1, expiredTime: now - 60},
		{status: 2, expiredTime: 0},
		{status: 3, expiredTime: 0},
		{status: 4, expiredTime: 0},
		// With Redis enabled, NewAPI can leave exhausted tokens as status=1 in
		// the database and enforce exhaustion from cached quota state.
		{status: 1, expiredTime: 0, remainQuota: 0},
		{status: 1, expiredTime: 0, remainQuota: 0, unlimitedQuota: true},
	}
	for _, row := range rows {
		if _, err := db.Exec(
			"INSERT INTO tokens(status, expired_time, remain_quota, unlimited_quota) VALUES (?, ?, ?, ?)",
			row.status, row.expiredTime, row.remainQuota, row.unlimitedQuota,
		); err != nil {
			t.Fatalf("insert token: %v", err)
		}
	}

	stats, err := NewTokenService().GetTokenStatistics()
	if err != nil {
		t.Fatalf("GetTokenStatistics returned error: %v", err)
	}
	if stats.Total != 7 || stats.Active != 2 || stats.Disabled != 1 || stats.Expired != 2 || stats.Exhausted != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
