package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

const maxTrackedPublicModelClients = 10000

type publicModelRateEntry struct {
	windowStart time.Time
	lastSeen    time.Time
	requests    int
}

var publicModelRateState = struct {
	sync.Mutex
	clients   map[string]publicModelRateEntry
	nextSweep time.Time
}{clients: make(map[string]publicModelRateEntry)}

// PublicModelStatusRateLimit places a bounded per-peer request budget in front
// of unauthenticated embed endpoints. Operators should additionally rate-limit
// at their reverse proxy when exposing these routes publicly.
func PublicModelStatusRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := config.Get().PublicModelRequestsPerMinute
		now := time.Now()
		key := loginClientKey(c)

		publicModelRateState.Lock()
		entry, ok := publicModelRateState.clients[key]
		if !ok || now.Sub(entry.windowStart) >= time.Minute {
			entry = publicModelRateEntry{windowStart: now}
		}
		entry.lastSeen = now
		if entry.requests >= limit {
			retryAfter := time.Minute - now.Sub(entry.windowStart)
			publicModelRateState.clients[key] = entry
			publicModelRateState.Unlock()
			seconds := int64((retryAfter + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			c.Header("Retry-After", strconv.FormatInt(seconds, 10))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, models.ErrorResp(
				"RATE_LIMITED", "Too many public model-status requests", ""))
			return
		}
		entry.requests++
		publicModelRateState.clients[key] = entry
		if len(publicModelRateState.clients) > maxTrackedPublicModelClients {
			if !now.Before(publicModelRateState.nextSweep) {
				for client, candidate := range publicModelRateState.clients {
					if now.Sub(candidate.lastSeen) >= time.Minute {
						delete(publicModelRateState.clients, client)
					}
				}
				publicModelRateState.nextSweep = now.Add(time.Minute)
			}
			if len(publicModelRateState.clients) > maxTrackedPublicModelClients {
				for client := range publicModelRateState.clients {
					if client != key {
						delete(publicModelRateState.clients, client)
						break
					}
				}
			}
		}
		publicModelRateState.Unlock()
		c.Next()
	}
}

// RegisterModelStatusRoutes registers /api/model-status endpoints (auth required)
func RegisterModelStatusRoutes(r *gin.RouterGroup) {
	g := r.Group("/model-status")
	{
		g.GET("/time-windows", GetTimeWindows)
		g.GET("/models", GetAvailableModels)
		g.GET("/status/:model_name", GetSingleModelStatus)
		g.POST("/status/multiple", GetMultipleModelsStatusHandler)
		g.POST("/status/batch", GetMultipleModelsStatusHandler)
		g.GET("/status/all", GetAllModelsStatusHandler)
		g.GET("/selected", GetSelectedModels)
		g.PUT("/selected", SetSelectedModels)
		g.GET("/config/selected", GetSelectedModels)
		g.POST("/config/selected", SetSelectedModels)
		g.GET("/config/time-window", GetTimeWindowConfig)
		g.PUT("/config/time-window", SetTimeWindowConfig)
		g.PUT("/config/window", SetTimeWindowConfig)
		g.POST("/config/window", SetTimeWindowConfig)
		g.GET("/config/theme", GetThemeConfig)
		g.PUT("/config/theme", SetThemeConfig)
		g.POST("/config/theme", SetThemeConfig)
		g.GET("/config/refresh-interval", GetRefreshIntervalConfig)
		g.PUT("/config/refresh-interval", SetRefreshIntervalConfig)
		g.PUT("/config/refresh", SetRefreshIntervalConfig)
		g.POST("/config/refresh", SetRefreshIntervalConfig)
		g.GET("/config/sort-mode", GetSortModeConfig)
		g.PUT("/config/sort-mode", SetSortModeConfig)
		g.PUT("/config/sort", SetSortModeConfig)
		g.POST("/config/sort", SetSortModeConfig)
		g.PUT("/config/custom-order", SetCustomOrderConfig)
		g.GET("/config/groups", GetCustomGroupsConfig)
		g.PUT("/config/groups", SetCustomGroupsConfig)
		g.POST("/config/groups", SetCustomGroupsConfig)
		g.GET("/config/site-title", GetSiteTitleConfig)
		g.PUT("/config/site-title", SetSiteTitleConfig)
		g.POST("/config/site-title", SetSiteTitleConfig)
		g.GET("/token-groups", GetTokenGroupsForModelStatus)
	}

}

