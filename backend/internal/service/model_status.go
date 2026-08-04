package service

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
)

// Constants for model status
var (
	AvailableTimeWindows = []string{"1h", "6h", "12h", "24h"}
	DefaultTimeWindow    = "24h"
	AvailableThemes      = []string{
		"daylight", "obsidian", "minimal", "neon", "forest", "ocean", "terminal",
		"cupertino", "material", "openai", "anthropic", "vercel", "linear",
		"stripe", "github", "discord", "tesla",
	}
	DefaultTheme = "daylight"
	// LegacyThemeMap maps old theme names to valid ones
	LegacyThemeMap = map[string]string{
		"light":  "daylight",
		"dark":   "obsidian",
		"system": "daylight",
	}
	AvailableRefreshIntervals = []int{0, 30, 60, 120, 300}
	AvailableSortModes        = []string{"default", "availability", "custom"}
)

// Time window slot configurations: {totalSeconds, numSlots, slotSeconds}
// Must match Python backend and frontend TIME_WINDOWS exactly
type timeWindowConfig struct {
	totalSeconds int64
	numSlots     int
	slotSeconds  int64
}

type modelStatusSlotInfo struct {
	total   int64
	success int64
	failure int64
	empty   int64
	latest  int64
}

var timeWindowConfigs = map[string]timeWindowConfig{
	"1h":  {3600, 60, 60},    // 1 hour, 60 slots, 1 minute each
	"6h":  {21600, 24, 900},  // 6 hours, 24 slots, 15 minutes each
	"12h": {43200, 24, 1800}, // 12 hours, 24 slots, 30 minutes each
	"24h": {86400, 24, 3600}, // 24 hours, 24 slots, 1 hour each
}

const (
	modelStatusBatchChunkSize    = 500
	modelStatusBatchQueryTimeout = 15 * time.Second
)

// getStatusColor determines status color based on success rate (matches Python backend)
func getStatusColor(successRate float64, totalRequests int64) string {
	if totalRequests == 0 {
		return "unknown"
	}
	if successRate >= 95 {
		return "green"
	} else if successRate >= 80 {
		return "yellow"
	}
	return "red"
}

// roundRate rounds a float to 2 decimal places
func roundRate(rate float64) float64 {
	return math.Round(rate*100) / 100
}

func buildModelStatusResult(
	modelName, window string,
	twConfig timeWindowConfig,
	fetchedAt time.Time,
	slotMap map[int64]*modelStatusSlotInfo,
) map[string]interface{} {
	startTime := fetchedAt.Unix() - twConfig.totalSeconds
	slotData := make([]map[string]interface{}, 0, twConfig.numSlots)
	var totalReqs, totalSuccess, totalFailure, totalEmpty, latestTrafficAt int64
	for slot := 0; slot < twConfig.numSlots; slot++ {
		slotStart := startTime + int64(slot)*twConfig.slotSeconds
		slotEnd := slotStart + twConfig.slotSeconds
		var slotTotal, slotSuccess, slotFailure, slotEmpty int64
		if info := slotMap[int64(slot)]; info != nil {
			slotTotal = info.total
			slotSuccess = info.success
			slotFailure = info.failure
			slotEmpty = info.empty
			if info.latest > latestTrafficAt {
				latestTrafficAt = info.latest
			}
		}
		slotRate := float64(0)
		if slotTotal > 0 {
			slotRate = float64(slotSuccess) / float64(slotTotal) * 100
		}
		slotData = append(slotData, map[string]interface{}{
			"slot":           slot,
			"start_time":     slotStart,
			"end_time":       slotEnd,
			"total_requests": slotTotal,
			"success_count":  slotSuccess,
			"failure_count":  slotFailure,
			"empty_count":    slotEmpty,
			"success_rate":   roundRate(slotRate),
			"status":         getStatusColor(slotRate, slotTotal),
		})
		totalReqs += slotTotal
		totalSuccess += slotSuccess
		totalFailure += slotFailure
		totalEmpty += slotEmpty
	}

	overallRate := float64(0)
	if totalReqs > 0 {
		overallRate = float64(totalSuccess) / float64(totalReqs) * 100
	}
	legacyStatus := getStatusColor(overallRate, totalReqs)
	trafficHealth := map[string]string{
		"green": "healthy", "yellow": "degraded", "red": "unhealthy", "unknown": "unknown",
	}[legacyStatus]
	sourceState := "empty"
	if totalReqs > 0 {
		freshness := 15 * time.Minute
		if cfg := config.GetOptional(); cfg != nil && cfg.LogFreshnessMaxAge > 0 {
			freshness = cfg.LogFreshnessMaxAge
		}
		sourceState = "fresh"
		if latestTrafficAt == 0 || time.Unix(latestTrafficAt, 0).Before(fetchedAt.Add(-freshness)) {
			sourceState = "stale"
		}
	}
	authoritativeStatus := legacyStatus
	authoritativeHealth := trafficHealth
	var authoritativeRate any
	var observedRate any
	if totalReqs > 0 {
		observedRate = roundRate(overallRate)
	}
	if sourceState == "fresh" {
		authoritativeRate = observedRate
	} else {
		authoritativeStatus = "unknown"
		authoritativeHealth = "unknown"
	}

	return map[string]interface{}{
		"model_name":      modelName,
		"display_name":    modelName,
		"time_window":     window,
		"total_requests":  totalReqs,
		"success_count":   totalSuccess,
		"failure_count":   totalFailure,
		"empty_count":     totalEmpty,
		"success_rate":    authoritativeRate,
		"current_status":  authoritativeStatus,
		"traffic_health":  authoritativeHealth,
		"source_state":    sourceState,
		"observed_status": legacyStatus,
		"observed_rate":   observedRate,
		"last_traffic_at": latestTrafficAt,
		"fetched_at":      fetchedAt.Format(time.RFC3339Nano),
		"slot_data":       slotData,
	}
}

