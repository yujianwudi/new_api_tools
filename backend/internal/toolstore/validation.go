package toolstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var (
	decimalPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.[0-9]+)?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

func requireText(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalid, field)
	}
	return value, nil
}

func optionalKey(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func normalizedJSON(field string, value json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(value)) == 0 {
		return nil, nil
	}
	if !json.Valid(value) {
		return nil, fmt.Errorf("%w: %s must be valid JSON", ErrInvalid, field)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return nil, fmt.Errorf("%w: compact %s: %v", ErrInvalid, field, err)
	}
	return compact.Bytes(), nil
}

func validOperationStatus(value OperationStatus) bool {
	switch value {
	case OperationSucceeded, OperationFailed, OperationDenied, OperationCancelled:
		return true
	default:
		return false
	}
}

func validSeverity(value RiskSeverity) bool {
	switch value {
	case RiskSeverityLow, RiskSeverityMedium, RiskSeverityHigh, RiskSeverityCritical:
		return true
	default:
		return false
	}
}

func validRiskStatus(value RiskCaseStatus) bool {
	switch value {
	case RiskCaseOpen, RiskCaseInvestigating, RiskCaseMitigated, RiskCaseClosed:
		return true
	default:
		return false
	}
}

func validVisibility(value NoteVisibility) bool {
	return value == NoteInternal || value == NoteCustomer
}

func validReconciliationStatus(value ReconciliationStatus) bool {
	switch value {
	case ReconciliationRunning, ReconciliationSucceeded, ReconciliationFailed, ReconciliationCancelled:
		return true
	default:
		return false
	}
}

func validateCurrency(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !currencyPattern.MatchString(value) {
		return "", fmt.Errorf("%w: currency must be a three-letter code", ErrInvalid)
	}
	return value, nil
}

func validateExactAmount(decimal string, minor int64, scale int) error {
	decimal = strings.TrimSpace(decimal)
	if !decimalPattern.MatchString(decimal) {
		return fmt.Errorf("%w: amount_decimal must be a non-negative plain decimal", ErrInvalid)
	}
	if minor < 0 {
		return fmt.Errorf("%w: amount_minor cannot be negative", ErrInvalid)
	}
	if scale < 0 || scale > 18 {
		return fmt.Errorf("%w: minor_unit_scale must be between 0 and 18", ErrInvalid)
	}

	parts := strings.SplitN(decimal, ".", 2)
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > scale {
		extra := fraction[scale:]
		if strings.Trim(extra, "0") != "" {
			return fmt.Errorf("%w: amount_decimal has more precision than minor_unit_scale", ErrInvalid)
		}
		fraction = fraction[:scale]
	}
	fraction += strings.Repeat("0", scale-len(fraction))
	digits := strings.TrimLeft(parts[0]+fraction, "0")
	if digits == "" {
		digits = "0"
	}
	want := new(big.Int)
	if _, ok := want.SetString(digits, 10); !ok || !want.IsInt64() {
		return fmt.Errorf("%w: amount_decimal exceeds int64 minor units", ErrInvalid)
	}
	if want.Int64() != minor {
		return fmt.Errorf("%w: amount_decimal does not match amount_minor at scale %d", ErrInvalid, scale)
	}
	return nil
}

func validateWindow(start, end time.Time) error {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return fmt.Errorf("%w: reconciliation window end must be after start", ErrInvalid)
	}
	return nil
}

func normalizeOccurred(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value.UTC().Truncate(time.Millisecond)
}

func normalizeOptionalTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC().Truncate(time.Millisecond)
	return &normalized
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UnixMilli() == right.UnixMilli()
}

func pageResult[T any](items []T, limit int, idOf func(T) int64) ([]T, int64, bool) {
	if len(items) <= limit {
		return items, 0, false
	}
	items = items[:limit]
	return items, idOf(items[len(items)-1]), true
}
