package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func configurePublicModelStatusTest(t *testing.T) {
	t.Helper()
	t.Setenv("PUBLIC_MODEL_MAX_BATCH", "50")
	t.Setenv("PUBLIC_MODEL_MAX_BODY_BYTES", "16384")
	t.Setenv("PUBLIC_MODEL_REQUESTS_PER_MINUTE", "600")
	config.Load()

	resetRateState := func() {
		publicModelRateState.Lock()
		publicModelRateState.clients = make(map[string]publicModelRateEntry)
		publicModelRateState.nextSweep = time.Time{}
		publicModelRateState.Unlock()
	}
	resetRateState()
	t.Cleanup(resetRateState)
}

func installEmptyHandlerDatabase(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite test database: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		_ = db.Close()
		database.SetForTesting(nil)
	})
	return db
}

func TestSanitizeModelNamesBoundsAndDeduplicates(t *testing.T) {
	got, message := sanitizeModelNames([]string{" gpt-4 ", "gpt-4", "claude"}, 3)
	if message != "" {
		t.Fatalf("unexpected validation error: %s", message)
	}
	if len(got) != 2 || got[0] != "gpt-4" || got[1] != "claude" {
		t.Fatalf("unexpected sanitized models: %#v", got)
	}
	if _, message := sanitizeModelNames([]string{"a", "b", "c"}, 2); message == "" {
		t.Fatal("expected oversized model batch to be rejected")
	}
	if _, message := sanitizeModelNames([]string{""}, 2); message == "" {
		t.Fatal("expected empty model name to be rejected")
	}
}

func TestValidModelStatusWindowUsesAllowlist(t *testing.T) {
	if !validModelStatusWindow("1h") || !validModelStatusWindow("24h") {
		t.Fatal("documented windows were rejected")
	}
	if validModelStatusWindow("365d") || validModelStatusWindow("1h' OR 1=1 --") {
		t.Fatal("unknown time window was accepted")
	}
}

func TestPublicModelRateLimitAmortizesCapacitySweeps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)

	now := time.Now()
	publicModelRateState.Lock()
	for i := 0; i < maxTrackedPublicModelClients; i++ {
		publicModelRateState.clients[fmt.Sprintf("existing-%d", i)] = publicModelRateEntry{
			windowStart: now,
			lastSeen:    now,
			requests:    1,
		}
	}
	publicModelRateState.Unlock()

	router := gin.New()
	router.Use(PublicModelStatusRateLimit())
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.20:12345"
	router.ServeHTTP(httptest.NewRecorder(), request)

	publicModelRateState.Lock()
	firstSweep := publicModelRateState.nextSweep
	firstSize := len(publicModelRateState.clients)
	publicModelRateState.Unlock()
	if firstSweep.IsZero() || firstSize != maxTrackedPublicModelClients {
		t.Fatalf("first capacity sweep did not preserve bounds: next=%v size=%d", firstSweep, firstSize)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.21:12345"
	router.ServeHTTP(httptest.NewRecorder(), request)

	publicModelRateState.Lock()
	secondSweep := publicModelRateState.nextSweep
	secondSize := len(publicModelRateState.clients)
	publicModelRateState.Unlock()
	if !secondSweep.Equal(firstSweep) || secondSize != maxTrackedPublicModelClients {
		t.Fatalf("capacity handling repeated a full sweep or exceeded the bound: first=%v second=%v size=%d", firstSweep, secondSweep, secondSize)
	}
}