// ModelStatusService handles model availability monitoring
type ModelStatusService struct {
	db            *database.Manager
	logDB         *database.Manager
	configRuntime *modelStatusConfigRuntime
}

type ModelCatalogResult struct {
	Models      []map[string]interface{}
	SourceState string
}

// NewModelStatusService creates a new ModelStatusService
func NewModelStatusService() *ModelStatusService {
	return &ModelStatusService{
		db: database.Get(), logDB: database.GetLog(), configRuntime: currentModelStatusConfigRuntime(),
	}
}

// GetAvailableModels returns all models with 24h request counts
func (s *ModelStatusService) GetAvailableModels() ([]map[string]interface{}, error) {
	result, err := s.GetAvailableModelsWithState()
	return result.Models, err
}

// GetAvailableModelsWithState distinguishes a complete two-source catalog
// from a log-only partial result. Only complete catalogs may enter cache.
func (s *ModelStatusService) GetAvailableModelsWithState() (ModelCatalogResult, error) {
	cm := cache.Get()
	var cached []map[string]interface{}
	found, _ := cm.GetJSON("model_status:available_models", &cached)
	if found {
		return ModelCatalogResult{Models: cached, SourceState: "complete"}, nil
	}

	startTime := time.Now().Unix() - 86400

	query := s.logDB.RebindQuery(`
		SELECT model_name, COUNT(*) as request_count_24h
		FROM logs
		WHERE type IN (2, 5) AND model_name != '' AND created_at >= ?
		GROUP BY model_name
		ORDER BY request_count_24h DESC`)

	rows, err := s.logDB.Query(query, startTime)
	if err != nil {
		return ModelCatalogResult{SourceState: "unavailable"}, err
	}
	byName := make(map[string]map[string]interface{}, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(fmt.Sprintf("%v", row["model_name"]))
		if name == "" {
			continue
		}
		row["model_name"] = name
		row["catalog_state"] = "observed"
		byName[name] = row
	}

	// Active probes must also be able to monitor catalog models that have no
	// recent user traffic. Abilities is read from the NewAPI main database and
	// merged with log counts without writing to either upstream database.
	catalogRows, catalogErr := s.db.Query(`SELECT DISTINCT model AS model_name
		FROM abilities WHERE model IS NOT NULL AND model <> '' ORDER BY model`)
	sourceState := "complete"
	if catalogErr == nil {
		for _, row := range catalogRows {
			name := strings.TrimSpace(fmt.Sprintf("%v", row["model_name"]))
			if name == "" {
				continue
			}
			if existing, ok := byName[name]; ok {
				existing["catalog_state"] = "catalog_and_observed"
				continue
			}
			byName[name] = map[string]interface{}{
				"model_name": name, "request_count_24h": int64(0), "catalog_state": "catalog_only",
			}
		}
	} else {
		sourceState = "partial"
	}

	result := make([]map[string]interface{}, 0, len(byName))
	for _, item := range byName {
		result = append(result, item)
	}
	sort.Slice(result, func(left, right int) bool {
		leftCount := toInt64(result[left]["request_count_24h"])
		rightCount := toInt64(result[right]["request_count_24h"])
		if leftCount != rightCount {
			return leftCount > rightCount
		}
		return fmt.Sprintf("%v", result[left]["model_name"]) < fmt.Sprintf("%v", result[right]["model_name"])
	})

	if sourceState == "complete" {
		_ = cm.Set("model_status:available_models", result, 5*time.Minute)
	}
	return ModelCatalogResult{Models: result, SourceState: sourceState}, nil
}

