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
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/service"
	_ "modernc.org/sqlite"
)

func installChannelQualityHandlerDB(t *testing.T, status database.LogSourceStatus) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open channel quality handler database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE logs (
		channel_id INTEGER,
		type INTEGER,
		quota INTEGER,
		created_at INTEGER,
		use_time REAL
	)`)
	manager := &database.Manager{DB: db, IsPG: false}
	database.SetForTesting(manager)
	database.SetLogForTesting(manager, status)
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db
}

func channelQualityTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Production route wiring is intentionally not part of this change.
	router.GET("/channel-quality", GetChannelQuality)
	return router
}

func TestChannelQualityHandlerReturnsReadOnlySnapshotMetadata(t *testing.T) {
	status := database.LogSourceStatus{
		Mode:          database.LogSourceModeFallback,
		Configured:    true,
		Healthy:       false,
		UsingFallback: true,
		CheckedAt:     time.Now().Add(-time.Minute),
	}
	db := installChannelQualityHandlerDB(t, status)
	now := time.Now().Unix()
	db.MustExec(
		`INSERT INTO logs(channel_id, type, quota, created_at, use_time) VALUES
			(8, 2, 10, ?, 1.5),
			(8, 5, 0, ?, 2.5)`,
		now-1,
		now-2,
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/channel-quality?window=1h", nil)
	channelQualityTestRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("channel quality status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("channel quality response is cacheable: %q", recorder.Header().Get("Cache-Control"))
	}
	var body struct {
		Success bool                         `json:"success"`
		Data    service.ChannelQualityReport `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode channel quality response: %v", err)
	}
	if !body.Success || body.Data.GeneratedAt <= 0 || body.Data.Window != "1h" {
		t.Fatalf("missing snapshot metadata: %#v", body)
	}
	if body.Data.DataSource.Mode != database.LogSourceModeFallback || !body.Data.DataSource.Fallback {
		t.Fatalf("fallback source was hidden: %#v", body.Data.DataSource)
	}
	if len(body.Data.Channels) != 1 {
		t.Fatalf("channel count = %d, want 1", len(body.Data.Channels))
	}
	channel := body.Data.Channels[0]
	if channel.ChannelID != 8 || channel.SuccessCount != 1 || channel.FailureCount != 1 || channel.SuccessRate != 50 {
		t.Fatalf("unexpected channel quality payload: %#v", channel)
	}
	if !channel.SmallSample || channel.Confidence != "low" {
		t.Fatalf("small sample confidence missing: %#v", channel)
	}
	for _, forbidden := range []string{"profit", "margin", "ban", "route_action", "auto_disable"} {
		if strings.Contains(strings.ToLower(recorder.Body.String()), forbidden) {
			t.Fatalf("read-only response contained decision/profit field %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestChannelQualityHandlerRejectsInvalidWindowBeforeDatabaseAccess(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/channel-quality?window=30d", nil)
	channelQualityTestRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid window status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid-window response is cacheable: %q", recorder.Header().Get("Cache-Control"))
	}
	if !strings.Contains(recorder.Body.String(), "INVALID_WINDOW") {
		t.Fatalf("stable invalid-window error missing: %s", recorder.Body.String())
	}
}

func TestChannelQualityHandlerHidesDatabaseDiagnostics(t *testing.T) {
	db := installChannelQualityHandlerDB(t, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})
	if err := db.Close(); err != nil {
		t.Fatalf("close channel quality database: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/channel-quality?window=24h", nil)
	channelQualityTestRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("database failure status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), genericUnavailableMessage) {
		t.Fatalf("safe query error missing: %s", recorder.Body.String())
	}
	for _, sensitive := range []string{"database is closed", "SELECT channel_id", "query channel quality logs"} {
		if strings.Contains(recorder.Body.String(), sensitive) {
			t.Fatalf("response leaked diagnostic %q: %s", sensitive, recorder.Body.String())
		}
	}
}

func TestChannelQualityHandlerPropagatesCanceledRequestContext(t *testing.T) {
	installChannelQualityHandlerDB(t, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/channel-quality?window=7d", nil).WithContext(ctx)
	channelQualityTestRouter().ServeHTTP(recorder, request)

	if recorder.Code != statusClientClosedRequest {
		t.Fatalf("canceled request status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "QUERY_CANCELED") || strings.Contains(recorder.Body.String(), "context canceled") {
		t.Fatalf("canceled request response was unsafe: %s", recorder.Body.String())
	}
}

func TestChannelQualityHandlerDistinguishesDeadlineExceeded(t *testing.T) {
	installChannelQualityHandlerDB(t, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/channel-quality?window=7d", nil).WithContext(ctx)
	channelQualityTestRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline request status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "QUERY_TIMEOUT") || strings.Contains(recorder.Body.String(), "context deadline exceeded") {
		t.Fatalf("deadline response was unsafe: %s", recorder.Body.String())
	}
}
