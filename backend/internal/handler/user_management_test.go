package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/database"
)

func TestDeleteUserRequiresConfirmTextBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "user_id", Value: "1"}}
	c.Request = httptest.NewRequest(
		http.MethodDelete,
		"/api/users/1?hard_delete=true",
		strings.NewReader(`{"confirm_text":"错误"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	DeleteUser(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CONFIRM_TEXT_REQUIRED") {
		t.Fatalf("expected confirmation error, got %s", w.Body.String())
	}
}

func TestBatchDeleteRequiresConfirmTextBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/batch-delete",
		strings.NewReader(`{"activity_level":"never","dry_run":false,"hard_delete":false}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	BatchDeleteInactiveUsers(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CONFIRM_TEXT_REQUIRED") {
		t.Fatalf("expected confirmation error, got %s", w.Body.String())
	}
}

func TestPurgeSoftDeletedRequiresConfirmTextBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/soft-deleted/purge",
		strings.NewReader(`{"dry_run":false}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	PurgeSoftDeletedUsers(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CONFIRM_TEXT_REQUIRED") {
		t.Fatalf("expected confirmation error, got %s", w.Body.String())
	}
}

func TestPurgeSoftDeletedRequiresPreviewSnapshotBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/soft-deleted/purge",
		strings.NewReader(`{"dry_run":false,"confirm_text":"彻底删除"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	PurgeSoftDeletedUsers(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SNAPSHOT_REQUIRED") {
		t.Fatalf("expected snapshot error, got %s", w.Body.String())
	}
}

func TestUnbanUserRejectsLegacyBulkTokenReactivation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database.SetForTesting(&database.Manager{})
	t.Cleanup(func() { database.SetForTesting(nil) })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "user_id", Value: "1"}}
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/1/unban",
		strings.NewReader(`{"enable_tokens":true}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	UnbanUser(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "TOKEN_REACTIVATION_DISABLED") || !strings.Contains(w.Body.String(), "逐个复核") {
		t.Fatalf("expected explicit token-reactivation rejection, got %s", w.Body.String())
	}
}

func TestUnbanUserRejectsMalformedJSONBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "user_id", Value: "1"}}
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/1/unban",
		strings.NewReader(`{"reason":`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	UnbanUser(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_PARAMS") {
		t.Fatalf("expected invalid-body error, got %s", w.Body.String())
	}
}

func TestBindOptionalUnbanUserRequestAllowsEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/users/1/unban", nil)

	req, err := bindOptionalUnbanUserRequest(c)
	if err != nil {
		t.Fatalf("empty body should be accepted, got %v", err)
	}
	if req.EnableTokens || req.Reason != "" {
		t.Fatalf("empty body should use zero-value request, got %+v", req)
	}
}
