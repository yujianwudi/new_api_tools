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

func TestResolvePendingAutoGroupAuditReturnsFixedReadOnlyResponseBeforeParsing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{"", `{"operation_id":`, `{"operation_id":"legacy-1"}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/api/auto-group/pending-audits/resolve",
			strings.NewReader(body),
		)
		c.Request.Header.Set("Content-Type", "application/json")

		ResolvePendingAutoGroupAudit(c)

		if w.Code != http.StatusNotImplemented {
			t.Fatalf("body %q: expected status %d, got %d: %s", body, http.StatusNotImplemented, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "LEGACY_AUDIT_READ_ONLY") {
			t.Fatalf("body %q: expected fixed read-only response, got %s", body, w.Body.String())
		}
	}
}
