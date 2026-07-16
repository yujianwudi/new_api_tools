package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterAutoGroupRoutes registers /api/auto-group endpoints
func RegisterAutoGroupRoutes(r *gin.RouterGroup) {
	g := r.Group("/auto-group")
	{
		g.GET("/config", GetAutoGroupConfig)
		g.POST("/config", SaveAutoGroupConfig)
		g.GET("/stats", GetAutoGroupStats)
		g.GET("/groups", GetAutoGroupAvailableGroups)
		g.GET("/preview", GetPendingAutoGroupUsers)
		g.GET("/users", GetAutoGroupUsers)
		g.POST("/scan", RunAutoGroupScan)
		g.POST("/batch-move", BatchMoveAutoGroupUsers)
		g.GET("/logs", GetAutoGroupLogs)
		g.GET("/pending-audits", GetPendingAutoGroupAudits)
		g.POST("/pending-audits/resolve", ResolvePendingAutoGroupAudit)
		g.POST("/revert", RevertAutoGroupUser)
	}
}

// GET /api/auto-group/config
func GetAutoGroupConfig(c *gin.Context) {
	svc := service.NewAutoGroupService()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": svc.GetConfig()})
}

// POST /api/auto-group/config
func SaveAutoGroupConfig(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"AUDITED_CONFIGURATION_REQUIRED",
		"Auto-group configuration is read-only until it is migrated to the audited Tool Store",
		"",
	))
}

// GET /api/auto-group/stats
func GetAutoGroupStats(c *gin.Context) {
	svc := service.NewAutoGroupService()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": svc.GetStats()})
}

// GET /api/auto-group/groups
func GetAutoGroupAvailableGroups(c *gin.Context) {
	svc := service.NewAutoGroupService()
	groups := svc.GetAvailableGroups()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items": groups,
			"total": len(groups),
		},
	})
}

// GET /api/auto-group/preview
func GetPendingAutoGroupUsers(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))

	svc := service.NewAutoGroupService()
	data := svc.GetPendingUsers(page, pageSize)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/auto-group/users
func GetAutoGroupUsers(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	group := c.Query("group")
	source := c.Query("source")
	keyword := c.Query("keyword")

	// Validate source parameter
	if source != "" {
		validSources := map[string]bool{
			"github": true, "wechat": true, "telegram": true,
			"discord": true, "oidc": true, "linux_do": true, "password": true,
		}
		if !validSources[source] {
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "无效的注册来源: "+source, ""))
			return
		}
	}

	svc := service.NewAutoGroupService()
	data := svc.GetUsers(page, pageSize, group, source, keyword)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/auto-group/scan
func RunAutoGroupScan(c *gin.Context) {
	dryRunStr := c.DefaultQuery("dry_run", "true")
	dryRun := dryRunStr == "true"
	if !dryRun {
		c.JSON(http.StatusNotImplemented, models.ErrorResp(
			"NEWAPI_ADAPTER_REQUIRED",
			"Auto-group execution is preview-only until a versioned NewAPI group-management API is available",
			"",
		))
		return
	}

	svc := service.NewAutoGroupService()
	if !svc.IsEnabled() {
		c.JSON(http.StatusBadRequest, models.ErrorResp("DISABLED", "自动分组功能未启用", ""))
		return
	}
	data := svc.RunScan(dryRun)
	success, _ := data["success"].(bool)
	c.JSON(http.StatusOK, gin.H{"success": success, "data": data})
}

// POST /api/auto-group/batch-move
func BatchMoveAutoGroupUsers(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NEWAPI_ADAPTER_REQUIRED",
		"Direct batch group mutation is disabled; use preview and apply changes through NewAPI",
		"",
	))
}

// GET /api/auto-group/logs
func GetAutoGroupLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	action := c.Query("action")

	var userID *int64
	if uid := c.Query("user_id"); uid != "" {
		v, _ := strconv.ParseInt(uid, 10, 64)
		userID = &v
	}

	svc := service.NewAutoGroupService()
	data := svc.GetLogs(page, pageSize, action, userID)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// GET /api/auto-group/pending-audits
func GetPendingAutoGroupAudits(c *gin.Context) {
	svc := service.NewAutoGroupService()
	data, err := svc.GetPendingAudits()
	if err != nil {
		respondHandlerError(c, http.StatusServiceUnavailable, "AUDIT_STORAGE_ERROR", "Pending audit storage is temporarily unavailable", "auto-group pending audit list", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/auto-group/pending-audits/resolve
func ResolvePendingAutoGroupAudit(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"LEGACY_AUDIT_READ_ONLY",
		"Legacy pending auto-group audits are read-only in v0.5; create a Tool Store risk case for reconciliation",
		"",
	))
}

// POST /api/auto-group/revert
func RevertAutoGroupUser(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"NEWAPI_ADAPTER_REQUIRED",
		"Direct group rollback is disabled until a versioned NewAPI group-management API is available",
		"",
	))
}