// GetModelStatus returns status for a specific model
// Uses a single GROUP BY FLOOR query (matches Python backend optimization)
func (s *ModelStatusService) GetModelStatus(modelName, window string) (map[string]interface{}, error) {
	cacheKey := fmt.Sprintf("model_status:%s:%s", modelName, window)
	cm := cache.Get()
	var cached map[string]interface{}
	found, _ := cm.GetJSON(cacheKey, &cached)
	if found {
		return cached, nil
	}

	// Get window configuration (dynamic slot count per window)
	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs["24h"]
	}

	fetchedAt := time.Now().UTC()
	now := fetchedAt.Unix()
	startTime := now - twConfig.totalSeconds
	numSlots := twConfig.numSlots
	slotSeconds := twConfig.slotSeconds

	// Single optimized query — aggregate by time slot using FLOOR division
	// This reduces N queries to 1 query per model (matches Python backend)
	//
	// NewAPI log types are the source of truth for request outcome:
	//   - type=2 is a successful consume record, including embeddings, rerank,
	//     image, audio and other endpoints that legitimately emit zero
	//     completion tokens.
	//   - type=5 is an explicit failure.
	// Zero completion tokens remain diagnostic metadata only; they must not
	// lower availability.
	slotQuery := s.logDB.RebindQuery(fmt.Sprintf(`
		SELECT FLOOR((created_at - %d) / %d) as slot_idx,
			COUNT(*) as total,
			SUM(CASE WHEN type = 2 THEN 1 ELSE 0 END) as success,
			SUM(CASE WHEN type = 5 THEN 1 ELSE 0 END) as failure,
			SUM(CASE WHEN type = 2 AND completion_tokens = 0 THEN 1 ELSE 0 END) as empty,
			MAX(created_at) as latest_at
		FROM logs
		WHERE model_name = ?
			AND created_at >= ? AND created_at < ?
			AND type IN (2, 5)
		GROUP BY FLOOR((created_at - %d) / %d)`,
		startTime, slotSeconds,
		startTime, slotSeconds))

	rows, err := s.logDB.Query(slotQuery, modelName, startTime, now)
	if err != nil {
		return nil, fmt.Errorf("query model status slots: %w", err)
	}

	// Initialize all slots with zeros
	slotMap := make(map[int64]*modelStatusSlotInfo, numSlots)

	// Fill in actual data from query results
	if rows != nil {
		for _, row := range rows {
			idx := toInt64(row["slot_idx"])
			if idx >= 0 && idx < int64(numSlots) {
				slotMap[idx] = &modelStatusSlotInfo{
					total:   toInt64(row["total"]),
					success: toInt64(row["success"]),
					failure: toInt64(row["failure"]),
					empty:   toInt64(row["empty"]),
					latest:  toInt64(row["latest_at"]),
				}
			}
		}
	}

	result := buildModelStatusResult(modelName, window, twConfig, fetchedAt, slotMap)

	cm.Set(cacheKey, result, 30*time.Second)
	return result, nil
}

