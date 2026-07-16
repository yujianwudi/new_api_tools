package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/requestmeta"
)

func TestRequestIDMiddlewareAcceptsSafeCallerID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestIDMiddleware())
	router.GET("/test", func(c *gin.Context) {
		if got := requestmeta.RequestID(c.Request.Context()); got != "relay-req_1234" {
			t.Fatalf("context request id = %q", got)
		}
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set(RequestIDHeader, "relay-req_1234")
	router.ServeHTTP(recorder, request)

	if got := recorder.Header().Get(RequestIDHeader); got != "relay-req_1234" {
		t.Fatalf("response request id = %q", got)
	}
}

func TestRequestIDMiddlewareRejectsUnsafeCallerID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestIDMiddleware())
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set(RequestIDHeader, "bad\nlog-entry")
	router.ServeHTTP(recorder, request)

	got := recorder.Header().Get(RequestIDHeader)
	if got == "" || strings.ContainsAny(got, "\r\n") || got == "bad\nlog-entry" {
		t.Fatalf("unsafe request id was reflected: %q", got)
	}
}
