package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/controlplane"
)

func TestMutationRoutesFailClosedWithoutAuditedHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		register func(*gin.RouterGroup)
	}{
		{
			name: "redemptions",
			register: func(group *gin.RouterGroup) {
				RegisterRedemptionRoutes(group, nil)
			},
		},
		{
			name: "users",
			register: func(group *gin.RouterGroup) {
				RegisterUserManagementRoutes(group, nil)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deferred := false
			func() {
				defer func() {
					deferred = recover() != nil
				}()
				test.register(gin.New().Group("/api"))
			}()
			if !deferred {
				t.Fatal("unsafe route registration did not fail closed")
			}
		})
	}
}

func TestOperationMetaNeverInventsAnAdminActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/control-plane/users/1/disable", nil)
		return ctx
	}

	ctx := newContext()
	meta := operationMeta(ctx, "reviewed change")
	if meta.Actor != "" || meta.AuthMethod != "" {
		t.Fatalf("anonymous metadata invented identity: %#v", meta)
	}

	ctx = newContext()
	ctx.Set("auth_method", "forged")
	ctx.Set("user_sub", "admin")
	meta = operationMeta(ctx, "reviewed change")
	if meta.Actor != "" || meta.AuthMethod != "" {
		t.Fatalf("unknown auth method was trusted: %#v", meta)
	}

	ctx = newContext()
	ctx.Set("auth_method", "jwt")
	meta = operationMeta(ctx, "reviewed change")
	if meta.Actor != "" || meta.AuthMethod != "jwt" {
		t.Fatalf("JWT without a subject should remain incomplete: %#v", meta)
	}

	ctx = newContext()
	ctx.Set("auth_method", "api_key")
	auth.SetRole(ctx, auth.RoleOperator)
	meta = operationMeta(ctx, "reviewed change")
	if meta.Actor != "api-key-operator" || meta.ActorRole != "operator" || meta.AuthMethod != "api_key" {
		t.Fatalf("verified API key metadata = %#v", meta)
	}
}

func TestRedemptionBatchLimitMapsToUnprocessableEntity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	respondControlPlaneMutationError(ctx, controlplane.ErrRedemptionBatchTooLarge, controlplane.RedemptionCreateResult{})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "REDEMPTION_BATCH_TOO_LARGE") || strings.Contains(body, "key") {
		t.Fatalf("unsafe or unclear batch-limit response: %s", body)
	}
}
