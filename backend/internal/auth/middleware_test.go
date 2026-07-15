package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAuthMiddlewareDisablesAPICaching(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	api := router.Group("/api")
	api.Use(AuthMiddleware())
	api.POST("/auth/login", func(c *gin.Context) {
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
