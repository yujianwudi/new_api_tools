package util

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
)

// TokensPerUSD is the conversion rate (1 USD = 500,000 tokens)
const TokensPerUSD = 500000

func amountToQuota(amount float64) (int64, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, fmt.Errorf("amount must be finite")
	}
	if amount < 0 {
		return 0, fmt.Errorf("amount must be non-negative")
	}

	scaled := amount * TokensPerUSD
	// float64 cannot represent math.MaxInt64 exactly: converting that integer
	// rounds to 2^63, which is already outside int64. Reject the boundary too
	// so the float-to-int conversion can never wrap to math.MinInt64.
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("amount exceeds supported quota range")
	}
	return int64(math.Round(scaled)), nil
}

// CalculateFixedQuota converts a USD amount to tokens
func CalculateFixedQuota(amount float64) (int64, error) {
	return amountToQuota(amount)
}

// CalculateRandomQuota generates a random token amount within the specified USD range
func CalculateRandomQuota(minAmount, maxAmount float64) (int64, error) {
	if minAmount > maxAmount {
		return 0, fmt.Errorf("min_amount must not exceed max_amount")
	}

	minQuota, err := amountToQuota(minAmount)
	if err != nil {
		return 0, fmt.Errorf("invalid min_amount: %w", err)
	}
	maxQuota, err := amountToQuota(maxAmount)
	if err != nil {
		return 0, fmt.Errorf("invalid max_amount: %w", err)
	}

	if minQuota == maxQuota {
		return minQuota, nil
	}

	// Use arbitrary-precision arithmetic so an inclusive range ending at
	// math.MaxInt64 cannot overflow before the random draw.
	span := new(big.Int).Sub(big.NewInt(maxQuota), big.NewInt(minQuota))
	span.Add(span, big.NewInt(1))
	offset, err := rand.Int(rand.Reader, span)
	if err != nil {
		return 0, fmt.Errorf("generate random quota: %w", err)
	}
	return minQuota + offset.Int64(), nil
}

// GenerateQuotas generates a list of quotas based on mode
func GenerateQuotas(count int, mode string, fixedAmount, minAmount, maxAmount float64) ([]int64, error) {
	if count < 1 {
		return nil, fmt.Errorf("count must be at least 1")
	}

	quotas := make([]int64, count)

	switch mode {
	case "fixed":
		quota, err := CalculateFixedQuota(fixedAmount)
		if err != nil {
			return nil, err
		}
		for i := range quotas {
			quotas[i] = quota
		}
	case "random":
		for i := range quotas {
			q, err := CalculateRandomQuota(minAmount, maxAmount)
			if err != nil {
				return nil, err
			}
			quotas[i] = q
		}
	default:
		return nil, fmt.Errorf("unknown quota mode: %s", mode)
	}

	return quotas, nil
}
