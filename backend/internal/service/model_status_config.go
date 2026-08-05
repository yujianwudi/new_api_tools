package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	MaxModelStatusConfigBytes           = 64 << 10
	MaxModelStatusSelectedModels        = 200
	MaxModelStatusCustomOrderModels     = 200
	MaxModelStatusCustomGroups          = 32
	MaxModelsPerCustomGroup             = 100
	MaxModelsAcrossCustomGroups         = 1000
	MaxCustomGroupNameBytes             = 64
	MaxCustomGroupIDBytes               = 64
	MaxCustomGroupIconBytes             = 32
	MaxModelStatusSiteTitleBytes        = 128
	legacyModelStatusConfigOperationKey = "model-config:legacy-redis-v0.6.0"
)

var legacyModelStatusConfigKeys = []string{
	"model_status:selected_models",
	"model_status:time_window",
	"model_status:theme",
	"model_status:refresh_interval",
	"model_status:sort_mode",
	"model_status:custom_order",
	"model_status:custom_groups",
	"model_status:site_title",
}

var (
	ErrModelStatusConfigInvalid     = errors.New("model status config: invalid")
	ErrModelStatusConfigConflict    = errors.New("model status config: conflict")
	ErrModelStatusConfigUnavailable = errors.New("model status config: unavailable")
)

type ModelStatusCustomGroup struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Icon   string   `json:"icon,omitempty"`
	Models []string `json:"models"`
}

type ModelStatusConfig struct {
	SelectedModels  []string                 `json:"selected_models"`
	TimeWindow      string                   `json:"time_window"`
	Theme           string                   `json:"theme"`
	RefreshInterval int                      `json:"refresh_interval"`
	SortMode        string                   `json:"sort_mode"`
	CustomOrder     []string                 `json:"custom_order"`
	CustomGroups    []ModelStatusCustomGroup `json:"custom_groups"`
	SiteTitle       string                   `json:"site_title"`
}

type ModelStatusConfigSnapshot struct {
	Version int64             `json:"version"`
	Config  ModelStatusConfig `json:"config"`
}

type modelStatusConfigPublisher interface {
	Preflight(context.Context) error
	Publish(context.Context, int64, json.RawMessage) error
}

// LegacyModelStatusConfigReader must return one authoritative snapshot of all
// requested Redis keys. Missing keys are omitted from the result; read errors
// must be returned rather than represented as an empty snapshot.
type LegacyModelStatusConfigReader interface {
	GetRawJSONGroup(context.Context, []string) (map[string]json.RawMessage, error)
}

type modelStatusConfigCache struct {
	sync.RWMutex
	snapshot ModelStatusConfigSnapshot
	loaded   bool
}

type modelStatusConfigRuntime struct {
	store     *toolstore.Store
	publisher modelStatusConfigPublisher
	cache     *modelStatusConfigCache
}

var modelStatusConfigRuntimeState = struct {
	sync.RWMutex
	runtime *modelStatusConfigRuntime
}{runtime: &modelStatusConfigRuntime{cache: &modelStatusConfigCache{}}}

// ConfigureModelStatusConfigRuntime installs the durable configuration
// dependencies used by subsequently created ModelStatusService instances. The
// returned restore function is intended for tests.
func ConfigureModelStatusConfigRuntime(store *toolstore.Store, manager *cache.Manager, redisRequired bool) func() {
	runtime := &modelStatusConfigRuntime{
		store: store, cache: &modelStatusConfigCache{},
		publisher: &redisModelStatusConfigPublisher{manager: manager, required: redisRequired},
	}
	modelStatusConfigRuntimeState.Lock()
	previous := modelStatusConfigRuntimeState.runtime
	modelStatusConfigRuntimeState.runtime = runtime
	modelStatusConfigRuntimeState.Unlock()
	return func() {
		modelStatusConfigRuntimeState.Lock()
		if modelStatusConfigRuntimeState.runtime == runtime {
			modelStatusConfigRuntimeState.runtime = previous
		}
		modelStatusConfigRuntimeState.Unlock()
	}
}

func currentModelStatusConfigRuntime() *modelStatusConfigRuntime {
	modelStatusConfigRuntimeState.RLock()
	runtime := modelStatusConfigRuntimeState.runtime
	modelStatusConfigRuntimeState.RUnlock()
	return runtime
}

