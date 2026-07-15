package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterAutoGroupPendingAuditRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAutoGroupRoutes(router.Group("/api"))

	routes := make(map[string]struct{})
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, want := range []string{
		"GET /api/auto-group/pending-audits",
		"POST /api/auto-group/pending-audits/resolve",
	} {
		if _, ok := routes[want]; !ok {
			t.Fatalf("pending audit route %q is not registered", want)
		}
	}
}

func TestResolvePendingAutoGroupAuditRejectsMalformedJSONBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/auto-group/pending-audits/resolve",
		strings.NewReader(`{"operation_id":`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	ResolvePendingAutoGroupAudit(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_PARAMS") {
		t.Fatalf("expected invalid request error, got %s", w.Body.String())
	}
}
