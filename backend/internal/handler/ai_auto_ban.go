package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterAIAutoBanRoutes registers /api/ai-ban endpoints
func RegisterAIAutoBanRoutes(r *gin.RouterGroup) {
	g := r.Group("/ai-ban")
	{
		g.GET("/config", GetAIBanConfig)
		g.POST("/config", SaveAIBanConfig)
		g.POST("/reset-api-health", ResetAPIHealth)
		g.GET("/audit-logs", GetAuditLogs)
		g.DELETE("/audit-logs", ClearAuditLogs)
		g.GET("/groups", GetAvailableGroupsForBan)
		g.GET("/available-groups", GetAvailableGroupsForBan)
		g.GET("/models", GetAvailableModelsForExclude)
		g.GET("/available-models-for-exclude", GetAvailableModelsForExclude)
		g.GET("/suspicious", GetSuspiciousUsers)
		g.GET("/suspicious-users", GetSuspiciousUsers)
		g.POST("/assess", ManualAssess)
		g.POST("/scan", RunAIBanScan)
		g.POST("/test-connection", TestAIConnection)
		g.GET("/whitelist", GetAIBanWhitelist)
		g.POST("/whitelist/add", AddToAIBanWhitelist)
		g.POST("/whitelist/remove", RemoveFromAIBanWhitelist)
		g.GET("/whitelist/search", SearchUserForAIWhitelist)
		// Model fetching / testing
		g.POST("/models", FetchAIModels)       // 前端实际调用的路径
		g.POST("/fetch-models", FetchAIModels) // 保持向后兼容
		g.POST("/test-model", TestAIModel)
	}
}

// GET /api/ai-ban/config
func GetAIBanConfig(c *gin.Context) {
	svc := service.NewAIAutoBanService()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": svc.GetConfig()})
}

// POST /api/ai-ban/config
func SaveAIBanConfig(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"AUDITED_CONFIGURATION_REQUIRED",
		"AI auto-ban configuration is read-only until it is migrated to the audited Tool Store",
		"",
	))
}

// POST /api/ai-ban/reset-api-health
func ResetAPIHealth(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"AUDITED_CONFIGURATION_REQUIRED",
		"AI provider health mutation is disabled until it is recorded in the Tool Store",
		"",
	))
}

// GET /api/ai-ban/audit-logs
func GetAuditLogs(c *gin.Context) {
	limit := parseLimit(c, 50, 500)
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	status := c.Query("status")

	svc := service.NewAIAutoBanService()
	data := svc.GetAuditLogs(limit, offset, status)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// DELETE /api/ai-ban/audit-logs
func ClearAuditLogs(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"APPEND_ONLY_AUDIT_REQUIRED",
		"Legacy AI audit deletion is disabled; v0.5 audit records are append-only",
		"",
	))
}

// GET /api/ai-ban/groups
func GetAvailableGroupsForBan(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	svc := service.NewAIAutoBanService()
	data, err := svc.GetAvailableGroups(days)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "AI auto-ban available groups query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/ai-ban/models
func GetAvailableModelsForExclude(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	svc := service.NewAIAutoBanService()
	data, err := svc.GetAvailableModelsForExclude(days)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "AI auto-ban available models query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/ai-ban/suspicious
func GetSuspiciousUsers(c *gin.Context) {
	window := c.DefaultQuery("window", "1h")
	if !validWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid window value", ""))
		return
	}
	limit := parseLimit(c, 20, 200)

	svc := service.NewAIAutoBanService()
	data, err := svc.GetSuspiciousUsers(window, limit)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "AI auto-ban suspicious users query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/ai-ban/assess
func ManualAssess(c *gin.Context) {
	var req struct {
		UserID int64  `json:"user_id"`
		Window string `json:"window"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", ""))
		return
	}
	if req.Window == "" {
		req.Window = "1h"
	}
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NOT_IMPLEMENTED",
		"AI user assessment is not implemented",
		"",
	))
}

// POST /api/ai-ban/scan
func RunAIBanScan(c *gin.Context) {
	window := c.DefaultQuery("window", "1h")
	if !validWindow(window) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid window value", ""))
		return
	}
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NOT_IMPLEMENTED",
		"AI risk scanning is not implemented",
		"",
	))
}

// POST /api/ai-ban/test-connection
func TestAIConnection(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NOT_IMPLEMENTED",
		"AI connection testing is not implemented",
		"",
	))
}

// GET /api/ai-ban/whitelist
func GetAIBanWhitelist(c *gin.Context) {
	svc := service.NewAIAutoBanService()
	data := svc.GetWhitelist()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/ai-ban/whitelist/add
func AddToAIBanWhitelist(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"RISK_CASE_WORKFLOW_REQUIRED",
		"AI whitelist mutation is disabled; record reviewed exceptions as Tool Store risk cases",
		"",
	))
}

// POST /api/ai-ban/whitelist/remove
func RemoveFromAIBanWhitelist(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"RISK_CASE_WORKFLOW_REQUIRED",
		"AI whitelist mutation is disabled; record reviewed exceptions as Tool Store risk cases",
		"",
	))
}

// GET /api/ai-ban/whitelist/search
func SearchUserForAIWhitelist(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Missing search keyword", ""))
		return
	}
	svc := service.NewAIAutoBanService()
	data, err := svc.SearchUserForWhitelist(q)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "AI auto-ban whitelist user search", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/ai-ban/models or /api/ai-ban/fetch-models
func FetchAIModels(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"AUDITED_EXTERNAL_CALL_REQUIRED",
		"External AI model discovery is disabled until intent and outcome auditing is available",
		"",
	))
}

// POST /api/ai-ban/test-model
func TestAIModel(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"AUDITED_EXTERNAL_CALL_REQUIRED",
		"Billable AI model tests are disabled until reason, idempotency, and outcome auditing are available",
		"",
	))
}
