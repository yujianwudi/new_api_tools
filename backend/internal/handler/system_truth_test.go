package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func installSystemTruthRouter(t *testing.T) (*gin.Engine, *sqlx.DB) {
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
	RegisterSystemRoutes(router.Group("/api"))
	return router, db
}

func TestSystemScaleUsesDashboardMetricsAndRefreshesTruthfully(t *testing.T) {
	router, db := installSystemTruthRouter(t)
	db.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, deleted_at INTEGER)`)
	db.MustExec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, created_at INTEGER)`)
	db.MustExec(`INSERT INTO users(id, deleted_at) VALUES (1, NULL), (2, NULL), (3, 123)`)
	db.MustExec(`INSERT INTO logs(id, created_at) VALUES (1, ?), (2, ?)`, time.Now().Unix(), time.Now().Unix()-60)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/system/scale"},
		{method: http.MethodPost, path: "/api/system/scale/refresh"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, body=%s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
		var body struct {
			Success bool `json:"success"`
			Data    struct {
				Scale   string `json:"scale"`
				Metrics struct {
					TotalUsers int64 `json:"total_users"`
					Logs24H    int64 `json:"logs_24h"`
					TotalLogs  int64 `json:"total_logs"`
				} `json:"metrics"`
				Settings struct {
					FrontendRefreshInterval int `json:"frontend_refresh_interval"`
				} `json:"settings"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode scale response: %v", err)
		}
		if !body.Success || body.Data.Scale != "small" || body.Data.Metrics.TotalUsers != 2 || body.Data.Metrics.Logs24H != 2 || body.Data.Metrics.TotalLogs != 2 {
			t.Fatalf("scale response did not use live metrics: %s", recorder.Body.String())
		}
		if body.Data.Settings.FrontendRefreshInterval != 30 {
			t.Fatalf("scale settings were not derived from the detected scale: %s", recorder.Body.String())
		}
	}
}

func TestWarmupStatusIsExplicitlyNotImplemented(t *testing.T) {
	router, _ := installSystemTruthRouter(t)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/system/warmup-status", nil))

	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("warmup status = %d, want 501; body=%s", recorder.Code, recorder.Body.String())
	}
	if !json.Valid(recorder.Body.Bytes()) || !containsJSONErrorCode(recorder.Body.Bytes(), "NOT_IMPLEMENTED") {
		t.Fatalf("warmup response is missing NOT_IMPLEMENTED: %s", recorder.Body.String())
	}
}

func containsJSONErrorCode(body []byte, want string) bool {
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &response) == nil && response.Error.Code == want
}
