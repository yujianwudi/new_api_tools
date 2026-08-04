package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	maxTrackedPublicModelClients         = 10000
	authenticatedModelStatusMaxBatch     = 200
	authenticatedModelStatusMaxAll       = 1000
	authenticatedModelStatusMaxBodyBytes = int64(1 << 20)
)

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
	operator := auth.RequireRole(auth.RoleOperator)
	{
		g.GET("/time-windows", GetTimeWindows)
		g.GET("/models", GetAvailableModels)
		g.GET("/status/:model_name", GetSingleModelStatus)
		g.POST("/status/multiple", GetMultipleModelsStatusHandler)
		g.POST("/status/batch", GetMultipleModelsStatusHandler)
		g.GET("/status/all", GetAllModelsStatusHandler)
		g.GET("/selected", GetSelectedModels)
		g.PUT("/selected", operator, SetSelectedModels)
		g.GET("/config/selected", GetSelectedModels)
		g.PUT("/config/selected", operator, SetSelectedModels)
		g.POST("/config/selected", operator, SetSelectedModels)
		g.GET("/config/time-window", GetTimeWindowConfig)
		g.PUT("/config/time-window", operator, SetTimeWindowConfig)
		g.PUT("/config/window", operator, SetTimeWindowConfig)
		g.POST("/config/window", operator, SetTimeWindowConfig)
		g.GET("/config/theme", GetThemeConfig)
		g.PUT("/config/theme", operator, SetThemeConfig)
		g.POST("/config/theme", operator, SetThemeConfig)
		g.GET("/config/refresh-interval", GetRefreshIntervalConfig)
		g.PUT("/config/refresh-interval", operator, SetRefreshIntervalConfig)
		g.PUT("/config/refresh", operator, SetRefreshIntervalConfig)
		g.POST("/config/refresh", operator, SetRefreshIntervalConfig)
		g.GET("/config/sort-mode", GetSortModeConfig)
		g.PUT("/config/sort-mode", operator, SetSortModeConfig)
		g.PUT("/config/sort", operator, SetSortModeConfig)
		g.POST("/config/sort", operator, SetSortModeConfig)
		g.PUT("/config/custom-order", operator, SetCustomOrderConfig)
		g.GET("/config/groups", GetCustomGroupsConfig)
		g.PUT("/config/groups", operator, SetCustomGroupsConfig)
		g.POST("/config/groups", operator, SetCustomGroupsConfig)
		g.GET("/config/site-title", GetSiteTitleConfig)
		g.PUT("/config/site-title", operator, SetSiteTitleConfig)
		g.POST("/config/site-title", operator, SetSiteTitleConfig)
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
		g.GET("/config/selected", GetPublicSelectedModels)
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
		e.GET("/config/selected", GetPublicSelectedModels)
		e.GET("/token-groups", GetPublicTokenGroupsForModelStatus)
	}
}

