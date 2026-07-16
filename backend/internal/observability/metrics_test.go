package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMetricsUseRouteTemplatesAndRequireToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := NewRegistry()
	router := gin.New()
	router.Use(registry.HTTPMiddleware())
	router.GET("/users/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/metrics", registry.Handler("metrics-secret"))

	request := httptest.NewRequest(http.MethodGet, "/users/123456", nil)
	router.ServeHTTP(httptest.NewRecorder(), request)

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without token = %d", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Authorization", "Bearer metrics-secret")
	router.ServeHTTP(authorized, metricsRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("metrics with token = %d", authorized.Code)
	}
	body := authorized.Body.String()
	if !strings.Contains(body, `route="/users/:id"`) || strings.Contains(body, "123456") {
		t.Fatalf("metrics did not use bounded route template: %s", body)
	}
}

func TestMetricsExposeDependenciesFreshnessAndOperations(t *testing.T) {
	registry := NewRegistry()
	registry.SetDependency("main_database", true)
	registry.SetLogFreshness(1710000000, 5*time.Second)
	registry.IncOperation("user_disable", "success")

	body := registry.snapshot()
	for _, expected := range []string{
		`new_api_tools_dependency_up{dependency="main_database"} 1`,
		`new_api_tools_log_freshness_lag_seconds 5.000`,
		`new_api_tools_control_operations_total{action="user_disable",result="success"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing metric %q in:\n%s", expected, body)
		}
	}
}
