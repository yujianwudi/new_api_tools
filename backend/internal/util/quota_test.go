package util

import (
	"math"
	"testing"
)

func TestCalculateFixedQuota(t *testing.T) {
	quota, err := CalculateFixedQuota(1.5)
	if err != nil {
		t.Fatalf("CalculateFixedQuota returned error: %v", err)
	}
	if quota != 750000 {
		t.Fatalf("quota = %d, want 750000", quota)
	}
}

func TestCalculateFixedQuotaRejectsNonFiniteAndOverflowingValues(t *testing.T) {
	int64OverflowBoundary := math.Ldexp(1, 63) / TokensPerUSD
	for _, amount := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, int64OverflowBoundary} {
		if _, err := CalculateFixedQuota(amount); err == nil {
			t.Fatalf("CalculateFixedQuota(%v) unexpectedly succeeded", amount)
		}
	}
}

func TestCalculateRandomQuotaStaysWithinInclusiveRange(t *testing.T) {
	const minAmount = 0.01
	const maxAmount = 0.02
	for i := 0; i < 100; i++ {
		quota, err := CalculateRandomQuota(minAmount, maxAmount)
		if err != nil {
			t.Fatalf("CalculateRandomQuota returned error: %v", err)
		}
		if quota < 5000 || quota > 10000 {
			t.Fatalf("quota = %d, want value in [5000, 10000]", quota)
		}
	}
}

func TestCalculateRandomQuotaRejectsInvalidBounds(t *testing.T) {
	if _, err := CalculateRandomQuota(2, 1); err == nil {
		t.Fatal("expected inverted bounds to fail")
	}
	if _, err := CalculateRandomQuota(math.NaN(), 1); err == nil {
		t.Fatal("expected NaN minimum to fail")
	}
}