func modelStatusQueryError(c *gin.Context, operation string, err error) {
	respondInternalError(c, "QUERY_ERROR", "Model status data is temporarily unavailable", "model-status "+operation, err)
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
		modelStatusQueryError(c, "models query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func GetPublicAvailableModels(c *gin.Context) {
	svc := service.NewModelStatusService()
	data, err := svc.GetAvailableModels()
	if err != nil {
		modelStatusQueryError(c, "public models query", err)
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
			modelStatusQueryError(c, "public single status query", err)
		} else {
			modelStatusQueryError(c, "single status query", err)
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
	maxModels := authenticatedModelStatusMaxBatch
	maxBodyBytes := authenticatedModelStatusMaxBodyBytes
	if publicRequest {
		maxModels = config.Get().PublicModelMaxBatch
		maxBodyBytes = config.Get().PublicModelMaxBodyBytes
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	var modelNames []string
	if err := c.ShouldBindJSON(&modelNames); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Expected array of model names", ""))
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
			modelStatusQueryError(c, "public multiple status query", err)
		} else {
			modelStatusQueryError(c, "multiple status query", err)
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
	catalog, err := svc.GetAvailableModelsWithState()
	if err != nil {
		modelStatusQueryError(c, "all model statuses query", err)
		return
	}
	names := make([]string, 0, authenticatedModelStatusMaxAll)
	totalModels := 0
	for _, item := range catalog.Models {
		name, ok := item["model_name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		totalModels++
		if len(names) < authenticatedModelStatusMaxAll {
			names = append(names, name)
		}
	}
	data, err := svc.GetMultipleModelsStatus(names, window)
	if err != nil {
		modelStatusQueryError(c, "all model statuses query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"data":         data,
		"time_window":  window,
		"cache_ttl":    60,
		"source_state": catalog.SourceState,
		"total_models": totalModels,
		"returned":     len(data),
		"limit":        authenticatedModelStatusMaxAll,
		"truncated":    totalModels > len(data),
	})
}

func GetPublicAllModelsStatusHandler(c *gin.Context) {
	window := c.DefaultQuery("window", service.DefaultTimeWindow)
	if !validModelStatusWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid time window", ""))
		return
	}

	svc := service.NewModelStatusService()
	catalog, err := svc.GetAvailableModelsWithState()
	if err != nil {
		modelStatusQueryError(c, "public all models query", err)
		return
	}
	available := catalog.Models
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
		modelStatusQueryError(c, "public all model statuses query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"data":         data,
		"time_window":  window,
		"cache_ttl":    60,
		"source_state": catalog.SourceState,
		"total_models": len(available),
		"returned":     len(data),
		"limit":        maxModels,
		"truncated":    len(available) > len(names),
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

func sanitizeSelectedModelNames(values []string) ([]string, string) {
	if len(values) == 0 {
		return []string{}, ""
	}
	return sanitizeModelNames(values, authenticatedModelStatusMaxBatch)
}

type modelConfigMutationMetadata struct {
	Reason          string `json:"reason"`
	ExpectedVersion *int64 `json:"expected_version"`
}

func decodeModelConfigJSON(c *gin.Context, destination any) bool {
	if c.ContentType() != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, models.ErrorResp(
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json", ""))
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(service.MaxModelStatusConfigBytes+4096))
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("MODEL_CONFIG_INVALID", "Invalid model configuration payload", ""))
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("MODEL_CONFIG_INVALID", "Invalid model configuration payload", ""))
		return false
	}
	return true
}

func modelConfigMutationAudit(c *gin.Context, key, reason string) (toolstore.OperationAuditInput, bool) {
	actor, authMethod := controlPlaneMutationIdentity(c)
	requestID := strings.TrimSpace(middleware.RequestID(c))
	if requestID == "" {
		requestID = strings.TrimSpace(c.GetString("request_id"))
	}
	if actor == "" || authMethod == "" || requestID == "" {
		c.JSON(http.StatusInternalServerError, models.ErrorResp(
			"REQUEST_METADATA_UNAVAILABLE", "Trusted request metadata is unavailable", ""))
		return toolstore.OperationAuditInput{}, false
	}
	idempotencyKey := requestID
	if len(c.Request.Header.Values("Idempotency-Key")) > 0 {
		var ok bool
		idempotencyKey, ok = controlPlaneIdempotencyKey(c)
		if !ok {
			c.JSON(http.StatusBadRequest, models.ErrorResp(
				"MODEL_CONFIG_INVALID", "Idempotency-Key is invalid", ""))
			return toolstore.OperationAuditInput{}, false
		}
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "update model status " + key
	}
	identity := controlPlaneIdentity{
		Actor: actor, AuthMethod: authMethod, SourceIP: strings.TrimSpace(c.ClientIP()),
		RequestID: requestID, IdempotencyKey: idempotencyKey,
	}
	return mutationAuditInput(identity, "model_status.config", reason), true
}

func validateExpectedModelConfigVersion(c *gin.Context, version *int64) bool {
	if version != nil && *version <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp(
			"MODEL_CONFIG_INVALID", "expected_version must be positive", ""))
		return false
	}
	return true
}

func writeModelConfigError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrModelStatusConfigInvalid):
		c.JSON(http.StatusBadRequest, models.ErrorResp("MODEL_CONFIG_INVALID", "Invalid model configuration", ""))
	case errors.Is(err, service.ErrModelStatusConfigConflict):
		c.JSON(http.StatusConflict, models.ErrorResp("MODEL_CONFIG_CONFLICT", "Model configuration version changed; reload and retry", ""))
	case errors.Is(err, service.ErrModelStatusConfigUnavailable):
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("MODEL_CONFIG_UNAVAILABLE", "Model configuration could not be durably published", ""))
	default:
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("MODEL_CONFIG_UNAVAILABLE", "Model configuration is temporarily unavailable", ""))
	}
}

func readModelConfig(c *gin.Context) (map[string]interface{}, bool) {
	value, err := service.NewModelStatusService().GetConfig()
	if err != nil {
		writeModelConfigError(c, err)
		return nil, false
	}
	return value, true
}

// GET /selected
func GetSelectedModels(c *gin.Context) {
	respondSelectedModels(c, authenticatedModelStatusMaxBatch)
}

func GetPublicSelectedModels(c *gin.Context) {
	respondSelectedModels(c, config.Get().PublicModelMaxBatch)
}

func respondSelectedModels(c *gin.Context, maxBatch int) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
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
		"version":          config["version"],
		"max_batch":        maxBatch,
	})
}

// PUT /selected
func SetSelectedModels(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		Models *[]string `json:"models"`
	}
	if !decodeModelConfigJSON(c, &req) {
		return
	}
	if req.Models == nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Models must be provided as an array", ""))
		return
	}
	validatedModels, errMessage := sanitizeSelectedModelNames(*req.Models)
	if errMessage != "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", errMessage, ""))
		return
	}
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "selected_models", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetSelectedModels(c.Request.Context(), validatedModels, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    snapshot.Config.SelectedModels,
		"version": snapshot.Version,
		"message": "Selected models updated",
	})
}

