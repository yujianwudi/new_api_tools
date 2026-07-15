package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func TestDatabaseHealthCheckHidesPingError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite test database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite test database: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() { database.SetForTesting(nil) })

	router := gin.New()
	RegisterHealthRoutes(router)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/health/db", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "Database connection unavailable") {
		t.Fatalf("safe database error message missing: %s", body)
	}
	for _, sensitive := range []string{"database is closed", "sql:"} {
		if strings.Contains(body, sensitive) {
			t.Fatalf("health response leaked database detail %q: %s", sensitive, body)
		}
	}
}
