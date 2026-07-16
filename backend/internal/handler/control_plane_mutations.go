package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/controlplane"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/util"
)

type MutationHandler struct {
	service *controlplane.Service
}

func NewMutationHandler(service *controlplane.Service) *MutationHandler {
	return &MutationHandler{service: service}
}

// RegisterControlPlaneMutationRoutes exposes explicit v0.5 mutation paths in
// addition to the compatibility routes wired by the existing modules.
func (h *MutationHandler) RegisterControlPlaneMutationRoutes(api *gin.RouterGroup) {
	group := api.Group("/control-plane")
	group.POST("/users/:user_id/disable", h.BanUser)
	group.POST("/users/:user_id/enable", h.UnbanUser)
	group.DELETE("/users/:user_id", h.DeleteUser)
	group.POST("/redemptions", auth.RequireRole(auth.RoleAdmin), h.GenerateRedemptionCodes)
	group.DELETE("/redemptions/:id", h.DeleteRedemptionCode)
	group.POST("/redemptions/batch-delete", h.BatchDeleteRedemptionCodes)
}

func (h *MutationHandler) DeleteUser(c *gin.Context) {
	userID, err := strconv.Atoi(c.Param("user_id"))
	if err != nil || userID <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}
	hardDelete := c.DefaultQuery("hard_delete", "false") == "true"
	var request struct {
		ConfirmText string `json:"confirm_text"`
		Reason      string `json:"reason"`
	}
	if err := bindOptionalJSON(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	expected := confirmTextSoftDelete
	if hardDelete {
		expected = confirmTextHardDelete
		if !auth.HasRole(c, auth.RoleAdmin) {
			c.JSON(http.StatusForbidden, models.NewErrorResponse(
				"INSUFFICIENT_ROLE",
				"admin role is required for permanent user deletion",
			))
			return
		}
	}
	if !requireDeleteConfirmText(c, request.ConfirmText, expected) {
		return
	}

	result, err := h.service.MutateUser(c.Request.Context(), operationMeta(c, request.Reason), controlplane.UserMutationRequest{
		UserID: userID, Action: "delete", HardDelete: hardDelete,
	})
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "User deletion accepted by NewAPI", "data": result})
}

func (h *MutationHandler) BanUser(c *gin.Context) {
	userID, err := strconv.Atoi(c.Param("user_id"))
	if err != nil || userID <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}
	var request struct {
		Reason        string `json:"reason"`
		DisableTokens bool   `json:"disable_tokens"`
	}
	request.DisableTokens = true
	if err := bindOptionalJSON(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	result, err := h.service.MutateUser(c.Request.Context(), operationMeta(c, request.Reason), controlplane.UserMutationRequest{
		UserID: userID, Action: "disable",
	})
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "User disabled through NewAPI",
		"data": gin.H{
			"operation":    result,
			"token_policy": "access_blocked_by_user_status",
		},
	})
}

func (h *MutationHandler) UnbanUser(c *gin.Context) {
	userID, err := strconv.Atoi(c.Param("user_id"))
	if err != nil || userID <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid user ID", ""))
		return
	}
	request, err := bindOptionalUnbanUserRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	if request.EnableTokens {
		c.JSON(http.StatusBadRequest, models.ErrorResp(
			"TOKEN_REACTIVATION_DISABLED",
			"Bulk token reactivation is disabled; review tokens individually",
			"",
		))
		return
	}
	result, err := h.service.MutateUser(c.Request.Context(), operationMeta(c, request.Reason), controlplane.UserMutationRequest{
		UserID: userID, Action: "enable",
	})
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "User enabled through NewAPI", "data": result})
}

func (h *MutationHandler) GenerateRedemptionCodes(c *gin.Context) {
	var request service.GenerateParams
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	upstreamRequest, err := newAPIRedemptionRequest(request)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", err.Error(), ""))
		return
	}
	result, err := h.service.CreateRedemptions(c.Request.Context(), operationMeta(c, request.Reason), upstreamRequest)
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Redemption codes created through NewAPI",
		"data":    result,
	})
}

