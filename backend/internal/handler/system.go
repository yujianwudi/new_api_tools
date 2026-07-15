package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
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

// GET /api/system/scale — placeholder until system_scale service is migrated
func GetSystemScale(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"scale": "medium",
			"metrics": gin.H{
				"total_users": 0,
				"total_logs":  0,
			},
			"settings": gin.H{
				"cache_ttl":                 300,
				"refresh_interval":          300,
				"frontend_refresh_interval": 60,
				"description":               "中型系统",
			},
		},
	})
}

// POST /api/system/scale/refresh
func RefreshSystemScale(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"scale":   "medium",
			"message": "Scale detection refreshed",
		},
	})
}

// GET /api/system/warmup-status
func GetWarmupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"status":   "ready",
			"progress": 100,
			"message":  "System is ready",
		},
	})
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
