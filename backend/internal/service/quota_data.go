package service

import (
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
)

var (
	quotaDataMu                sync.Mutex
	quotaDataCheckedAt         time.Time
	quotaDataAvailable         bool
	quotaDataRefreshing        bool
	quotaDataAvailabilityCheck = checkQuotaDataAvailability
)

const (
	quotaDataCoverageWindow = 30 * 24 * time.Hour
	quotaDataFreshnessLimit = 48 * time.Hour
	quotaDataCheckTTL       = 5 * time.Minute
)

// IsQuotaDataAvailable verifies that quota_data covers the same 30-day
// window used by the logs fallback and is still receiving fresh aggregates.
// A single historical row is not enough to make it the authoritative source.
func IsQuotaDataAvailable() bool {
	now := time.Now()
	quotaDataMu.Lock()
	cached := quotaDataAvailable
	if !quotaDataCheckedAt.IsZero() && now.Sub(quotaDataCheckedAt) < quotaDataCheckTTL {
		quotaDataMu.Unlock()
		return cached
	}
	if quotaDataRefreshing {
		quotaDataMu.Unlock()
		return cached
	}
	quotaDataRefreshing = true
	quotaDataMu.Unlock()

	available := false
	defer func() {
		quotaDataMu.Lock()
		quotaDataAvailable = available
		quotaDataCheckedAt = time.Now()
		quotaDataRefreshing = false
		quotaDataMu.Unlock()
	}()

	available = quotaDataAvailabilityCheck()
	return available
}

func checkQuotaDataAvailability() bool {
	db := database.Get()
	exists, err := db.TableExists("quota_data")
	if err != nil {
		logger.L.Warn("quota_data 表检查失败，使用 logs 回退查询")
		return false
	}
	if !exists {
		logger.L.System("quota_data 不存在，使用 logs 回退查询")
		return false
	}

	row, err := db.QueryOneWithTimeout(10*time.Second, `
		SELECT MIN(created_at) AS min_created_at,
			MAX(created_at) AS max_created_at,
			COALESCE(SUM(count), 0) AS request_count
		FROM quota_data`)
	if err != nil || row == nil {
		logger.L.Warn("quota_data 覆盖范围检查失败，使用 logs 回退查询")
		return false
	}

	now := time.Now().Unix()
	minCreatedAt := toInt64(row["min_created_at"])
	maxCreatedAt := toInt64(row["max_created_at"])
	requestCount := toInt64(row["request_count"])
	windowStart := now - int64(quotaDataCoverageWindow/time.Second)
	freshnessCutoff := now - int64(quotaDataFreshnessLimit/time.Second)
	if requestCount <= 0 || minCreatedAt <= 0 || minCreatedAt > windowStart || maxCreatedAt < freshnessCutoff {
		logger.L.Warn("quota_data 覆盖不完整或已过期，使用 logs 回退查询")
		return false
	}

	logger.L.System("quota_data 覆盖最近 30 天且数据新鲜，启用加速查询")
	return true
}

func resetQuotaDataAvailabilityForTesting() {
	quotaDataMu.Lock()
	defer quotaDataMu.Unlock()
	quotaDataCheckedAt = time.Time{}
	quotaDataAvailable = false
	quotaDataRefreshing = false
}
