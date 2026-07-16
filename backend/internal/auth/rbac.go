package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
)

type Role string

const (
	RoleInvalid  Role = ""
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

const roleContextKey = "auth_role"

var viewerReadOnlyPOST = map[string]struct{}{
	"/api/tokens/search":                {},
	"/api/ip/geo/batch":                 {},
	"/api/model-status/status/multiple": {},
	"/api/model-status/status/batch":    {},
}

func ParseRole(value string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(value))) {
	case RoleViewer:
		return RoleViewer
	case RoleOperator:
		return RoleOperator
	case RoleAdmin:
		return RoleAdmin
	default:
		return RoleInvalid
	}
}

func SetRole(c *gin.Context, role Role) {
	if c != nil {
		c.Set(roleContextKey, role)
	}
}

func ContextRole(c *gin.Context) Role {
	if c == nil {
		return ""
	}
	value, _ := c.Get(roleContextKey)
	switch role := value.(type) {
	case Role:
		return ParseRole(string(role))
	case string:
		return ParseRole(role)
	default:
		return ""
	}
}

func HasRole(c *gin.Context, required Role) bool {
	if !validRole(required) {
		return false
	}
	return roleRank(ContextRole(c)) >= roleRank(required)
}

// RBACMiddleware establishes a conservative baseline: viewers can read,
// operators can mutate, and individual high-risk routes may additionally
// require admin. A small allowlist covers POST endpoints that are genuinely
// read-only but cannot be represented as GET for compatibility reasons.
func RBACMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authMethod, _ := c.Get("auth_method")
		if authMethod == "skip" {
			c.Next()
			return
		}
		if ContextRole(c) == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, models.NewErrorResponse(
				"ROLE_MISSING",
				"Authenticated principal has no assigned role",
			))
			return
		}
		if requestIsReadOnly(c.Request.Method, c.Request.URL.Path) || HasRole(c, RoleOperator) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, models.NewErrorResponse(
			"INSUFFICIENT_ROLE",
			"Operator role is required for this action",
		))
	}
}

func RequireRole(required Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if HasRole(c, required) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, models.NewErrorResponse(
			"INSUFFICIENT_ROLE",
			string(required)+" role is required for this action",
		))
	}
}

func requestIsReadOnly(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		_, ok := viewerReadOnlyPOST[path]
		return ok
	default:
		return false
	}
}

func roleRank(role Role) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

func validRole(role Role) bool {
	switch role {
	case RoleViewer, RoleOperator, RoleAdmin:
		return true
	default:
		return false
	}
}