// RegisterModelStatusEmbedRoutes registers public embed endpoints (no auth)
// Supports both /api/embed/model-status/... and /api/model-status/embed/... paths
func RegisterModelStatusEmbedRoutes(r *gin.Engine) {
	// Original embed path: /api/embed/model-status/...
	g := r.Group("/api/embed/model-status")
	g.Use(PublicModelStatusRateLimit())
	{
		g.GET("/time-windows", GetTimeWindows)
		g.GET("/models", GetPublicAvailableModels)
		g.GET("/status/:model_name", GetPublicSingleModelStatus)
		g.POST("/status/multiple", GetPublicMultipleModelsStatusHandler)
		g.POST("/status/batch", GetPublicMultipleModelsStatusHandler)
		g.GET("/status/all", GetPublicAllModelsStatusHandler)
		g.GET("/config", GetEmbedConfig)
		g.GET("/config/selected", GetSelectedModels)
		g.GET("/token-groups", GetPublicTokenGroupsForModelStatus)
	}

	// Compat embed path: /api/model-status/embed/... (used by embed.html frontend)
	e := r.Group("/api/model-status/embed")
	e.Use(PublicModelStatusRateLimit())
	{
		e.GET("/time-windows", GetTimeWindows)
		e.GET("/models", GetPublicAvailableModels)
		e.GET("/status/:model_name", GetPublicSingleModelStatus)
		e.POST("/status/multiple", GetPublicMultipleModelsStatusHandler)
		e.POST("/status/batch", GetPublicMultipleModelsStatusHandler)
		e.GET("/status/all", GetPublicAllModelsStatusHandler)
		e.GET("/config", GetEmbedConfig)
		e.GET("/config/selected", GetSelectedModels)
		e.GET("/token-groups", GetPublicTokenGroupsForModelStatus)
	}
}

func publicModelStatusError(c *gin.Context, operation string, err error) {
	logger.L.Error(fmt.Sprintf("Public model-status %s failed: %v", operation, err), logger.CatAPI)
	c.JSON(http.StatusInternalServerError, models.ErrorResp(
		"QUERY_ERROR", "Model status data is temporarily unavailable", ""))
}

// GET /time-windows
func GetTimeWindows(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    service.AvailableTimeWindows,
		"default": service.DefaultTimeWindow,
	})
}

// GET /models
func GetAvailableModels(c *gin.Context) {
	svc := service.NewModelStatusService()
	data, err := svc.GetAvailableModels()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func GetPublicAvailableModels(c *gin.Context) {
	svc := service.NewModelStatusService()
	data, err := svc.GetAvailableModels()
	if err != nil {
		publicModelStatusError(c, "models query", err)
		return
	}
	maxModels := config.Get().PublicModelMaxBatch
	truncated := len(data) > maxModels
	if truncated {
		data = data[:maxModels]
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data, "truncated": truncated})
}

// GET /status/:model_name
func GetSingleModelStatus(c *gin.Context) {
	getSingleModelStatus(c, false)
}

func GetPublicSingleModelStatus(c *gin.Context) {
	getSingleModelStatus(c, true)
}