// GetMultipleModelsStatus returns status for multiple models
func (s *ModelStatusService) GetMultipleModelsStatus(modelNames []string, window string) ([]map[string]interface{}, error) {
	results := make([]map[string]interface{}, len(modelNames))
	if len(modelNames) == 0 {
		return results, nil
	}
	cm := cache.Get()
	missIndexes := make(map[string][]int, len(modelNames))
	missNames := make([]string, 0, len(modelNames))
	for index, name := range modelNames {
		cacheKey := fmt.Sprintf("model_status:%s:%s", name, window)
		var cached map[string]interface{}
		found, _ := cm.GetJSON(cacheKey, &cached)
		if found {
			results[index] = cached
			continue
		}
		if _, exists := missIndexes[name]; !exists {
			missNames = append(missNames, name)
		}
		missIndexes[name] = append(missIndexes[name], index)
	}
	if len(missNames) == 0 {
		return results, nil
	}

	twConfig, ok := timeWindowConfigs[window]
	if !ok {
		twConfig = timeWindowConfigs[DefaultTimeWindow]
	}
	fetchedAt := time.Now().UTC()
	now := fetchedAt.Unix()
	startTime := now - twConfig.totalSeconds
	slotsByModel := make(map[string]map[int64]*modelStatusSlotInfo, len(missNames))
	for _, name := range missNames {
		slotsByModel[name] = make(map[int64]*modelStatusSlotInfo, twConfig.numSlots)
	}

	deadline := time.Now().Add(modelStatusBatchQueryTimeout)
	for start := 0; start < len(missNames); start += modelStatusBatchChunkSize {
		end := start + modelStatusBatchChunkSize
		if end > len(missNames) {
			end = len(missNames)
		}
		chunk := missNames[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		query := s.logDB.RebindQuery(fmt.Sprintf(`
			SELECT model_name,
				FLOOR((created_at - %d) / %d) AS slot_idx,
				COUNT(*) AS total,
				SUM(CASE WHEN type = 2 THEN 1 ELSE 0 END) AS success,
				SUM(CASE WHEN type = 5 THEN 1 ELSE 0 END) AS failure,
				SUM(CASE WHEN type = 2 AND completion_tokens = 0 THEN 1 ELSE 0 END) AS empty,
				MAX(created_at) AS latest_at
			FROM logs
			WHERE model_name IN (%s)
				AND created_at >= ? AND created_at < ?
				AND type IN (2, 5)
			GROUP BY model_name, FLOOR((created_at - %d) / %d)`,
			startTime, twConfig.slotSeconds, placeholders,
			startTime, twConfig.slotSeconds,
		))
		args := make([]interface{}, 0, len(chunk)+2)
		for _, name := range chunk {
			args = append(args, name)
		}
		args = append(args, startTime, now)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("query model status batch: deadline exceeded")
		}
		rows, err := s.logDB.QueryWithTimeout(remaining, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query model status batch chunk %d-%d: %w", start, end, err)
		}
		for _, row := range rows {
			name := strings.TrimSpace(fmt.Sprintf("%v", row["model_name"]))
			slotMap, exists := slotsByModel[name]
			if !exists {
				return nil, fmt.Errorf("query model status batch returned unexpected model %q", name)
			}
			index := toInt64(row["slot_idx"])
			if index < 0 || index >= int64(twConfig.numSlots) {
				continue
			}
			slotMap[index] = &modelStatusSlotInfo{
				total:   toInt64(row["total"]),
				success: toInt64(row["success"]),
				failure: toInt64(row["failure"]),
				empty:   toInt64(row["empty"]),
				latest:  toInt64(row["latest_at"]),
			}
		}
	}

	// Do not publish a partial batch to cache. Only after every chunk succeeds
	// do we construct and cache the complete set of misses.
	for _, name := range missNames {
		status := buildModelStatusResult(name, window, twConfig, fetchedAt, slotsByModel[name])
		for _, index := range missIndexes[name] {
			results[index] = status
		}
	}
	for _, name := range missNames {
		cm.Set(fmt.Sprintf("model_status:%s:%s", name, window), results[missIndexes[name][0]], 30*time.Second)
	}
	return results, nil
}

// GetAllModelsStatus returns status for all models that have requests
func (s *ModelStatusService) GetAllModelsStatus(window string) ([]map[string]interface{}, error) {
	models, err := s.GetAvailableModels()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(models))
	for _, m := range models {
		if name, ok := m["model_name"].(string); ok {
			names = append(names, name)
		}
	}

	return s.GetMultipleModelsStatus(names, window)
}

