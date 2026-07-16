package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

const (
	confirmTextSoftDelete = "注销用户"
	confirmTextHardDelete = "彻底删除"
)

type unbanUserRequest struct {
	Reason       string `json:"reason"`
	EnableTokens bool   `json:"enable_tokens"`
}

type purgeSoftDeletedRequest struct {
	DryRun      bool   `json:"dry_run"`
	ConfirmText string `json:"confirm_text"`
	SnapshotID  string `json:"snapshot_id"`
}

func bindOptionalUnbanUserRequest(c *gin.Context) (unbanUserRequest, error) {
	var req unbanUserRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		return unbanUserRequest{}, err
	}
	return req, nil
}

func bindOptionalPurgeSoftDeletedRequest(c *gin.Context) (purgeSoftDeletedRequest, error) {
	req := purgeSoftDeletedRequest{DryRun: true}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		return purgeSoftDeletedRequest{}, err
	}
	return req, nil
}

func classifyDestructiveOperationError(err error) (int, string, string, bool) {
	if errors.Is(err, service.ErrInvalidOrExpiredSnapshot) {
		return http.StatusBadRequest, "SNAPSHOT_INVALID", "操作快照无效、已过期或已使用，请重新预览后再确认", true
	}
	if errors.Is(err, service.ErrSnapshotInvalidated) {
		return http.StatusConflict, "SNAPSHOT_INVALIDATED", "预览后的候选用户已发生变化，请重新预览后再确认", true
	}
	if errors.Is(err, service.ErrDestructiveSnapshotStoreUnavailable) {
		return http.StatusServiceUnavailable, "SNAPSHOT_STORE_UNAVAILABLE", "一次性操作快照存储暂不可用，请稍后重试", true
	}
	if errors.Is(err, service.ErrSeparateLogDBBatchDeleteBlocked) {
		return http.StatusConflict, "BATCH_DELETE_UNSUPPORTED", "日志分库场景不支持安全执行旁路批量删除，请使用 NewAPI 管理 API", true
	}
	message := err.Error()
	if strings.Contains(message, "ALLOW_UNSAFE_BATCH_DELETE=true") ||
		strings.Contains(message, "ALLOW_UNSAFE_HARD_DELETE=true") ||
		strings.Contains(message, "direct user/token mutation blocked") {
		return http.StatusConflict, "OPERATION_SAFETY_GUARD", "Operation blocked by the configured production safety policy", true
	}
	if strings.Contains(message, "invalid activity level") {
		return http.StatusBadRequest, "INVALID_ACTIVITY_LEVEL", "不支持的活跃度条件", true
	}
	return 0, "", "", false
}

func RegisterUserManagementRoutes(r *gin.RouterGroup, mutationHandler *MutationHandler) {
	if mutationHandler == nil {
		panic("user management routes require the audited NewAPI mutation handler")
	}
	g := r.Group("/users")
	{
		g.GET("/activity-stats", GetActivityStats)
		g.GET("/stats", GetActivityStats)
		g.GET("/banned", GetBannedUsers)
		g.GET("", GetUsers)
		g.DELETE("/:user_id", mutationHandler.DeleteUser)
		g.POST("/batch-delete", mutationHandler.BatchDeleteInactiveUsers)
		g.GET("/soft-deleted/count", GetSoftDeletedCount)
		g.POST("/soft-deleted/purge", mutationHandler.PurgeSoftDeletedUsers)
		g.POST("/:user_id/ban", mutationHandler.BanUser)
		g.POST("/:user_id/unban", mutationHandler.UnbanUser)
		g.GET("/:user_id/invited", GetInvitedUsers)
		g.POST("/tokens/:token_id/disable", mutationHandler.DisableToken)
	}
}

// GET /api/users/activity-stats
func GetActivityStats(c *gin.Context) {
	quick := c.DefaultQuery("quick", "false") == "true"
	svc := service.NewUserManagementService()

	stats, err := svc.GetActivityStats(quick)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "user activity statistics query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": stats})
}

// GET /api/users/banned
func GetBannedUsers(c *gin.Context) {
	page := parsePage(c)
	pageSize := parsePageSize(c, 50, 200)
	search := c.Query("search")

	svc := service.NewUserManagementService()
	result, err := svc.GetBannedUsers(page, pageSize, search)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "banned user list query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// GET /api/users
func GetUsers(c *gin.Context) {
	page := parsePage(c)
	pageSize := parsePageSize(c, 20, 200)

	params := service.ListUsersParams{
		Page:           page,
		PageSize:       pageSize,
		ActivityFilter: c.Query("activity"),
		GroupFilter:    c.Query("group"),
		SourceFilter:   c.Query("source"),
		Search:         c.Query("search"),
		OrderBy:        c.DefaultQuery("order_by", "request_count"),
		OrderDir:       c.DefaultQuery("order_dir", "DESC"),
	}

	svc := service.NewUserManagementService()
	result, err := svc.GetUsers(params)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "user list query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// DELETE /api/users/:user_id
func DeleteUser(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}

	hardDelete := c.DefaultQuery("hard_delete", "false") == "true"
	var req struct {
		ConfirmText string `json:"confirm_text"`
	}
	_ = c.ShouldBindJSON(&req)

	expectedConfirmText := confirmTextSoftDelete
	if hardDelete {
		expectedConfirmText = confirmTextHardDelete
	}
	if !requireDeleteConfirmText(c, req.ConfirmText, expectedConfirmText) {
		return
	}

	svc := service.NewUserManagementService()
	affected, err := svc.DeleteUser(userID, hardDelete)
	if err != nil {
		respondInternalError(c, "DELETE_ERROR", "Unable to delete user", "user delete", err)
		return
	}
	if affected == 0 {
		c.JSON(http.StatusNotFound, models.ErrorResp("NOT_FOUND", "User not found", ""))
		return
	}

	action := "注销"
	if hardDelete {
		action = "彻底删除"
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "用户已" + action,
		"data":    gin.H{"affected": affected},
	})
}