func getSingleModelStatus(c *gin.Context, publicRequest bool) {
	modelName := strings.TrimSpace(c.Param("model_name"))
	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	if modelName == "" || len(modelName) > 256 || !validModelStatusWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid model name or time window", ""))
		return
	}

	svc := service.NewModelStatusService()
	data, err := svc.GetModelStatus(modelName, window)
	if err != nil {
		if publicRequest {
			publicModelStatusError(c, "single status query", err)
		} else {
			c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /status/multiple
func GetMultipleModelsStatusHandler(c *gin.Context) {
	getMultipleModelsStatus(c, false)
}

func GetPublicMultipleModelsStatusHandler(c *gin.Context) {
	getMultipleModelsStatus(c, true)
}

func getMultipleModelsStatus(c *gin.Context, publicRequest bool) {
	maxModels := int(^uint(0) >> 1)
	if publicRequest {
		maxModels = config.Get().PublicModelMaxBatch
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, config.Get().PublicModelMaxBodyBytes)
	}
	var modelNames []string
	if err := c.ShouldBindJSON(&modelNames); err != nil {
		details := err.Error()
		if publicRequest {
			details = ""
		}
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Expected array of model names", details))
		return
	}
	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	if !validModelStatusWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid time window", ""))
		return
	}
	modelNames, errMessage := sanitizeModelNames(modelNames, maxModels)
	if errMessage != "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", errMessage, ""))
		return
	}

	svc := service.NewModelStatusService()
	data, err := svc.GetMultipleModelsStatus(modelNames, window)
	if err != nil {
		if publicRequest {
			publicModelStatusError(c, "multiple status query", err)
		} else {
			c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"data":        data,
		"time_window": window,
		"cache_ttl":   60,
	})
}

// GET /status/all
func GetAllModelsStatusHandler(c *gin.Context) {
	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	if !validModelStatusWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid time window", ""))
		return
	}

	svc := service.NewModelStatusService()
	data, err := svc.GetAllModelsStatus(window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"data":        data,
		"time_window": window,
		"cache_ttl":   60,
	})
}

func GetPublicAllModelsStatusHandler(c *gin.Context) {
	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	if !validModelStatusWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid time window", ""))
		return
	}

	svc := service.NewModelStatusService()
	available, err := svc.GetAvailableModels()
	if err != nil {
		publicModelStatusError(c, "all models query", err)
		return
	}
	maxModels := config.Get().PublicModelMaxBatch
	names := make([]string, 0, maxModels)
	for _, item := range available {
		name, ok := item["model_name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		names = append(names, name)
		if len(names) >= maxModels {
			break
		}
	}
	data, err := svc.GetMultipleModelsStatus(names, window)
	if err != nil {
		publicModelStatusError(c, "all model statuses query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"data":        data,
		"time_window": window,
		"cache_ttl":   60,
		"truncated":   len(available) > len(names),
	})
}

func validModelStatusWindow(window string) bool {
	for _, candidate := range service.AvailableTimeWindows {
		if window == candidate {
			return true
		}
	}
	return false
}

func sanitizeModelNames(values []string, maxModels int) ([]string, string) {
	if len(values) == 0 {
		return nil, "At least one model name is required"
	}
	if len(values) > maxModels {
		return nil, fmt.Sprintf("At most %d model names are allowed", maxModels)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 {
			return nil, "Model names must be between 1 and 256 bytes"
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, ""
}

// GET /selected
func GetSelectedModels(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"data":             config["selected_models"],
		"time_window":      config["time_window"],
		"theme":            config["theme"],
		"refresh_interval": config["refresh_interval"],
		"sort_mode":        config["sort_mode"],
		"custom_order":     config["custom_order"],
		"custom_groups":    config["custom_groups"],
		"site_title":       config["site_title"],
	})
}

// PUT /selected
func SetSelectedModels(c *gin.Context) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetSelectedModels(req.Models)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    req.Models,
		"message": "Selected models updated",
	})
}

// GET /config/time-window
func GetTimeWindowConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"time_window": config["time_window"],
	})
}

