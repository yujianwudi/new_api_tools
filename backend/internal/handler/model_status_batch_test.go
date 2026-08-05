package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/cache"
)

func TestAllModelStatusCapsAt1000AndReportsTruncation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := installEmptyHandlerDatabase(t)
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
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
		t.Fatalf("begin capped status/all fixture: %v", err)
	}
	abilityStatement, err := tx.Preparex(`INSERT INTO abilities(model) VALUES (?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare capped abilities fixture: %v", err)
	}
	defer abilityStatement.Close()
	logStatement, err := tx.Preparex(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES (?, ?, 2, 1)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare capped logs fixture: %v", err)
	}
	defer logStatement.Close()

	now := time.Now().Unix() - 1
	for index := 0; index < authenticatedModelStatusMaxAll+1; index++ {
		name := fmt.Sprintf("cap-model-%04d", index)
		if _, err := abilityStatement.Exec(name); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert capped ability %d: %v", index, err)
		}
		if _, err := logStatement.Exec(name, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert capped log %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit capped status/all fixture: %v", err)
	}

	cache.Get().ClearLocal()
	t.Cleanup(func() { cache.Get().ClearLocal() })
	router := gin.New()
	api := router.Group("/api")
	api.Use(func(c *gin.Context) {
		auth.SetRole(c, auth.RoleViewer)
		c.Set("auth_method", "api_key")
		c.Next()
	})
	RegisterModelStatusRoutes(api)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/model-status/status/all?window=24h", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("capped status/all returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    []struct {
			ModelName string `json:"model_name"`
		} `json:"data"`
		SourceState string `json:"source_state"`
		TotalModels int    `json:"total_models"`
		Returned    int    `json:"returned"`
		Limit       int    `json:"limit"`
		Truncated   bool   `json:"truncated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode capped status/all response: %v", err)
	}
	if !response.Success || response.SourceState != "complete" || !response.Truncated ||
		response.TotalModels != authenticatedModelStatusMaxAll+1 ||
		response.Returned != authenticatedModelStatusMaxAll || response.Limit != authenticatedModelStatusMaxAll ||
		len(response.Data) != authenticatedModelStatusMaxAll {
		t.Fatalf("unexpected capped status/all metadata: success=%v state=%q total=%d returned=%d limit=%d truncated=%v data=%d",
			response.Success, response.SourceState, response.TotalModels, response.Returned,
			response.Limit, response.Truncated, len(response.Data))
	}
	if response.Data[0].ModelName != "cap-model-0000" ||
		response.Data[len(response.Data)-1].ModelName != "cap-model-0999" {
		t.Fatalf("capped status/all selected unexpected deterministic range: first=%q last=%q",
			response.Data[0].ModelName, response.Data[len(response.Data)-1].ModelName)
	}
}

func TestPublicAllModelStatusCountsOnlyValidModelNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configurePublicModelStatusTest(t)
	db := installEmptyHandlerDatabase(t)
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model_name TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		type INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL
	)`)

	cache.Get().ClearLocal()
	t.Cleanup(func() { cache.Get().ClearLocal() })
	catalog := []map[string]interface{}{
		{"model_name": "valid-a"},
		{"model_name": " valid-a "},
		{"model_name": "   "},
		{"request_count_24h": int64(1)},
		{"model_name": int64(7)},
		{"model_name": "  valid-b  "},
	}
	if err := cache.Get().Set("model_status:available_models", catalog, time.Minute); err != nil {
		t.Fatalf("cache malformed catalog fixture: %v", err)
	}

	router := gin.New()
	router.GET("/status/all", GetPublicAllModelsStatusHandler)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status/all?window=24h", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public status/all returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data        []map[string]interface{} `json:"data"`
		TotalModels int                      `json:"total_models"`
		Returned    int                      `json:"returned"`
		Truncated   bool                     `json:"truncated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode public status/all response: %v", err)
	}
	if response.TotalModels != 2 || response.Returned != 2 || len(response.Data) != 2 || response.Truncated {
		t.Fatalf("invalid catalog entries affected public metadata: total=%d returned=%d data=%d truncated=%v",
			response.TotalModels, response.Returned, len(response.Data), response.Truncated)
	}
	if response.Data[0]["model_name"] != "valid-a" || response.Data[1]["model_name"] != "valid-b" {
		t.Fatalf("public model names were not canonicalized/deduplicated: %#v", response.Data)
	}
}