// ImportLegacyModelStatusConfig performs the one-time v0.6.0 Redis-to-Tool
// Store migration before the HTTP server starts. Even an entirely missing
// legacy group writes an audited default snapshot, which is the durable marker
// preventing a later stale Redis restore from overwriting newer configuration.
func ImportLegacyModelStatusConfig(
	ctx context.Context,
	store *toolstore.Store,
	reader LegacyModelStatusConfigReader,
	legacyRedisConfigured bool,
) (ModelStatusConfigSnapshot, bool, error) {
	if store == nil {
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: Tool Store is unavailable", ErrModelStatusConfigUnavailable)
	}
	if existing, err := store.GetModelStatusConfigByOperationKey(ctx, legacyModelStatusConfigOperationKey); err == nil {
		snapshot, validateErr := validateLegacyModelStatusConfigImport(ctx, store, existing)
		if validateErr != nil {
			return ModelStatusConfigSnapshot{}, false, validateErr
		}
		return snapshot, false, nil
	} else if !errors.Is(err, toolstore.ErrNotFound) {
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: inspect legacy import marker: %v", ErrModelStatusConfigUnavailable, err)
	}

	current, err := store.GetLatestModelStatusConfig(ctx)
	if err != nil {
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: read pre-import config: %v", ErrModelStatusConfigUnavailable, err)
	}
	if current.Version != 1 || current.ChangedKey != "bootstrap" || current.PreviousVersion != nil {
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf(
			"%w: legacy import marker is missing after durable configuration advanced",
			ErrModelStatusConfigUnavailable,
		)
	}

	rawValues := map[string]json.RawMessage{}
	if legacyRedisConfigured {
		if reader == nil {
			return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: configured legacy Redis is unavailable", ErrModelStatusConfigUnavailable)
		}
		rawValues, err = reader.GetRawJSONGroup(ctx, append([]string(nil), legacyModelStatusConfigKeys...))
		if err != nil {
			return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: read legacy Redis config: %v", ErrModelStatusConfigUnavailable, err)
		}
	}
	configValue, presentKeys, err := decodeLegacyModelStatusConfig(rawValues)
	if err != nil {
		return ModelStatusConfigSnapshot{}, false, err
	}
	configJSON, err := json.Marshal(configValue)
	if err != nil || len(configJSON) > MaxModelStatusConfigBytes {
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: encode legacy model config", ErrModelStatusConfigInvalid)
	}

	reason := "initialize model status config with v0.6.0 defaults; no legacy Redis keys were present"
	if len(presentKeys) > 0 {
		reason = "migrate v0.6.0 Redis model status config keys: " + strings.Join(presentKeys, ",")
	}
	expectedVersion := current.Version
	record, _, replayed, err := store.ImportLegacyModelStatusConfigAudited(ctx, toolstore.ModelStatusConfigWriteInput{
		ConfigJSON: configJSON, ExpectedVersion: &expectedVersion, OperationKey: legacyModelStatusConfigOperationKey,
	}, toolstore.OperationAuditInput{
		RequestID: "startup-model-config-legacy-v060", Actor: "system:model-config-migration",
		SourceIP: "local", AuthMethod: "startup", Reason: reason,
	})
	if err != nil {
		if errors.Is(err, toolstore.ErrConflict) {
			return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: legacy model config import conflicted with another write", ErrModelStatusConfigConflict)
		}
		if errors.Is(err, toolstore.ErrInvalid) {
			return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: legacy model config import was rejected", ErrModelStatusConfigInvalid)
		}
		return ModelStatusConfigSnapshot{}, false, fmt.Errorf("%w: persist legacy model config import", ErrModelStatusConfigUnavailable)
	}
	snapshot, err := validateLegacyModelStatusConfigImport(ctx, store, record)
	if err != nil {
		return ModelStatusConfigSnapshot{}, false, err
	}
	return snapshot, !replayed, nil
}

