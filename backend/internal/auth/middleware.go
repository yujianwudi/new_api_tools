package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
)

// SkipPaths are paths that don't require authentication
var SkipPaths = map[string]bool{
	"/api/health":      true,
	"/api/health/db":   true,
	"/api/auth/login":  true,
	"/api/auth/logout": true,
}

// AuthMiddleware provides authentication via API Key or JWT Token
// Matches Python's verify_auth dependency
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Authenticated API responses can contain admin JWTs, redemption codes,
		// user data, or other bearer secrets. Prevent private browser caches and
		// intermediaries from retaining them, including the whitelisted login path.
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")

		path := c.Request.URL.Path

		// Skip authentication for health check endpoints
		if SkipPaths[path] {
			c.Set("auth_method", "skip")
			c.Next()
			return
		}

		// Try API Key authentication first (X-API-Key header)
		apiKey := c.GetHeader("X-API-Key")
		if apiKey != "" {
			if VerifyAPIKey(apiKey) {
				role := ParseRole(config.Get().APIKeyRole)
				if role == RoleInvalid {
					logger.L.Warn("API key authentication rejected because API_KEY_ROLE is invalid", logger.CatAuth)
					c.AbortWithStatusJSON(http.StatusForbidden, models.NewErrorResponse(
						"ROLE_INVALID",
						"API key role configuration is invalid",
					))
					return
				}
				c.Set("auth_method", "api_key")
				SetRole(c, role)
				c.Next()
				return
			}

			logger.L.Warn("Invalid API key for request: "+c.Request.Method+" "+path, logger.CatAuth)
			c.AbortWithStatusJSON(http.StatusUnauthorized, models.NewErrorResponse(
				"UNAUTHORIZED",
				"Invalid API key",
			))
			return
		}

		// Try JWT Token authentication (Authorization: Bearer <token>)
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			// Extract token from "Bearer <token>"
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				tokenString := parts[1]

				claims, err := ValidateToken(tokenString)
				if err == nil && claims != nil {
					c.Set("auth_method", "jwt")
					c.Set("user_sub", claims.Subject)
					SetRole(c, RoleAdmin)
					c.Next()
					return
				}

				// Invalid or expired token
				c.AbortWithStatusJSON(http.StatusUnauthorized, models.NewErrorResponse(
					"UNAUTHORIZED",
					"Invalid or expired token",
				))
				return
			}
		}

		// No authentication provided
		logger.L.Warn("Missing authentication for request: "+c.Request.Method+" "+path, logger.CatAuth)
		c.AbortWithStatusJSON(http.StatusUnauthorized, models.NewErrorResponse(
			"UNAUTHORIZED",
			"Authentication required (API Key or JWT Token)",
		))
	}
}