// GET /config/time-window
func GetTimeWindowConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"time_window": config["time_window"],
		"version":     config["version"],
	})
}

// PUT /config/time-window
func SetTimeWindowConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		TimeWindow string `json:"time_window"`
	}
	if !decodeModelConfigJSON(c, &req) {
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
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "time_window", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetTimeWindow(c.Request.Context(), req.TimeWindow, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"time_window": snapshot.Config.TimeWindow,
		"version":     snapshot.Version,
		"message":     "Time window updated",
	})
}

// GET /config/theme
func GetThemeConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"theme":            config["theme"],
		"version":          config["version"],
		"available_themes": service.AvailableThemes,
	})
}

// PUT /config/theme
func SetThemeConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		Theme string `json:"theme"`
	}
	if !decodeModelConfigJSON(c, &req) {
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
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "theme", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetTheme(c.Request.Context(), theme, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"theme":   snapshot.Config.Theme,
		"version": snapshot.Version,
		"message": "Theme updated",
	})
}

// GET /config/refresh-interval
func GetRefreshIntervalConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"refresh_interval": config["refresh_interval"],
		"version":          config["version"],
		"available":        service.AvailableRefreshIntervals,
	})
}

// PUT /config/refresh-interval
func SetRefreshIntervalConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		RefreshInterval int `json:"refresh_interval"`
	}
	if !decodeModelConfigJSON(c, &req) {
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
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "refresh_interval", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetRefreshInterval(c.Request.Context(), req.RefreshInterval, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"refresh_interval": snapshot.Config.RefreshInterval,
		"version":          snapshot.Version,
		"message":          "Refresh interval updated",
	})
}

// GET /config/sort-mode
func GetSortModeConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"sort_mode": config["sort_mode"],
		"version":   config["version"],
		"available": service.AvailableSortModes,
	})
}

// PUT /config/sort-mode
func SetSortModeConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		SortMode string `json:"sort_mode"`
	}
	if !decodeModelConfigJSON(c, &req) {
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
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "sort_mode", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetSortMode(c.Request.Context(), req.SortMode, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"sort_mode": snapshot.Config.SortMode,
		"version":   snapshot.Version,
		"message":   "Sort mode updated",
	})
}

// PUT /config/custom-order
func SetCustomOrderConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		CustomOrder []string `json:"custom_order"`
	}
	if !decodeModelConfigJSON(c, &req) {
		return
	}
	validatedOrder, errMessage := sanitizeSelectedModelNames(req.CustomOrder)
	if errMessage != "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("MODEL_CONFIG_INVALID", errMessage, ""))
		return
	}
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "custom_order", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetCustomOrder(c.Request.Context(), validatedOrder, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"custom_order": snapshot.Config.CustomOrder,
		"version":      snapshot.Version,
		"message":      "Custom order updated",
	})
}

// GET /config (embed)
func GetEmbedConfig(c *gin.Context) {
	svc := service.NewModelStatusService()
	config, err := svc.GetEmbedConfig()
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

// GET /config/groups
func GetCustomGroupsConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    config["custom_groups"],
		"version": config["version"],
	})
}

// PUT /config/groups
func SetCustomGroupsConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		Groups []service.ModelStatusCustomGroup `json:"groups"`
	}
	if !decodeModelConfigJSON(c, &req) {
		return
	}
	if err := service.ValidateModelStatusCustomGroups(req.Groups); err != nil {
		writeModelConfigError(c, err)
		return
	}
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "custom_groups", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetCustomGroups(c.Request.Context(), req.Groups, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    snapshot.Config.CustomGroups,
		"version": snapshot.Version,
		"message": "Custom groups updated",
	})
}

// GET /token-groups
func GetTokenGroupsForModelStatus(c *gin.Context) {
	svc := service.NewModelStatusService()
	groups, err := svc.GetTokenGroups()
	if err != nil {
		modelStatusQueryError(c, "token groups query", err)
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
		modelStatusQueryError(c, "public token groups query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    groups,
	})
}

// GET /config/site-title
func GetSiteTitleConfig(c *gin.Context) {
	config, ok := readModelConfig(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"site_title": config["site_title"],
		"version":    config["version"],
	})
}

// PUT /config/site-title
func SetSiteTitleConfig(c *gin.Context) {
	var req struct {
		modelConfigMutationMetadata
		SiteTitle string `json:"site_title"`
	}
	if !decodeModelConfigJSON(c, &req) {
		return
	}
	if !validateExpectedModelConfigVersion(c, req.ExpectedVersion) {
		return
	}
	audit, ok := modelConfigMutationAudit(c, "site_title", req.Reason)
	if !ok {
		return
	}
	svc := service.NewModelStatusService()
	snapshot, err := svc.SetSiteTitle(c.Request.Context(), req.SiteTitle, audit, req.ExpectedVersion)
	if err != nil {
		writeModelConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"site_title": snapshot.Config.SiteTitle,
		"version":    snapshot.Version,
		"message":    "Site title updated",
	})
}
