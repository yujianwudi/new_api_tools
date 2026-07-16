package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/controlplane"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
)

type redemptionRouteUpstream struct {
	mu          sync.Mutex
	createCalls int
}

func (u *redemptionRouteUpstream) Status(context.Context) (*newapi.Status, error) {
	return &newapi.Status{Version: "v1.0.0-rc.21"}, nil
}

func (u *redemptionRouteUpstream) ManageUser(context.Context, newapi.ManageUserRequest) error {
	return nil
}

func (u *redemptionRouteUpstream) HardDeleteUser(context.Context, int, newapi.Capabilities) error {
	return nil
}

func (u *redemptionRouteUpstream) CreateRedemptions(_ context.Context, request newapi.RedemptionCreateRequest) ([]string, error) {
	u.mu.Lock()
	u.createCalls++
	u.mu.Unlock()
	keys := make([]string, request.Count)
	for index := range keys {
		keys[index] = fmt.Sprintf("secret-key-%d", index)
	}
	return keys, nil
}

func (u *redemptionRouteUpstream) DeleteRedemption(context.Context, int) error {
	return nil
}

func TestRedemptionCreationRoutesRequireAdminForAPIKeyAndJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	upstream := &redemptionRouteUpstream{}
	mutationService := controlplane.NewService(
		upstream,
		store,
		nil,
		observability.NewRegistry(),
		controlplane.RedemptionLimits{MaxQuotaPerCode: 1_000_000, MaxTotalQuota: 2_000_000},
	)
	mutationHandler := NewMutationHandler(mutationService)

	tests := []struct {
		name       string
		path       string
		authMethod string
		role       auth.Role
		wantStatus int
	}{
		{name: "operator API key control plane", path: "/api/control-plane/redemptions", authMethod: "api_key", role: auth.RoleOperator, wantStatus: http.StatusForbidden},
		{name: "operator JWT control plane", path: "/api/control-plane/redemptions", authMethod: "jwt", role: auth.RoleOperator, wantStatus: http.StatusForbidden},
		{name: "operator API key compatibility path", path: "/api/redemptions/generate", authMethod: "api_key", role: auth.RoleOperator, wantStatus: http.StatusForbidden},
		{name: "operator JWT compatibility path", path: "/api/redemptions/generate", authMethod: "jwt", role: auth.RoleOperator, wantStatus: http.StatusForbidden},
		{name: "admin JWT", path: "/api/control-plane/redemptions", authMethod: "jwt", role: auth.RoleAdmin, wantStatus: http.StatusOK},
		{name: "admin API key", path: "/api/control-plane/redemptions", authMethod: "api_key", role: auth.RoleAdmin, wantStatus: http.StatusOK},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.Use(middleware.RequestIDMiddleware())
			api := router.Group("/api")
			api.Use(func(c *gin.Context) {
				c.Set("auth_method", test.authMethod)
				auth.SetRole(c, test.role)
				if test.authMethod == "jwt" {
					c.Set("user_sub", "test-user")
				}
				c.Next()
			}, auth.RBACMiddleware())
			mutationHandler.RegisterControlPlaneMutationRoutes(api)
			RegisterRedemptionRoutes(api, mutationHandler)

			body := `{"name":"finance-approved","count":1,"quota_mode":"fixed","fixed_amount":1,"reason":"approved finance issuance"}`
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", fmt.Sprintf("redemption-role-%d", index))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantStatus == http.StatusForbidden {
				var response models.ErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode role error: %v", err)
				}
				if response.Error.Code != "INSUFFICIENT_ROLE" || response.Error.Message != "admin role is required for this action" {
					t.Fatalf("unstable role error: %+v", response)
				}
			}
		})
	}

	upstream.mu.Lock()
	calls := upstream.createCalls
	upstream.mu.Unlock()
	if calls != 2 {
		t.Fatalf("operator requests reached upstream; create calls = %d, want 2 admin calls", calls)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "redemption_batch"})
	if listErr != nil || len(page.Items) != 4 {
		t.Fatalf("role-gated audit trail = %+v err=%v; want only two admin intent/outcome pairs", page.Items, listErr)
	}
}

func TestRedemptionFinancialLimitErrorsHaveStableResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		err     error
		code    string
		message string
	}{
		{
			err:     controlplane.ErrRedemptionQuotaPerCodeTooLarge,
			code:    "REDEMPTION_QUOTA_PER_CODE_TOO_LARGE",
			message: "Redemption quota per code exceeds the configured safety limit",
		},
		{
			err:     controlplane.ErrRedemptionTotalQuotaTooLarge,
			code:    "REDEMPTION_TOTAL_QUOTA_TOO_LARGE",
			message: "Redemption batch total quota exceeds the configured safety limit",
		},
	}

	for _, test := range tests {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		respondControlPlaneMutationError(ctx, test.err, controlplane.RedemptionCreateResult{})
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d; body=%s", test.code, recorder.Code, recorder.Body.String())
		}
		var response models.ErrorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode %s response: %v", test.code, err)
		}
		if response.Error.Code != test.code || response.Error.Message != test.message {
			t.Fatalf("unstable %s response: %+v", test.code, response)
		}
	}
}
