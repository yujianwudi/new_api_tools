package service

import (
	"errors"
	"testing"
	"time"
)

func TestDetermineDashboardScaleBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		metrics DashboardScaleMetrics
		want    DashboardSystemScale
	}{
		{name: "empty is small", metrics: DashboardScaleMetrics{}, want: DashboardScaleSmall},
		{name: "small user upper bound", metrics: DashboardScaleMetrics{TotalUsers: 999}, want: DashboardScaleSmall},
		{name: "small daily log upper bound", metrics: DashboardScaleMetrics{Logs24H: 99_999}, want: DashboardScaleSmall},
		{name: "medium by users", metrics: DashboardScaleMetrics{TotalUsers: 1_000}, want: DashboardScaleMedium},
		{name: "medium by logs", metrics: DashboardScaleMetrics{Logs24H: 100_000}, want: DashboardScaleMedium},
		{name: "medium user upper bound", metrics: DashboardScaleMetrics{TotalUsers: 9_999}, want: DashboardScaleMedium},
		{name: "medium daily log upper bound", metrics: DashboardScaleMetrics{Logs24H: 999_999}, want: DashboardScaleMedium},
		{name: "large by users", metrics: DashboardScaleMetrics{TotalUsers: 10_000}, want: DashboardScaleLarge},
		{name: "large by logs", metrics: DashboardScaleMetrics{Logs24H: 1_000_000}, want: DashboardScaleLarge},
		{name: "large user upper bound", metrics: DashboardScaleMetrics{TotalUsers: 50_000}, want: DashboardScaleLarge},
		{name: "large daily log upper bound", metrics: DashboardScaleMetrics{Logs24H: 10_000_000}, want: DashboardScaleLarge},
		{name: "xlarge by users", metrics: DashboardScaleMetrics{TotalUsers: 50_001}, want: DashboardScaleXLarge},
		{name: "xlarge by logs", metrics: DashboardScaleMetrics{Logs24H: 10_000_001}, want: DashboardScaleXLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := determineDashboardScale(tt.metrics); got != tt.want {
				t.Fatalf("determineDashboardScale(%+v) = %q, want %q", tt.metrics, got, tt.want)
			}
		})
	}
}

func TestBuildDashboardRefreshEstimate(t *testing.T) {
	info := buildDashboardSystemInfo(DashboardScaleMetrics{
		TotalUsers: 10_001,
		Logs24H:    2_000_000,
		TotalLogs:  25_000_000,
	})
	estimate := buildDashboardRefreshEstimate(info, "7d")

	if !estimate.ShowEstimate || estimate.Scale != DashboardScaleLarge {
		t.Fatalf("large system estimate was not enabled: %+v", estimate)
	}
	if estimate.EstimatedLogs != 25_000_000 || estimate.EstimatedLogsFormatted != "25.0M" {
		t.Fatalf("unexpected estimated logs: %+v", estimate)
	}
	if estimate.EstimatedSeconds != 125 || estimate.EstimatedTimeFormatted != "125~187 秒" {
		t.Fatalf("unexpected estimated duration: %+v", estimate)
	}
	if estimate.Warning == "" || estimate.TotalLogs != 25_000_000 || estimate.Logs24H != 2_000_000 {
		t.Fatalf("large estimate metadata incomplete: %+v", estimate)
	}

	small := buildDashboardSystemInfo(DashboardScaleMetrics{TotalUsers: 100, Logs24H: 500})
	smallEstimate := buildDashboardRefreshEstimate(small, "24h")
	if smallEstimate.ShowEstimate || smallEstimate.Scale != DashboardScaleSmall {
		t.Fatalf("small system should not show an estimate: %+v", smallEstimate)
	}

	medium := buildDashboardSystemInfo(DashboardScaleMetrics{TotalUsers: 9_000, Logs24H: 900_000})
	mediumEstimate := buildDashboardRefreshEstimate(medium, "14d")
	if medium.IsLargeSystem || !mediumEstimate.ShowEstimate || mediumEstimate.EstimatedLogs != 12_600_000 {
		t.Fatalf("long-period medium refresh was underestimated: info=%+v estimate=%+v", medium, mediumEstimate)
	}

	quietHistory := buildDashboardSystemInfo(DashboardScaleMetrics{
		TotalUsers: 100,
		Logs24H:    500,
		TotalLogs:  2_000_000,
	})
	quietHistoryEstimate := buildDashboardRefreshEstimate(quietHistory, "3d")
	if quietHistory.IsLargeSystem || !quietHistoryEstimate.ShowEstimate {
		t.Fatalf("large historical refresh bypassed confirmation: info=%+v estimate=%+v", quietHistory, quietHistoryEstimate)
	}
	if quietHistoryEstimate.EstimatedLogs != 2_000_000 || quietHistoryEstimate.EstimatedSeconds != 8 {
		t.Fatalf("historical workload was not used conservatively: %+v", quietHistoryEstimate)
	}

	quiet24H := buildDashboardRefreshEstimate(quietHistory, "24h")
	if quiet24H.ShowEstimate || quiet24H.EstimatedLogs != 500 {
		t.Fatalf("24h refresh should use the bounded daily workload: %+v", quiet24H)
	}
}

func TestDashboardRefreshEstimateRejectsInvalidPeriod(t *testing.T) {
	svc := &DashboardService{}
	if _, err := svc.GetDashboardRefreshEstimate("30d"); !errors.Is(err, ErrInvalidDashboardPeriod) {
		t.Fatalf("expected ErrInvalidDashboardPeriod, got %v", err)
	}
	for _, period := range []string{"24h", "3d", "7d", "14d"} {
		if !IsValidDashboardRefreshPeriod(period) {
			t.Fatalf("supported period %q was rejected", period)
		}
	}
}

func TestDashboardSystemInfoCollectsMetrics(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, deleted_at INTEGER)`)
	db.MustExec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, created_at INTEGER)`)
	db.MustExec(`INSERT INTO users(id, deleted_at) VALUES (1, NULL), (2, NULL), (3, 123)`)
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO logs(id, created_at) VALUES (1, ?), (2, ?), (3, ?)`, now, now-60, now-48*3600)

	info, err := NewDashboardService().GetDashboardSystemInfo()
	if err != nil {
		t.Fatalf("GetDashboardSystemInfo returned error: %v", err)
	}
	if info.Metrics.TotalUsers != 2 || info.Metrics.Logs24H != 2 || info.Metrics.TotalLogs != 3 {
		t.Fatalf("unexpected dashboard metrics: %+v", info.Metrics)
	}
	if info.Scale != DashboardScaleSmall || info.IsLargeSystem || info.Degraded {
		t.Fatalf("unexpected scale for test metrics: %+v", info)
	}
}

func TestDashboardSystemInfoFailsClosedOnQueryError(t *testing.T) {
	installSQLiteForTests(t) // Deliberately leave users/logs tables unavailable.

	info, err := NewDashboardService().GetDashboardSystemInfo()
	if err == nil {
		t.Fatal("expected metric query failure")
	}
	if info.Scale != DashboardScaleXLarge || !info.IsLargeSystem || !info.Degraded {
		t.Fatalf("query failure did not fail closed: %+v", info)
	}
	if info.Tips == nil || !info.Tips.RefreshWarning {
		t.Fatalf("fail-closed response is missing the refresh warning: %+v", info)
	}
}