// PUT /config/time-window
func SetTimeWindowConfig(c *gin.Context) {
	var req struct {
		TimeWindow string `json:"time_window"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	// Validate
	valid := false
	for _, w := range service.AvailableTimeWindows {
		if w == req.TimeWindow {
			valid = true
			break
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid time window", ""))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetTimeWindow(req.TimeWindow)
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"time_window": req.TimeWindow,
		"message":     "Time window updated",
	})
}

// GET /config/theme
func GetThemeConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"theme":            config["theme"],
		"available_themes": service.AvailableThemes,
	})
}

// PUT /config/theme
func SetThemeConfig(c *gin.Context) {
	var req struct {
		Theme string `json:"theme"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	// Map legacy theme names to valid ones
	theme := req.Theme
	if mapped, ok := service.LegacyThemeMap[theme]; ok {
		theme = mapped
	}
	valid := false
	for _, t := range service.AvailableThemes {
		if t == theme {
			valid = true
			break
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid theme", ""))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetTheme(theme)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"theme":   theme,
		"message": "Theme updated",
	})
}

// GET /config/refresh-interval
func GetRefreshIntervalConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"refresh_interval": config["refresh_interval"],
		"available":        service.AvailableRefreshIntervals,
	})
}

// PUT /config/refresh-interval
func SetRefreshIntervalConfig(c *gin.Context) {
	var req struct {
		RefreshInterval int `json:"refresh_interval"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	valid := false
	for _, i := range service.AvailableRefreshIntervals {
		if i == req.RefreshInterval {
			valid = true
			break
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid refresh interval", ""))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetRefreshInterval(req.RefreshInterval)
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"refresh_interval": req.RefreshInterval,
		"message":          "Refresh interval updated",
	})
}

// GET /config/sort-mode
func GetSortModeConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"sort_mode": config["sort_mode"],
		"available": service.AvailableSortModes,
	})
}

// PUT /config/sort-mode
func SetSortModeConfig(c *gin.Context) {
	var req struct {
		SortMode string `json:"sort_mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	valid := false
	for _, m := range service.AvailableSortModes {
		if m == req.SortMode {
			valid = true
			break
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid sort mode", ""))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetSortMode(req.SortMode)
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"sort_mode": req.SortMode,
		"message":   "Sort mode updated",
	})
}

// PUT /config/custom-order
func SetCustomOrderConfig(c *gin.Context) {
	var req struct {
		CustomOrder []string `json:"custom_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetCustomOrder(req.CustomOrder)
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"custom_order": req.CustomOrder,
		"message":      "Custom order updated",
	})
}

// GET /config (embed)
func GetEmbedConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config := svc.GetEmbedConfig()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

// GET /config/groups
func GetCustomGroupsConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	groups := svc.GetCustomGroups()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    groups,
	})
}

// PUT /config/groups
func SetCustomGroupsConfig(c *gin.Context) {
	var req struct {
		Groups []map[string]interface{} `json:"groups"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetCustomGroups(req.Groups)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    req.Groups,
		"message": "Custom groups updated",
	})
}

// GET /token-groups
func GetTokenGroupsForModelStatus(c *gin.Context) {
	svc := service.NewModelStatusService()
	groups, err := svc.GetTokenGroups()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResp("QUERY_ERROR", err.Error(), ""))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    groups,
	})
}

func GetPublicTokenGroupsForModelStatus(c *gin.Context) {
	svc := service.NewModelStatusService()
	groups, err := svc.GetTokenGroups()
	if err != nil {
		publicModelStatusError(c, "token groups query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    groups,
	})
}

// GET /config/site-title
func GetSiteTitleConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"site_title": svc.GetSiteTitle(),
	})
}

// PUT /config/site-title
func SetSiteTitleConfig(c *gin.Context) {
	var req struct {
		SiteTitle string `json:"site_title"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", err.Error()))
		return
	}
	svc := service.NewModelStatusService()
	svc.SetSiteTitle(req.SiteTitle)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"site_title": req.SiteTitle,
		"message":    "Site title updated",
	})
}