func decodeLegacyModelStatusConfig(rawValues map[string]json.RawMessage) (ModelStatusConfig, []string, error) {
	configValue := defaultModelStatusConfig()
	present := make([]string, 0, len(legacyModelStatusConfigKeys))
	totalBytes := 0
	for _, key := range legacyModelStatusConfigKeys {
		raw, found := rawValues[key]
		if !found {
			continue
		}
		present = append(present, strings.TrimPrefix(key, "model_status:"))
		totalBytes += len(raw)
		if len(raw) == 0 {
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis key %s is empty", ErrModelStatusConfigInvalid, key)
		}
		if totalBytes > MaxModelStatusConfigBytes {
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis configuration exceeds the size limit", ErrModelStatusConfigInvalid)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis key %s contains null", ErrModelStatusConfigInvalid, key)
		}
		var destination any
		switch key {
		case "model_status:selected_models":
			destination = &configValue.SelectedModels
		case "model_status:time_window":
			destination = &configValue.TimeWindow
		case "model_status:theme":
			destination = &configValue.Theme
		case "model_status:refresh_interval":
			destination = &configValue.RefreshInterval
		case "model_status:sort_mode":
			destination = &configValue.SortMode
		case "model_status:custom_order":
			destination = &configValue.CustomOrder
		case "model_status:custom_groups":
			destination = &configValue.CustomGroups
		case "model_status:site_title":
			destination = &configValue.SiteTitle
		default:
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: unknown legacy model config key", ErrModelStatusConfigInvalid)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(destination); err != nil {
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis key %s is invalid", ErrModelStatusConfigInvalid, key)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis key %s has trailing JSON", ErrModelStatusConfigInvalid, key)
		}
	}
	if err := ValidateModelStatusConfig(&configValue); err != nil {
		return ModelStatusConfig{}, nil, fmt.Errorf("%w: legacy Redis configuration failed validation", ErrModelStatusConfigInvalid)
	}
	return configValue, present, nil
}

func defaultModelStatusConfig() ModelStatusConfig {
	return ModelStatusConfig{
		SelectedModels: []string{}, TimeWindow: DefaultTimeWindow, Theme: DefaultTheme,
		RefreshInterval: 60, SortMode: "default", CustomOrder: []string{},
		CustomGroups: []ModelStatusCustomGroup{}, SiteTitle: "",
	}
}

func validateLegacyModelStatusConfigImport(
	ctx context.Context,
	store *toolstore.Store,
	record toolstore.ModelStatusConfigVersion,
) (ModelStatusConfigSnapshot, error) {
	if record.ChangedKey != "bootstrap" || record.OperationKey != legacyModelStatusConfigOperationKey {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: legacy import marker is inconsistent", ErrModelStatusConfigUnavailable)
	}
	intent, intentErr := store.GetOperationAuditByIdempotencyKey(ctx, legacyModelStatusConfigOperationKey+":intent")
	outcome, outcomeErr := store.GetOperationAuditByIdempotencyKey(ctx, legacyModelStatusConfigOperationKey+":outcome")
	if intentErr != nil || outcomeErr != nil || intent.ID >= outcome.ID ||
		intent.Action != "model_status.config.legacy_import.intent" ||
		outcome.Action != "model_status.config.legacy_import.outcome" ||
		intent.Actor != "system:model-config-migration" || outcome.Actor != intent.Actor ||
		intent.TargetType != "model_status_config" || outcome.TargetType != intent.TargetType ||
		intent.TargetID != "bootstrap" || outcome.TargetID != intent.TargetID ||
		intent.Status != toolstore.OperationSucceeded || outcome.Status != toolstore.OperationSucceeded {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: legacy import audit chain is incomplete", ErrModelStatusConfigUnavailable)
	}
	var audited struct {
		Version int64           `json:"version"`
		Config  json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(outcome.AfterJSON, &audited); err != nil || audited.Version != record.Version ||
		!bytes.Equal(audited.Config, record.ConfigJSON) {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: legacy import audit outcome does not match durable config", ErrModelStatusConfigUnavailable)
	}
	configValue, err := decodeModelStatusConfig(record.ConfigJSON)
	if err != nil {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: imported durable config failed validation", ErrModelStatusConfigUnavailable)
	}
	return ModelStatusConfigSnapshot{Version: record.Version, Config: configValue}, nil
}

func newModelStatusServiceWithConfigRuntime(store *toolstore.Store, publisher modelStatusConfigPublisher) *ModelStatusService {
	return &ModelStatusService{
		db: database.Get(), logDB: database.GetLog(),
		configRuntime: &modelStatusConfigRuntime{store: store, publisher: publisher, cache: &modelStatusConfigCache{}},
	}
}

type redisModelStatusConfigPublisher struct {
	manager  *cache.Manager
	required bool
}

func (p *redisModelStatusConfigPublisher) Preflight(ctx context.Context) error {
	if p == nil || !p.required {
		return nil
	}
	if p.manager == nil || p.manager.RedisClient() == nil {
		return errors.New("configured Redis cache is unavailable")
	}
	if err := p.manager.RedisClient().Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping configured Redis cache: %w", err)
	}
	return nil
}

