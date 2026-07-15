package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

// DashboardSystemScale is the size class used by the dashboard to decide
// whether an explicit confirmation is required before expensive refreshes.
type DashboardSystemScale string

const (
	DashboardScaleSmall  DashboardSystemScale = "small"
	DashboardScaleMedium DashboardSystemScale = "medium"
	DashboardScaleLarge  DashboardSystemScale = "large"
	DashboardScaleXLarge DashboardSystemScale = "xlarge"

	dashboardMediumUserThreshold int64 = 1_000
	dashboardLargeUserThreshold  int64 = 10_000
	dashboardXLargeUserThreshold int64 = 50_000

	dashboardMediumDailyLogThreshold int64 = 100_000
	dashboardLargeDailyLogThreshold  int64 = 1_000_000
	dashboardXLargeDailyLogThreshold int64 = 10_000_000

	dashboardMetricQueryTimeout        = 10 * time.Second
	dashboardScaleCacheTTL             = time.Minute
	dashboardConfirmLogThreshold int64 = 1_000_000
	dashboardScaleCacheKey             = "dashboard:system-scale:v1"
)

var ErrInvalidDashboardPeriod = errors.New("invalid dashboard refresh period")

var dashboardRefreshPeriods = map[string]struct{}{
	"24h": {},
	"3d":  {},
	"7d":  {},
	"14d": {},
}

// DashboardScaleMetrics contains only the metrics needed by the frontend's
// SystemInfo contract and refresh estimator.
type DashboardScaleMetrics struct {
	TotalUsers int64 `json:"total_users"`
	Logs24H    int64 `json:"logs_24h"`
	TotalLogs  int64 `json:"total_logs"`
}

type DashboardCacheSettings struct {
	FrontendRefreshInterval int `json:"frontend_refresh_interval"`
	LeaderboardCacheTTL     int `json:"leaderboard_cache_ttl"`
}

type DashboardSystemTips struct {
	RefreshWarning   bool   `json:"refresh_warning"`
	Logs24HFormatted string `json:"logs_24h_formatted"`
	Message          string `json:"message"`
}

// DashboardSystemInfo matches the frontend SystemInfo interface. Degraded is
// additive metadata used to distinguish a conservative fail-closed response.
type DashboardSystemInfo struct {
	Scale            DashboardSystemScale   `json:"scale"`
	ScaleDescription string                 `json:"scale_description"`
	IsLargeSystem    bool                   `json:"is_large_system"`
	Metrics          DashboardScaleMetrics  `json:"metrics"`
	CacheTTL         int                    `json:"cache_ttl"`
	CacheSettings    DashboardCacheSettings `json:"cache_settings"`
	Tips             *DashboardSystemTips   `json:"tips,omitempty"`
	Degraded         bool                   `json:"degraded,omitempty"`
}

// DashboardRefreshEstimate matches the frontend RefreshEstimate interface.
type DashboardRefreshEstimate struct {
	ShowEstimate           bool                 `json:"show_estimate"`
	Scale                  DashboardSystemScale `json:"scale"`
	ScaleDescription       string               `json:"scale_description,omitempty"`
	Period                 string               `json:"period,omitempty"`
	EstimatedLogs          int64                `json:"estimated_logs,omitempty"`
	EstimatedLogsFormatted string               `json:"estimated_logs_formatted,omitempty"`
	EstimatedSeconds       int64                `json:"estimated_seconds,omitempty"`
	EstimatedTimeFormatted string               `json:"estimated_time_formatted,omitempty"`
	Warning                string               `json:"warning,omitempty"`
	TotalLogs              int64                `json:"total_logs,omitempty"`
	Logs24H                int64                `json:"logs_24h,omitempty"`
}

type dashboardScaleSettings struct {
	description             string
	leaderboardCacheTTL     int
	statsCacheTTL           int
	frontendRefreshInterval int
}

var dashboardSettingsByScale = map[DashboardSystemScale]dashboardScaleSettings{
	DashboardScaleSmall: {
		description:             "小型系统 (< 1000 用户)",
		leaderboardCacheTTL:     30,
		statsCacheTTL:           60,
		frontendRefreshInterval: 30,
	},
	DashboardScaleMedium: {
		description:             "中型系统 (1000-9999 用户)",
		leaderboardCacheTTL:     60,
		statsCacheTTL:           120,
		frontendRefreshInterval: 60,
	},
	DashboardScaleLarge: {
		description:             "大型系统 (10000-50000 用户)",
		leaderboardCacheTTL:     300,
		statsCacheTTL:           300,
		frontendRefreshInterval: 300,
	},
	DashboardScaleXLarge: {
		description:             "超大型系统 (> 50000 用户)",
		leaderboardCacheTTL:     600,
		statsCacheTTL:           600,
		frontendRefreshInterval: 600,
	},
}