func TestPublicBatchLimitsDoNotConstrainAuthenticatedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)
	installEmptyHandlerDatabase(t)

	modelNames := make([]string, 51)
	for i := range modelNames {
		modelNames[i] = fmt.Sprintf("model-%d", i)
	}
	payload, err := json.Marshal(modelNames)
	if err != nil {
		t.Fatalf("marshal model names: %v", err)
	}

	publicRouter := gin.New()
	RegisterModelStatusEmbedRoutes(publicRouter)
	publicRecorder := httptest.NewRecorder()
	publicRequest := httptest.NewRequest(http.MethodPost, "/api/embed/model-status/status/multiple", strings.NewReader(string(payload)))
	publicRequest.Header.Set("Content-Type", "application/json")
	publicRequest.RemoteAddr = "192.0.2.22:12345"
	publicRouter.ServeHTTP(publicRecorder, publicRequest)
	if publicRecorder.Code != http.StatusBadRequest || !strings.Contains(publicRecorder.Body.String(), "At most 50") {
		t.Fatalf("public batch limit was not enforced: status=%d body=%s", publicRecorder.Code, publicRecorder.Body.String())
	}

	authenticatedRouter := gin.New()
	authenticatedAPI := authenticatedRouter.Group("/api")
	RegisterModelStatusRoutes(authenticatedAPI)
	authenticatedRecorder := httptest.NewRecorder()
	authenticatedRequest := httptest.NewRequest(http.MethodPost, "/api/model-status/status/multiple", strings.NewReader(string(payload)))
	authenticatedRequest.Header.Set("Content-Type", "application/json")
	authenticatedRouter.ServeHTTP(authenticatedRecorder, authenticatedRequest)
	if authenticatedRecorder.Code == http.StatusBadRequest {
		t.Fatalf("authenticated batch was incorrectly constrained by public limits: %s", authenticatedRecorder.Body.String())
	}
}

func TestPublicModelStatusRoutesHideDatabaseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)
	installEmptyHandlerDatabase(t)

	router := gin.New()
	RegisterModelStatusEmbedRoutes(router)

	paths := []string{
		"/api/embed/model-status/models",
		"/api/embed/model-status/status/all",
		"/api/embed/model-status/token-groups",
		"/api/model-status/embed/models",
		"/api/model-status/embed/status/all",
		"/api/model-status/embed/token-groups",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.RemoteAddr = "192.0.2.10:12345"
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("expected status 500, got %d: %s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "Model status data is temporarily unavailable") {
				t.Fatalf("safe public error message missing: %s", body)
			}
			for _, sensitive := range []string{"no such table", "SQL logic error", "logs", "abilities"} {
				if strings.Contains(body, sensitive) {
					t.Fatalf("public response leaked database detail %q: %s", sensitive, body)
				}
			}
		})
	}
}

func TestPublicMultipleModelStatusHidesJSONParserDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)
	installEmptyHandlerDatabase(t)

	publicRouter := gin.New()
	RegisterModelStatusEmbedRoutes(publicRouter)
	publicRecorder := httptest.NewRecorder()
	publicRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/embed/model-status/status/multiple",
		strings.NewReader("{"),
	)
	publicRequest.Header.Set("Content-Type", "application/json")
	publicRequest.RemoteAddr = "192.0.2.11:12345"
	publicRouter.ServeHTTP(publicRecorder, publicRequest)

	if publicRecorder.Code != http.StatusBadRequest {
		t.Fatalf("expected public status 400, got %d: %s", publicRecorder.Code, publicRecorder.Body.String())
	}
	if strings.Contains(publicRecorder.Body.String(), "details") ||
		strings.Contains(publicRecorder.Body.String(), "unexpected EOF") {
		t.Fatalf("public parser error leaked implementation details: %s", publicRecorder.Body.String())
	}

	authenticatedRouter := gin.New()
	authenticatedAPI := authenticatedRouter.Group("/api")
	RegisterModelStatusRoutes(authenticatedAPI)
	authenticatedRecorder := httptest.NewRecorder()
	authenticatedRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/model-status/status/multiple",
		strings.NewReader("{"),
	)
	authenticatedRequest.Header.Set("Content-Type", "application/json")
	authenticatedRouter.ServeHTTP(authenticatedRecorder, authenticatedRequest)

	if authenticatedRecorder.Code != http.StatusBadRequest {
		t.Fatalf("expected authenticated status 400, got %d: %s", authenticatedRecorder.Code, authenticatedRecorder.Body.String())
	}
	if !strings.Contains(authenticatedRecorder.Body.String(), "details") {
		t.Fatalf("authenticated route lost parser diagnostics: %s", authenticatedRecorder.Body.String())
	}
}

func TestAuthenticatedModelStatusRetainsDatabaseDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)
	installEmptyHandlerDatabase(t)

	router := gin.New()
	api := router.Group("/api")
	RegisterModelStatusRoutes(api)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/model-status/models", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no such table") ||
		!strings.Contains(recorder.Body.String(), "logs") {
		t.Fatalf("authenticated route lost database diagnostics: %s", recorder.Body.String())
	}
}
