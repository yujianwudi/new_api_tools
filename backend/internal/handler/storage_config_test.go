package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStorageConfigAccessIsDisabledInV050(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterStorageRoutes(router.Group("/api"))

	requests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "list", method: http.MethodGet, path: "/api/storage/config"},
		{name: "read legacy secret", method: http.MethodGet, path: "/api/storage/config/NEWAPI_ADMIN_ACCESS_TOKEN"},
		{name: "set", method: http.MethodPost, path: "/api/storage/config", body: `{"key":"unsafe","value":"write"}`},
		{name: "delete", method: http.MethodDelete, path: "/api/storage/config/unsafe"},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotImplemented, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "NOT_IMPLEMENTED") {
				t.Fatalf("response did not identify disabled storage config access: %s", recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "NEWAPI_ADMIN_ACCESS_TOKEN") {
				t.Fatalf("response echoed a legacy secret key or value: %s", recorder.Body.String())
			}
		})
	}
}
