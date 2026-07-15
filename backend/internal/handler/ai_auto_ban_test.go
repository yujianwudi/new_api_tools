package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSaveAIBanConfigHidesJSONBindingDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/ai-ban/config", strings.NewReader("{"))
	ctx.Request.Header.Set("Content-Type", "application/json")

	SaveAIBanConfig(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "Invalid request body") {
		t.Fatalf("stable validation message missing: %s", body)
	}
	if strings.Contains(body, "unexpected EOF") || strings.Contains(body, "details") {
		t.Fatalf("binding diagnostics leaked to client: %s", body)
	}
}
