package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/database"
)

const (
	ChannelQualitySampleLimit = 50000

	channelQualityMediumConfidenceMin = int64(20)
	channelQualityHighConfidenceMin   = int64(100)
)

var ErrInvalidChannelQualityWindow = errors.New("channel quality window must be one of 1h, 24h, or 7d")

var channelQualityWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

type ChannelQualityDataSource struct {
	Mode       string `json:"mode"`
	Fallback   bool   `json:"fallback"`
	Configured bool   `json:"configured"`
	Healthy    bool   `json:"healthy"`
	CheckedAt  int64  `json:"checked_at,omitempty"`
}

type ChannelQualitySample struct {
	Limit        int  `json:"limit"`
	SampledRows  int  `json:"sampled_rows"`
	LimitReached bool `json:"limit_reached"`
}

type ChannelQualityConfidencePolicy struct {
	Basis             string `json:"basis"`
	MediumMinRequests int64  `json:"medium_min_requests"`
	HighMinRequests   int64  `json:"high_min_requests"`
}

type ChannelQualityMetric struct {
	ChannelID          int64   `json:"channel_id"`
	RequestCount       int64   `json:"request_count"`
	SuccessCount       int64   `json:"success_count"`
	FailureCount       int64   `json:"failure_count"`
	SuccessRate        float64 `json:"success_rate"`
	Quota              int64   `json:"quota"`
	LastRequestAt      int64   `json:"last_request_at"`
	LatencySampleCount int64   `json:"latency_sample_count"`
	AvgUseTimeSeconds  float64 `json:"avg_use_time_seconds"`
	P95UseTimeSeconds  float64 `json:"p95_use_time_seconds"`
	Confidence         string  `json:"confidence"`
	SmallSample        bool    `json:"small_sample"`
}

type ChannelQualityReport struct {
	Window           string                         `json:"window"`
	WindowSeconds    int64                          `json:"window_seconds"`
	WindowStart      int64                          `json:"window_start"`
	WindowEnd        int64                          `json:"window_end"`
	GeneratedAt      int64                          `json:"generated_at"`
	SuccessRateUnit  string                         `json:"success_rate_unit"`
	LatencyUnit      string                         `json:"latency_unit"`
	QuotaUnit        string                         `json:"quota_unit"`
	DataSource       ChannelQualityDataSource       `json:"data_source"`
	Sample           ChannelQualitySample           `json:"sample"`
	ConfidencePolicy ChannelQualityConfidencePolicy `json:"confidence_policy"`
	Channels         []ChannelQualityMetric         `json:"channels"`
}

type ChannelQualityService struct {
	logDB        *database.Manager
	now          func() time.Time
	sourceStatus func() database.LogSourceStatus
}

type channelQualityAggregate struct {
	channelID     int64
	requestCount  int64
	successCount  int64
	failureCount  int64
	quota         int64
	lastRequestAt int64
	useTimeSum    float64
	useTimes      []float64
}

// NewChannelQualityService uses the selected log source. The control tower is
// deliberately read-only and remains usable when a dedicated log DB has
// degraded to the main-database fallback.
func NewChannelQualityService() *ChannelQualityService {
	return &ChannelQualityService{
		logDB:        database.GetLog(),
		now:          time.Now,
		sourceStatus: database.GetLogSourceStatus,
	}
}

func IsValidChannelQualityWindow(window string) bool {
	_, ok := channelQualityWindows[normalizeChannelQualityWindow(window)]
	return ok
}

