package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/service"
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

func TestPurgeSoftDeletedRejectsMalformedJSONBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/soft-deleted/purge",
		strings.NewReader(`{"dry_run":`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	PurgeSoftDeletedUsers(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_PARAMS") {
		t.Fatalf("expected invalid-body error, got %s", w.Body.String())
	}
}

func TestBindOptionalPurgeSoftDeletedRequestAllowsEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/users/soft-deleted/purge", nil)

	req, err := bindOptionalPurgeSoftDeletedRequest(c)
	if err != nil {
		t.Fatalf("empty body should be accepted, got %v", err)
	}
	if !req.DryRun || req.ConfirmText != "" || req.SnapshotID != "" {
		t.Fatalf("empty body should preserve dry-run defaults, got %+v", req)
	}
}

func TestPurgeSoftDeletedMapsInvalidSnapshotToBadRequest(t *testing.T) {
	status, code, _, ok := classifyDestructiveOperationError(service.ErrInvalidOrExpiredSnapshot)
	if !ok || status != http.StatusBadRequest || code != "SNAPSHOT_INVALID" {
		t.Fatalf("unexpected invalid snapshot mapping: status=%d code=%q ok=%t", status, code, ok)
	}
	status, code, _, ok = classifyDestructiveOperationError(service.ErrSnapshotInvalidated)
	if !ok || status != http.StatusConflict || code != "SNAPSHOT_INVALIDATED" {
		t.Fatalf("unexpected invalidated snapshot mapping: status=%d code=%q ok=%t", status, code, ok)
	}
	status, code, _, ok = classifyDestructiveOperationError(service.ErrDestructiveSnapshotStoreUnavailable)
	if !ok || status != http.StatusServiceUnavailable || code != "SNAPSHOT_STORE_UNAVAILABLE" {
		t.Fatalf("unexpected unavailable snapshot-store mapping: status=%d code=%q ok=%t", status, code, ok)
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

func TestBanUserRejectsMalformedJSONBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "user_id", Value: "1"}}
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/users/1/ban",
		strings.NewReader(`{"disable_tokens":`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	BanUser(c)

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
