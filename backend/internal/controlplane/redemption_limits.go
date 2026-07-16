package controlplane

import "errors"

const (
	// NewAPI stores quota in internal units where 500,000 units currently
	// represent US$1. Defaults therefore cap one code at US$100 and one
	// operation at US$1,000. Deployments may lower or explicitly raise them.
	DefaultRedemptionMaxQuotaPerCode int64 = 50_000_000
	DefaultRedemptionMaxTotalQuota   int64 = 500_000_000
)

var (
	ErrRedemptionQuotaPerCodeTooLarge = errors.New("controlplane: redemption quota per code exceeds the configured safety limit")
	ErrRedemptionTotalQuotaTooLarge   = errors.New("controlplane: redemption batch total quota exceeds the configured safety limit")
)

// RedemptionLimits are server-side financial guardrails. They are enforced
// before an operation intent is persisted or NewAPI is called.
type RedemptionLimits struct {
	MaxQuotaPerCode int64
	MaxTotalQuota   int64
}

func DefaultRedemptionLimits() RedemptionLimits {
	return RedemptionLimits{
		MaxQuotaPerCode: DefaultRedemptionMaxQuotaPerCode,
		MaxTotalQuota:   DefaultRedemptionMaxTotalQuota,
	}
}

func (limits RedemptionLimits) normalized() RedemptionLimits {
	defaults := DefaultRedemptionLimits()
	if limits.MaxQuotaPerCode <= 0 {
		limits.MaxQuotaPerCode = defaults.MaxQuotaPerCode
	}
	if limits.MaxTotalQuota <= 0 {
		limits.MaxTotalQuota = defaults.MaxTotalQuota
	}
	return limits
}

func (limits RedemptionLimits) validate(count int, quota int64) error {
	limits = limits.normalized()
	if quota > limits.MaxQuotaPerCode {
		return ErrRedemptionQuotaPerCodeTooLarge
	}
	// Division keeps the check safe even when count*quota would overflow int64.
	// count and quota have already been validated as positive by the caller.
	if quota > limits.MaxTotalQuota/int64(count) {
		return ErrRedemptionTotalQuotaTooLarge
	}
	return nil
}