func (p *redisModelStatusConfigPublisher) Publish(ctx context.Context, version int64, configJSON json.RawMessage) error {
	if p == nil || !p.required {
		return nil
	}
	if p.manager == nil || p.manager.RedisClient() == nil {
		return errors.New("configured Redis cache is unavailable")
	}
	pipe := p.manager.RedisClient().TxPipeline()
	pipe.Set(ctx, "model_status:config:version", strconv.FormatInt(version, 10), 0)
	pipe.Set(ctx, "model_status:config:current", []byte(configJSON), 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("publish model status config version: %w", err)
	}
	return nil
}

func (s *ModelStatusService) GetConfigSnapshot(ctx context.Context) (ModelStatusConfigSnapshot, error) {
	if s == nil || s.configRuntime == nil || s.configRuntime.store == nil {
		return ModelStatusConfigSnapshot{}, ErrModelStatusConfigUnavailable
	}
	record, err := s.configRuntime.store.GetLatestModelStatusConfig(ctx)
	if err != nil {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: read durable config: %v", ErrModelStatusConfigUnavailable, err)
	}
	cacheState := s.configRuntime.cache
	if cacheState != nil {
		cacheState.RLock()
		if cacheState.loaded && cacheState.snapshot.Version == record.Version {
			cached := cloneModelStatusConfigSnapshot(cacheState.snapshot)
			cacheState.RUnlock()
			return cached, nil
		}
		cacheState.RUnlock()
	}
	configValue, err := decodeModelStatusConfig(record.ConfigJSON)
	if err != nil {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: durable config failed semantic validation", ErrModelStatusConfigUnavailable)
	}
	snapshot := ModelStatusConfigSnapshot{Version: record.Version, Config: configValue}
	if cacheState != nil {
		cacheState.Lock()
		if !cacheState.loaded || snapshot.Version >= cacheState.snapshot.Version {
			cacheState.snapshot = cloneModelStatusConfigSnapshot(snapshot)
			cacheState.loaded = true
		}
		cacheState.Unlock()
	}
	return cloneModelStatusConfigSnapshot(snapshot), nil
}

func (s *ModelStatusService) GetConfig() (map[string]interface{}, error) {
	snapshot, err := s.GetConfigSnapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return modelStatusConfigMap(snapshot), nil
}

func (s *ModelStatusService) GetEmbedConfig() (map[string]interface{}, error) {
	configValue, err := s.GetConfig()
	if err != nil {
		return nil, err
	}
	configValue["available_time_windows"] = AvailableTimeWindows
	configValue["available_themes"] = AvailableThemes
	configValue["available_refresh_intervals"] = AvailableRefreshIntervals
	configValue["available_sort_modes"] = AvailableSortModes
	return configValue, nil
}

func (s *ModelStatusService) SetSelectedModels(ctx context.Context, value []string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "selected_models", append([]string{}, value...), audit, expected)
}

func (s *ModelStatusService) SetTimeWindow(ctx context.Context, value string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "time_window", value, audit, expected)
}

func (s *ModelStatusService) SetTheme(ctx context.Context, value string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "theme", value, audit, expected)
}

func (s *ModelStatusService) SetRefreshInterval(ctx context.Context, value int, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "refresh_interval", value, audit, expected)
}

func (s *ModelStatusService) SetSortMode(ctx context.Context, value string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "sort_mode", value, audit, expected)
}

func (s *ModelStatusService) SetCustomOrder(ctx context.Context, value []string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "custom_order", append([]string{}, value...), audit, expected)
}

func (s *ModelStatusService) SetCustomGroups(ctx context.Context, value []ModelStatusCustomGroup, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "custom_groups", cloneCustomGroups(value), audit, expected)
}

