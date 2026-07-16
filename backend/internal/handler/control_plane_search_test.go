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
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

func newControlPlaneSearchHandlerEnvironment(t *testing.T, authenticated bool) (*gin.Engine, *sqlx.DB, *sqlx.DB, *toolstore.Store) {
	return newControlPlaneSearchHandlerEnvironmentWithRole(t, authenticated, auth.RoleViewer)
}

func newControlPlaneSearchHandlerEnvironmentWithRole(t *testing.T, authenticated bool, role auth.Role) (*gin.Engine, *sqlx.DB, *sqlx.DB, *toolstore.Store) {
	t.Helper()
	mainDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open search handler main database: %v", err)
	}
	logDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open search handler log database: %v", err)
	}
	mainDB.SetMaxOpenConns(1)
	logDB.SetMaxOpenConns(1)
	mainManager := &database.Manager{DB: mainDB, IsPG: false}
	logManager := &database.Manager{DB: logDB, IsPG: false}
	database.SetForTesting(mainManager)
	database.SetLogForTesting(logManager, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	store, err := toolstore.Init(":memory:")
	if err != nil {
		database.SetForTesting(nil)
		_ = logDB.Close()
		_ = mainDB.Close()
		t.Fatalf("open search handler tool store: %v", err)
	}
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = store.Close()
		_ = logDB.Close()
		_ = mainDB.Close()
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	if authenticated {
		router.Use(func(c *gin.Context) {
			c.Set("auth_method", "jwt")
			c.Set("user_sub", "search-admin")
			if role != auth.RoleInvalid {
				auth.SetRole(c, role)
			}
			c.Next()
		})
	}
	NewSearchHandler(store).RegisterRoutes(router.Group("/api"))
	return router, mainDB, logDB, store
}

func createControlPlaneSearchHandlerTables(t *testing.T, mainDB, logDB *sqlx.DB) {
	t.Helper()
	mainDB.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`)
	mainDB.MustExec(`CREATE TABLE top_ups (
		id INTEGER PRIMARY KEY, user_id INTEGER, trade_no TEXT,
		amount INTEGER, status TEXT, create_time INTEGER
	)`)
	logDB.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY, user_id INTEGER, username TEXT,
		model_name TEXT, created_at INTEGER, type INTEGER, quota INTEGER,
		request_id TEXT
	)`)
}

func TestSearchHandlerRequiresAuthenticationAndValidatesBounds(t *testing.T) {
	unauthenticated, _, _, _ := newControlPlaneSearchHandlerEnvironment(t, false)
	response := httptest.NewRecorder()
	unauthenticated.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search status = %d: %s", response.Code, response.Body.String())
	}

	authenticated, _, _, _ := newControlPlaneSearchHandlerEnvironment(t, true)
	for _, path := range []string{
		"/api/control-plane/search?q=a",
		"/api/control-plane/search?q=alice&limit=51",
		"/api/control-plane/search?q=alice&unknown=1",
		"/api/control-plane/users/0/timeline",
		"/api/control-plane/users/42/timeline?limit=101",
		"/api/control-plane/users/42/timeline?before=invalid",
	} {
		response = httptest.NewRecorder()
		authenticated.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request %s status = %d: %s", path, response.Code, response.Body.String())
		}
	}

	roleless, _, _, _ := newControlPlaneSearchHandlerEnvironmentWithRole(t, true, auth.RoleInvalid)
	response = httptest.NewRecorder()
	roleless.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("roleless search status = %d: %s", response.Code, response.Body.String())
	}
}

