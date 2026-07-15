package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	// Validate mode if provided
	if mode, ok := req["mode"].(string); ok && mode != "simple" && mode != "by_source" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "无效的分组模式", ""))
		return
	}

	// Validate scan_interval_minutes if provided
	if interval, ok := req["scan_interval_minutes"]; ok {
		var minutes int64
		switch v := interval.(type) {
		case float64:
			minutes = int64(v)
		case int:
			minutes = int64(v)
		case int64:
			minutes = v
		}
		if minutes < 1 || minutes > 1440 {
			c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "扫描间隔必须在 1-1440 分钟之间", ""))
			return
		}
	}

	// Validate no empty config
	if len(req) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "没有要保存的配置", ""))
		return
	}

	svc := service.NewAutoGroupService()
	if !svc.SaveConfig(req) {
		respondInternalError(c, "SAVE_ERROR", "Unable to save auto-group configuration", "auto-group configuration save", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "配置已保存",
		"data":    svc.GetConfig(),
	})
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
	var req struct {
		UserIDs     []int64 `json:"user_ids"`
		TargetGroup string  `json:"target_group"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", ""))
		return
	}
	if len(req.UserIDs) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "未选择用户", ""))
		return
	}
	if req.TargetGroup == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "未指定目标分组", ""))
		return
	}

	svc := service.NewAutoGroupService()
	data := svc.BatchMoveUsers(req.UserIDs, req.TargetGroup)
	success, _ := data["success"].(bool)
	c.JSON(http.StatusOK, gin.H{"success": success, "data": data})
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
	var req struct {
		OperationID  string `json:"operation_id"`
		Resolution   string `json:"resolution"`
		Confirmation string `json:"confirmation"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", ""))
		return
	}
	actor := "admin"
	if subject, exists := c.Get("user_sub"); exists {
		if value := strings.TrimSpace(fmt.Sprint(subject)); value != "" {
			actor = value
		}
	}
	svc := service.NewAutoGroupService()
	data, err := svc.ResolvePendingAudit(req.OperationID, req.Resolution, req.Confirmation, actor)
	if err != nil {
		respondHandlerError(c, http.StatusBadRequest, "PENDING_AUDIT_RESOLUTION_ERROR", "Unable to resolve pending audit operation", "auto-group pending audit resolution", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// POST /api/auto-group/revert
func RevertAutoGroupUser(c *gin.Context) {
	var req struct {
		LogID int `json:"log_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request", ""))
		return
	}
	svc := service.NewAutoGroupService()
	data := svc.RevertUser(req.LogID)
	success, _ := data["success"].(bool)
	c.JSON(http.StatusOK, gin.H{"success": success, "data": data})
}
