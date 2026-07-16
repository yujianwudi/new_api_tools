package controlplane

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func TestCreateRedemptionsRejectsFinancialLimitViolationsBeforeIntent(t *testing.T) {
	tests := []struct {
		name    string
		limits  RedemptionLimits
		request newapi.RedemptionCreateRequest
		wantErr error
	}{
		{
			name:    "quota per code",
			limits:  RedemptionLimits{MaxQuotaPerCode: 100, MaxTotalQuota: 1_000},
			request: newapi.RedemptionCreateRequest{Name: "single-limit", Count: 1, Quota: 101},
			wantErr: ErrRedemptionQuotaPerCodeTooLarge,
		},
		{
			name:    "batch total",
			limits:  RedemptionLimits{MaxQuotaPerCode: 100, MaxTotalQuota: 100},
			request: newapi.RedemptionCreateRequest{Name: "total-limit", Count: 3, Quota: 40},
			wantErr: ErrRedemptionTotalQuotaTooLarge,
		},
		{
			name:    "overflow cannot bypass total",
			limits:  RedemptionLimits{MaxQuotaPerCode: math.MaxInt64, MaxTotalQuota: math.MaxInt64},
			request: newapi.RedemptionCreateRequest{Name: "overflow-limit", Count: 2, Quota: math.MaxInt64},
			wantErr: ErrRedemptionTotalQuotaTooLarge,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
			service, store, _ := setupMutationService(t, upstream)
			service.redemptionLimits = test.limits
			meta := testMeta()
			meta.IdempotencyKey = "financial-limit-" + test.request.Name

			_, err := service.CreateRedemptions(context.Background(), meta, test.request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			upstream.mu.Lock()
			calls := upstream.createCalls
			upstream.mu.Unlock()
			if calls != 0 {
				t.Fatalf("financial limit violation reached upstream %d times", calls)
			}
			page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "redemption_batch"})
			if listErr != nil || len(page.Items) != 0 {
				t.Fatalf("financial validation must precede intent: page=%+v err=%v", page, listErr)
			}
		})
	}
}

func TestCreateRedemptionsAcceptsExactFinancialLimitBoundaries(t *testing.T) {
	upstream := &fakeUpstream{
		version: "v1.0.0-rc.21",
		onCreate: func(newapi.RedemptionCreateRequest) ([]string, error) {
			return []string{"secret-key-a", "secret-key-b"}, nil
		},
	}
	service, store, _ := setupMutationService(t, upstream)
	service.redemptionLimits = RedemptionLimits{MaxQuotaPerCode: 50, MaxTotalQuota: 100}
	meta := testMeta()
	meta.IdempotencyKey = "financial-limit-boundary"

	result, err := service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{
		Name: "boundary", Count: 2, Quota: 50,
	})
	if err != nil {
		t.Fatalf("exact financial boundary rejected: %v", err)
	}
	if result.Count != 2 || result.AuditID == 0 {
		t.Fatalf("boundary result = %+v", result)
	}
	upstream.mu.Lock()
	calls := upstream.createCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("boundary request reached upstream %d times, want 1", calls)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "redemption_batch"})
	if listErr != nil || len(page.Items) != 2 {
		t.Fatalf("boundary audit trail = %+v err=%v", page.Items, listErr)
	}
}

func TestRedemptionLimitsUseFiniteDefaults(t *testing.T) {
	limits := (RedemptionLimits{}).normalized()
	if limits.MaxQuotaPerCode != DefaultRedemptionMaxQuotaPerCode || limits.MaxTotalQuota != DefaultRedemptionMaxTotalQuota {
		t.Fatalf("zero limits did not fail safe: %+v", limits)
	}
}