// IsValidDashboardRefreshPeriod restricts refresh estimates to periods the
// dashboard actually supports. Keeping this explicit prevents accidental
// fallback to an underestimated default.
func IsValidDashboardRefreshPeriod(period string) bool {
	_, ok := dashboardRefreshPeriods[period]
	return ok
}

// GetDashboardSystemInfo detects the current scale from bounded database
// queries. The caller must treat any returned error conservatively.
func (s *DashboardService) GetDashboardSystemInfo() (DashboardSystemInfo, error) {
	if err := s.validateDashboardScaleSource(); err != nil {
		return FailClosedDashboardSystemInfo(), err
	}

	cacheable := !strings.Contains(strings.ToLower(s.db.DB.DriverName()), "sqlite")
	if cacheable {
		var cached DashboardSystemInfo
		if found, _ := cache.Get().GetJSON(dashboardScaleCacheKey, &cached); found {
			return cached, nil
		}
	}

	metrics, err := s.collectDashboardScaleMetrics()
	if err != nil {
		return FailClosedDashboardSystemInfo(), err
	}
	info := buildDashboardSystemInfo(metrics)
	if cacheable {
		_ = cache.Get().Set(dashboardScaleCacheKey, info, dashboardScaleCacheTTL)
	}
	return info, nil
}

// GetDashboardRefreshEstimate estimates the amount of log data scanned by a
// forced refresh. Invalid periods are rejected instead of silently using 7d.
func (s *DashboardService) GetDashboardRefreshEstimate(period string) (DashboardRefreshEstimate, error) {
	if !IsValidDashboardRefreshPeriod(period) {
		return DashboardRefreshEstimate{}, ErrInvalidDashboardPeriod
	}

	info, err := s.GetDashboardSystemInfo()
	if err != nil {
		return DashboardRefreshEstimate{}, err
	}
	return buildDashboardRefreshEstimate(info, period), nil
}

// FailClosedDashboardSystemInfo enables the large-system frontend protections
// when scale metrics cannot be trusted. Returning medium here would allow a
// refresh to bypass the confirmation and extended timeout paths.
func FailClosedDashboardSystemInfo() DashboardSystemInfo {
	settings := dashboardSettingsByScale[DashboardScaleXLarge]
	return DashboardSystemInfo{
		Scale:            DashboardScaleXLarge,
		ScaleDescription: "系统规模暂时无法确认，已启用保守保护",
		IsLargeSystem:    true,
		Metrics:          DashboardScaleMetrics{},
		CacheTTL:         settings.statsCacheTTL,
		CacheSettings: DashboardCacheSettings{
			FrontendRefreshInterval: settings.frontendRefreshInterval,
			LeaderboardCacheTTL:     settings.leaderboardCacheTTL,
		},
		Tips: &DashboardSystemTips{
			RefreshWarning:   true,
			Logs24HFormatted: "未知",
			Message:          "无法确认当前日志规模，刷新前必须进行大型系统确认",
		},
		Degraded: true,
	}
}

func (s *DashboardService) collectDashboardScaleMetrics() (DashboardScaleMetrics, error) {
	if err := s.validateDashboardScaleSource(); err != nil {
		return DashboardScaleMetrics{}, err
	}

	userRow, err := s.db.QueryOneWithTimeout(
		dashboardMetricQueryTimeout,
		"SELECT COUNT(*) AS total_users FROM users WHERE deleted_at IS NULL",
	)
	if err != nil {
		return DashboardScaleMetrics{}, fmt.Errorf("count dashboard users: %w", err)
	}
	totalUsers, err := dashboardMetricValue(userRow, "total_users")
	if err != nil {
		return DashboardScaleMetrics{}, fmt.Errorf("read dashboard user count: %w", err)
	}

	start24H := time.Now().Add(-24 * time.Hour).Unix()
	logs24HQuery := s.logDB.RebindQuery(
		"SELECT COUNT(*) AS logs_24h FROM logs WHERE created_at >= ?",
	)
	logs24HRow, err := s.logDB.QueryOneWithTimeout(dashboardMetricQueryTimeout, logs24HQuery, start24H)
	if err != nil {
		return DashboardScaleMetrics{}, fmt.Errorf("count dashboard logs for 24h: %w", err)
	}
	logs24H, err := dashboardMetricValue(logs24HRow, "logs_24h")
	if err != nil {
		return DashboardScaleMetrics{}, fmt.Errorf("read dashboard 24h log count: %w", err)
	}

	totalLogs, err := s.queryDashboardTotalLogs(logs24H)
	if err != nil {
		return DashboardScaleMetrics{}, err
	}

	return DashboardScaleMetrics{
		TotalUsers: totalUsers,
		Logs24H:    logs24H,
		TotalLogs:  totalLogs,
	}, nil
}