func (s *ModelStatusService) SetSiteTitle(ctx context.Context, value string, audit toolstore.OperationAuditInput, expected *int64) (ModelStatusConfigSnapshot, error) {
	return s.updateModelStatusConfig(ctx, "site_title", value, audit, expected)
}

func (s *ModelStatusService) updateModelStatusConfig(
	ctx context.Context,
	key string,
	value any,
	audit toolstore.OperationAuditInput,
	expected *int64,
) (ModelStatusConfigSnapshot, error) {
	current, err := s.GetConfigSnapshot(ctx)
	if err != nil {
		return ModelStatusConfigSnapshot{}, err
	}
	next := cloneModelStatusConfigSnapshot(current)
	switch key {
	case "selected_models":
		next.Config.SelectedModels = append([]string{}, value.([]string)...)
	case "time_window":
		next.Config.TimeWindow = value.(string)
	case "theme":
		next.Config.Theme = value.(string)
	case "refresh_interval":
		next.Config.RefreshInterval = value.(int)
	case "sort_mode":
		next.Config.SortMode = value.(string)
	case "custom_order":
		next.Config.CustomOrder = append([]string{}, value.([]string)...)
	case "custom_groups":
		next.Config.CustomGroups = cloneCustomGroups(value.([]ModelStatusCustomGroup))
	case "site_title":
		next.Config.SiteTitle = value.(string)
	default:
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: unsupported key", ErrModelStatusConfigInvalid)
	}
	if err := ValidateModelStatusConfig(&next.Config); err != nil {
		return ModelStatusConfigSnapshot{}, err
	}
	configJSON, err := json.Marshal(next.Config)
	if err != nil || len(configJSON) > MaxModelStatusConfigBytes {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: config exceeds the durable size limit", ErrModelStatusConfigInvalid)
	}
	if s.configRuntime == nil || s.configRuntime.store == nil {
		return ModelStatusConfigSnapshot{}, ErrModelStatusConfigUnavailable
	}
	if s.configRuntime.publisher != nil {
		if err := s.configRuntime.publisher.Preflight(ctx); err != nil {
			return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: Redis preflight failed", ErrModelStatusConfigUnavailable)
		}
	}
	// Every write is a compare-and-swap, including callers that omit an
	// explicit expected_version. The full configuration snapshot was built from
	// current.Version above; accepting it after another writer advances the
	// durable version would silently revert that writer's unrelated fields.
	writeExpected := expected
	if writeExpected == nil {
		currentVersion := current.Version
		writeExpected = &currentVersion
	}
	operationKey := "model-config:" + strings.TrimSpace(audit.IdempotencyKey)
	if strings.TrimSpace(audit.IdempotencyKey) == "" {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: idempotency identity is required", ErrModelStatusConfigInvalid)
	}
	record, _, _, err := s.configRuntime.store.WriteModelStatusConfigAudited(ctx, toolstore.ModelStatusConfigWriteInput{
		ChangedKey: key, ConfigJSON: configJSON, ExpectedVersion: writeExpected, OperationKey: operationKey,
	}, audit)
	if err != nil {
		switch {
		case errors.Is(err, toolstore.ErrInvalid):
			return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: %v", ErrModelStatusConfigInvalid, err)
		case errors.Is(err, toolstore.ErrConflict):
			return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: %v", ErrModelStatusConfigConflict, err)
		default:
			return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: durable write failed", ErrModelStatusConfigUnavailable)
		}
	}
	writtenConfig, err := decodeModelStatusConfig(record.ConfigJSON)
	if err != nil {
		return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: committed config cannot be decoded", ErrModelStatusConfigUnavailable)
	}
	written := ModelStatusConfigSnapshot{Version: record.Version, Config: writtenConfig}
	if s.configRuntime.publisher != nil {
		if err := s.configRuntime.publisher.Publish(ctx, record.Version, record.ConfigJSON); err != nil {
			return ModelStatusConfigSnapshot{}, fmt.Errorf("%w: durable version %d committed but Redis invalidation publish failed",
				ErrModelStatusConfigUnavailable, record.Version)
		}
	}
	if cacheState := s.configRuntime.cache; cacheState != nil {
		cacheState.Lock()
		if !cacheState.loaded || written.Version >= cacheState.snapshot.Version {
			cacheState.snapshot = cloneModelStatusConfigSnapshot(written)
			cacheState.loaded = true
		}
		cacheState.Unlock()
	}
	return cloneModelStatusConfigSnapshot(written), nil
}

