package service

import (
	"fmt"

	"github.com/new-api-tools/backend/internal/config"
)

var getMutationSafetyConfig = config.GetOptional

// ensureNewAPIDirectMutationSafe blocks direct user/token database writes when
// NewAPI may serve authorization state from Redis. NewAPI hashes token cache
// keys with its private CRYPTO_SECRET, so this sidecar cannot safely invalidate
// those caches without an upstream API/cache integration. Missing configuration
// is unsafe because the Redis state cannot be established, so the guard fails
// closed until callers provide an explicit configuration.
func ensureNewAPIDirectMutationSafe() error {
	return validateNewAPIDirectMutationSafety(getMutationSafetyConfig())
}

func validateNewAPIDirectMutationSafety(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("direct user/token mutation blocked: mutation safety configuration is unavailable")
	}
	if !cfg.NewAPIRedisDisabled {
		return fmt.Errorf("direct user/token mutation blocked: NewAPI Redis is enabled or unknown; use the NewAPI admin API, or set NEWAPI_REDIS_DISABLED=true only when NewAPI has no REDIS_CONN_STRING")
	}
	return nil
}

// ensureUnsafeBatchDeleteAllowed protects the legacy inactivity-based direct
// database deletion path. This sidecar cannot atomically coordinate with
// NewAPI's in-flight requests, batched request counters, consume-log settings,
// or log-retention policy, so the path stays disabled unless an operator has
// stopped and drained NewAPI traffic and explicitly accepts that compatibility
// risk. The supported NewAPI admin API remains the preferred deletion path.
func ensureUnsafeBatchDeleteAllowed() error {
	return validateUnsafeBatchDeleteSafety(getMutationSafetyConfig())
}

func validateUnsafeBatchDeleteSafety(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("direct inactive-user batch delete blocked: mutation safety configuration is unavailable")
	}
	if !cfg.AllowUnsafeBatchDelete {
		return fmt.Errorf("direct inactive-user batch delete blocked: this sidecar cannot prove that NewAPI traffic is drained or that consume logs are complete; use the NewAPI admin API, or stop and drain NewAPI traffic and explicitly set ALLOW_UNSAFE_BATCH_DELETE=true only after accepting that risk")
	}
	if err := validateNewAPIDirectMutationSafety(cfg); err != nil {
		return fmt.Errorf("direct inactive-user batch delete blocked: %w", err)
	}
	return nil
}

// ensureUnsafeHardDeleteAllowed protects the legacy direct-DB hard-delete
// path. NewAPI's supported deletion flow removes additional authentication
// records that this sidecar cannot safely enumerate across upstream versions.
func ensureUnsafeHardDeleteAllowed() error {
	return validateUnsafeHardDeleteSafety(getMutationSafetyConfig())
}

func validateUnsafeHardDeleteSafety(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("direct hard delete blocked: mutation safety configuration is unavailable")
	}
	if !cfg.AllowUnsafeHardDelete {
		return fmt.Errorf("direct hard delete blocked: this legacy database path may leave NewAPI authentication records behind; use the NewAPI admin API, or explicitly set ALLOW_UNSAFE_HARD_DELETE=true only after accepting that risk")
	}
	if err := validateNewAPIDirectMutationSafety(cfg); err != nil {
		return fmt.Errorf("direct hard delete blocked: %w", err)
	}
	return nil
}
