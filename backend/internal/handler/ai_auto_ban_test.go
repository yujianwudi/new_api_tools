package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSaveAIBanConfigIsReadOnlyBeforeParsingSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/ai-ban/config", strings.NewReader("{"))
	ctx.Request.Header.Set("Content-Type", "application/json")

	SaveAIBanConfig(ctx)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "AUDITED_CONFIGURATION_REQUIRED") {
		t.Fatalf("stable safety response missing: %s", body)
	}
	if strings.Contains(body, "unexpected EOF") || strings.Contains(body, "api_key") {
		t.Fatalf("binding diagnostics leaked to client: %s", body)
	}
}

func TestAIPlaceholderOperationsReturnNotImplemented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		code    string
		handler gin.HandlerFunc
	}{
		{name: "manual assessment", method: http.MethodPost, path: "/api/ai-ban/assess", body: `{`, code: "NOT_IMPLEMENTED", handler: ManualAssess},
		{name: "risk scan", method: http.MethodPost, path: "/api/ai-ban/scan?window=1h", code: "NOT_IMPLEMENTED", handler: RunAIBanScan},
		{name: "connection test", method: http.MethodPost, path: "/api/ai-ban/test-connection", code: "NOT_IMPLEMENTED", handler: TestAIConnection},
		{name: "model discovery", method: http.MethodPost, path: "/api/ai-ban/models", body: `{"api_key":"secret"}`, code: "AUDITED_EXTERNAL_CALL_REQUIRED", handler: FetchAIModels},
		{name: "billable model test", method: http.MethodPost, path: "/api/ai-ban/test-model", body: `{"api_key":"secret"}`, code: "AUDITED_EXTERNAL_CALL_REQUIRED", handler: TestAIModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			tt.handler(ctx)
			if recorder.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"code":"`+tt.code+`"`) || strings.Contains(recorder.Body.String(), `"success":true`) {
				t.Fatalf("placeholder operation returned an ambiguous response: %s", recorder.Body.String())
			}
		})
	}
}
