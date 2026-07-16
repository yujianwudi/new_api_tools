package middleware

import (
	"net/url"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
)

// CORSMiddleware configures CORS settings
// Empty CORS_ALLOWED_ORIGINS means same-origin only. Cross-origin access must
// opt in with exact http(s) origins; wildcard origins are never accepted.
func CORSMiddleware() gin.HandlerFunc {
	cfg := config.Get()
	origins := trustedCORSOrigins(cfg.CORSAllowedOrigins)
	if len(origins) == 0 {
		return func(c *gin.Context) {
			c.Next()
		}
	}
	return cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-API-Key", "Idempotency-Key", "X-Request-ID"},
		ExposeHeaders:    []string{"Content-Length", "X-Request-ID"},
		AllowCredentials: cfg.CORSAllowCredentials && len(origins) > 0,
		MaxAge:           12 * time.Hour,
	})
}

func trustedCORSOrigins(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(value, "*") {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			continue
		}
		if parsed.Path != "" && parsed.Path != "/" {
			continue
		}
		origin := strings.TrimRight(value, "/")
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		result = append(result, origin)
	}
	return result
}