// GetChannelQuality returns observed channel quality only. It does not make
// routing, banning, profitability, or other mutation decisions.
func (s *ChannelQualityService) GetChannelQuality(ctx context.Context, window string) (*ChannelQualityReport, error) {
	if ctx == nil {
		return nil, errors.New("channel quality context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.logDB == nil || s.logDB.DB == nil {
		return nil, errors.New("channel quality log database is unavailable")
	}

	normalizedWindow := normalizeChannelQualityWindow(window)
	duration, ok := channelQualityWindows[normalizedWindow]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidChannelQualityWindow, window)
	}

	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	windowEnd := now.Unix()
	windowStart := now.Add(-duration).Unix()
	query := s.logDB.RebindQuery(`
		SELECT channel_id, type, quota, created_at, use_time
		FROM logs
		WHERE created_at >= ? AND created_at <= ?
			AND type IN (2, 5) AND channel_id > 0
		ORDER BY created_at DESC, channel_id ASC
		LIMIT ?`)

	rows, err := s.logDB.QueryContext(ctx, query, windowStart, windowEnd, ChannelQualitySampleLimit+1)
	if err != nil {
		return nil, fmt.Errorf("query channel quality logs: %w", err)
	}
	limitReached := len(rows) > ChannelQualitySampleLimit
	if limitReached {
		rows = rows[:ChannelQualitySampleLimit]
	}

	aggregates := make(map[int64]*channelQualityAggregate)
	for index, row := range rows {
		if index%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		channelID := channelQualityInt64(row["channel_id"])
		logType := channelQualityInt64(row["type"])
		if channelID <= 0 || (logType != 2 && logType != 5) {
			continue
		}

		aggregate := aggregates[channelID]
		if aggregate == nil {
			aggregate = &channelQualityAggregate{channelID: channelID}
			aggregates[channelID] = aggregate
		}
		aggregate.requestCount++
		if logType == 2 {
			aggregate.successCount++
		} else {
			aggregate.failureCount++
		}
		aggregate.quota += channelQualityInt64(row["quota"])
		createdAt := channelQualityInt64(row["created_at"])
		if createdAt > aggregate.lastRequestAt {
			aggregate.lastRequestAt = createdAt
		}
		// NewAPI records use_time in seconds. Include both success and failure
		// observations, but do not turn missing/invalid latency into a zero.
		useTime, validUseTime := channelQualityFloat64(row["use_time"])
		if validUseTime && !math.IsNaN(useTime) && !math.IsInf(useTime, 0) && useTime >= 0 {
			aggregate.useTimeSum += useTime
			aggregate.useTimes = append(aggregate.useTimes, useTime)
		}
	}

	channels := make([]ChannelQualityMetric, 0, len(aggregates))
	for _, aggregate := range aggregates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		successRate := float64(0)
		if aggregate.requestCount > 0 {
			successRate = float64(aggregate.successCount) / float64(aggregate.requestCount) * 100
		}
		averageUseTime := float64(0)
		if len(aggregate.useTimes) > 0 {
			averageUseTime = aggregate.useTimeSum / float64(len(aggregate.useTimes))
		}
		confidence, smallSample := channelQualityConfidence(aggregate.requestCount)
		channels = append(channels, ChannelQualityMetric{
			ChannelID:          aggregate.channelID,
			RequestCount:       aggregate.requestCount,
			SuccessCount:       aggregate.successCount,
			FailureCount:       aggregate.failureCount,
			SuccessRate:        roundChannelQuality(successRate, 2),
			Quota:              aggregate.quota,
			LastRequestAt:      aggregate.lastRequestAt,
			LatencySampleCount: int64(len(aggregate.useTimes)),
			AvgUseTimeSeconds:  roundChannelQuality(averageUseTime, 4),
			P95UseTimeSeconds:  roundChannelQuality(channelQualityP95(aggregate.useTimes), 4),
			Confidence:         confidence,
			SmallSample:        smallSample,
		})
	}
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].RequestCount == channels[j].RequestCount {
			return channels[i].ChannelID < channels[j].ChannelID
		}
		return channels[i].RequestCount > channels[j].RequestCount
	})

	status := database.GetLogSourceStatus()
	if s.sourceStatus != nil {
		status = s.sourceStatus()
	}
	checkedAt := int64(0)
	if !status.CheckedAt.IsZero() {
		checkedAt = status.CheckedAt.Unix()
	}

	return &ChannelQualityReport{
		Window:          normalizedWindow,
		WindowSeconds:   int64(duration.Seconds()),
		WindowStart:     windowStart,
		WindowEnd:       windowEnd,
		GeneratedAt:     windowEnd,
		SuccessRateUnit: "percent",
		LatencyUnit:     "seconds",
		QuotaUnit:       "newapi_quota",
		DataSource: ChannelQualityDataSource{
			Mode:       status.Mode,
			Fallback:   status.UsingFallback,
			Configured: status.Configured,
			Healthy:    status.Healthy,
			CheckedAt:  checkedAt,
		},
		Sample: ChannelQualitySample{
			Limit:        ChannelQualitySampleLimit,
			SampledRows:  len(rows),
			LimitReached: limitReached,
		},
		ConfidencePolicy: ChannelQualityConfidencePolicy{
			Basis:             "sampled_request_count",
			MediumMinRequests: channelQualityMediumConfidenceMin,
			HighMinRequests:   channelQualityHighConfidenceMin,
		},
		Channels: channels,
	}, nil
}

func normalizeChannelQualityWindow(window string) string {
	return strings.ToLower(strings.TrimSpace(window))
}

func channelQualityConfidence(requestCount int64) (string, bool) {
	switch {
	case requestCount >= channelQualityHighConfidenceMin:
		return "high", false
	case requestCount >= channelQualityMediumConfidenceMin:
		return "medium", true
	default:
		return "low", true
	}
}

func channelQualityP95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(float64(len(sorted))*0.95)) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func roundChannelQuality(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}

func channelQualityInt64(value interface{}) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		if uint64(typed) <= math.MaxInt64 {
			return int64(typed)
		}
	case uint8:
		return int64(typed)
	case uint16:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed <= math.MaxInt64 {
			return int64(typed)
		}
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return 0
		}
		return int64(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0
		}
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed
	case []byte:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(string(typed)), 10, 64)
		return parsed
	}
	return 0
}

func channelQualityFloat64(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	case []byte:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(string(typed)), 64)
		return parsed, err == nil
	}
	return 0, false
}
