package handler

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterDashboardRoutes registers /api/dashboard endpoints
func RegisterDashboardRoutes(r *gin.RouterGroup) {
	g := r.Group("/dashboard")
	{
		g.GET("/overview", GetSystemOverview)
		g.GET("/usage", GetUsageStatistics)
		g.GET("/models", GetModelUsage)
		g.GET("/trends/daily", GetDailyTrends)
		g.GET("/trends/hourly", GetHourlyTrends)
		g.GET("/top-users", GetTopUsers)
		g.GET("/channels", GetChannelStatus)
		g.POST("/cache/invalidate", InvalidateDashboardCache)
		g.GET("/refresh-estimate", GetRefreshEstimate)
		g.GET("/system-info", GetDashboardSystemInfo)
		g.GET("/ip-distribution", GetIPDistribution)
	}
}

// GET /api/dashboard/overview
func GetSystemOverview(c *gin.Context) {
	period := c.DefaultQuery("period", "7d")
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetSystemOverview(period, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/usage
func GetUsageStatistics(c *gin.Context) {
	period := c.DefaultQuery("period", "24h")
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetUsageStatistics(period, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/models
func GetModelUsage(c *gin.Context) {
	period := c.DefaultQuery("period", "7d")
	limit := parseLimit(c, 10, 200)
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetModelUsage(period, limit, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/trends/daily
func GetDailyTrends(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	days = clampInt(days, 1, 90)
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetDailyTrends(days, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/trends/hourly
func GetHourlyTrends(c *gin.Context) {
	hours, _ := strconv.Atoi(c.DefaultQuery("hours", "24"))
	hours = clampInt(hours, 1, 168)
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetHourlyTrends(hours, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/top-users
func GetTopUsers(c *gin.Context) {
	period := c.DefaultQuery("period", "7d")
	limit := parseLimit(c, 10, 200)
	noCache := c.Query("no_cache") == "true"
	svc := service.NewDashboardService()

	data, err := svc.GetTopUsers(period, limit, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/channels
func GetChannelStatus(c *gin.Context) {
	svc := service.NewDashboardService()

	data, err := svc.GetChannelStatus()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/dashboard/cache/invalidate
func InvalidateDashboardCache(c *gin.Context) {
	svc := service.NewDashboardService()
	svc.InvalidateDashboardCache()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Dashboard cache invalidated",
	})
}

// GET /api/dashboard/refresh-estimate
func GetRefreshEstimate(c *gin.Context) {
	period := c.DefaultQuery("period", "7d")
	if !service.IsValidDashboardRefreshPeriod(period) {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"message": "Invalid period value"},
		})
		return
	}

	svc := service.NewDashboardService()
	data, err := svc.GetDashboardRefreshEstimate(period)
	if err != nil {
		logger.L.Error(fmt.Sprintf("Dashboard refresh estimate failed: %v", err), logger.CatAnalytics)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"error":   gin.H{"message": "Dashboard refresh estimate unavailable"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/system-info
func GetDashboardSystemInfo(c *gin.Context) {
	svc := service.NewDashboardService()
	data, err := svc.GetDashboardSystemInfo()
	if err != nil {
		// This endpoint controls a frontend safety gate. Keep the response usable
		// but conservatively classify the system as xlarge when metrics are not
		// trustworthy, rather than allowing an unconfirmed refresh.
		logger.L.Error(fmt.Sprintf("Dashboard system scale detection failed: %v", err), logger.CatAnalytics)
		data = service.FailClosedDashboardSystemInfo()
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/dashboard/ip-distribution
func GetIPDistribution(c *gin.Context) {
	window := c.DefaultQuery("window", "24h")
	if !validWindow(window) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"message": "Invalid window value"}})
		return
	}
	noCache := c.Query("no_cache") == "true"

	svc := service.NewDashboardService()
	data, err := svc.GetIPDistribution(window, noCache)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}
