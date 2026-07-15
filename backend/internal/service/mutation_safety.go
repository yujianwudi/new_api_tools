package service

import (
	"fmt"

	"github.com/new-api-tools/backend/internal/config"
)

var getMutationSafetyConfig = config.GetOptional

// ensureNewAPIDirectMutationSafe blocks direct user/token database writes when
// NewAPI may serve authorization state from Redis. NewAPI hashes token cache
// keys with its private CRYPTO_SECRET, so this sidecar cannot safely invalidate
// those caches without an upstream API/cache integration. Isolated unit tests
// do not load application config and therefore exercise the database logic.
func ensureNewAPIDirectMutationSafe() error {
	return validateNewAPIDirectMutationSafety(getMutationSafetyConfig())
}

func validateNewAPIDirectMutationSafety(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	if !cfg.NewAPIRedisDisabled {
		return fmt.Errorf("direct user/token mutation blocked: NewAPI Redis is enabled or unknown; use the NewAPI admin API, or set NEWAPI_REDIS_DISABLED=true only when NewAPI has no REDIS_CONN_STRING")
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
		return nil
	}
	if !cfg.AllowUnsafeHardDelete {
		return fmt.Errorf("direct hard delete blocked: this legacy database path may leave NewAPI authentication records behind; use the NewAPI admin API, or explicitly set ALLOW_UNSAFE_HARD_DELETE=true only after accepting that risk")
	}
	if err := validateNewAPIDirectMutationSafety(cfg); err != nil {
		return fmt.Errorf("direct hard delete blocked: %w", err)
	}
	return nil
}