func (h *MutationHandler) DeleteRedemptionCode(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid redemption ID", ""))
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if err := bindOptionalJSON(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	result, err := h.service.DeleteRedemptions(c.Request.Context(), operationMeta(c, request.Reason), []int{id})
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Redemption deleted through NewAPI", "data": result})
}

func (h *MutationHandler) BatchDeleteRedemptionCodes(c *gin.Context) {
	var request struct {
		IDs    []int  `json:"ids"`
		Reason string `json:"reason"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	result, err := h.service.DeleteRedemptions(c.Request.Context(), operationMeta(c, request.Reason), request.IDs)
	if err != nil {
		respondControlPlaneMutationError(c, err, result)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Redemptions deleted through NewAPI", "data": result})
}

// Batch deletion remains preview-only until NewAPI exposes a bounded bulk API
// with an upstream idempotency contract. The old direct-database execution path
// is intentionally not registered in production.
func (h *MutationHandler) BatchDeleteInactiveUsers(c *gin.Context) {
	var request struct {
		ActivityLevel string `json:"activity_level"`
		DryRun        bool   `json:"dry_run"`
		HardDelete    bool   `json:"hard_delete"`
	}
	request.ActivityLevel = service.ActivityVeryInactive
	request.DryRun = true
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	if !request.DryRun {
		c.JSON(http.StatusNotImplemented, models.ErrorResp(
			"BATCH_MUTATION_NOT_SUPPORTED",
			"Batch user deletion is preview-only until NewAPI provides an auditable bulk API",
			"",
		))
		return
	}
	result, err := service.NewUserManagementService().BatchDeleteInactiveUsers(request.ActivityLevel, true, request.HardDelete, "")
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "inactive user delete preview", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *MutationHandler) PurgeSoftDeletedUsers(c *gin.Context) {
	request, err := bindOptionalPurgeSoftDeletedRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}
	if !request.DryRun {
		c.JSON(http.StatusNotImplemented, models.ErrorResp(
			"PURGE_NOT_SUPPORTED",
			"Permanent bulk purge is disabled; use version-gated NewAPI user deletion individually",
			"",
		))
		return
	}
	result, err := service.NewUserManagementService().PreviewSoftDeletedUsers()
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "soft-deleted user purge preview", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Preview completed", "data": result})
}

func (h *MutationHandler) DisableToken(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, models.ErrorResp(
		"TOKEN_MUTATION_NOT_SUPPORTED",
		"Token mutation is disabled until the versioned NewAPI token adapter is available",
		"",
	))
}

func operationMeta(c *gin.Context, reason string) controlplane.OperationMeta {
	authMethod, _ := c.Get("auth_method")
	authMethodText, _ := authMethod.(string)
	authMethodText = strings.TrimSpace(authMethodText)
	actor := ""
	verifiedAuthMethod := ""
	switch authMethodText {
	case "jwt":
		verifiedAuthMethod = "jwt"
		if value, exists := c.Get("user_sub"); exists {
			if subject, ok := value.(string); ok && strings.TrimSpace(subject) != "" {
				actor = strings.TrimSpace(subject)
			}
		}
	case "api_key":
		verifiedAuthMethod = "api_key"
		if role := auth.ContextRole(c); role != auth.RoleInvalid {
			actor = "api-key-" + string(role)
		}
	}
	return controlplane.OperationMeta{
		RequestID:      middleware.RequestID(c),
		Actor:          actor,
		ActorRole:      string(auth.ContextRole(c)),
		SourceIP:       c.ClientIP(),
		AuthMethod:     verifiedAuthMethod,
		Reason:         reason,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
	}
}

func bindOptionalJSON(c *gin.Context, target any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if err := c.ShouldBindJSON(target); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func newAPIRedemptionRequest(request service.GenerateParams) (newapi.RedemptionCreateRequest, error) {
	if strings.TrimSpace(request.KeyPrefix) != "" {
		return newapi.RedemptionCreateRequest{}, errors.New("custom key prefixes are not supported by the NewAPI admin API")
	}
	quotaMode := strings.TrimSpace(request.QuotaMode)
	if quotaMode == "" {
		quotaMode = "fixed"
	}
	if quotaMode != "fixed" || request.FixedAmount == nil {
		return newapi.RedemptionCreateRequest{}, errors.New("NewAPI redemption creation requires a fixed amount")
	}
	quotas, err := util.GenerateQuotas(1, "fixed", *request.FixedAmount, 0, 0)
	if err != nil || len(quotas) != 1 || quotas[0] <= 0 {
		return newapi.RedemptionCreateRequest{}, errors.New("invalid fixed redemption amount")
	}
	expireMode := request.ExpireMode
	if expireMode == "" {
		expireMode = "never"
	}
	expireDays := 0
	if request.ExpireDays != nil {
		expireDays = *request.ExpireDays
	}
	expireDate := ""
	if request.ExpireDate != nil {
		expireDate = *request.ExpireDate
	}
	expiredTime, err := util.CalculateExpiration(expireMode, expireDays, expireDate)
	if err != nil {
		return newapi.RedemptionCreateRequest{}, errors.New("invalid redemption expiration")
	}
	return newapi.RedemptionCreateRequest{
		Name: strings.TrimSpace(request.Name), Count: request.Count, Quota: quotas[0], ExpiredTime: expiredTime,
	}, nil
}

func respondControlPlaneMutationError(c *gin.Context, err error, result any) {
	status := http.StatusBadGateway
	code := "NEWAPI_OPERATION_FAILED"
	message := "NewAPI operation failed"
	details := gin.H{}
	switch {
	case errors.Is(err, controlplane.ErrIdempotencyRequired):
		status, code, message = http.StatusPreconditionRequired, "IDEMPOTENCY_KEY_REQUIRED", "A valid Idempotency-Key header is required"
	case errors.Is(err, controlplane.ErrReasonRequired):
		status, code, message = http.StatusBadRequest, "REASON_REQUIRED", "A concrete operator reason is required"
	case errors.Is(err, controlplane.ErrRedemptionBatchTooLarge):
		status, code, message = http.StatusUnprocessableEntity, "REDEMPTION_BATCH_TOO_LARGE", "At most 100 redemption codes may be created per operation"
	case errors.Is(err, controlplane.ErrRedemptionQuotaPerCodeTooLarge):
		status, code, message = http.StatusUnprocessableEntity, "REDEMPTION_QUOTA_PER_CODE_TOO_LARGE", "Redemption quota per code exceeds the configured safety limit"
	case errors.Is(err, controlplane.ErrRedemptionTotalQuotaTooLarge):
		status, code, message = http.StatusUnprocessableEntity, "REDEMPTION_TOTAL_QUOTA_TOO_LARGE", "Redemption batch total quota exceeds the configured safety limit"
	case errors.Is(err, controlplane.ErrOperatorRoleRequired):
		status, code, message = http.StatusForbidden, "INSUFFICIENT_ROLE", "operator role is required for this action"
	case errors.Is(err, controlplane.ErrAdminRoleRequired):
		status, code, message = http.StatusForbidden, "INSUFFICIENT_ROLE", "admin role is required for this action"
	case errors.Is(err, controlplane.ErrProtectedRootTarget):
		status, code, message = http.StatusForbidden, "PROTECTED_ROOT_USER", "NewAPI root users cannot be changed through this control plane"
	case errors.Is(err, controlplane.ErrTargetNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "Target not found"
	case errors.Is(err, controlplane.ErrCapabilityUnavailable):
		status, code, message = http.StatusConflict, "NEWAPI_CAPABILITY_UNAVAILABLE", "Operation is disabled for the detected NewAPI version"
	case errors.Is(err, controlplane.ErrAuditUnavailable):
		status, code, message = http.StatusServiceUnavailable, "AUDIT_STORE_UNAVAILABLE", "Operation was not sent because the audit store is unavailable"
	case errors.Is(err, controlplane.ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key belongs to a different operation"
	case errors.Is(err, controlplane.ErrOperationUncertain):
		status, code, message = http.StatusConflict, "OPERATION_OUTCOME_UNCERTAIN", "The operation outcome is uncertain; reconcile before retrying"
		details["do_not_retry"] = true
	case errors.Is(err, controlplane.ErrPreviousOperationFailed):
		status, code, message = http.StatusConflict, "PREVIOUS_OPERATION_FAILED", "The operation already has a recorded failed outcome; use a new idempotency key after review"
	case errors.Is(err, controlplane.ErrReplayRequiresReconciliation):
		status, code, message = http.StatusConflict, "OPERATION_ALREADY_APPLIED", "The operation already succeeded; reconcile the original response instead of creating more codes"
		details["do_not_retry"] = true
	case isAppliedButUnaudited(err):
		status, code, message = http.StatusConflict, "AUDIT_OUTCOME_PERSIST_FAILED", "Upstream applied the operation but its audit outcome is uncertain"
		details["operation_applied"] = true
		details["do_not_retry"] = true
		details["result"] = result
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		status, code, message = http.StatusGatewayTimeout, "NEWAPI_TIMEOUT", "NewAPI operation timed out; reconcile before retrying"
		details["do_not_retry"] = true
	}
	if len(details) == 0 {
		c.JSON(status, models.NewErrorResponse(code, message))
		return
	}
	c.JSON(status, models.NewErrorResponse(code, message, details))
}

func isAppliedButUnaudited(err error) bool {
	var target *controlplane.AppliedButUnauditedError
	return errors.As(err, &target)
}
