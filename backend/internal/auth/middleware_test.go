package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
)

func TestAuthMiddlewareDisablesAPICaching(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	api := router.Group("/api")
	api.Use(AuthMiddleware())
	api.POST("/auth/login", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	api.POST("/auth/logout", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	api.GET("/auth/session", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	api.GET("/auth/login/extra", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	api.GET("/protected", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{
			name:       "whitelisted login response",
			method:     http.MethodPost,
			path:       "/api/auth/login",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "whitelisted logout response",
			method:     http.MethodPost,
			path:       "/api/auth/logout",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "unlisted auth endpoint remains protected",
			method:     http.MethodGet,
			path:       "/api/auth/session",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "login subpath remains protected",
			method:     http.MethodGet,
			path:       "/api/auth/login/extra",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "rejected protected response",
			method:     http.MethodGet,
			path:       "/api/protected",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, nil)

			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if got := recorder.Header().Get("Pragma"); got != "no-cache" {
				t.Fatalf("Pragma = %q, want no-cache", got)
			}
		})
	}
}

func TestAuthMiddlewareRejectsValidAPIKeyWithInvalidRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("API_KEY", "unit-test-api-key")
	t.Setenv("API_KEY_ROLE", "superuser")
	t.Setenv("JWT_SECRET_KEY", "unit-test-jwt-secret")
	if role := config.Load().APIKeyRole; role != "" {
		t.Fatalf("invalid configured role = %q, want empty", role)
	}

	router := gin.New()
	router.GET("/protected", AuthMiddleware(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("X-API-Key", "unit-test-api-key")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("invalid API key role status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if !strings.Contains(recorder.Body.String(), "ROLE_INVALID") {
		t.Fatalf("invalid role response missing stable code: %s", recorder.Body.String())
	}
}