// POST /api/users/batch-delete
func BatchDeleteInactiveUsers(c *gin.Context) {
	var req struct {
		ActivityLevel string `json:"activity_level"`
		DryRun        bool   `json:"dry_run"`
		HardDelete    bool   `json:"hard_delete"`
		ConfirmText   string `json:"confirm_text"`
		SnapshotID    string `json:"snapshot_id"`
	}
	req.ActivityLevel = "very_inactive"
	req.DryRun = true

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	if !req.DryRun {
		expectedConfirmText := confirmTextSoftDelete
		if req.HardDelete {
			expectedConfirmText = confirmTextHardDelete
		}
		if !requireDeleteConfirmText(c, req.ConfirmText, expectedConfirmText) {
			return
		}
	}

	svc := service.NewUserManagementService()
	result, err := svc.BatchDeleteInactiveUsers(req.ActivityLevel, req.DryRun, req.HardDelete, req.SnapshotID)
	if err != nil {
		if status, code, message, ok := classifyDestructiveOperationError(err); ok {
			if status >= http.StatusInternalServerError {
				respondHandlerError(c, status, code, message, "inactive user batch delete", err)
			} else {
				c.JSON(status, models.ErrorResp(code, message, ""))
			}
			return
		}
		respondInternalError(c, "DELETE_ERROR", "Batch user deletion failed", "inactive user batch delete", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// GET /api/users/soft-deleted/count
func GetSoftDeletedCount(c *gin.Context) {
	svc := service.NewUserManagementService()
	count, err := svc.GetSoftDeletedCount()
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "soft-deleted user count query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"count": count}})
}

// POST /api/users/soft-deleted/purge
func PurgeSoftDeletedUsers(c *gin.Context) {
	req, err := bindOptionalPurgeSoftDeletedRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	if !req.DryRun && !requireDeleteConfirmText(c, req.ConfirmText, confirmTextHardDelete) {
		return
	}
	if !req.DryRun && strings.TrimSpace(req.SnapshotID) == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResp(
			"SNAPSHOT_REQUIRED",
			"永久清理前必须重新预览并提交一次性快照",
			"",
		))
		return
	}

	svc := service.NewUserManagementService()
	if req.DryRun {
		result, err := svc.PreviewSoftDeletedUsers()
		if err != nil {
			if status, code, message, ok := classifyDestructiveOperationError(err); ok {
				if status >= http.StatusInternalServerError {
					respondHandlerError(c, status, code, message, "soft-deleted user purge preview", err)
				} else {
					c.JSON(status, models.ErrorResp(code, message, ""))
				}
				return
			}
			respondInternalError(c, "DELETE_ERROR", "Purge preview failed", "soft-deleted user purge preview", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "预览完成",
			"data":    result,
		})
		return
	}

	affected, err := svc.PurgeSoftDeleted(req.SnapshotID)
	if err != nil {
		if status, code, message, ok := classifyDestructiveOperationError(err); ok {
			if status >= http.StatusInternalServerError {
				respondHandlerError(c, status, code, message, "soft-deleted user purge", err)
			} else {
				c.JSON(status, models.ErrorResp(code, message, ""))
			}
			return
		}
		respondInternalError(c, "DELETE_ERROR", "Purge failed", "soft-deleted user purge", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "清理完成",
		"data":    gin.H{"affected": affected},
	})
}

func requireDeleteConfirmText(c *gin.Context, got, expected string) bool {
	if strings.TrimSpace(got) == expected {
		return true
	}
	c.JSON(http.StatusBadRequest, models.ErrorResp(
		"CONFIRM_TEXT_REQUIRED",
		"请输入 "+expected+" 以确认该高风险操作",
		"",
	))
	return false
}

// POST /api/users/:user_id/ban
func BanUser(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}

	var req struct {
		Reason        string `json:"reason"`
		DisableTokens bool   `json:"disable_tokens"`
	}
	req.DisableTokens = true
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	svc := service.NewUserManagementService()
	if err := svc.BanUser(userID, req.DisableTokens); err != nil {
		respondInternalError(c, "BAN_ERROR", "Unable to ban user", "user ban", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "用户已封禁",
	})
}

// POST /api/users/:user_id/unban
func UnbanUser(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}

	req, err := bindOptionalUnbanUserRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	svc := service.NewUserManagementService()
	if err := svc.UnbanUser(userID, req.EnableTokens); err != nil {
		if errors.Is(err, service.ErrBulkTokenReactivationDisabled) {
			c.JSON(http.StatusBadRequest, models.ErrorResp(
				"TOKEN_REACTIVATION_DISABLED",
				"批量恢复 Token 已禁用，请逐个复核后启用",
				"",
			))
			return
		}
		respondInternalError(c, "UNBAN_ERROR", "Unable to unban user", "user unban", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "用户已解封",
	})
}

// POST /api/users/tokens/:token_id/disable
func DisableToken(c *gin.Context) {
	tokenID, err := strconv.ParseInt(c.Param("token_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid token ID", ""))
		return
	}

	svc := service.NewUserManagementService()
	if err := svc.DisableToken(tokenID); err != nil {
		respondInternalError(c, "DISABLE_ERROR", "Unable to disable token", "user token disable", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Token 已禁用",
	})
}

// GET /api/users/:user_id/invited
func GetInvitedUsers(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}

	page := parsePage(c)
	pageSize := parsePageSize(c, 20, 200)

	svc := service.NewUserManagementService()
	data, err := svc.GetInvitedUsers(userID, page, pageSize)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "invited user list query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}