func (s *DashboardService) validateDashboardScaleSource() error {
	if s == nil || s.db == nil || s.db.DB == nil || s.logDB == nil || s.logDB.DB == nil {
		return errors.New("dashboard database manager unavailable")
	}

	logStatus := database.GetLogSourceStatus()
	if !logStatus.Healthy || logStatus.UsingFallback {
		return fmt.Errorf("dashboard log source is not trustworthy (mode=%s)", logStatus.Mode)
	}
	return nil
}

// queryDashboardTotalLogs prefers engine metadata so a scale check does not
// scan a very large logs table. MAX(id) is an intentionally conservative
// fallback: it is index-friendly and any gaps only over-estimate the row count.
func (s *DashboardService) queryDashboardTotalLogs(logs24H int64) (int64, error) {
	maxRow, err := s.logDB.QueryOneWithTimeout(
		dashboardMetricQueryTimeout,
		"SELECT COALESCE(MAX(id), 0) AS total_logs FROM logs",
	)
	if err != nil {
		return 0, fmt.Errorf("estimate dashboard total logs: %w", err)
	}
	maxID, err := dashboardMetricValue(maxRow, "total_logs")
	if err != nil {
		return 0, fmt.Errorf("read dashboard maximum log id: %w", err)
	}
	conservativeTotal := maxID
	if conservativeTotal < logs24H {
		conservativeTotal = logs24H
	}

	driverName := strings.ToLower(s.logDB.DB.DriverName())
	var approximateQuery string
	switch {
	case s.logDB.IsPG:
		approximateQuery = "SELECT COALESCE(reltuples::bigint, 0) AS total_logs FROM pg_class WHERE oid = to_regclass('logs')"
	case strings.Contains(driverName, "mysql"):
		approximateQuery = "SELECT COALESCE(TABLE_ROWS, 0) AS total_logs FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'logs'"
	}

	if approximateQuery != "" {
		row, queryErr := s.logDB.QueryOneWithTimeout(dashboardMetricQueryTimeout, approximateQuery)
		if queryErr == nil && row != nil {
			value, valueErr := dashboardMetricValue(row, "total_logs")
			if valueErr == nil && value > conservativeTotal {
				conservativeTotal = value
			}
		}
	}
	return conservativeTotal, nil
}

func dashboardMetricValue(row map[string]interface{}, key string) (int64, error) {
	if row == nil {
		return 0, errors.New("query returned no row")
	}
	value, ok := row[key]
	if !ok || value == nil {
		return 0, errors.New("query returned no value")
	}
	count := toInt64(value)
	if count < 0 {
		return 0, errors.New("query returned a negative count")
	}
	return count, nil
}

func determineDashboardScale(metrics DashboardScaleMetrics) DashboardSystemScale {
	switch {
	case metrics.TotalUsers > dashboardXLargeUserThreshold || metrics.Logs24H > dashboardXLargeDailyLogThreshold:
		return DashboardScaleXLarge
	case metrics.TotalUsers >= dashboardLargeUserThreshold || metrics.Logs24H >= dashboardLargeDailyLogThreshold:
		return DashboardScaleLarge
	case metrics.TotalUsers >= dashboardMediumUserThreshold || metrics.Logs24H >= dashboardMediumDailyLogThreshold:
		return DashboardScaleMedium
	default:
		return DashboardScaleSmall
	}
}

