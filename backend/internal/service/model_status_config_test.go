package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/new-api-tools/backend/internal/toolstore"
)

type recordingModelConfigPublisher struct {
	preflightErr error
	publishErr   error
	preflights   int
	publishes    int
	versions     []int64
}

func (p *recordingModelConfigPublisher) Preflight(context.Context) error {
	p.preflights++
	return p.preflightErr
}

func (p *recordingModelConfigPublisher) Publish(_ context.Context, version int64, _ json.RawMessage) error {
	p.publishes++
	p.versions = append(p.versions, version)
	return p.publishErr
}

func newModelConfigTestStore(t *testing.T) *toolstore.Store {
	t.Helper()
	store, err := toolstore.Init(filepath.Join(t.TempDir(), "toolstore.db"))
	if err != nil {
		t.Fatalf("initialize Tool Store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newModelConfigTestService(store *toolstore.Store, publisher modelStatusConfigPublisher) *ModelStatusService {
	return &ModelStatusService{configRuntime: &modelStatusConfigRuntime{
		store: store, publisher: publisher, cache: &modelStatusConfigCache{},
	}}
}

func modelConfigAudit(key string) toolstore.OperationAuditInput {
	return toolstore.OperationAuditInput{
		RequestID: key, Actor: "model-config-operator", SourceIP: "192.0.2.20",
		AuthMethod: "jwt", Reason: "test durable model configuration", IdempotencyKey: key,
	}
}

func TestModelStatusConfigCommitPublishAndRetryOrdering(t *testing.T) {
	ctx := context.Background()
	store := newModelConfigTestStore(t)
	publisher := &recordingModelConfigPublisher{}
	service := newModelConfigTestService(store, publisher)

	initial, err := service.GetConfigSnapshot(ctx)
	if err != nil || initial.Version != 1 {
		t.Fatalf("initial snapshot = %+v, %v", initial, err)
	}
	written, err := service.SetSelectedModels(ctx, []string{"model-a"}, modelConfigAudit("config-write-1"), nil)
	if err != nil || written.Version != 2 || len(written.Config.SelectedModels) != 1 ||
		publisher.preflights != 1 || publisher.publishes != 1 || publisher.versions[0] != 2 {
		t.Fatalf("first write = %+v, %v; publisher=%+v", written, err, publisher)
	}
	intent, err := store.GetOperationAuditByIdempotencyKey(ctx, "model-config:config-write-1:intent")
	if err != nil || intent.Actor != "model-config-operator" || intent.RequestID != "config-write-1" ||
		intent.Reason != "test durable model configuration" || intent.Action != "model_status.config.intent" {
		t.Fatalf("intent audit = %+v, %v", intent, err)
	}
	outcome, err := store.GetOperationAuditByIdempotencyKey(ctx, "model-config:config-write-1:outcome")
	if err != nil || outcome.Action != "model_status.config.outcome" || len(outcome.AfterJSON) == 0 {
		t.Fatalf("outcome audit = %+v, %v", outcome, err)
	}

	publisher.publishErr = errors.New("redis unavailable after durable commit")
	_, err = service.SetTimeWindow(ctx, "12h", modelConfigAudit("config-write-2"), nil)
	if !errors.Is(err, ErrModelStatusConfigUnavailable) {
		t.Fatalf("publish failure error = %v", err)
	}
	durable, err := store.GetLatestModelStatusConfig(ctx)
	if err != nil || durable.Version != 3 {
		t.Fatalf("durable record after publish failure = %+v, %v", durable, err)
	}
	service.configRuntime.cache.RLock()
	cachedVersion := service.configRuntime.cache.snapshot.Version
	service.configRuntime.cache.RUnlock()
	if cachedVersion != 2 {
		t.Fatalf("L1 cache advanced before successful publish: version=%d", cachedVersion)
	}

	publisher.publishErr = nil
	replayed, err := service.SetTimeWindow(ctx, "12h", modelConfigAudit("config-write-2"), nil)
	if err != nil || replayed.Version != 3 || replayed.Config.TimeWindow != "12h" {
		t.Fatalf("idempotent retry = %+v, %v", replayed, err)
	}
	service.configRuntime.cache.RLock()
	cachedVersion = service.configRuntime.cache.snapshot.Version
	service.configRuntime.cache.RUnlock()
	if cachedVersion != 3 || publisher.publishes != 3 {
		t.Fatalf("successful retry did not publish and advance cache: cache=%d publisher=%+v", cachedVersion, publisher)
	}

	publisher.preflightErr = errors.New("redis ping failed")
	_, err = service.SetSiteTitle(ctx, "should-not-commit", modelConfigAudit("config-write-3"), nil)
	if !errors.Is(err, ErrModelStatusConfigUnavailable) {
		t.Fatalf("preflight failure error = %v", err)
	}
	durable, err = store.GetLatestModelStatusConfig(ctx)
	if err != nil || durable.Version != 3 {
		t.Fatalf("Redis preflight failure changed durable version: %+v, %v", durable, err)
	}
}

func TestModelStatusConfigMultipleInstancesInvalidateByDurableVersion(t *testing.T) {
	ctx := context.Background()
	store := newModelConfigTestStore(t)
	first := newModelConfigTestService(store, &recordingModelConfigPublisher{})
	second := newModelConfigTestService(store, &recordingModelConfigPublisher{})

	before, err := first.GetConfigSnapshot(ctx)
	if err != nil || before.Version != 1 {
		t.Fatalf("first instance initial snapshot = %+v, %v", before, err)
	}
	written, err := second.SetTheme(ctx, "obsidian", modelConfigAudit("multi-instance-write"), nil)
	if err != nil || written.Version != 2 {
		t.Fatalf("second instance write = %+v, %v", written, err)
	}
	after, err := first.GetConfigSnapshot(ctx)
	if err != nil || after.Version != 2 || after.Config.Theme != "obsidian" {
		t.Fatalf("first instance retained stale version: %+v, %v", after, err)
	}
}

type barrierModelConfigPublisher struct {
	arrived chan struct{}
	release chan struct{}
}

func (p *barrierModelConfigPublisher) Preflight(context.Context) error {
	p.arrived <- struct{}{}
	<-p.release
	return nil
}

func (p *barrierModelConfigPublisher) Publish(context.Context, int64, json.RawMessage) error {
	return nil
}

func TestModelStatusConfigImplicitCASRejectsConcurrentLostUpdate(t *testing.T) {
	ctx := context.Background()
	store := newModelConfigTestStore(t)
	publisher := &barrierModelConfigPublisher{
		arrived: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	first := newModelConfigTestService(store, publisher)
	second := newModelConfigTestService(store, publisher)

	type writeResult struct {
		key      string
		snapshot ModelStatusConfigSnapshot
		err      error
	}
	results := make(chan writeResult, 2)
	go func() {
		snapshot, err := first.SetTheme(ctx, "obsidian", modelConfigAudit("implicit-cas-theme"), nil)
		results <- writeResult{key: "theme", snapshot: snapshot, err: err}
	}()
	go func() {
		snapshot, err := second.SetTimeWindow(ctx, "12h", modelConfigAudit("implicit-cas-window"), nil)
		results <- writeResult{key: "time_window", snapshot: snapshot, err: err}
	}()

	<-publisher.arrived
	<-publisher.arrived
	close(publisher.release)

	var winner, loser writeResult
	for range 2 {
		result := <-results
		if result.err == nil {
			winner = result
		} else if errors.Is(result.err, ErrModelStatusConfigConflict) {
			loser = result
		} else {
			t.Fatalf("concurrent config write %s error = %v", result.key, result.err)
		}
	}
	if winner.key == "" || loser.key == "" || winner.key == loser.key || winner.snapshot.Version != 2 {
		t.Fatalf("concurrent CAS outcomes winner=%+v loser=%+v", winner, loser)
	}

	durable, err := first.GetConfigSnapshot(ctx)
	if err != nil || durable.Version != 2 {
		t.Fatalf("durable winner snapshot = %+v, %v", durable, err)
	}
	if winner.key == "theme" {
		if durable.Config.Theme != "obsidian" || durable.Config.TimeWindow != DefaultTimeWindow {
			t.Fatalf("theme winner was corrupted: %+v", durable)
		}
		_, err = second.SetTimeWindow(ctx, "12h", modelConfigAudit("implicit-cas-window-retry"), nil)
	} else {
		if durable.Config.TimeWindow != "12h" || durable.Config.Theme != DefaultTheme {
			t.Fatalf("time-window winner was corrupted: %+v", durable)
		}
		_, err = first.SetTheme(ctx, "obsidian", modelConfigAudit("implicit-cas-theme-retry"), nil)
	}
	if err != nil {
		t.Fatalf("retry losing field after reload: %v", err)
	}
	merged, err := first.GetConfigSnapshot(ctx)
	if err != nil || merged.Version != 3 || merged.Config.Theme != "obsidian" || merged.Config.TimeWindow != "12h" {
		t.Fatalf("merged retry snapshot = %+v, %v", merged, err)
	}
}

type fakeLegacyModelStatusConfigReader struct {
	values    map[string]json.RawMessage
	err       error
	reads     int
	requested []string
}

func (r *fakeLegacyModelStatusConfigReader) GetRawJSONGroup(_ context.Context, keys []string) (map[string]json.RawMessage, error) {
	r.reads++
	r.requested = append([]string(nil), keys...)
	if r.err != nil {
		return nil, r.err
	}
	result := make(map[string]json.RawMessage, len(r.values))
	for key, raw := range r.values {
		result[key] = append(json.RawMessage(nil), raw...)
	}
	return result, nil
}

func TestImportLegacyModelStatusConfigReadsValidatesAndAuditsWholeRedisGroup(t *testing.T) {
	ctx := context.Background()
	store := newModelConfigTestStore(t)
	reader := &fakeLegacyModelStatusConfigReader{values: map[string]json.RawMessage{
		"model_status:selected_models":  json.RawMessage(`[" gpt-a","gpt-b "]`),
		"model_status:time_window":      json.RawMessage(`"12h"`),
		"model_status:theme":            json.RawMessage(`"dark"`),
		"model_status:refresh_interval": json.RawMessage(`120`),
		"model_status:sort_mode":        json.RawMessage(`"custom"`),
		"model_status:custom_order":     json.RawMessage(`[" gpt-b ","gpt-a"]`),
		"model_status:custom_groups":    json.RawMessage(`[{"id":"primary","name":"Primary","icon":"star","models":["gpt-a"]}]`),
		"model_status:site_title":       json.RawMessage(`"Production models"`),
	}}

	snapshot, imported, err := ImportLegacyModelStatusConfig(ctx, store, reader, true)
	if err != nil || !imported || snapshot.Version != 2 {
		t.Fatalf("legacy import = %+v imported=%t err=%v", snapshot, imported, err)
	}
	if reader.reads != 1 || len(reader.requested) != len(legacyModelStatusConfigKeys) {
		t.Fatalf("legacy group reads=%d keys=%v", reader.reads, reader.requested)
	}
	for index, key := range legacyModelStatusConfigKeys {
		if reader.requested[index] != key {
			t.Fatalf("legacy key[%d] = %q, want %q", index, reader.requested[index], key)
		}
	}
	if snapshot.Config.TimeWindow != "12h" || snapshot.Config.Theme != "obsidian" ||
		snapshot.Config.RefreshInterval != 120 || snapshot.Config.SortMode != "custom" ||
		snapshot.Config.SiteTitle != "Production models" || len(snapshot.Config.SelectedModels) != 2 ||
		len(snapshot.Config.CustomOrder) != 2 || len(snapshot.Config.CustomGroups) != 1 ||
		snapshot.Config.SelectedModels[0] != "gpt-a" || snapshot.Config.SelectedModels[1] != "gpt-b" ||
		snapshot.Config.CustomOrder[0] != "gpt-b" || snapshot.Config.CustomOrder[1] != "gpt-a" {
		t.Fatalf("imported legacy config = %+v", snapshot.Config)
	}
	record, err := store.GetModelStatusConfigByOperationKey(ctx, legacyModelStatusConfigOperationKey)
	if err != nil || record.ChangedKey != "bootstrap" || record.Actor != "system:model-config-migration" ||
		!strings.Contains(record.Reason, "selected_models") || !strings.Contains(record.Reason, "site_title") {
		t.Fatalf("legacy import record = %+v, %v", record, err)
	}
	intent, err := store.GetOperationAuditByIdempotencyKey(ctx, legacyModelStatusConfigOperationKey+":intent")
	if err != nil || intent.Action != "model_status.config.legacy_import.intent" {
		t.Fatalf("legacy import intent = %+v, %v", intent, err)
	}
	outcome, err := store.GetOperationAuditByIdempotencyKey(ctx, legacyModelStatusConfigOperationKey+":outcome")
	if err != nil || outcome.Action != "model_status.config.legacy_import.outcome" || len(outcome.AfterJSON) == 0 {
		t.Fatalf("legacy import outcome = %+v, %v", outcome, err)
	}

	reader.err = errors.New("Redis should not be read after the durable marker exists")
	replayed, imported, err := ImportLegacyModelStatusConfig(ctx, store, reader, true)
	if err != nil || imported || replayed.Version != snapshot.Version || reader.reads != 1 {
		t.Fatalf("idempotent import replay = %+v imported=%t reads=%d err=%v", replayed, imported, reader.reads, err)
	}
}

func TestImportLegacyModelStatusConfigFailsClosedWithoutAdvancingDurableVersion(t *testing.T) {
	tests := []struct {
		name      string
		reader    *fakeLegacyModelStatusConfigReader
		wantError error
	}{
		{
			name:      "Redis group read fails",
			reader:    &fakeLegacyModelStatusConfigReader{err: errors.New("injected Redis MGET failure")},
			wantError: ErrModelStatusConfigUnavailable,
		},
		{
			name: "legacy group has unknown custom group field",
			reader: &fakeLegacyModelStatusConfigReader{values: map[string]json.RawMessage{
				"model_status:custom_groups": json.RawMessage(`[{"id":"one","name":"One","models":[],"unknown":true}]`),
			}},
			wantError: ErrModelStatusConfigInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newModelConfigTestStore(t)
			_, imported, err := ImportLegacyModelStatusConfig(context.Background(), store, test.reader, true)
			if !errors.Is(err, test.wantError) || imported {
				t.Fatalf("fail-closed import imported=%t err=%v, want %v", imported, err, test.wantError)
			}
			latest, latestErr := store.GetLatestModelStatusConfig(context.Background())
			if latestErr != nil || latest.Version != 1 {
				t.Fatalf("failed import advanced durable config: %+v, %v", latest, latestErr)
			}
			if _, markerErr := store.GetModelStatusConfigByOperationKey(context.Background(), legacyModelStatusConfigOperationKey); !errors.Is(markerErr, toolstore.ErrNotFound) {
				t.Fatalf("failed import marker error = %v, want ErrNotFound", markerErr)
			}
		})
	}
}

func TestDecodeLegacyModelStatusConfigReportsEmptyValue(t *testing.T) {
	_, _, err := decodeLegacyModelStatusConfig(map[string]json.RawMessage{
		"model_status:theme": json.RawMessage{},
	})
	if !errors.Is(err, ErrModelStatusConfigInvalid) ||
		!strings.Contains(err.Error(), "legacy Redis key model_status:theme is empty") ||
		strings.Contains(err.Error(), "exceeds the size limit") {
		t.Fatalf("empty legacy value error = %v", err)
	}
}

func TestValidateModelStatusConfigTrimsModelLists(t *testing.T) {
	configValue := defaultModelStatusConfig()
	configValue.SelectedModels = []string{" model-a ", "\tmodel-b"}
	configValue.CustomOrder = []string{" model-b\n", "model-a "}

	if err := ValidateModelStatusConfig(&configValue); err != nil {
		t.Fatalf("ValidateModelStatusConfig() error = %v", err)
	}
	if got := strings.Join(configValue.SelectedModels, ","); got != "model-a,model-b" {
		t.Fatalf("trimmed selected_models = %q", got)
	}
	if got := strings.Join(configValue.CustomOrder, ","); got != "model-b,model-a" {
		t.Fatalf("trimmed custom_order = %q", got)
	}
}

func TestImportLegacyModelStatusConfigPersistsAuditedDefaultsWhenRedisWasNotConfigured(t *testing.T) {
	store := newModelConfigTestStore(t)
	snapshot, imported, err := ImportLegacyModelStatusConfig(context.Background(), store, nil, false)
	if err != nil || !imported || snapshot.Version != 2 {
		t.Fatalf("default legacy import = %+v imported=%t err=%v", snapshot, imported, err)
	}
	want := defaultModelStatusConfig()
	if encoded, _ := json.Marshal(snapshot.Config); string(encoded) != mustJSON(t, want) {
		t.Fatalf("default import config = %s, want %s", encoded, mustJSON(t, want))
	}
	record, err := store.GetModelStatusConfigByOperationKey(context.Background(), legacyModelStatusConfigOperationKey)
	if err != nil || !strings.Contains(record.Reason, "no legacy Redis keys") {
		t.Fatalf("default import audit record = %+v, %v", record, err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestValidateModelStatusCustomGroupsBounds(t *testing.T) {
	valid := []ModelStatusCustomGroup{{ID: "primary", Name: "Primary", Models: []string{"model-a"}}}
	if err := ValidateModelStatusCustomGroups(valid); err != nil {
		t.Fatalf("valid custom group rejected: %v", err)
	}
	duplicate := []ModelStatusCustomGroup{
		{ID: "one", Name: "One", Models: []string{"model-a"}},
		{ID: "two", Name: "Two", Models: []string{"MODEL-A"}},
	}
	if err := ValidateModelStatusCustomGroups(duplicate); !errors.Is(err, ErrModelStatusConfigInvalid) {
		t.Fatalf("duplicate cross-group model error = %v", err)
	}

	tooManyGroups := make([]ModelStatusCustomGroup, MaxModelStatusCustomGroups+1)
	if err := ValidateModelStatusCustomGroups(tooManyGroups); !errors.Is(err, ErrModelStatusConfigInvalid) {
		t.Fatalf("group count limit error = %v", err)
	}

	tooManyModels := make([]ModelStatusCustomGroup, 11)
	for groupIndex := range tooManyModels {
		tooManyModels[groupIndex] = ModelStatusCustomGroup{
			ID: fmt.Sprintf("group-%02d", groupIndex), Name: fmt.Sprintf("Group %02d", groupIndex),
			Models: make([]string, MaxModelsPerCustomGroup),
		}
		for modelIndex := range tooManyModels[groupIndex].Models {
			tooManyModels[groupIndex].Models[modelIndex] = fmt.Sprintf("model-%02d-%03d", groupIndex, modelIndex)
		}
	}
	if err := ValidateModelStatusCustomGroups(tooManyModels); !errors.Is(err, ErrModelStatusConfigInvalid) {
		t.Fatalf("total model limit error = %v", err)
	}
}
