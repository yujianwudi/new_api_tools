package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func installDashboardScaleRouter(t *testing.T) *gin.Engine {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite test database: %v", err)
	}
	db.SetMaxOpenConns(1)
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api")
	RegisterDashboardRoutes(api)
	return router
}

func TestRefreshEstimateRejectsInvalidPeriod(t *testing.T) {
	router := installDashboardScaleRouter(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/dashboard/refresh-estimate?period=30d", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(strings.ToLower(recorder.Body.String()), "sql") {
		t.Fatalf("invalid period should be rejected before database access: %s", recorder.Body.String())
	}
}

func TestDashboardMetricQueryFailuresAreSafe(t *testing.T) {
	router := installDashboardScaleRouter(t) // Deliberately no users/logs tables.

	systemRecorder := httptest.NewRecorder()
	systemRequest := httptest.NewRequest(http.MethodGet, "/api/dashboard/system-info", nil)
	router.ServeHTTP(systemRecorder, systemRequest)
	if systemRecorder.Code != http.StatusOK {
		t.Fatalf("system-info status = %d, body = %s", systemRecorder.Code, systemRecorder.Body.String())
	}
	var systemBody struct {
		Success bool `json:"success"`
		Data    struct {
			Scale         string `json:"scale"`
			IsLargeSystem bool   `json:"is_large_system"`
			Degraded      bool   `json:"degraded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(systemRecorder.Body.Bytes(), &systemBody); err != nil {
		t.Fatalf("decode system-info response: %v", err)
	}
	if !systemBody.Success || systemBody.Data.Scale != "xlarge" || !systemBody.Data.IsLargeSystem || !systemBody.Data.Degraded {
		t.Fatalf("system-info did not fail closed: %s", systemRecorder.Body.String())
	}
	assertNoDashboardDatabaseDetails(t, systemRecorder.Body.String())

	estimateRecorder := httptest.NewRecorder()
	estimateRequest := httptest.NewRequest(http.MethodGet, "/api/dashboard/refresh-estimate?period=7d", nil)
	router.ServeHTTP(estimateRecorder, estimateRequest)
	if estimateRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("refresh-estimate status = %d, body = %s", estimateRecorder.Code, estimateRecorder.Body.String())
	}
	if !strings.Contains(estimateRecorder.Body.String(), "Dashboard refresh estimate unavailable") {
		t.Fatalf("refresh-estimate should return a stable safe error: %s", estimateRecorder.Body.String())
	}
	assertNoDashboardDatabaseDetails(t, estimateRecorder.Body.String())
}

func assertNoDashboardDatabaseDetails(t *testing.T, body string) {
	t.Helper()
	for _, detail := range []string{"no such table", "SQL logic error", "database is closed", "SELECT COUNT"} {
		if strings.Contains(body, detail) {
			t.Fatalf("dashboard response leaked database detail %q: %s", detail, body)
		}
	}
}
