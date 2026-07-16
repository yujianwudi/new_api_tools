package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func newChannelQualityTestService(
	t *testing.T,
	now time.Time,
	status database.LogSourceStatus,
) (*sqlx.DB, *ChannelQualityService) {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open channel quality test database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE logs (
		channel_id INTEGER,
		type INTEGER,
		quota INTEGER,
		created_at INTEGER,
		use_time REAL
	)`)
	manager := &database.Manager{DB: db, IsPG: false}
	database.SetForTesting(manager)
	database.SetLogForTesting(manager, status)
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	service := NewChannelQualityService()
	service.now = func() time.Time { return now }
	return db, service
}

func insertChannelQualityLog(
	t *testing.T,
	db *sqlx.DB,
	channelID, logType, quota, createdAt int64,
	useTime float64,
) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO logs(channel_id, type, quota, created_at, use_time) VALUES (?, ?, ?, ?, ?)`,
		channelID,
		logType,
		quota,
		createdAt,
		useTime,
	); err != nil {
		t.Fatalf("insert channel quality log: %v", err)
	}
}

func TestChannelQualityAggregatesObservedChannelMetrics(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	checkedAt := now.Add(-time.Minute)
	db, service := newChannelQualityTestService(t, now, database.LogSourceStatus{
		Mode:          database.LogSourceModeFallback,
		Configured:    true,
		Healthy:       false,
		UsingFallback: true,
		CheckedAt:     checkedAt,
	})

	for index := 0; index < 100; index++ {
		logType := int64(2)
		if index >= 95 {
			logType = 5
		}
		insertChannelQualityLog(t, db, 3, logType, 1, now.Unix()-int64(index), float64(index+1))
	}
	for index := 0; index < 25; index++ {
		logType := int64(2)
		if index >= 20 {
			logType = 5
		}
		insertChannelQualityLog(t, db, 2, logType, 2, now.Unix()-200-int64(index), 2)
	}
	insertChannelQualityLog(t, db, 1, 2, 10, now.Unix()-10, 1)
	insertChannelQualityLog(t, db, 1, 2, 20, now.Unix()-20, 2)
	insertChannelQualityLog(t, db, 1, 2, 30, now.Unix()-30, 3)
	insertChannelQualityLog(t, db, 1, 5, 40, now.Unix()-40, 4)
	if _, err := db.Exec(
		`INSERT INTO logs(channel_id, type, quota, created_at, use_time) VALUES (?, ?, ?, ?, NULL)`,
		1,
		5,
		0,
		now.Unix()-50,
	); err != nil {
		t.Fatalf("insert missing-latency log: %v", err)
	}

	// Rows outside the selected window or outside NewAPI request result types
	// must never affect the quality snapshot.
	insertChannelQualityLog(t, db, 99, 2, 999, now.Add(-2*time.Hour).Unix(), 999)
	insertChannelQualityLog(t, db, 1, 1, 999, now.Unix(), 999)

	report, err := service.GetChannelQuality(context.Background(), "1h")
	if err != nil {
		t.Fatalf("get channel quality: %v", err)
	}
	if report.Window != "1h" || report.WindowSeconds != 3600 {
		t.Fatalf("unexpected window metadata: %#v", report)
	}
	if report.GeneratedAt != now.Unix() || report.WindowEnd != now.Unix() || report.WindowStart != now.Add(-time.Hour).Unix() {
		t.Fatalf("unexpected report timestamps: %#v", report)
	}
	if report.DataSource.Mode != database.LogSourceModeFallback || !report.DataSource.Fallback || report.DataSource.Healthy {
		t.Fatalf("fallback source was not disclosed: %#v", report.DataSource)
	}
	if report.DataSource.CheckedAt != checkedAt.Unix() {
		t.Fatalf("source checked_at = %d, want %d", report.DataSource.CheckedAt, checkedAt.Unix())
	}
	if report.Sample.SampledRows != 130 || report.Sample.Limit != ChannelQualitySampleLimit || report.Sample.LimitReached {
		t.Fatalf("unexpected sample metadata: %#v", report.Sample)
	}
	if report.LatencyUnit != "seconds" || report.SuccessRateUnit != "percent" || report.QuotaUnit != "newapi_quota" {
		t.Fatalf("metric units are ambiguous: %#v", report)
	}
	if len(report.Channels) != 3 {
		t.Fatalf("channel count = %d, want 3: %#v", len(report.Channels), report.Channels)
	}

	high := report.Channels[0]
	if high.ChannelID != 3 || high.RequestCount != 100 || high.SuccessCount != 95 || high.FailureCount != 5 {
		t.Fatalf("unexpected high-volume channel counts: %#v", high)
	}
	if high.SuccessRate != 95 || high.Quota != 100 || high.LastRequestAt != now.Unix() {
		t.Fatalf("unexpected high-volume channel totals: %#v", high)
	}
	if high.LatencySampleCount != 100 || high.AvgUseTimeSeconds != 50.5 || high.P95UseTimeSeconds != 95 {
		t.Fatalf("use_time was not aggregated in seconds: %#v", high)
	}
	if high.Confidence != "high" || high.SmallSample {
		t.Fatalf("unexpected high confidence marker: %#v", high)
	}

	medium := report.Channels[1]
	if medium.ChannelID != 2 || medium.RequestCount != 25 || medium.SuccessRate != 80 || medium.Confidence != "medium" || !medium.SmallSample {
		t.Fatalf("unexpected medium confidence channel: %#v", medium)
	}

	low := report.Channels[2]
	if low.ChannelID != 1 || low.RequestCount != 5 || low.SuccessCount != 3 || low.FailureCount != 2 || low.SuccessRate != 60 {
		t.Fatalf("unexpected low-volume channel counts: %#v", low)
	}
	if low.Quota != 100 || low.LatencySampleCount != 4 || low.AvgUseTimeSeconds != 2.5 || low.P95UseTimeSeconds != 4 {
		t.Fatalf("unexpected low-volume channel totals: %#v", low)
	}
	if low.Confidence != "low" || !low.SmallSample {
		t.Fatalf("small sample was not explicit: %#v", low)
	}
}

func TestChannelQualityRejectsUnsupportedWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, service := newChannelQualityTestService(t, now, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})

	if _, err := service.GetChannelQuality(context.Background(), "30d"); !errors.Is(err, ErrInvalidChannelQualityWindow) {
		t.Fatalf("unsupported window error = %v, want ErrInvalidChannelQualityWindow", err)
	}
	for _, window := range []string{"1h", "24h", "7d", " 7D "} {
		if !IsValidChannelQualityWindow(window) {
			t.Fatalf("valid window rejected: %q", window)
		}
	}
}

func TestChannelQualityHonorsCallerCancellation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	_, service := newChannelQualityTestService(t, now, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := service.GetChannelQuality(ctx, "24h"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query error = %v, want context.Canceled", err)
	}
}

func TestChannelQualityReportsLimitOnlyWhenSentinelRowExists(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	db, service := newChannelQualityTestService(t, now, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin sample insert transaction: %v", err)
	}
	statement, err := tx.Prepare(`INSERT INTO logs(channel_id, type, quota, created_at, use_time) VALUES (?, 2, 1, ?, 1)`)
	if err != nil {
		t.Fatalf("prepare sample insert: %v", err)
	}
	for index := 0; index < ChannelQualitySampleLimit; index++ {
		if _, err := statement.Exec(7, now.Unix()-int64(index%60)); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert sample row %d: %v", index, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close sample insert statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sample rows: %v", err)
	}

	report, err := service.GetChannelQuality(context.Background(), "1h")
	if err != nil {
		t.Fatalf("get exact-limit channel quality sample: %v", err)
	}
	if report.Sample.SampledRows != ChannelQualitySampleLimit || report.Sample.LimitReached {
		t.Fatalf("exact-limit sample incorrectly reported truncation: %#v", report.Sample)
	}
	if len(report.Channels) != 1 || report.Channels[0].RequestCount != ChannelQualitySampleLimit {
		t.Fatalf("exact-limit aggregation mismatch: %#v", report.Channels)
	}

	insertChannelQualityLog(t, db, 7, 2, 1, now.Unix(), 1)
	report, err = service.GetChannelQuality(context.Background(), "1h")
	if err != nil {
		t.Fatalf("get sentinel-capped channel quality sample: %v", err)
	}
	if report.Sample.SampledRows != ChannelQualitySampleLimit || !report.Sample.LimitReached {
		t.Fatalf("sample cap was not disclosed: %#v", report.Sample)
	}
	if len(report.Channels) != 1 || report.Channels[0].RequestCount != ChannelQualitySampleLimit {
		t.Fatalf("aggregation exceeded sample cap: %#v", report.Channels)
	}
}
