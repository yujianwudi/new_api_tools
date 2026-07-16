package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/controlplane"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/toolstore"
)

// GetOperationReconciliation returns only the durable status of the caller's
// own idempotent control-plane mutation. A missing key and an actor or
// authentication-channel mismatch are intentionally indistinguishable.
func (h *StoreHandler) GetOperationReconciliation(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	actor, authMethod := controlPlaneMutationIdentity(c)
	if actor == "" || authMethod == "" {
		c.JSON(http.StatusUnauthorized, models.NewErrorResponse(
			"UNAUTHORIZED", "Authenticated control-plane access is required"))
		return
	}

	result, err := controlplane.LookupOperationReconciliation(
		c.Request.Context(), h.store, c.Param("idempotency_key"), actor, authMethod,
	)
	if err == nil {
		c.JSON(http.StatusOK, models.NewSuccessResponse(result))
		return
	}
	switch {
	case errors.Is(err, toolstore.ErrInvalid):
		c.JSON(http.StatusBadRequest, models.NewErrorResponse(
			"INVALID_IDEMPOTENCY_KEY", "A valid idempotency key is required"))
	case errors.Is(err, toolstore.ErrNotFound):
		c.JSON(http.StatusNotFound, models.NewErrorResponse(
			"OPERATION_NOT_FOUND", "Operation audit was not found"))
	default:
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"OPERATION_AUDIT_UNAVAILABLE", "Operation audit is temporarily unavailable"))
	}
}

// controlPlaneMutationIdentity mirrors the trusted actor and authentication
// channel attached to audited NewAPI mutations. Keep API-key role naming
// aligned with operationMeta while JWT uses the verified subject.
func controlPlaneMutationIdentity(c *gin.Context) (string, string) {
	switch strings.TrimSpace(c.GetString("auth_method")) {
	case "jwt":
		return strings.TrimSpace(c.GetString("user_sub")), "jwt"
	case "api_key":
		if role := auth.ContextRole(c); role != auth.RoleInvalid {
			return "api-key-" + string(role), "api_key"
		}
		return "", "api_key"
	}
	return "", ""
}
