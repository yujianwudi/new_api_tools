package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

type stubNewAPIStatusClient struct {
	status *newapi.Status
	err    error
}

func (s stubNewAPIStatusClient) Status(context.Context) (*newapi.Status, error) {
	return s.status, s.err
}

func setupHealthTestDatabase(t *testing.T, latestLog int64) {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, created_at INTEGER)`)
	if latestLog > 0 {
		db.MustExec(`INSERT INTO logs (created_at) VALUES (?)`, latestLog)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		_ = db.Close()
		database.SetForTesting(nil)
	})
}

func TestDependencyHealthTreatsStalenessAsDiagnostic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupHealthTestDatabase(t, time.Now().Add(-time.Hour).Unix())
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := NewHealthHandler(&config.Config{
		LogFreshnessMaxAge:     time.Minute,
		NewAPIAdminAccessToken: "admin-token",
		NewAPIAdminUserID:      1,
	}, stubNewAPIStatusClient{status: &newapi.Status{Version: "v1.0.0-rc.21"}}, store, observability.NewRegistry())

	router := gin.New()
	router.GET("/dependencies", h.DependencyHealth)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dependencies", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("dependency health = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status string            `json:"status"`
		Checks []DependencyCheck `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode dependency health response: %v", err)
	}
	if response.Status != "healthy" {
		t.Fatalf("dependency health status = %q, want healthy: %s", response.Status, recorder.Body.String())
	}
	for _, check := range response.Checks {
		if check.Name != "log_freshness" {
			continue
		}
		if check.Status != "stale" || !check.Diagnostic || check.Required {
			t.Fatalf("log freshness check = %#v, want optional stale diagnostic", check)
		}
		return
	}
	t.Fatalf("dependency health omitted log_freshness check: %s", recorder.Body.String())
}

func TestLogFreshnessRejectsFutureTimestampsAsClockSkew(t *testing.T) {
	setupHealthTestDatabase(t, time.Now().Add(time.Hour).Unix())
	h := NewHealthHandler(&config.Config{LogFreshnessMaxAge: time.Minute}, nil, nil, observability.NewRegistry())

	check := h.checkLogFreshness(context.Background())
	if check.Status != "clock_skew" || check.OK {
		t.Fatalf("future log freshness = %#v, want unhealthy clock_skew", check)
	}
	if check.Details["ahead_seconds"] == nil || check.Details["latest_log_at"] == nil {
		t.Fatalf("clock skew details are incomplete: %#v", check.Details)
	}
}

func TestReadinessRequiresToolStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupHealthTestDatabase(t, 0)
	h := NewHealthHandler(&config.Config{}, nil, nil, observability.NewRegistry())
	router := gin.New()
	router.GET("/readyz", h.Readiness)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"tool_store":"unavailable"`) {
		t.Fatalf("readiness without tool store = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCompatibilityHealthRoutesDoNotRegisterUnconfiguredReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterHealthRoutes(router)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("compatibility /readyz status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestNewAPICapabilitiesFailClosedForUnknownVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHealthHandler(&config.Config{
		NewAPIAdminAccessToken: "admin-token",
		NewAPIAdminUserID:      1,
	}, stubNewAPIStatusClient{status: &newapi.Status{Version: "v2.0.0"}}, nil, observability.NewRegistry())
	router := gin.New()
	router.GET("/capabilities", h.NewAPICapabilities)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("capabilities = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	data, _ := body["data"].(map[string]any)
	if data["write_mode"] != "read_only" {
		t.Fatalf("unknown version write mode = %#v", data["write_mode"])
	}
}