func ValidateModelStatusConfig(configValue *ModelStatusConfig) error {
	if configValue == nil {
		return fmt.Errorf("%w: config is required", ErrModelStatusConfigInvalid)
	}
	if len(configValue.SelectedModels) > MaxModelStatusSelectedModels ||
		validateUniqueModelNames(configValue.SelectedModels, false) != nil {
		return fmt.Errorf("%w: selected_models is invalid or exceeds %d entries", ErrModelStatusConfigInvalid, MaxModelStatusSelectedModels)
	}
	if !modelConfigContainsString(AvailableTimeWindows, configValue.TimeWindow) {
		return fmt.Errorf("%w: unsupported time_window", ErrModelStatusConfigInvalid)
	}
	if mapped, ok := LegacyThemeMap[configValue.Theme]; ok {
		configValue.Theme = mapped
	}
	if !modelConfigContainsString(AvailableThemes, configValue.Theme) {
		return fmt.Errorf("%w: unsupported theme", ErrModelStatusConfigInvalid)
	}
	if !modelConfigContainsInt(AvailableRefreshIntervals, configValue.RefreshInterval) {
		return fmt.Errorf("%w: unsupported refresh_interval", ErrModelStatusConfigInvalid)
	}
	if !modelConfigContainsString(AvailableSortModes, configValue.SortMode) {
		return fmt.Errorf("%w: unsupported sort_mode", ErrModelStatusConfigInvalid)
	}
	if len(configValue.CustomOrder) > MaxModelStatusCustomOrderModels ||
		validateUniqueModelNames(configValue.CustomOrder, false) != nil {
		return fmt.Errorf("%w: custom_order is invalid or exceeds %d entries", ErrModelStatusConfigInvalid, MaxModelStatusCustomOrderModels)
	}
	if err := ValidateModelStatusCustomGroups(configValue.CustomGroups); err != nil {
		return err
	}
	configValue.SiteTitle = strings.TrimSpace(configValue.SiteTitle)
	if len(configValue.SiteTitle) > MaxModelStatusSiteTitleBytes || containsControl(configValue.SiteTitle) {
		return fmt.Errorf("%w: site_title exceeds %d bytes or contains control characters",
			ErrModelStatusConfigInvalid, MaxModelStatusSiteTitleBytes)
	}
	configValue.SelectedModels = nonNilStrings(configValue.SelectedModels)
	configValue.CustomOrder = nonNilStrings(configValue.CustomOrder)
	if configValue.CustomGroups == nil {
		configValue.CustomGroups = []ModelStatusCustomGroup{}
	}
	raw, err := json.Marshal(configValue)
	if err != nil || len(raw) > MaxModelStatusConfigBytes {
		return fmt.Errorf("%w: configuration exceeds %d bytes", ErrModelStatusConfigInvalid, MaxModelStatusConfigBytes)
	}
	return nil
}

