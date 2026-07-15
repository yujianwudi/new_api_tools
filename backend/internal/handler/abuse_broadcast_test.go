package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
)

func TestUpdateAbuseBroadcastSettingsReturnsSanitizedJSONError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAbuseBroadcastRoutes(router.Group("/api"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/abuse-broadcast/settings", strings.NewReader(`{"secret":`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"invalid JSON body"`) {
		t.Fatalf("safe JSON error message missing: %s", body)
	}
	for _, sensitive := range []string{"unexpected eof", "syntax error"} {
		if strings.Contains(strings.ToLower(body), sensitive) {
			t.Fatalf("JSON error response leaked decoder detail %q: %s", sensitive, body)
		}
	}
}

func TestMatchAbuseBroadcastReportReturnsSanitizedNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SQL_DSN", "not-used")
	t.Setenv("DATA_DIR", t.TempDir())
	config.Load()

	router := gin.New()
	RegisterAbuseBroadcastRoutes(router.Group("/api"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/abuse-broadcast/reports/missing-report/matches", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"Abuse broadcast report not found"`) {
		t.Fatalf("safe not-found message missing: %s", body)
	}
	for _, sensitive := range []string{"report_id", "missing-report"} {
		if strings.Contains(body, sensitive) {
			t.Fatalf("not-found response leaked service detail %q: %s", sensitive, body)
		}
	}
}

func TestMatchAbuseBroadcastReportReturnsSanitizedInternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	blockedDataDir := filepath.Join(t.TempDir(), "blocked-data-dir")
	if err := os.WriteFile(blockedDataDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	t.Setenv("SQL_DSN", "not-used")
	t.Setenv("DATA_DIR", blockedDataDir)
	config.Load()

	router := gin.New()
	RegisterAbuseBroadcastRoutes(router.Group("/api"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/abuse-broadcast/reports/any-report/matches", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"Unable to match the abuse broadcast report"`) {
		t.Fatalf("safe internal-error message missing: %s", body)
	}
	for _, sensitive := range []string{"blocked-data-dir", "not a directory", "file exists", "mkdir"} {
		if strings.Contains(strings.ToLower(body), sensitive) {
			t.Fatalf("internal-error response leaked service detail %q: %s", sensitive, body)
		}
	}
}
