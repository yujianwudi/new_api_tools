package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/modelprobe"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestModelProbeRoutesExposeDisabledStateAndRejectUnconfiguredRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "probe-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := modelprobe.NewManager(&config.Config{
		ModelProbeEnabled: false, ModelProbeDailyRequestBudget: 500,
		ModelProbeInterval: 300_000_000_000, ModelProbeStaleAfter: 900_000_000_000,
		ModelProbeRetention: 30 * 24 * 60 * 60 * 1_000_000_000,
	}, store)
	defer manager.Close()
	router := gin.New()
	NewModelProbeHandler(manager).RegisterRoutes(router.Group("/api"))

	summaryBody, _ := json.Marshal(map[string]any{"models": []string{"provider/model-with-slash"}})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/model-status/probes/summary", bytes.NewReader(summaryBody))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"state":"disabled"`)) {
		t.Fatalf("disabled summary = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/model-status/probes/run", bytes.NewReader(summaryBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "handler-probe-test")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !bytes.Contains(recorder.Body.Bytes(), []byte("PROBE_DISABLED")) {
		t.Fatalf("disabled run = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/model-status/probes/history?model=provider%2Fmodel-with-slash&limit=20", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("slash model history = %d %s", recorder.Code, recorder.Body.String())
	}
}