func buildDashboardSystemInfo(metrics DashboardScaleMetrics) DashboardSystemInfo {
	scale := determineDashboardScale(metrics)
	settings := dashboardSettingsByScale[scale]
	info := DashboardSystemInfo{
		Scale:            scale,
		ScaleDescription: settings.description,
		IsLargeSystem:    scale == DashboardScaleLarge || scale == DashboardScaleXLarge,
		Metrics:          metrics,
		CacheTTL:         settings.statsCacheTTL,
		CacheSettings: DashboardCacheSettings{
			FrontendRefreshInterval: settings.frontendRefreshInterval,
			LeaderboardCacheTTL:     settings.leaderboardCacheTTL,
		},
	}
	if info.IsLargeSystem {
		logsFormatted := formatDashboardCount(metrics.Logs24H, false)
		info.Tips = &DashboardSystemTips{
			RefreshWarning:   true,
			Logs24HFormatted: logsFormatted,
			Message:          fmt.Sprintf("当前系统日均 %s 条日志，强制刷新可能需要较长时间", logsFormatted),
		}
	}
	return info
}

func buildDashboardRefreshEstimate(info DashboardSystemInfo, period string) DashboardRefreshEstimate {
	estimatedLogs := estimateDashboardLogs(info.Metrics.Logs24H, period)
	// A quiet latest 24-hour window can substantially understate a multi-day
	// refresh after an earlier traffic spike. TotalLogs is already a
	// conservative estimate (MAX(id) or engine metadata), so use it as the
	// workload floor for 3d-14d refreshes. This deliberately favors showing the
	// confirmation over letting a large historical scan bypass it.
	if period != "24h" && info.Metrics.TotalLogs > estimatedLogs {
		estimatedLogs = info.Metrics.TotalLogs
	}
	estimate := DashboardRefreshEstimate{
		ShowEstimate:           false,
		Scale:                  info.Scale,
		ScaleDescription:       info.ScaleDescription,
		Period:                 period,
		EstimatedLogs:          estimatedLogs,
		EstimatedLogsFormatted: formatDashboardCount(estimatedLogs, true),
	}
	if !info.IsLargeSystem && estimatedLogs < dashboardConfirmLogThreshold {
		return estimate
	}

	estimatedSeconds := estimateDashboardQuerySeconds(estimatedLogs)
	upperSeconds := saturatingRatio(estimatedSeconds, 3, 2)

	estimate.ShowEstimate = true
	estimate.EstimatedSeconds = estimatedSeconds
	estimate.EstimatedTimeFormatted = fmt.Sprintf("%d~%d 秒", estimatedSeconds, upperSeconds)
	estimate.TotalLogs = info.Metrics.TotalLogs
	estimate.Logs24H = info.Metrics.Logs24H
	if estimatedLogs >= dashboardConfirmLogThreshold {
		estimate.Warning = "刷新过程中数据库负载会升高，请在低峰期执行"
	}
	return estimate
}

func estimateDashboardLogs(logs24H int64, period string) int64 {
	switch period {
	case "24h":
		return logs24H
	case "3d":
		return saturatingMultiply(logs24H, 3)
	case "7d":
		return saturatingMultiply(logs24H, 7)
	case "14d":
		return saturatingMultiply(logs24H, 14)
	default:
		return 0
	}
}

func estimateDashboardQuerySeconds(estimatedLogs int64) int64 {
	switch {
	case estimatedLogs > 5_000_000:
		return saturatingRatio(estimatedLogs, 5, 1_000_000)
	case estimatedLogs > 1_000_000:
		return saturatingRatio(estimatedLogs, 4, 1_000_000)
	case estimatedLogs > 100_000:
		seconds := saturatingRatio(estimatedLogs, 3, 200_000)
		if seconds < 3 {
			return 3
		}
		return seconds
	default:
		return 2
	}
}

func saturatingMultiply(value, multiplier int64) int64 {
	if value <= 0 || multiplier <= 0 {
		return 0
	}
	if value > math.MaxInt64/multiplier {
		return math.MaxInt64
	}
	return value * multiplier
}

func saturatingRatio(value, numerator, denominator int64) int64 {
	if value <= 0 || numerator <= 0 || denominator <= 0 {
		return 0
	}
	whole := saturatingMultiply(value/denominator, numerator)
	fraction := saturatingMultiply(value%denominator, numerator) / denominator
	if whole > math.MaxInt64-fraction {
		return math.MaxInt64
	}
	return whole + fraction
}

func formatDashboardCount(value int64, oneDecimal bool) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	case value >= 1_000:
		if oneDecimal {
			return fmt.Sprintf("%.1fK", float64(value)/1_000)
		}
		return fmt.Sprintf("%.0fK", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}