// GetTokenGroups 返回令牌分组列表及其关联的模型（基于 abilities 表）
func (s *ModelStatusService) GetTokenGroups() ([]map[string]interface{}, error) {
	cm := cache.Get()
	var cached []map[string]interface{}
	found, _ := cm.GetJSON("model_status:token_groups", &cached)
	if found {
		return cached, nil
	}

	// 从 abilities 表获取分组及其模型列表（abilities 表定义了 group-model-channel 的映射）
	// 注意：不再过滤 c.status = 1，否则 ManuallyDisabled / AutoDisabled 的渠道会
	// 让分组里临时不可用的模型从下拉中消失，与用户"这个分组本来就有这个模型"的心智不符。
	groupCol := s.getGroupCol()
	query := s.db.RebindQuery(fmt.Sprintf(`
		SELECT COALESCE(NULLIF(a.%s, ''), 'default') as group_name,
			COUNT(DISTINCT a.model) as model_count
		FROM abilities a
		INNER JOIN channels c ON c.id = a.channel_id
		GROUP BY COALESCE(NULLIF(a.%s, ''), 'default')
		ORDER BY model_count DESC`, groupCol, groupCol))

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}

	// 一次性读出 NewAPI 的分组描述（UserUsableGroups）和倍率（GroupRatio）
	descMap, ratioMap := s.loadGroupMetadata()

	// 为每个分组获取其模型列表
	results := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		groupName := fmt.Sprintf("%v", row["group_name"])

		modelsQuery := s.db.RebindQuery(fmt.Sprintf(`
			SELECT DISTINCT a.model as model_name
			FROM abilities a
			INNER JOIN channels c ON c.id = a.channel_id
			WHERE COALESCE(NULLIF(a.%s, ''), 'default') = ?
			ORDER BY a.model`, groupCol))

		modelRows, err := s.db.Query(modelsQuery, groupName)
		if err != nil {
			continue
		}

		modelNames := make([]string, 0, len(modelRows))
		for _, mr := range modelRows {
			if name, ok := mr["model_name"].(string); ok && name != "" {
				modelNames = append(modelNames, name)
			}
		}

		entry := map[string]interface{}{
			"group_name":  groupName,
			"model_count": row["model_count"],
			"models":      modelNames,
		}
		if d, ok := descMap[groupName]; ok && d != "" && d != groupName {
			entry["description"] = d
		}
		if r, ok := ratioMap[groupName]; ok {
			entry["ratio"] = r
		}
		results = append(results, entry)
	}

	cm.Set("model_status:token_groups", results, 5*time.Minute)
	return results, nil
}

// loadGroupMetadata 一次性从 NewAPI 的 options 表读出分组描述和倍率配置。
// 返回两张 map，缺失时为 nil 不影响主流程。
func (s *ModelStatusService) loadGroupMetadata() (descMap map[string]string, ratioMap map[string]float64) {
	descMap = map[string]string{}
	ratioMap = map[string]float64{}

	keyCol := `"key"`
	if !s.db.IsPG {
		keyCol = "`key`"
	}
	query := s.db.RebindQuery(fmt.Sprintf(
		`SELECT %s as opt_key, value FROM options WHERE %s IN ('UserUsableGroups', 'GroupRatio')`,
		keyCol, keyCol))

	rows, err := s.db.Query(query)
	if err != nil {
		return
	}
	for _, row := range rows {
		key := fmt.Sprintf("%v", row["opt_key"])
		val, _ := row["value"].(string)
		if val == "" {
			continue
		}
		switch key {
		case "UserUsableGroups":
			_ = json.Unmarshal([]byte(val), &descMap)
		case "GroupRatio":
			// GroupRatio 的值可能是 number 或 string number，先按 number 解
			raw := map[string]interface{}{}
			if err := json.Unmarshal([]byte(val), &raw); err == nil {
				for k, v := range raw {
					switch n := v.(type) {
					case float64:
						ratioMap[k] = n
					case json.Number:
						if f, err := n.Float64(); err == nil {
							ratioMap[k] = f
						}
					}
				}
			}
		}
	}
	return
}

// getGroupCol 返回正确引用的 group 列名（group 是保留字）
func (s *ModelStatusService) getGroupCol() string {
	if s.db.IsPG {
		return `"group"`
	}
	return "`group`"
}
