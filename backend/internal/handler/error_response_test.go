package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRespondHandlerErrorHidesDatabaseDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sentinel := errors.New("sentinel-db-error: SELECT secret FROM users; postgres://admin:password@db.internal/newapi")

	tests := []struct {
		name          string
		status        int
		code          string
		clientMessage string
	}{
		{
			name:          "internal error",
			status:        http.StatusInternalServerError,
			code:          "QUERY_ERROR",
			clientMessage: genericUnavailableMessage,
		},
		{
			name:          "service unavailable",
			status:        http.StatusServiceUnavailable,
			code:          "STORAGE_ERROR",
			clientMessage: "Storage is temporarily unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)

			respondHandlerError(ctx, tt.status, tt.code, tt.clientMessage, "sentinel database query", sentinel)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.status, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, tt.clientMessage) || !strings.Contains(body, tt.code) {
				t.Fatalf("stable client error missing: %s", body)
			}
			for _, sensitive := range []string{"sentinel-db-error", "SELECT secret", "postgres://", "password@db.internal"} {
				if strings.Contains(body, sensitive) {
					t.Fatalf("response leaked database diagnostic %q: %s", sensitive, body)
				}
			}
		})
	}
}

func TestSanitizeHandlerErrorForLogRedactsCredentials(t *testing.T) {
	err := errors.New("postgres://admin:password@db.internal/newapi password='second secret' root:hunter2@tcp(mysql:3306) Authorization: Bearer abc.def.ghi")
	got := sanitizeHandlerErrorForLog(err)

	for _, secret := range []string{"password@", "second secret", "hunter2", "abc.def.ghi"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized log message leaked %q: %s", secret, got)
		}
	}
	for _, marker := range []string{"admin:[REDACTED]@", "password=[REDACTED]", "root:[REDACTED]@tcp(", "Bearer [REDACTED]"} {
		if !strings.Contains(got, marker) {
			t.Fatalf("sanitized log message missing %q: %s", marker, got)
		}
	}
}
