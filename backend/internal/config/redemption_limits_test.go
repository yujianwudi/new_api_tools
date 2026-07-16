package config

import (
	"math"
	"strconv"
	"testing"
)

func TestRedemptionFinancialLimitsAreConfigurableAndFailSafe(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "test-secret")

	t.Setenv("REDEMPTION_MAX_QUOTA_PER_CODE", "123456")
	t.Setenv("REDEMPTION_MAX_TOTAL_QUOTA", "654321")
	loaded := Load()
	if loaded.RedemptionMaxQuotaPerCode != 123456 || loaded.RedemptionMaxTotalQuota != 654321 {
		t.Fatalf("configured redemption limits = %d/%d", loaded.RedemptionMaxQuotaPerCode, loaded.RedemptionMaxTotalQuota)
	}

	// Invalid or non-positive values must restore finite defaults rather than
	// disabling the guardrail.
	t.Setenv("REDEMPTION_MAX_QUOTA_PER_CODE", "not-an-int")
	t.Setenv("REDEMPTION_MAX_TOTAL_QUOTA", "0")
	loaded = Load()
	if loaded.RedemptionMaxQuotaPerCode != defaultRedemptionMaxQuotaPerCode || loaded.RedemptionMaxTotalQuota != defaultRedemptionMaxTotalQuota {
		t.Fatalf("invalid redemption limits did not fail safe: %d/%d", loaded.RedemptionMaxQuotaPerCode, loaded.RedemptionMaxTotalQuota)
	}

	t.Setenv("REDEMPTION_MAX_QUOTA_PER_CODE", "-1")
	t.Setenv("REDEMPTION_MAX_TOTAL_QUOTA", "9223372036854775808")
	loaded = Load()
	if loaded.RedemptionMaxQuotaPerCode != defaultRedemptionMaxQuotaPerCode || loaded.RedemptionMaxTotalQuota != defaultRedemptionMaxTotalQuota {
		t.Fatalf("out-of-range redemption limits did not fail safe: %d/%d", loaded.RedemptionMaxQuotaPerCode, loaded.RedemptionMaxTotalQuota)
	}

	// MaxInt64 is a valid explicit limit; the service must still perform its
	// batch multiplication check without overflowing.
	maxInt64 := strconv.FormatInt(math.MaxInt64, 10)
	t.Setenv("REDEMPTION_MAX_QUOTA_PER_CODE", maxInt64)
	t.Setenv("REDEMPTION_MAX_TOTAL_QUOTA", maxInt64)
	loaded = Load()
	if loaded.RedemptionMaxQuotaPerCode != math.MaxInt64 || loaded.RedemptionMaxTotalQuota != math.MaxInt64 {
		t.Fatalf("MaxInt64 redemption limits were not preserved: %d/%d", loaded.RedemptionMaxQuotaPerCode, loaded.RedemptionMaxTotalQuota)
	}
}
