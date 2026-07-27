package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/modelprobe"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const modelProbeRequestBodyLimit = int64(32 * 1024)

type ModelProbeHandler struct {
	manager *modelprobe.Manager
}

func NewModelProbeHandler(manager *modelprobe.Manager) *ModelProbeHandler {
	return &ModelProbeHandler{manager: manager}
}

func (h *ModelProbeHandler) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/model-status/probes")
	group.GET("/status", h.status)
	group.POST("/summary", h.summary)
	group.POST("/run", h.run)
	group.GET("/history", h.history)
	group.GET("/history/:model_name", h.history)
}

func (h *ModelProbeHandler) status(c *gin.Context) {
	if h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("PROBE_UNAVAILABLE", "Active model probes are unavailable", ""))
		return
	}
	status, err := h.manager.Status(c.Request.Context())
	if err != nil {
		respondInternalError(c, "PROBE_STATUS_FAILED", "Active model probe status is temporarily unavailable", "model-probe status", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": status})
}

func (h *ModelProbeHandler) summary(c *gin.Context) {
	if h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("PROBE_UNAVAILABLE", "Active model probes are unavailable", ""))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, modelProbeRequestBodyLimit)
	var request struct {
		Models []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Expected a models array", ""))
		return
	}
	if len(request.Models) > authenticatedModelStatusMaxBatch {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "At most 200 model names are allowed", ""))
		return
	}
	for _, model := range request.Models {
		if strings.TrimSpace(model) == "" || len(model) > 256 {
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Model names must be non-empty and at most 256 bytes", ""))
			return
		}
	}
	result, err := h.manager.Summary(c.Request.Context(), request.Models)
	if err != nil {
		if errors.Is(err, toolstore.ErrInvalid) {
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid model probe summary request", ""))
			return
		}
		respondInternalError(c, "PROBE_SUMMARY_FAILED", "Active model probe data is temporarily unavailable", "model-probe summary", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *ModelProbeHandler) run(c *gin.Context) {
	if h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("PROBE_UNAVAILABLE", "Active model probes are unavailable", ""))
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must be 8 to 128 characters", ""))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, modelProbeRequestBodyLimit)
	var request struct {
		Models []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || len(request.Models) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "At least one model is required", ""))
		return
	}
	run, replayed, err := h.manager.QueueManual(c.Request.Context(), modelProbeActor(c), idempotencyKey, request.Models)
	if err != nil {
		switch {
		case errors.Is(err, modelprobe.ErrDisabled):
			c.JSON(http.StatusConflict, models.ErrorResp("PROBE_DISABLED", "Active model probes are disabled", ""))
		case errors.Is(err, modelprobe.ErrNotConfigured):
			c.JSON(http.StatusConflict, models.ErrorResp("PROBE_NOT_CONFIGURED", "Configure a dedicated probe token and model allowlist first", ""))
		case errors.Is(err, modelprobe.ErrBusy):
			c.JSON(http.StatusConflict, models.ErrorResp("PROBE_BUSY", "Another model probe run is already active", ""))
		case errors.Is(err, modelprobe.ErrBudgetExceeded):
			now := time.Now().UTC()
			tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			c.Header("Retry-After", strconv.FormatInt(int64(time.Until(tomorrow).Seconds()), 10))
			c.JSON(http.StatusTooManyRequests, models.ErrorResp("PROBE_BUDGET_EXCEEDED", "Daily model probe request budget would be exceeded", ""))
		case errors.Is(err, modelprobe.ErrNoModels), errors.Is(err, modelprobe.ErrModelNotAllowed), errors.Is(err, toolstore.ErrInvalid):
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PROBE_MODELS", "Requested models must be in the configured probe allowlist and within the per-run limit", ""))
		case errors.Is(err, toolstore.ErrConflict):
			c.JSON(http.StatusConflict, models.ErrorResp("IDEMPOTENCY_CONFLICT", "Idempotency-Key was already used with a different probe request", ""))
		default:
			respondInternalError(c, "PROBE_QUEUE_FAILED", "Active model probe could not be queued", "model-probe run", err)
		}
		return
	}
	status := http.StatusAccepted
	if replayed {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"success": true, "data": run, "replayed": replayed})
}

func (h *ModelProbeHandler) history(c *gin.Context) {
	if h.manager == nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp("PROBE_UNAVAILABLE", "Active model probes are unavailable", ""))
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = strings.TrimSpace(c.Param("model_name"))
	}
	if model == "" || len(model) > 256 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid model name", ""))
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "History limit must be between 1 and 100", ""))
		return
	}
	history, err := h.manager.History(c.Request.Context(), model, limit)
	if err != nil {
		if errors.Is(err, toolstore.ErrInvalid) {
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid model name", ""))
			return
		}
		respondInternalError(c, "PROBE_HISTORY_FAILED", "Active model probe history is temporarily unavailable", "model-probe history", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": history})
}

func modelProbeActor(c *gin.Context) string {
	if subject, ok := c.Get("user_sub"); ok {
		if value, ok := subject.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if method, ok := c.Get("auth_method"); ok && method == "api_key" {
		return "api-key-operator"
	}
	return "authenticated-operator"
}
