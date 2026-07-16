package middleware

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
)

func TestTrustedCORSOriginsRejectsWildcardsAndPaths(t *testing.T) {
	got := trustedCORSOrigins([]string{
		"https://admin.example.com",
		"https://admin.example.com/",
		"*",
		"https://*.example.com",
		"https://admin.example.com/path",
		"javascript:alert(1)",
	})
	want := []string{"https://admin.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trusted origins = %#v, want %#v", got, want)
	}
}

func TestCORSMiddlewareAllowsOnlyConfiguredOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://admin.example.com")
	t.Setenv("CORS_ALLOW_CREDENTIALS", "false")
	t.Setenv("JWT_SECRET_KEY", "cors-test-secret")
	config.Load()

	router := gin.New()
	router.Use(CORSMiddleware())
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	trusted := httptest.NewRecorder()
	trustedReq := httptest.NewRequest(http.MethodGet, "/test", nil)
	trustedReq.Header.Set("Origin", "https://admin.example.com")
	router.ServeHTTP(trusted, trustedReq)
	if got := trusted.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Fatalf("trusted origin header = %q", got)
	}
	if got := trusted.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credentials unexpectedly enabled: %q", got)
	}

	untrusted := httptest.NewRecorder()
	untrustedReq := httptest.NewRequest(http.MethodGet, "/test", nil)
	untrustedReq.Header.Set("Origin", "https://evil.example")
	router.ServeHTTP(untrusted, untrustedReq)
	if got := untrusted.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("untrusted origin was allowed: %q", got)
	}
}

func TestCORSMiddlewareAllowsAuditedMutationHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://admin.example.com")
	t.Setenv("CORS_ALLOW_CREDENTIALS", "false")
	t.Setenv("JWT_SECRET_KEY", "cors-mutation-test-secret")
	config.Load()

	router := gin.New()
	router.Use(CORSMiddleware())
	router.POST("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	preflight := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/test", nil)
	request.Header.Set("Origin", "https://admin.example.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "authorization, content-type, idempotency-key, x-api-key, x-request-id")
	router.ServeHTTP(preflight, request)

	if preflight.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", preflight.Code, http.StatusNoContent)
	}
	allowed := strings.ToLower(preflight.Header().Get("Access-Control-Allow-Headers"))
	for _, header := range []string{"authorization", "content-type", "idempotency-key", "x-api-key", "x-request-id"} {
		if !strings.Contains(allowed, header) {
			t.Fatalf("preflight omitted %q from Access-Control-Allow-Headers: %q", header, allowed)
		}
	}
}

func TestCORSMiddlewareDefaultsToSameOriginOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	t.Setenv("CORS_ALLOW_CREDENTIALS", "true")
	t.Setenv("JWT_SECRET_KEY", "cors-default-test-secret")
	config.Load()

	router := gin.New()
	router.Use(CORSMiddleware())
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	sameOrigin := httptest.NewRecorder()
	router.ServeHTTP(sameOrigin, httptest.NewRequest(http.MethodGet, "/test", nil))
	if sameOrigin.Code != http.StatusNoContent {
		t.Fatalf("same-origin request failed with %d", sameOrigin.Code)
	}

	crossOrigin := httptest.NewRecorder()
	crossOriginReq := httptest.NewRequest(http.MethodGet, "/test", nil)
	crossOriginReq.Header.Set("Origin", "https://evil.example")
	router.ServeHTTP(crossOrigin, crossOriginReq)
	if got := crossOrigin.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("default CORS policy allowed cross-origin request: %q", got)
	}
	if got := crossOrigin.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("default CORS policy enabled credentials: %q", got)
	}
}
