package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterSystemRoutes registers /api/system endpoints
func RegisterSystemRoutes(r *gin.RouterGroup) {
	g := r.Group("/system")
	{
		g.GET("/scale", GetSystemScale)
		g.POST("/scale/refresh", RefreshSystemScale)
		g.GET("/warmup-status", GetWarmupStatus)
		g.GET("/indexes", GetIndexStatus)
	}
}

// GET /api/system/scale
func GetSystemScale(c *gin.Context) {
	info, err := service.NewDashboardService().GetDashboardSystemInfo()
	if err != nil {
		// Scale controls refresh pressure in multiple frontend views. Preserve a
		// usable fail-closed response while truthfully marking it degraded.
		logger.L.Error(fmt.Sprintf("System scale detection failed: %v", err), logger.CatAnalytics)
		info = service.FailClosedDashboardSystemInfo()
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": systemScaleResponse(info)})
}

// POST /api/system/scale/refresh
func RefreshSystemScale(c *gin.Context) {
	svc := service.NewDashboardService()
	svc.InvalidateDashboardCache()
	info, err := svc.GetDashboardSystemInfo()
	if err != nil {
		respondHandlerError(c, http.StatusServiceUnavailable, "QUERY_ERROR", "System scale refresh unavailable", "system scale refresh", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": systemScaleResponse(info)})
}

// GET /api/system/warmup-status
func GetWarmupStatus(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NOT_IMPLEMENTED",
		"Cache warmup status is not implemented; readiness is reported by the health endpoints",
		"",
	))
}

func systemScaleResponse(info service.DashboardSystemInfo) gin.H {
	return gin.H{
		"scale":           info.Scale,
		"is_large_system": info.IsLargeSystem,
		"degraded":        info.Degraded,
		"metrics":         info.Metrics,
		"settings": gin.H{
			"cache_ttl":                 info.CacheTTL,
			"refresh_interval":          info.CacheTTL,
			"frontend_refresh_interval": info.CacheSettings.FrontendRefreshInterval,
			"leaderboard_cache_ttl":     info.CacheSettings.LeaderboardCacheTTL,
			"description":               info.ScaleDescription,
		},
	}
}

// GET /api/system/indexes
func GetIndexStatus(c *gin.Context) {
	indexResults := make([]gin.H, 0, len(database.RecommendedIndexes))
	existing := 0

	for _, idx := range database.RecommendedIndexes {
		db := database.Get()
		source := "main"
		if idx.Table == "logs" {
			db = database.GetLog()
			source = "log"
		}

		exists, err := db.IndexExists(idx.Name, idx.Table)
		if err != nil {
			logger.L.Error("Index status check failed for "+idx.Name+": "+err.Error(), logger.CatDatabase)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"success": false,
				"message": "检查索引状态失败",
			})
			return
		}
		if exists {
			existing++
		}
		indexResults = append(indexResults, gin.H{
			"name":    idx.Name,
			"table":   idx.Table,
			"columns": idx.Columns,
			"source":  source,
			"exists":  exists,
		})
	}
	total := len(database.RecommendedIndexes)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"indexes":   indexResults,
			"total":     total,
			"existing":  existing,
			"missing":   total - existing,
			"all_ready": existing == total,
		},
	})
}