func ValidateModelStatusCustomGroups(groups []ModelStatusCustomGroup) error {
	if len(groups) > MaxModelStatusCustomGroups {
		return fmt.Errorf("%w: custom_groups exceeds %d groups", ErrModelStatusConfigInvalid, MaxModelStatusCustomGroups)
	}
	seenIDs := make(map[string]struct{}, len(groups))
	seenNames := make(map[string]struct{}, len(groups))
	seenModels := make(map[string]struct{})
	totalModels := 0
	for index := range groups {
		group := &groups[index]
		group.ID = strings.TrimSpace(group.ID)
		group.Name = strings.TrimSpace(group.Name)
		group.Icon = strings.TrimSpace(group.Icon)
		if group.ID == "" || len(group.ID) > MaxCustomGroupIDBytes || containsControl(group.ID) ||
			group.Name == "" || len(group.Name) > MaxCustomGroupNameBytes || containsControl(group.Name) ||
			len(group.Icon) > MaxCustomGroupIconBytes || containsControl(group.Icon) {
			return fmt.Errorf("%w: custom_groups[%d] has invalid id, name, or icon", ErrModelStatusConfigInvalid, index)
		}
		idKey, nameKey := strings.ToLower(group.ID), strings.ToLower(group.Name)
		if _, exists := seenIDs[idKey]; exists {
			return fmt.Errorf("%w: duplicate custom group id", ErrModelStatusConfigInvalid)
		}
		if _, exists := seenNames[nameKey]; exists {
			return fmt.Errorf("%w: duplicate custom group name", ErrModelStatusConfigInvalid)
		}
		seenIDs[idKey], seenNames[nameKey] = struct{}{}, struct{}{}
		if len(group.Models) > MaxModelsPerCustomGroup {
			return fmt.Errorf("%w: custom group %q exceeds %d models", ErrModelStatusConfigInvalid, group.Name, MaxModelsPerCustomGroup)
		}
		for modelIndex := range group.Models {
			model := strings.TrimSpace(group.Models[modelIndex])
			if model == "" || len(model) > 256 || containsControl(model) {
				return fmt.Errorf("%w: custom group %q has an invalid model", ErrModelStatusConfigInvalid, group.Name)
			}
			modelKey := strings.ToLower(model)
			if _, exists := seenModels[modelKey]; exists {
				return fmt.Errorf("%w: duplicate model %q across custom groups", ErrModelStatusConfigInvalid, model)
			}
			seenModels[modelKey] = struct{}{}
			group.Models[modelIndex] = model
			totalModels++
		}
		group.Models = nonNilStrings(group.Models)
	}
	if totalModels > MaxModelsAcrossCustomGroups {
		return fmt.Errorf("%w: custom_groups exceeds %d total models", ErrModelStatusConfigInvalid, MaxModelsAcrossCustomGroups)
	}
	raw, err := json.Marshal(groups)
	if err != nil || len(raw) > MaxModelStatusConfigBytes {
		return fmt.Errorf("%w: custom_groups exceeds %d bytes", ErrModelStatusConfigInvalid, MaxModelStatusConfigBytes)
	}
	return nil
}

func decodeModelStatusConfig(raw json.RawMessage) (ModelStatusConfig, error) {
	if len(raw) > MaxModelStatusConfigBytes {
		return ModelStatusConfig{}, ErrModelStatusConfigInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value ModelStatusConfig
	if err := decoder.Decode(&value); err != nil {
		return ModelStatusConfig{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ModelStatusConfig{}, errors.New("model status config contains trailing JSON")
	}
	if err := ValidateModelStatusConfig(&value); err != nil {
		return ModelStatusConfig{}, err
	}
	return value, nil
}

func modelStatusConfigMap(snapshot ModelStatusConfigSnapshot) map[string]interface{} {
	configValue := cloneModelStatusConfig(snapshot.Config)
	return map[string]interface{}{
		"version":          snapshot.Version,
		"time_window":      configValue.TimeWindow,
		"theme":            configValue.Theme,
		"refresh_interval": configValue.RefreshInterval,
		"sort_mode":        configValue.SortMode,
		"custom_order":     configValue.CustomOrder,
		"selected_models":  configValue.SelectedModels,
		"custom_groups":    configValue.CustomGroups,
		"site_title":       configValue.SiteTitle,
	}
}

func cloneModelStatusConfigSnapshot(value ModelStatusConfigSnapshot) ModelStatusConfigSnapshot {
	return ModelStatusConfigSnapshot{Version: value.Version, Config: cloneModelStatusConfig(value.Config)}
}

func cloneModelStatusConfig(value ModelStatusConfig) ModelStatusConfig {
	value.SelectedModels = append([]string{}, value.SelectedModels...)
	value.CustomOrder = append([]string{}, value.CustomOrder...)
	value.CustomGroups = cloneCustomGroups(value.CustomGroups)
	return value
}

func cloneCustomGroups(groups []ModelStatusCustomGroup) []ModelStatusCustomGroup {
	result := make([]ModelStatusCustomGroup, len(groups))
	for index, group := range groups {
		result[index] = group
		result[index].Models = append([]string{}, group.Models...)
	}
	if result == nil {
		return []ModelStatusCustomGroup{}
	}
	return result
}

func validateUniqueModelNames(values []string, allowEmpty bool) error {
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		value := strings.TrimSpace(values[index])
		if (!allowEmpty && value == "") || len(value) > 256 || containsControl(value) {
			return errors.New("invalid model name")
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return errors.New("duplicate model name")
		}
		seen[key] = struct{}{}
		values[index] = value
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func modelConfigContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func modelConfigContainsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
