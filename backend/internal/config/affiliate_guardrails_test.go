package config

import (
	"strings"
	"testing"
	"time"
)

func TestAffiliateStatsGuardrailDefaultsAndBoundaries(t *testing.T) {
	for _, key := range []string{
		"AFFILIATE_EVIDENCE_ROW_CAP",
		"AFFILIATE_QUERY_TIMEOUT_SECONDS",
		"AFFILIATE_QUERY_MAX_CONCURRENCY",
	} {
		t.Setenv(key, "")
	}
	loaded := Load()
	if err := loaded.ValidateAffiliateStatsGuardrails(); err != nil {
		t.Fatalf("default affiliate guardrails rejected: %v", err)
	}
	if loaded.AffiliateEvidenceRowCap != 20_000 ||
		loaded.AffiliateQueryTimeout != 5*time.Second ||
		loaded.AffiliateQueryMaxConcurrency != 2 {
		t.Fatalf(
			"affiliate guardrail defaults = %d/%s/%d, want 20000/5s/2",
			loaded.AffiliateEvidenceRowCap,
			loaded.AffiliateQueryTimeout,
			loaded.AffiliateQueryMaxConcurrency,
		)
	}

	for _, values := range []struct {
		name        string
		rowCap      int64
		timeout     time.Duration
		concurrency int
	}{
		{name: "minimum", rowCap: 1, timeout: time.Second, concurrency: 1},
		{name: "maximum", rowCap: 100_000, timeout: time.Minute, concurrency: 16},
	} {
		t.Run(values.name, func(t *testing.T) {
			candidate := &Config{
				AffiliateEvidenceRowCap:      values.rowCap,
				AffiliateQueryTimeout:        values.timeout,
				AffiliateQueryMaxConcurrency: values.concurrency,
			}
			if err := candidate.ValidateAffiliateStatsGuardrails(); err != nil {
				t.Fatalf("valid boundary rejected: %v", err)
			}
		})
	}
}

func TestAffiliateStatsGuardrailEnvironmentFailsStartupValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantKey string
	}{
		{name: "malformed row cap", key: "AFFILIATE_EVIDENCE_ROW_CAP", value: "many", wantKey: "AFFILIATE_EVIDENCE_ROW_CAP"},
		{name: "row cap below range", key: "AFFILIATE_EVIDENCE_ROW_CAP", value: "0", wantKey: "AFFILIATE_EVIDENCE_ROW_CAP"},
		{name: "row cap above range", key: "AFFILIATE_EVIDENCE_ROW_CAP", value: "100001", wantKey: "AFFILIATE_EVIDENCE_ROW_CAP"},
		{name: "malformed timeout", key: "AFFILIATE_QUERY_TIMEOUT_SECONDS", value: "soon", wantKey: "AFFILIATE_QUERY_TIMEOUT_SECONDS"},
		{name: "timeout above range", key: "AFFILIATE_QUERY_TIMEOUT_SECONDS", value: "61", wantKey: "AFFILIATE_QUERY_TIMEOUT_SECONDS"},
		{name: "malformed concurrency", key: "AFFILIATE_QUERY_MAX_CONCURRENCY", value: "several", wantKey: "AFFILIATE_QUERY_MAX_CONCURRENCY"},
		{name: "concurrency above range", key: "AFFILIATE_QUERY_MAX_CONCURRENCY", value: "17", wantKey: "AFFILIATE_QUERY_MAX_CONCURRENCY"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, key := range []string{
				"AFFILIATE_EVIDENCE_ROW_CAP",
				"AFFILIATE_QUERY_TIMEOUT_SECONDS",
				"AFFILIATE_QUERY_MAX_CONCURRENCY",
			} {
				t.Setenv(key, "")
			}
			t.Setenv(test.key, test.value)
			loaded := Load()
			err := loaded.ValidateAffiliateStatsGuardrails()
			if err == nil || !strings.Contains(err.Error(), test.wantKey) {
				t.Fatalf("validation error = %v, want key %s", err, test.wantKey)
			}
		})
	}
}
