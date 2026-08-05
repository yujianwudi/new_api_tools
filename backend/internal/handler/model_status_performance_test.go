//go:build performance

package handler

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

const (
	modelStatusPerformanceModelCount = 1000
	modelStatusPerformanceWarmups    = 3
	modelStatusPerformanceSamples    = 20

	modelStatusBatchP95Limit       = 2 * time.Second
	modelStatusConfigWriteP95Limit = 500 * time.Millisecond
	modelStatusRedisFaultP95Limit  = 500 * time.Millisecond
)

// TestModelStatusPerformanceSLO is an opt-in release acceptance test. Run it
// with:
//
//	go test -tags=performance -run '^TestModelStatusPerformanceSLO$' -count=1 -v ./internal/handler
//
// The status measurement traverses the authenticated Gin route
// GET /api/model-status/status/all -> GetAllModelsStatusHandler ->
// ModelStatusService.GetAllModelsStatus for a real 1,000-model SQLite fixture.
// Three untimed requests warm SQLite's connection and page cache. The
// application result cache is cleared before every measured request so the 20
// hot samples still traverse the real catalog and status SQL path instead of
// benchmarking JSON serialization of cached service results. The durable-write
// measurement traverses the authenticated operator route
// PUT /api/model-status/config/theme through the service into Tool Store's
// audited BEGIN IMMEDIATE transaction. Any SLO violation calls Fatalf, so the
// command exits non-zero.
func TestModelStatusPerformanceSLO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configureModelStatusPerformanceAuth(t)
	installModelStatusPerformanceDatabase(t)

	manager := cache.Get()
	manager.ClearLocal()
	t.Cleanup(manager.ClearLocal)

	storePath := filepath.Join(t.TempDir(), "model-status-config.db")
	store, err := toolstore.Init(storePath)
	if err != nil {
		t.Fatalf("initialize performance Tool Store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restoreRuntime := service.ConfigureModelStatusConfigRuntime(store, manager, false)
	t.Cleanup(restoreRuntime)

	router := newModelStatusPerformanceRouter()

	t.Run("authenticated_1000_model_status_all", func(t *testing.T) {
		for warmup := 0; warmup < modelStatusPerformanceWarmups; warmup++ {
			manager.ClearLocal()
			recorder, _ := modelStatusPerformanceRequest(router, http.MethodGet,
				"/api/model-status/status/all?window=24h", "", fmt.Sprintf("model-status-warmup-%02d", warmup))
			assertModelStatusBatchResponse(t, recorder)
		}

		samples := make([]time.Duration, 0, modelStatusPerformanceSamples)
		for sample := 0; sample < modelStatusPerformanceSamples; sample++ {
			manager.ClearLocal()
			recorder, elapsed := modelStatusPerformanceRequest(router, http.MethodGet,
				"/api/model-status/status/all?window=24h", "", fmt.Sprintf("model-status-sample-%02d", sample))
			assertModelStatusBatchResponse(t, recorder)
			samples = append(samples, elapsed)
		}

		p95 := modelStatusPerformanceP95(samples)
		t.Logf("authenticated GET /api/model-status/status/all: models=%d warmups=%d samples=%d min=%s p95=%s max=%s limit=%s",
			modelStatusPerformanceModelCount, modelStatusPerformanceWarmups, len(samples),
			modelStatusPerformanceMin(samples), p95, modelStatusPerformanceMax(samples), modelStatusBatchP95Limit)
		if p95 > modelStatusBatchP95Limit {
			t.Fatalf("authenticated 1,000-model status/all p95 %s exceeds %s", p95, modelStatusBatchP95Limit)
		}
	})

	t.Run("durable_config_commit", func(t *testing.T) {
		initial, err := store.GetLatestModelStatusConfig(t.Context())
		if err != nil {
			t.Fatalf("read initial durable model-status config: %v", err)
		}
		sequence := 0
		write := func() (*httptest.ResponseRecorder, time.Duration) {
			sequence++
			theme := "daylight"
			if sequence%2 == 1 {
				theme = "obsidian"
			}
			body := fmt.Sprintf(`{"theme":%q,"reason":"model status performance acceptance"}`, theme)
			return modelStatusPerformanceRequest(router, http.MethodPut,
				"/api/model-status/config/theme", body, fmt.Sprintf("model-config-write-%04d", sequence))
		}

		for warmup := 0; warmup < modelStatusPerformanceWarmups; warmup++ {
			recorder, _ := write()
			assertModelStatusConfigWriteResponse(t, recorder)
		}

		samples := make([]time.Duration, 0, modelStatusPerformanceSamples)
		for sample := 0; sample < modelStatusPerformanceSamples; sample++ {
			recorder, elapsed := write()
			assertModelStatusConfigWriteResponse(t, recorder)
			samples = append(samples, elapsed)
		}

		p95 := modelStatusPerformanceP95(samples)
		t.Logf("authenticated PUT /api/model-status/config/theme durable commit: warmups=%d samples=%d min=%s p95=%s max=%s limit=%s",
			modelStatusPerformanceWarmups, len(samples), modelStatusPerformanceMin(samples), p95,
			modelStatusPerformanceMax(samples), modelStatusConfigWriteP95Limit)
		if p95 > modelStatusConfigWriteP95Limit {
			t.Fatalf("model-status durable config commit p95 %s exceeds %s", p95, modelStatusConfigWriteP95Limit)
		}

		latest, err := store.GetLatestModelStatusConfig(t.Context())
		if err != nil {
			t.Fatalf("read latest durable model-status config: %v", err)
		}
		wantVersion := initial.Version + int64(modelStatusPerformanceWarmups+modelStatusPerformanceSamples)
		if latest.Version != wantVersion || latest.ChangedKey != "theme" {
			t.Fatalf("timed writes were not durably committed: version=%d key=%q, want version=%d key=theme",
				latest.Version, latest.ChangedKey, wantVersion)
		}
	})

	t.Run("redis_fault_returns_fast_503_without_commit", func(t *testing.T) {
		before, err := store.GetLatestModelStatusConfig(t.Context())
		if err != nil {
			t.Fatalf("read durable version before Redis fault: %v", err)
		}

		// redisRequired with no usable manager is the deterministic unavailable
		// Redis state. The service must fail during preflight, before Tool Store.
		restoreUnavailable := service.ConfigureModelStatusConfigRuntime(store, nil, true)
		defer restoreUnavailable()

		for warmup := 0; warmup < modelStatusPerformanceWarmups; warmup++ {
			recorder, _ := modelStatusPerformanceRequest(router, http.MethodPut,
				"/api/model-status/config/theme", `{"theme":"github","reason":"Redis outage acceptance"}`,
				fmt.Sprintf("model-config-redis-warmup-%02d", warmup))
			assertModelStatusRedisUnavailableResponse(t, recorder)
		}

		samples := make([]time.Duration, 0, modelStatusPerformanceSamples)
		for sample := 0; sample < modelStatusPerformanceSamples; sample++ {
			recorder, elapsed := modelStatusPerformanceRequest(router, http.MethodPut,
				"/api/model-status/config/theme", `{"theme":"github","reason":"Redis outage acceptance"}`,
				fmt.Sprintf("model-config-redis-sample-%02d", sample))
			assertModelStatusRedisUnavailableResponse(t, recorder)
			samples = append(samples, elapsed)
		}

		p95 := modelStatusPerformanceP95(samples)
		t.Logf("authenticated PUT /api/model-status/config/theme with required Redis unavailable: warmups=%d samples=%d min=%s p95=%s max=%s limit=%s status=503",
			modelStatusPerformanceWarmups, len(samples), modelStatusPerformanceMin(samples), p95,
			modelStatusPerformanceMax(samples), modelStatusRedisFaultP95Limit)
		if p95 > modelStatusRedisFaultP95Limit {
			t.Fatalf("Redis-unavailable config write p95 %s exceeds fast-failure limit %s", p95, modelStatusRedisFaultP95Limit)
		}

		after, err := store.GetLatestModelStatusConfig(t.Context())
		if err != nil {
			t.Fatalf("read durable version after Redis fault: %v", err)
		}
		if after.Version != before.Version {
			t.Fatalf("Redis preflight failures were falsely committed: version advanced from %d to %d",
				before.Version, after.Version)
		}
	})
}

func configureModelStatusPerformanceAuth(t *testing.T) {
	t.Helper()
	// Register this cleanup before Setenv so the environment is restored first.
	t.Cleanup(func() { config.Load() })
	t.Setenv("API_KEY", "model-status-performance-api-key")
	t.Setenv("API_KEY_ROLE", "operator")
	t.Setenv("ADMIN_PASSWORD", "model-status-performance-admin-password")
	t.Setenv("JWT_SECRET_KEY", "model-status-performance-jwt-secret")
	config.Load()
}

func installModelStatusPerformanceDatabase(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", filepath.Join(t.TempDir(), "model-status.db"))
	if err != nil {
		t.Fatalf("open model-status performance database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`PRAGMA journal_mode = WAL`)
	db.MustExec(`PRAGMA synchronous = NORMAL`)
	db.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY,
		model_name TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		type INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL
	)`)
	db.MustExec(`CREATE INDEX idx_logs_model_name ON logs(model_name)`)
	db.MustExec(`CREATE INDEX idx_logs_type_created_model ON logs(type, created_at, model_name)`)
	db.MustExec(`CREATE TABLE abilities (model TEXT NOT NULL)`)
	db.MustExec(`CREATE UNIQUE INDEX idx_abilities_model ON abilities(model)`)

	tx, err := db.Beginx()
	if err != nil {
		_ = db.Close()
		t.Fatalf("begin model-status fixture transaction: %v", err)
	}
	abilityInsert, err := tx.Preparex(`INSERT INTO abilities(model) VALUES (?)`)
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("prepare ability fixture insert: %v", err)
	}
	defer abilityInsert.Close()
	logInsert, err := tx.Preparex(`INSERT INTO logs(id, model_name, created_at, type, completion_tokens)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("prepare log fixture insert: %v", err)
	}
	defer logInsert.Close()

	now := time.Now().Unix()
	for modelIndex := 0; modelIndex < modelStatusPerformanceModelCount; modelIndex++ {
		name := fmt.Sprintf("performance-model-%04d", modelIndex)
		if _, err := abilityInsert.Exec(name); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatalf("insert ability fixture %d: %v", modelIndex, err)
		}
		requestType := 2
		if modelIndex%20 == 0 {
			requestType = 5
		}
		if _, err := logInsert.Exec(modelIndex+1, name, now-int64(modelIndex%300), requestType, 1); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatalf("insert log fixture %d: %v", modelIndex, err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		t.Fatalf("commit model-status fixture: %v", err)
	}

	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db
}

func newModelStatusPerformanceRouter() *gin.Engine {
	router := gin.New()
	router.Use(middleware.RequestIDMiddleware())
	api := router.Group("/api")
	api.Use(auth.AuthMiddleware())
	api.Use(auth.RBACMiddleware())
	RegisterModelStatusRoutes(api)
	return router
}

func modelStatusPerformanceRequest(
	router http.Handler,
	method string,
	path string,
	body string,
	requestID string,
) (*httptest.ResponseRecorder, time.Duration) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-API-Key", "model-status-performance-api-key")
	request.Header.Set(middleware.RequestIDHeader, requestID)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	started := time.Now()
	router.ServeHTTP(recorder, request)
	return recorder, time.Since(started)
}

func assertModelStatusBatchResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("model-status batch returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode model-status batch response: %v", err)
	}
	if !response.Success || len(response.Data) != modelStatusPerformanceModelCount {
		t.Fatalf("model-status batch response success=%v models=%d, want success and %d models",
			response.Success, len(response.Data), modelStatusPerformanceModelCount)
	}
}

func assertModelStatusConfigWriteResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("model-status config write returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool  `json:"success"`
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode model-status config response: %v", err)
	}
	if !response.Success || response.Version <= 1 {
		t.Fatalf("config write was not reported as durable success: %s", recorder.Body.String())
	}
}

func assertModelStatusRedisUnavailableResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis-unavailable config write returned %d, want 503: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Redis-unavailable response: %v", err)
	}
	if response.Success || response.Error.Code != "MODEL_CONFIG_UNAVAILABLE" {
		t.Fatalf("Redis failure was disguised as success: %s", recorder.Body.String())
	}
}

func modelStatusPerformanceP95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	rank := int(math.Ceil(0.95*float64(len(ordered)))) - 1
	if rank < 0 {
		rank = 0
	}
	return ordered[rank]
}

func modelStatusPerformanceMin(samples []time.Duration) time.Duration {
	minimum := samples[0]
	for _, sample := range samples[1:] {
		if sample < minimum {
			minimum = sample
		}
	}
	return minimum
}

func modelStatusPerformanceMax(samples []time.Duration) time.Duration {
	maximum := samples[0]
	for _, sample := range samples[1:] {
		if sample > maximum {
			maximum = sample
		}
	}
	return maximum
}