func TestSearchHandlerServesSearchAndUnifiedTimeline(t *testing.T) {
	router, mainDB, logDB, store := newControlPlaneSearchHandlerEnvironment(t, true)
	createControlPlaneSearchHandlerTables(t, mainDB, logDB)
	now := time.Now().UTC().Truncate(time.Second)
	mainDB.MustExec(`INSERT INTO users(id, username) VALUES (42, 'alice')`)
	mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time)
		VALUES (11, 42, 'handler-alice-trade', 10, 'success', ?)`, now.Unix())
	logDB.MustExec(`INSERT INTO logs(id, user_id, username, model_name, created_at, type, quota, request_id)
		VALUES (21, 42, 'alice', 'handler-model', ?, 2, 1, 'handler-request-id')`, now.Unix())
	if _, err := store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "handler-timeline-audit", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
		Action: "user.inspect", TargetType: "user", TargetID: "42", Status: toolstore.OperationSucceeded,
	}); err != nil {
		t.Fatalf("append handler timeline audit: %v", err)
	}

	searchResponse := httptest.NewRecorder()
	router.ServeHTTP(searchResponse, httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice&limit=10", nil))
	if searchResponse.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", searchResponse.Code, searchResponse.Body.String())
	}
	if searchResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("search response was cacheable: %#v", searchResponse.Header())
	}
	var searchBody struct {
		Success bool `json:"success"`
		Data    struct {
			Sources []map[string]interface{} `json:"sources"`
			Results []map[string]interface{} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(searchResponse.Body.Bytes(), &searchBody); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	if !searchBody.Success || len(searchBody.Data.Sources) != 3 || len(searchBody.Data.Results) != 3 {
		t.Fatalf("unexpected search payload: %s", searchResponse.Body.String())
	}
	for _, result := range searchBody.Data.Results {
		if result["source"] == nil || result["grain"] == nil || result["freshness"] == nil || result["data_source"] == nil {
			t.Fatalf("search result metadata missing: %#v", result)
		}
	}

	timelineResponse := httptest.NewRecorder()
	router.ServeHTTP(timelineResponse, httptest.NewRequest(http.MethodGet, "/api/control-plane/users/42/timeline?limit=10", nil))
	if timelineResponse.Code != http.StatusOK {
		t.Fatalf("timeline status = %d: %s", timelineResponse.Code, timelineResponse.Body.String())
	}
	var timelineBody struct {
		Success bool `json:"success"`
		Data    struct {
			Sources []map[string]interface{} `json:"sources"`
			Events  []map[string]interface{} `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(timelineResponse.Body.Bytes(), &timelineBody); err != nil {
		t.Fatalf("decode timeline response: %v", err)
	}
	if !timelineBody.Success || len(timelineBody.Data.Sources) != 5 || len(timelineBody.Data.Events) != 3 {
		t.Fatalf("unexpected timeline payload: %s", timelineResponse.Body.String())
	}
	for _, event := range timelineBody.Data.Events {
		if event["source"] == nil || event["grain"] == nil || event["freshness"] == nil || event["data_source"] == nil {
			t.Fatalf("timeline event metadata missing: %#v", event)
		}
	}
}

func TestSearchHandlerReturnsPartialSourcesWithoutLeakingSchemaErrors(t *testing.T) {
	router, mainDB, logDB, _ := newControlPlaneSearchHandlerEnvironment(t, true)
	mainDB.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`)
	mainDB.MustExec(`INSERT INTO users(id, username) VALUES (1, 'alice')`)
	logDB.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY, user_id INTEGER, username TEXT,
		model_name TEXT, created_at INTEGER, type INTEGER, quota INTEGER
	)`)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("partial search status = %d: %s", response.Code, response.Body.String())
	}
	body := strings.ToLower(response.Body.String())
	if !strings.Contains(body, `"available":false`) || !strings.Contains(body, `"reason":"schema_unavailable"`) {
		t.Fatalf("missing source status not exposed: %s", response.Body.String())
	}
	for _, forbidden := range []string{"no such table", "no such column", "select ", "sqlite"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("partial response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestSearchHandlerFailsSafelyWhenMainDatabaseUnavailable(t *testing.T) {
	router, mainDB, _, _ := newControlPlaneSearchHandlerEnvironment(t, true)
	if err := mainDB.Close(); err != nil {
		t.Fatalf("close main database: %v", err)
	}

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed main database status = %d: %s", response.Code, response.Body.String())
	}
	body := strings.ToLower(response.Body.String())
	if !strings.Contains(body, "main_database_unavailable") || strings.Contains(body, "database is closed") || strings.Contains(body, "sqlite") {
		t.Fatalf("main database error was unsafe: %s", response.Body.String())
	}
}

func TestSearchHandlerHonorsCanceledRequestContext(t *testing.T) {
	router, mainDB, logDB, _ := newControlPlaneSearchHandlerEnvironment(t, true)
	createControlPlaneSearchHandlerTables(t, mainDB, logDB)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/control-plane/search?q=alice", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "QUERY_TIMEOUT") {
		t.Fatalf("canceled search status = %d: %s", response.Code, response.Body.String())
	}
}
