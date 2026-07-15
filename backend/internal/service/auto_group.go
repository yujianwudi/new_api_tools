package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/redis/go-redis/v9"
)

const (
	autoGroupLogsKey            = "auto_group:logs"
	autoGroupLogSequenceKey     = "auto_group:log_sequence"
	autoGroupPendingLogsKey     = "auto_group:pending_logs"
	autoGroupCommittedLogIDsKey = "auto_group:committed_log_ids"
	autoGroupCommitSequenceKey  = "auto_group:commit_sequence"
	autoGroupAuditVersion       = int64(2)
	autoGroupMaxLogs            = int64(1000)
	maxJSONSafeInteger          = int64(1<<53 - 1)
)

var reserveAutoGroupLogIDsScript = redis.NewScript(`
local current_raw = redis.call('GET', KEYS[1])
local current = 0
if current_raw then
  current = tonumber(current_raw)
  if not current then
    return redis.error_reply('invalid auto-group audit sequence')
  end
end
local seed = tonumber(ARGV[1])
local count = tonumber(ARGV[2])

if not current_raw then
  local logs = redis.call('LRANGE', KEYS[2], 0, -1)
  for _, raw in ipairs(logs) do
    local ok, entry = pcall(cjson.decode, raw)
    if ok and entry['id'] then
      local legacy_id = tonumber(entry['id'])
      if legacy_id and legacy_id > current then
        current = legacy_id
      end
    end
  end
  local pending_ids = redis.call('HKEYS', KEYS[3])
  for _, raw_id in ipairs(pending_ids) do
    local pending_id = tonumber(raw_id)
    if pending_id and pending_id > current then
      current = pending_id
    end
  end
end

if seed > current then
  current = seed
end
redis.call('SET', KEYS[1], current)
return redis.call('INCRBY', KEYS[1], count)
`)

var commitAutoGroupAuditLogsScript = redis.NewScript(`
local max_logs = tonumber(ARGV[1])
local pending = {}

local logs_type = redis.call('TYPE', KEYS[1]).ok
if logs_type ~= 'none' and logs_type ~= 'list' then
  return redis.error_reply('invalid committed auto-group audit log key type')
end
local pending_type = redis.call('TYPE', KEYS[2]).ok
if pending_type ~= 'hash' then
  return redis.error_reply('invalid pending auto-group audit log key type')
end
local markers_type = redis.call('TYPE', KEYS[3]).ok
if markers_type ~= 'none' and markers_type ~= 'zset' then
  return redis.error_reply('invalid auto-group audit marker key type')
end
local sequence_type = redis.call('TYPE', KEYS[4]).ok
if sequence_type ~= 'none' and sequence_type ~= 'string' then
  return redis.error_reply('invalid auto-group audit commit sequence key type')
end

local commit_sequence_raw = redis.call('GET', KEYS[4])
if commit_sequence_raw and not tonumber(commit_sequence_raw) then
  return redis.error_reply('invalid auto-group audit commit sequence')
end
local commit_sequence_seed = 0
if not commit_sequence_raw then
  local latest_marker = redis.call('ZREVRANGE', KEYS[3], 0, 0, 'WITHSCORES')
  if latest_marker[2] then
    commit_sequence_seed = tonumber(latest_marker[2]) or 0
  end
end

-- Validate the complete batch before mutating any Redis key. A missing field
-- therefore leaves every pending record available for manual recovery.
for i = 2, #ARGV do
  local id = ARGV[i]
  local raw = redis.call('HGET', KEYS[2], id)
  if not raw then
    return redis.error_reply('missing pending auto-group audit log ' .. id)
  end
  pending[#pending + 1] = {id = id, raw = raw}
end

if not commit_sequence_raw then
  redis.call('SET', KEYS[4], commit_sequence_seed)
end

for _, item in ipairs(pending) do
  local commit_sequence = redis.call('INCR', KEYS[4])
  redis.call('LPUSH', KEYS[1], item.raw)
  redis.call('ZADD', KEYS[3], commit_sequence, item.id)
  redis.call('HDEL', KEYS[2], item.id)
end

redis.call('LTRIM', KEYS[1], 0, max_logs - 1)
local marker_count = redis.call('ZCARD', KEYS[3])
if marker_count > max_logs then
  redis.call('ZREMRANGEBYRANK', KEYS[3], 0, marker_count - max_logs - 1)
end
return #pending
`)

// Audit records are staged in a separate Redis hash before the corresponding
// SQL transaction commits. A SQL failure can leave a pending recovery record,
// but it cannot evict committed history or leave an unaudited group mutation.
// After SQL commit, Redis atomically moves the staged records into the capped
// list and writes commit markers. Only those finalized records are revertible.
type autoGroupAuditStore interface {
	Ping(context.Context) error
	ReserveIDs(context.Context, int) ([]int64, error)
	StageLogs(context.Context, []string) error
	CommitLogs(context.Context, []int64) error
	IsCommitted(context.Context, int64) (bool, error)
	ReadLogs(context.Context) ([]string, error)
}

type redisAutoGroupAuditStore struct {
	client *redis.Client
}

func (s *redisAutoGroupAuditStore) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return errors.New("Redis client is not initialized")
	}
	return s.client.Ping(ctx).Err()
}

func (s *redisAutoGroupAuditStore) ReserveIDs(ctx context.Context, count int) ([]int64, error) {
	if count <= 0 {
		return []int64{}, nil
	}
	if s == nil || s.client == nil {
		return nil, errors.New("Redis client is not initialized")
	}

	// Millisecond time with three sequence digits remains exactly representable
	// by JSON/JavaScript numbers while keeping new IDs far away from legacy
	// LLEN-based IDs. Redis atomically advances the sequence across instances.
	seed := time.Now().UnixMilli() * 1000
	lastID, err := reserveAutoGroupLogIDsScript.Run(
		ctx,
		s.client,
		[]string{autoGroupLogSequenceKey, autoGroupLogsKey, autoGroupPendingLogsKey},
		seed,
		count,
	).Int64()
	if err != nil {
		return nil, fmt.Errorf("reserve audit log IDs: %w", err)
	}
	firstID := lastID - int64(count) + 1
	if firstID <= 0 || lastID > maxJSONSafeInteger {
		return nil, fmt.Errorf("audit log ID sequence is outside the supported range: %d", lastID)
	}

	ids := make([]int64, count)
	for i := range ids {
		ids[i] = firstID + int64(i)
	}
	return ids, nil
}

func (s *redisAutoGroupAuditStore) StageLogs(ctx context.Context, logs []string) error {
	if len(logs) == 0 {
		return nil
	}
	if s == nil || s.client == nil {
		return errors.New("Redis client is not initialized")
	}
	ids, err := autoGroupAuditLogIDs(logs)
	if err != nil {
		return err
	}
	values := make([]interface{}, 0, len(logs)*2)
	for i, id := range ids {
		values = append(values, strconv.FormatInt(id, 10), logs[i])
	}
	if err := s.client.HSet(ctx, autoGroupPendingLogsKey, values...).Err(); err != nil {
		return fmt.Errorf("stage audit logs: %w", err)
	}
	return nil
}

func (s *redisAutoGroupAuditStore) CommitLogs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if s == nil || s.client == nil {
		return errors.New("Redis client is not initialized")
	}

	args := make([]interface{}, 0, len(ids)+1)
	args = append(args, autoGroupMaxLogs)
	for _, id := range ids {
		if id <= 0 || id > maxJSONSafeInteger {
			return fmt.Errorf("invalid committed audit log ID %d", id)
		}
		args = append(args, strconv.FormatInt(id, 10))
	}

	committed, err := commitAutoGroupAuditLogsScript.Run(
		ctx,
		s.client,
		[]string{autoGroupLogsKey, autoGroupPendingLogsKey, autoGroupCommittedLogIDsKey, autoGroupCommitSequenceKey},
		args...,
	).Int64()
	if err != nil {
		// A connection can drop after Redis executed the atomic script but before
		// the reply reached this process. Treat the operation as successful only
		// when every commit marker is independently observable; otherwise fail
		// closed and leave any still-pending fields untouched.
		allCommitted := true
		for _, id := range ids {
			marked, markerErr := s.IsCommitted(ctx, id)
			if markerErr != nil || !marked {
				allCommitted = false
				break
			}
		}
		if allCommitted {
			return nil
		}
		return fmt.Errorf("commit staged audit logs: %w", err)
	}
	if committed != int64(len(ids)) {
		return fmt.Errorf("committed %d staged audit logs, want %d", committed, len(ids))
	}
	return nil
}

func (s *redisAutoGroupAuditStore) IsCommitted(ctx context.Context, id int64) (bool, error) {
	if s == nil || s.client == nil {
		return false, errors.New("Redis client is not initialized")
	}
	if id <= 0 || id > maxJSONSafeInteger {
		return false, nil
	}
	_, err := s.client.ZScore(ctx, autoGroupCommittedLogIDsKey, strconv.FormatInt(id, 10)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read audit commit marker: %w", err)
	}
	return true, nil
}

func (s *redisAutoGroupAuditStore) ReadLogs(ctx context.Context) ([]string, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("Redis client is not initialized")
	}
	return s.client.LRange(ctx, autoGroupLogsKey, 0, -1).Result()
}

type autoGroupLogUser struct {
	ID       int64
	Username string
	Source   string
}

type autoGroupAuditEntry struct {
	AuditVersion int64  `json:"audit_version,omitempty"`
	ID           int64  `json:"id"`
	Action       string `json:"action"`
	UserID       int64  `json:"user_id"`
	Username     string `json:"username"`
	OldGroup     string `json:"old_group"`
	NewGroup     string `json:"new_group"`
	Source       string `json:"source"`
	Operator     string `json:"operator"`
	Affected     int64  `json:"affected"`
	CreatedAt    int64  `json:"created_at"`
}

type autoGroupSQLExecutor interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// AutoGroupService handles automatic user group assignment
// Mirrors Python auto_group_service.py functionality
type AutoGroupService struct {
	db           *database.Manager
	cachedConfig map[string]interface{} // 优化3: 请求级配置缓存
	auditStore   autoGroupAuditStore
}

// Cached OAuth column existence checks for auto group
var (
	agOAuthColumnsOnce   sync.Once
	agAvailableOAuthCols []string
)

// allAutoGroupOAuthColumns lists all possible OAuth ID columns
var allAutoGroupOAuthColumns = []string{"github_id", "wechat_id", "telegram_id", "discord_id", "oidc_id", "linux_do_id"}

// NewAutoGroupService creates a new AutoGroupService
func NewAutoGroupService() *AutoGroupService {
	return &AutoGroupService{db: database.Get()}
}

// getGroupCol returns the properly quoted column name for "group"
func (s *AutoGroupService) getGroupCol() string {
	if s.db.IsPG {
		return `"group"`
	}
	return "`group`"
}

func normalizeAutoGroupName(group string) string {
	if group == "" {
		return "default"
	}
	return group
}

func (s *AutoGroupService) updateUserGroupIfCurrent(userID int64, targetGroup, expectedGroup string) (int64, error) {
	return s.updateUserGroupIfCurrentWith(s.db.DB, userID, targetGroup, expectedGroup)
}

func (s *AutoGroupService) updateUserGroupIfCurrentWith(exec autoGroupSQLExecutor, userID int64, targetGroup, expectedGroup string) (int64, error) {
	groupCol := s.getGroupCol()
	expectedGroup = normalizeAutoGroupName(expectedGroup)
	var result sql.Result
	var err error
	if s.db.IsPG {
		query := fmt.Sprintf(
			"UPDATE users SET %s = $1 WHERE id = $2 AND role != 100 AND COALESCE(NULLIF(%s, ''), 'default') = $3",
			groupCol, groupCol)
		result, err = exec.Exec(query, targetGroup, userID, expectedGroup)
	} else {
		query := fmt.Sprintf(
			"UPDATE users SET %s = ? WHERE id = ? AND role != 100 AND COALESCE(NULLIF(%s, ''), 'default') = ?",
			groupCol, groupCol)
		result, err = exec.Exec(query, targetGroup, userID, expectedGroup)
	}
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *AutoGroupService) getAuditStore() (autoGroupAuditStore, error) {
	if s.auditStore != nil {
		return s.auditStore, nil
	}
	rdb := cache.Get().RedisClient()
	if rdb == nil {
		return nil, errors.New("Redis client is not initialized")
	}
	return &redisAutoGroupAuditStore{client: rdb}, nil
}

func (s *AutoGroupService) requireAuditStore(ctx context.Context) (autoGroupAuditStore, error) {
	store, err := s.getAuditStore()
	if err != nil {
		return nil, fmt.Errorf("audit storage unavailable: %w", err)
	}
	if err := store.Ping(ctx); err != nil {
		return nil, fmt.Errorf("audit storage unavailable: %w", err)
	}
	return store, nil
}

func prepareAutoGroupAuditLogs(
	ctx context.Context,
	store autoGroupAuditStore,
	action string,
	users []autoGroupLogUser,
	oldGroup, newGroup, operator string,
) ([]string, error) {
	if len(users) == 0 {
		return []string{}, nil
	}
	ids, err := store.ReserveIDs(ctx, len(users))
	if err != nil {
		return nil, fmt.Errorf("reserve audit log IDs: %w", err)
	}
	if len(ids) != len(users) {
		return nil, fmt.Errorf("audit storage reserved %d IDs for %d entries", len(ids), len(users))
	}

	seen := make(map[int64]struct{}, len(ids))
	createdAt := time.Now().Unix()
	logs := make([]string, len(users))
	for i, user := range users {
		id := ids[i]
		if id <= 0 || id > maxJSONSafeInteger {
			return nil, fmt.Errorf("audit storage returned invalid log ID %d", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("audit storage returned duplicate log ID %d", id)
		}
		seen[id] = struct{}{}

		entry := autoGroupAuditEntry{
			AuditVersion: autoGroupAuditVersion,
			ID:           id,
			Action:       action,
			UserID:       user.ID,
			Username:     user.Username,
			OldGroup:     oldGroup,
			NewGroup:     newGroup,
			Source:       user.Source,
			Operator:     operator,
			Affected:     1,
			CreatedAt:    createdAt,
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("serialize audit log: %w", err)
		}
		logs[i] = string(data)
	}
	return logs, nil
}

func autoGroupAuditLogIDs(logs []string) ([]int64, error) {
	ids := make([]int64, len(logs))
	seen := make(map[int64]struct{}, len(logs))
	for i, raw := range logs {
		var entry autoGroupAuditEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, fmt.Errorf("decode prepared audit log: %w", err)
		}
		if entry.AuditVersion < autoGroupAuditVersion {
			return nil, fmt.Errorf("prepared audit log %d is missing the current audit version", entry.ID)
		}
		if entry.ID <= 0 || entry.ID > maxJSONSafeInteger {
			return nil, fmt.Errorf("prepared audit log contains invalid ID %d", entry.ID)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return nil, fmt.Errorf("prepared audit logs contain duplicate ID %d", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		ids[i] = entry.ID
	}
	return ids, nil
}

func commitAutoGroupAuditLogs(ctx context.Context, store autoGroupAuditStore, logs []string) error {
	ids, err := autoGroupAuditLogIDs(logs)
	if err != nil {
		return err
	}
	if err := store.CommitLogs(ctx, ids); err != nil {
		return fmt.Errorf("finalize audit logs: %w", err)
	}
	return nil
}

func autoGroupLogRequiresCommitMarker(entry map[string]interface{}) (bool, error) {
	rawVersion, exists := entry["audit_version"]
	if !exists {
		// v0.1.x logs predate commit markers and remain readable/revertible.
		return false, nil
	}
	version, valid := parseAutoGroupLogID(rawVersion)
	if !valid {
		return false, errors.New("日志记录的审计版本无效")
	}
	if version < autoGroupAuditVersion {
		return false, fmt.Errorf("不支持的审计版本 %d", version)
	}
	return true, nil
}

func parseAutoGroupLogID(value interface{}) (int64, bool) {
	var id int64
	switch typed := value.(type) {
	case int:
		id = int64(typed)
	case int32:
		id = int64(typed)
	case int64:
		id = typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed > float64(maxJSONSafeInteger) {
			return 0, false
		}
		id = int64(typed)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		id = parsed
	default:
		return 0, false
	}
	return id, id > 0 && id <= maxJSONSafeInteger
}

// getAvailableOAuthColumns returns OAuth columns that exist in the users table (cached)
func (s *AutoGroupService) getAvailableOAuthColumns() []string {
	agOAuthColumnsOnce.Do(func() {
		agAvailableOAuthCols = make([]string, 0)
		for _, col := range allAutoGroupOAuthColumns {
			if s.db.ColumnExists("users", col) {
				agAvailableOAuthCols = append(agAvailableOAuthCols, col)
			}
		}
	})
	return agAvailableOAuthCols
}

// 优化5: detectSource 只检查数据库中实际存在的列
func (s *AutoGroupService) detectSource(row map[string]interface{}) string {
	cols := s.getAvailableOAuthColumns()
	colSet := make(map[string]bool, len(cols))
	for _, c := range cols {
		colSet[c] = true
	}

	if colSet["github_id"] && toString(row["github_id"]) != "" {
		return "github"
	}
	if colSet["wechat_id"] && toString(row["wechat_id"]) != "" {
		return "wechat"
	}
	if colSet["telegram_id"] && toString(row["telegram_id"]) != "" {
		return "telegram"
	}
	if colSet["discord_id"] && toString(row["discord_id"]) != "" {
		return "discord"
	}
	if colSet["oidc_id"] && toString(row["oidc_id"]) != "" {
		return "oidc"
	}
	if colSet["linux_do_id"] && toString(row["linux_do_id"]) != "" {
		return "linux_do"
	}
	return "password"
}

// buildSourceCaseSQL builds a SQL CASE expression for source detection (优化2)
func (s *AutoGroupService) buildSourceCaseSQL() string {
	cols := s.getAvailableOAuthColumns()
	colSet := make(map[string]bool, len(cols))
	for _, c := range cols {
		colSet[c] = true
	}

	var parts []string
	colSourceMap := []struct{ col, source string }{
		{"github_id", "github"},
		{"wechat_id", "wechat"},
		{"telegram_id", "telegram"},
		{"discord_id", "discord"},
		{"oidc_id", "oidc"},
		{"linux_do_id", "linux_do"},
	}

	for _, cs := range colSourceMap {
		if colSet[cs.col] {
			parts = append(parts, fmt.Sprintf("WHEN %s IS NOT NULL AND %s != '' THEN '%s'", cs.col, cs.col, cs.source))
		}
	}

	if len(parts) == 0 {
		return "'password'"
	}

	return fmt.Sprintf("CASE %s ELSE 'password' END", strings.Join(parts, " "))
}

// Default auto group config — matches Python defaults
var defaultAutoGroupConfig = map[string]interface{}{
	"enabled":               false,
	"mode":                  "simple",
	"target_group":          "",
	"source_rules":          map[string]interface{}{"github": "", "wechat": "", "telegram": "", "discord": "", "oidc": "", "linux_do": "", "password": ""},
	"scan_interval_minutes": 60,
	"auto_scan_enabled":     false,
	"whitelist_ids":         []interface{}{},
	"last_scan_time":        0,
}

// 优化3: getConfigCached 请求级缓存，避免重复 Redis GET + JSON Unmarshal
func (s *AutoGroupService) getConfigCached() map[string]interface{} {
	if s.cachedConfig != nil {
		return s.cachedConfig
	}
	s.cachedConfig = s.GetConfig()
	return s.cachedConfig
}

// invalidateConfigCache clears the cached config (call after SaveConfig)
func (s *AutoGroupService) invalidateConfigCache() {
	s.cachedConfig = nil
}

// GetConfig returns auto group configuration (always fresh from Redis)
func (s *AutoGroupService) GetConfig() map[string]interface{} {
	cm := cache.Get()
	var config map[string]interface{}
	found, _ := cm.GetJSON("auto_group:config", &config)
	if found && config != nil {
		result := make(map[string]interface{})
		for k, v := range defaultAutoGroupConfig {
			result[k] = v
		}
		for k, v := range config {
			result[k] = v
		}
		return result
	}
	result := make(map[string]interface{})
	for k, v := range defaultAutoGroupConfig {
		result[k] = v
	}
	return result
}

// SaveConfig saves auto group configuration
func (s *AutoGroupService) SaveConfig(updates map[string]interface{}) bool {
	config := s.GetConfig()
	for k, v := range updates {
		config[k] = v
	}
	cm := cache.Get()
	if cm.RedisClient() == nil {
		logger.L.Error("保存自动分组配置失败: Redis client is not initialized")
		return false
	}
	if err := cm.RedisClient().Ping(context.Background()).Err(); err != nil {
		logger.L.Error(fmt.Sprintf("保存自动分组配置失败: Redis unavailable: %v", err))
		return false
	}
	if err := cm.Set("auto_group:config", config, 0); err != nil {
		// Manager.Set updates its local tier before Redis. Never leave a config
		// that the caller observed as failed active in this process.
		cm.DeleteLocal("auto_group:config")
		logger.L.Error(fmt.Sprintf("保存自动分组配置失败: %v", err))
		return false
	}
	s.invalidateConfigCache()
	logger.L.Business("自动分组配置已更新")
	return true
}

// IsEnabled returns whether auto group is enabled
func (s *AutoGroupService) IsEnabled() bool {
	config := s.getConfigCached()
	if enabled, ok := config["enabled"].(bool); ok {
		return enabled
	}
	return false
}

// getWhitelistIDs extracts whitelist IDs from config
func (s *AutoGroupService) getWhitelistIDs() []int64 {
	config := s.getConfigCached()
	rawList, ok := config["whitelist_ids"]
	if !ok || rawList == nil {
		return nil
	}

	var result []int64
	switch list := rawList.(type) {
	case []interface{}:
		for _, v := range list {
			result = append(result, toInt64(v))
		}
	case []int64:
		result = list
	case []float64:
		for _, v := range list {
			result = append(result, int64(v))
		}
	}
	return result
}

// getTargetGroupBySource returns the target group for a given source
func (s *AutoGroupService) getTargetGroupBySource(source string) string {
	config := s.getConfigCached()
	mode, _ := config["mode"].(string)

	if mode == "simple" {
		tg, _ := config["target_group"].(string)
		return tg
	}

	// by_source mode
	rules, ok := config["source_rules"].(map[string]interface{})
	if !ok {
		return ""
	}
	tg, _ := rules[source].(string)
	return tg
}

// buildWhitelistCondition builds the SQL condition and args for whitelist exclusion
func (s *AutoGroupService) buildWhitelistCondition(whitelistIDs []int64, argIdx int) (string, []interface{}, int) {
	if len(whitelistIDs) == 0 {
		return "", nil, argIdx
	}

	var args []interface{}
	if s.db.IsPG {
		placeholders := make([]string, len(whitelistIDs))
		for i, id := range whitelistIDs {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, id)
			argIdx++
		}
		return fmt.Sprintf("AND id NOT IN (%s)", strings.Join(placeholders, ",")), args, argIdx
	}

	placeholders := make([]string, len(whitelistIDs))
	for i, id := range whitelistIDs {
		placeholders[i] = "?"
		args = append(args, id)
		_ = i
	}
	return fmt.Sprintf("AND id NOT IN (%s)", strings.Join(placeholders, ",")), args, argIdx
}

// buildOAuthSelectCols builds the OAuth column select string
func (s *AutoGroupService) buildOAuthSelectCols() string {
	cols := s.getAvailableOAuthColumns()
	if len(cols) == 0 {
		return ""
	}
	result := ""
	for _, col := range cols {
		result += ", " + col
	}
	return result
}

// GetStats returns grouping statistics — matches Python's get_stats()
func (s *AutoGroupService) GetStats() map[string]interface{} {
	config := s.getConfigCached()
	enabled, _ := config["enabled"].(bool)
	autoScanEnabled, _ := config["auto_scan_enabled"].(bool)
	scanInterval := toInt64(config["scan_interval_minutes"])
	lastScanTime := toInt64(config["last_scan_time"])

	groupCol := s.getGroupCol()
	whitelistIDs := s.getWhitelistIDs()

	// Build whitelist condition
	wlCond, wlArgs, _ := s.buildWhitelistCondition(whitelistIDs, 1)

	// Count pending users (default group, active, not whitelisted)
	pendingSQL := fmt.Sprintf(`
		SELECT COUNT(*) as cnt
		FROM users
		WHERE (COALESCE(%s, 'default') = 'default' OR %s = '')
		AND deleted_at IS NULL
		AND status = 1
		AND role != 100
		%s`, groupCol, groupCol, wlCond)

	if !s.db.IsPG {
		pendingSQL = s.db.RebindQuery(pendingSQL)
	}

	pendingCount := int64(0)
	row, err := s.db.QueryOne(pendingSQL, wlArgs...)
	if err == nil && row != nil {
		pendingCount = toInt64(row["cnt"])
	}

	totalAssigned := int64(0)
	ctx := context.Background()

	// Count assign logs from the capped audit list. Read-only statistics degrade
	// to zero when Redis is unavailable instead of dereferencing a nil client.
	if store, storeErr := s.getAuditStore(); storeErr == nil {
		if logStrings, readErr := store.ReadLogs(ctx); readErr == nil {
			for _, logStr := range logStrings {
				var entry map[string]interface{}
				if json.Unmarshal([]byte(logStr), &entry) == nil {
					if action, _ := entry["action"].(string); action == "assign" {
						totalAssigned += toInt64(entry["affected"])
					}
				}
			}
		}
	}

	// Calculate next scan time
	nextScanTime := int64(0)
	if autoScanEnabled && scanInterval > 0 {
		nextScanTime = lastScanTime + (scanInterval * 60)
	}

	return map[string]interface{}{
		"pending_count":     pendingCount,
		"total_assigned":    totalAssigned,
		"last_scan_time":    lastScanTime,
		"next_scan_time":    nextScanTime,
		"enabled":           enabled,
		"auto_scan_enabled": autoScanEnabled,
	}
}

// GetAvailableGroups returns all distinct groups from users table
func (s *AutoGroupService) GetAvailableGroups() []map[string]interface{} {
	groupCol := s.getGroupCol()
	query := fmt.Sprintf(`
		SELECT COALESCE(%s, 'default') as group_name, COUNT(*) as user_count
		FROM users
		WHERE deleted_at IS NULL AND role != 100
		GROUP BY COALESCE(%s, 'default')
		ORDER BY user_count DESC`, groupCol, groupCol)

	rows, err := s.db.Query(query)
	if err != nil {
		logger.L.Error(fmt.Sprintf("获取可用分组列表失败: %v", err))
		return []map[string]interface{}{}
	}
	if rows == nil {
		return []map[string]interface{}{}
	}

	result := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		result = append(result, map[string]interface{}{
			"group_name": toString(row["group_name"]),
			"user_count": toInt64(row["user_count"]),
		})
	}
	return result
}

// GetPendingUsers returns users not yet assigned to a group
func (s *AutoGroupService) GetPendingUsers(page, pageSize int) map[string]interface{} {
	groupCol := s.getGroupCol()
	whitelistIDs := s.getWhitelistIDs()
	oauthCols := s.buildOAuthSelectCols()

	args := make([]interface{}, 0)
	argIdx := 1

	wlCond, wlArgs, nextIdx := s.buildWhitelistCondition(whitelistIDs, argIdx)
	args = append(args, wlArgs...)
	argIdx = nextIdx

	// Count total
	countSQL := fmt.Sprintf(`
		SELECT COUNT(*) as cnt
		FROM users
		WHERE (COALESCE(%s, 'default') = 'default' OR %s = '')
		AND deleted_at IS NULL
		AND status = 1
		AND role != 100
		%s`, groupCol, groupCol, wlCond)

	if !s.db.IsPG {
		countSQL = s.db.RebindQuery(countSQL)
	}

	total := int64(0)
	countRow, err := s.db.QueryOne(countSQL, args...)
	if err == nil && countRow != nil {
		total = toInt64(countRow["cnt"])
	}

	// Get user list
	offset := (page - 1) * pageSize
	var listArgs []interface{}
	listArgs = append(listArgs, args...)

	var listSQL string
	if s.db.IsPG {
		listSQL = fmt.Sprintf(`
			SELECT id, username, display_name, email, %s as user_group, status%s
			FROM users
			WHERE (COALESCE(%s, 'default') = 'default' OR %s = '')
			AND deleted_at IS NULL
			AND status = 1
			AND role != 100
			%s
			ORDER BY id DESC
			LIMIT $%d OFFSET $%d`,
			groupCol, oauthCols, groupCol, groupCol, wlCond, argIdx, argIdx+1)
		listArgs = append(listArgs, pageSize, offset)
	} else {
		listSQL = fmt.Sprintf(`
			SELECT id, username, display_name, email, %s as user_group, status%s
			FROM users
			WHERE (COALESCE(%s, 'default') = 'default' OR %s = '')
			AND deleted_at IS NULL
			AND status = 1
			AND role != 100
			%s
			ORDER BY id DESC
			LIMIT ? OFFSET ?`,
			groupCol, oauthCols, groupCol, groupCol, wlCond)
		listArgs = append(listArgs, pageSize, offset)
		listSQL = s.db.RebindQuery(listSQL)
	}

	rows, err := s.db.Query(listSQL, listArgs...)
	if err != nil {
		logger.L.Error(fmt.Sprintf("获取待分配用户列表失败: %v", err))
		rows = nil
	}

	items := make([]map[string]interface{}, 0)
	for _, row := range rows {
		source := s.detectSource(row)
		items = append(items, map[string]interface{}{
			"id":           toInt64(row["id"]),
			"username":     toString(row["username"]),
			"display_name": toString(row["display_name"]),
			"email":        toString(row["email"]),
			"group":        toString(row["user_group"]),
			"source":       source,
			"status":       toInt64(row["status"]),
		})
	}

	totalPages := int64(0)
	if total > 0 {
		totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
	}

	return map[string]interface{}{
		"items":       items,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
	}
}

// GetUsers returns users with filtering — matches Python's get_users()
// 优化2: source 过滤使用 SQL CASE WHEN 代替全表拉取
func (s *AutoGroupService) GetUsers(page, pageSize int, group, source, keyword string) map[string]interface{} {
	groupCol := s.getGroupCol()
	oauthCols := s.buildOAuthSelectCols()
	sourceCaseSQL := s.buildSourceCaseSQL()

	offset := (page - 1) * pageSize
	where := []string{"deleted_at IS NULL", "role != 100"}
	args := []interface{}{}
	argIdx := 1

	if group != "" {
		if group == "default" {
			where = append(where, fmt.Sprintf("(COALESCE(%s, 'default') = 'default' OR %s = '')", groupCol, groupCol))
		} else {
			if s.db.IsPG {
				where = append(where, fmt.Sprintf("%s = $%d", groupCol, argIdx))
				argIdx++
			} else {
				where = append(where, fmt.Sprintf("%s = ?", groupCol))
			}
			args = append(args, group)
		}
	}

	if keyword != "" {
		if s.db.IsPG {
			where = append(where, fmt.Sprintf("(username ILIKE $%d OR CAST(id AS TEXT) LIKE $%d)", argIdx, argIdx+1))
			args = append(args, "%"+keyword+"%", "%"+keyword+"%")
			argIdx += 2
		} else {
			where = append(where, "(username LIKE ? OR CAST(id AS CHAR) LIKE ?)")
			args = append(args, "%"+keyword+"%", "%"+keyword+"%")
		}
	}

	// 优化2: source 过滤下推到 SQL 层
	if source != "" {
		// Validate source against known values to prevent injection
		validSources := map[string]bool{
			"github": true, "wechat": true, "telegram": true,
			"discord": true, "oidc": true, "linux_do": true, "password": true,
		}
		if validSources[source] {
			if s.db.IsPG {
				where = append(where, fmt.Sprintf("(%s) = $%d", sourceCaseSQL, argIdx))
				argIdx++
			} else {
				where = append(where, fmt.Sprintf("(%s) = ?", sourceCaseSQL))
			}
			args = append(args, source)
		}
	}

	whereClause := strings.Join(where, " AND ")

	// Count total (now includes source filter if specified)
	countSQL := fmt.Sprintf("SELECT COUNT(*) as cnt FROM users WHERE %s", whereClause)
	if !s.db.IsPG {
		countSQL = s.db.RebindQuery(countSQL)
	}
	total := int64(0)
	countRow, err := s.db.QueryOne(countSQL, args...)
	if err == nil && countRow != nil {
		total = toInt64(countRow["cnt"])
	}

	// Get users
	var listArgs []interface{}
	listArgs = append(listArgs, args...)

	var listSQL string
	if s.db.IsPG {
		listSQL = fmt.Sprintf(`
			SELECT id, username, display_name, email, %s as user_group, status%s
			FROM users
			WHERE %s
			ORDER BY id DESC
			LIMIT $%d OFFSET $%d`,
			groupCol, oauthCols, whereClause, argIdx, argIdx+1)
		listArgs = append(listArgs, pageSize, offset)
	} else {
		listSQL = fmt.Sprintf(`
			SELECT id, username, display_name, email, %s as user_group, status%s
			FROM users
			WHERE %s
			ORDER BY id DESC
			LIMIT ? OFFSET ?`,
			groupCol, oauthCols, whereClause)
		listArgs = append(listArgs, pageSize, offset)
		listSQL = s.db.RebindQuery(listSQL)
	}

	rows, err := s.db.Query(listSQL, listArgs...)
	if err != nil {
		logger.L.Error(fmt.Sprintf("获取用户列表失败: %v", err))
		rows = nil
	}

	// Build items with source detection
	items := make([]map[string]interface{}, 0)
	for _, row := range rows {
		userSource := s.detectSource(row)
		items = append(items, map[string]interface{}{
			"id":           toInt64(row["id"]),
			"username":     toString(row["username"]),
			"display_name": toString(row["display_name"]),
			"email":        toString(row["email"]),
			"group":        toString(row["user_group"]),
			"source":       userSource,
			"status":       toInt64(row["status"]),
		})
	}

	totalPages := int64(0)
	if total > 0 {
		totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
	}

	return map[string]interface{}{
		"items":       items,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
	}
}

// assignUser assigns a single user to a target group — matches Python's assign_user()
func (s *AutoGroupService) assignUser(userID int64, targetGroup, operator string) map[string]interface{} {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}

	groupCol := s.getGroupCol()
	oauthCols := s.buildOAuthSelectCols()

	var userSQL string
	if s.db.IsPG {
		userSQL = fmt.Sprintf(
			"SELECT id, username, role, %s as user_group%s FROM users WHERE id = $1 AND deleted_at IS NULL",
			groupCol, oauthCols)
	} else {
		userSQL = fmt.Sprintf(
			"SELECT id, username, role, %s as user_group%s FROM users WHERE id = ? AND deleted_at IS NULL",
			groupCol, oauthCols)
	}

	userRow, err := s.db.QueryOne(userSQL, userID)
	if err != nil || userRow == nil {
		return map[string]interface{}{
			"success": false,
			"message": "用户不存在",
		}
	}
	if toInt64(userRow["role"]) == 100 {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("操作已阻止：root 用户 %d 受保护", userID),
		}
	}

	oldGroup := normalizeAutoGroupName(toString(userRow["user_group"]))
	username := toString(userRow["username"])
	source := s.detectSource(userRow)

	if oldGroup == targetGroup {
		return map[string]interface{}{
			"success":   true,
			"message":   fmt.Sprintf("用户 %s 已在 %s 分组", username, targetGroup),
			"user_id":   userID,
			"username":  username,
			"old_group": oldGroup,
			"new_group": targetGroup,
			"source":    source,
		}
	}

	ctx := context.Background()
	store, err := s.requireAuditStore(ctx)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}
	logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
		ID:       userID,
		Username: username,
		Source:   source,
	}}, oldGroup, targetGroup, operator)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("prepare audit log failed: %v", err),
		}
	}

	tx, err := s.db.DB.Beginx()
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("开始用户分组事务失败: %v", err),
		}
	}
	defer func() { _ = tx.Rollback() }()

	affected, err := s.updateUserGroupIfCurrentWith(tx, userID, targetGroup, oldGroup)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("更新用户分组失败: %v", err),
		}
	}
	if affected == 0 {
		_ = tx.Rollback()
		current, checkErr := s.db.QueryOne(userSQL, userID)
		if checkErr != nil || current == nil {
			return map[string]interface{}{
				"success": false,
				"message": "用户状态已变化，分组未更新",
			}
		}
		if toInt64(current["role"]) == 100 {
			return map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("操作已阻止：root 用户 %d 受保护", userID),
			}
		}
		currentGroup := normalizeAutoGroupName(toString(current["user_group"]))
		if currentGroup != oldGroup {
			return map[string]interface{}{
				"success": false,
				"message": "用户状态已并发变化，分组未更新",
			}
		}
		return map[string]interface{}{
			"success": false,
			"message": "用户分组未更新，请重试",
		}
	}

	// Stage outside the capped committed list before SQL commit. If Redis fails,
	// the database transaction rolls back; if SQL fails, committed history is
	// untouched and the pending record remains available for recovery.
	if err := store.StageLogs(ctx, logs); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("audit log staging failed; user group was rolled back: %v", err),
		}
	}
	if err := tx.Commit(); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("提交用户分组事务失败: %v", err),
		}
	}
	if err := commitAutoGroupAuditLogs(ctx, store, logs); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("用户分组已提交，但审计记录最终化失败；pending 记录保留在 Redis %s 且不可恢复，请人工核对: %v", autoGroupPendingLogsKey, err),
		}
	}

	logger.L.Business(fmt.Sprintf("自动分组: 用户分配 user_id=%d username=%s %s -> %s source=%s operator=%s",
		userID, username, oldGroup, targetGroup, source, operator))

	return map[string]interface{}{
		"success":   true,
		"message":   fmt.Sprintf("用户 %s 已分配到 %s", username, targetGroup),
		"user_id":   userID,
		"username":  username,
		"old_group": oldGroup,
		"new_group": targetGroup,
		"source":    source,
	}
}

// RunScan assigns pending users while preserving one audit record per mutation.
func (s *AutoGroupService) RunScan(dryRun bool) map[string]interface{} {
	if !dryRun {
		if err := ensureNewAPIDirectMutationSafe(); err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": err.Error(),
			}
		}
	}

	config := s.getConfigCached()
	mode, _ := config["mode"].(string)

	// Validate configuration
	if mode == "simple" {
		targetGroup, _ := config["target_group"].(string)
		if targetGroup == "" {
			return map[string]interface{}{
				"success": false,
				"message": "未配置目标分组",
			}
		}
	} else if mode == "by_source" {
		rules, _ := config["source_rules"].(map[string]interface{})
		hasAnyRule := false
		if rules != nil {
			for _, v := range rules {
				if sv, ok := v.(string); ok && sv != "" {
					hasAnyRule = true
					break
				}
			}
		}
		if !hasAnyRule {
			return map[string]interface{}{
				"success": false,
				"message": "未配置任何来源分组规则",
			}
		}
	}

	startTime := time.Now()

	// Get pending users for preview/logging
	pending := s.GetPendingUsers(1, 1000)
	users, _ := pending["items"].([]map[string]interface{})

	logger.L.Info(fmt.Sprintf("自动分组扫描: 发现 %d 个待分配用户", len(users)))

	if len(users) == 0 {
		return map[string]interface{}{
			"success": true,
			"dry_run": dryRun,
			"stats": map[string]interface{}{
				"total": 0, "assigned": 0, "skipped": 0, "errors": 0,
			},
			"elapsed_seconds": "0.00",
			"results":         []map[string]interface{}{},
		}
	}

	var scanAuditStore autoGroupAuditStore
	if !dryRun {
		var err error
		scanAuditStore, err = s.requireAuditStore(context.Background())
		if err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": err.Error(),
			}
		}
	}

	results := make([]map[string]interface{}, 0, len(users))
	assignedCount := 0
	skippedCount := 0
	errorCount := 0

	if mode == "simple" && !dryRun {
		// Keep the batch atomic in SQL, but use per-user compare-and-swap updates
		// so the audit set exactly matches the rows that were changed. The old
		// bulk UPDATE could update more rows than the 1000 users loaded above.
		targetGroup, _ := config["target_group"].(string)
		userInfos := make([]autoGroupLogUser, 0, len(users))
		for _, user := range users {
			userInfos = append(userInfos, autoGroupLogUser{
				ID:       toInt64(user["id"]),
				Username: toString(user["username"]),
				Source:   toString(user["source"]),
			})
		}

		ctx := context.Background()
		preparedLogs, err := prepareAutoGroupAuditLogs(ctx, scanAuditStore, "assign", userInfos, "default", targetGroup, "system")
		if err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": fmt.Sprintf("prepare scan audit logs failed: %v", err),
			}
		}

		tx, err := s.db.DB.Beginx()
		if err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": fmt.Sprintf("开始扫描事务失败: %v", err),
			}
		}
		defer func() { _ = tx.Rollback() }()

		logsToAppend := make([]string, 0, len(preparedLogs))
		assignedUsers := make([]autoGroupLogUser, 0, len(userInfos))
		for i, user := range userInfos {
			affected, updateErr := s.updateUserGroupIfCurrentWith(tx, user.ID, targetGroup, "default")
			if updateErr != nil {
				_ = tx.Rollback()
				return map[string]interface{}{
					"success": false,
					"dry_run": false,
					"message": fmt.Sprintf("扫描更新失败，整批已回滚: %v", updateErr),
				}
			}
			if affected == 0 {
				errorCount++
				results = append(results, map[string]interface{}{
					"user_id": user.ID, "username": user.Username, "source": user.Source,
					"action": "error", "message": "用户状态已并发变化，分组未更新",
				})
				continue
			}
			logsToAppend = append(logsToAppend, preparedLogs[i])
			assignedUsers = append(assignedUsers, user)
		}

		if err := scanAuditStore.StageLogs(ctx, logsToAppend); err != nil {
			_ = tx.Rollback()
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": fmt.Sprintf("scan audit staging failed; all group changes were rolled back: %v", err),
			}
		}
		if err := tx.Commit(); err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": fmt.Sprintf("提交扫描事务失败: %v", err),
			}
		}
		if err := commitAutoGroupAuditLogs(ctx, scanAuditStore, logsToAppend); err != nil {
			return map[string]interface{}{
				"success": false,
				"dry_run": false,
				"message": fmt.Sprintf("扫描分组已提交，但审计记录最终化失败；pending 记录保留在 Redis %s 且不可恢复，请人工核对: %v", autoGroupPendingLogsKey, err),
			}
		}

		assignedCount = len(assignedUsers)
		for _, user := range assignedUsers {
			results = append(results, map[string]interface{}{
				"user_id":      user.ID,
				"username":     user.Username,
				"source":       user.Source,
				"target_group": targetGroup,
				"action":       "assigned",
				"message":      fmt.Sprintf("已分配到 %s", targetGroup),
			})
		}
		logger.L.Business(fmt.Sprintf("自动分组: 事务内分配 %d 个用户到 %s", assignedCount, targetGroup))
	} else {
		// by_source 模式 or dry_run: 逐用户处理
		for _, user := range users {
			userID := toInt64(user["id"])
			username := toString(user["username"])
			userSource := toString(user["source"])

			targetGroup := s.getTargetGroupBySource(userSource)

			if targetGroup == "" {
				skippedCount++
				results = append(results, map[string]interface{}{
					"user_id": userID, "username": username, "source": userSource,
					"action": "skipped", "message": fmt.Sprintf("来源 %s 未配置目标分组", userSource),
				})
				continue
			}

			if dryRun {
				assignedCount++
				results = append(results, map[string]interface{}{
					"user_id": userID, "username": username, "source": userSource,
					"target_group": targetGroup, "action": "would_assign",
					"message": fmt.Sprintf("[试运行] 将分配到 %s", targetGroup),
				})
			} else {
				result := s.assignUser(userID, targetGroup, "system")
				if success, _ := result["success"].(bool); success {
					assignedCount++
					results = append(results, map[string]interface{}{
						"user_id": userID, "username": username, "source": userSource,
						"target_group": targetGroup, "action": "assigned",
						"message": toString(result["message"]),
					})
				} else {
					errorCount++
					results = append(results, map[string]interface{}{
						"user_id": userID, "username": username, "source": userSource,
						"action": "error", "message": toString(result["message"]),
					})
				}
			}
		}
	}

	elapsed := time.Since(startTime).Seconds()

	// Update last scan time
	s.SaveConfig(map[string]interface{}{
		"last_scan_time": time.Now().Unix(),
	})

	logger.L.Business(fmt.Sprintf("自动分组扫描完成 dry_run=%v total=%d assigned=%d skipped=%d errors=%d elapsed=%.2fs",
		dryRun, len(users), assignedCount, skippedCount, errorCount, elapsed))

	return map[string]interface{}{
		"success": true,
		"dry_run": dryRun,
		"stats": map[string]interface{}{
			"total":    len(users),
			"assigned": assignedCount,
			"skipped":  skippedCount,
			"errors":   errorCount,
		},
		"elapsed_seconds": fmt.Sprintf("%.2f", elapsed),
		"results":         results,
	}
}

// BatchMoveUsers moves users to a target group
func (s *AutoGroupService) BatchMoveUsers(userIDs []int64, targetGroup string) map[string]interface{} {
	if len(userIDs) == 0 {
		return map[string]interface{}{
			"success": false,
			"message": "未选择用户",
		}
	}
	if targetGroup == "" {
		return map[string]interface{}{
			"success": false,
			"message": "未指定目标分组",
		}
	}
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}
	if _, err := s.requireAuditStore(context.Background()); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}

	successCount := 0
	failedCount := 0
	results := make([]map[string]interface{}, 0)

	for _, userID := range userIDs {
		result := s.assignUser(userID, targetGroup, "admin")
		if success, _ := result["success"].(bool); success {
			successCount++
		} else {
			failedCount++
		}
		results = append(results, result)
	}

	return map[string]interface{}{
		"success":       failedCount == 0,
		"message":       fmt.Sprintf("成功移动 %d 个用户，失败 %d 个", successCount, failedCount),
		"success_count": successCount,
		"failed_count":  failedCount,
		"results":       results,
	}
}

// GetLogs returns group assignment logs — 优化4: 使用 Redis List
func (s *AutoGroupService) GetLogs(page, pageSize int, action string, userID *int64) map[string]interface{} {
	ctx := context.Background()

	logStrings := []string{}
	store, err := s.getAuditStore()
	if err == nil {
		logStrings, err = store.ReadLogs(ctx)
	}
	if err != nil {
		logger.L.Error(fmt.Sprintf("读取自动分组日志失败: %v", err))
		logStrings = []string{}
	}

	// Parse and filter
	filtered := make([]map[string]interface{}, 0)
	for _, logStr := range logStrings {
		var entry map[string]interface{}
		if json.Unmarshal([]byte(logStr), &entry) != nil {
			continue
		}

		if action != "" {
			if logAction, ok := entry["action"].(string); !ok || logAction != action {
				continue
			}
		}
		if userID != nil {
			logUserID := toInt64(entry["user_id"])
			if logUserID != *userID {
				continue
			}
		}
		filtered = append(filtered, entry)
	}

	total := len(filtered)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}

	return map[string]interface{}{
		"items":       filtered[start:end],
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
	}
}

// RevertUser reverts a user's group assignment
func (s *AutoGroupService) RevertUser(logID int) map[string]interface{} {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}

	ctx := context.Background()
	store, err := s.requireAuditStore(ctx)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}
	if logID <= 0 {
		return map[string]interface{}{
			"success": false,
			"message": "日志 ID 无效",
		}
	}

	// Read all logs from Redis list
	logStrings, err := store.ReadLogs(ctx)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("读取日志失败: %v", err),
		}
	}

	// Legacy LLEN-based IDs can be duplicated after list trimming. Never pick
	// the first match: an ambiguous ID could otherwise revert the wrong user.
	var targetLog map[string]interface{}
	matchCount := 0
	for _, logStr := range logStrings {
		var entry map[string]interface{}
		if json.Unmarshal([]byte(logStr), &entry) == nil {
			entryID, valid := parseAutoGroupLogID(entry["id"])
			if valid && entryID == int64(logID) {
				targetLog = entry
				matchCount++
			}
		}
	}
	if matchCount > 1 {
		return map[string]interface{}{
			"success": false,
			"message": "日志 ID 存在多条匹配记录，拒绝恢复以避免操作错误用户",
		}
	}

	if targetLog == nil {
		return map[string]interface{}{
			"success": false,
			"message": "日志记录不存在",
		}
	}
	requiresCommitMarker, err := autoGroupLogRequiresCommitMarker(targetLog)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": err.Error(),
		}
	}
	if requiresCommitMarker {
		committed, markerErr := store.IsCommitted(ctx, int64(logID))
		if markerErr != nil {
			return map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("无法确认日志是否已提交，拒绝恢复: %v", markerErr),
			}
		}
		if !committed {
			return map[string]interface{}{
				"success": false,
				"message": "日志对应的数据库变更未确认提交，拒绝恢复",
			}
		}
	}

	userIDVal := toInt64(targetLog["user_id"])
	oldGroup := toString(targetLog["old_group"])
	newGroup := toString(targetLog["new_group"])
	username := toString(targetLog["username"])
	source := toString(targetLog["source"])

	if userIDVal == 0 {
		return map[string]interface{}{
			"success": false,
			"message": "日志记录缺少用户信息，无法恢复",
		}
	}

	groupCol := s.getGroupCol()

	// Check current user group
	var userSQL string
	if s.db.IsPG {
		userSQL = fmt.Sprintf("SELECT id, role, %s as user_group FROM users WHERE id = $1 AND deleted_at IS NULL", groupCol)
	} else {
		userSQL = fmt.Sprintf("SELECT id, role, %s as user_group FROM users WHERE id = ? AND deleted_at IS NULL", groupCol)
	}

	userRow, err := s.db.QueryOne(userSQL, userIDVal)
	if err != nil || userRow == nil {
		return map[string]interface{}{
			"success": false,
			"message": "用户不存在",
		}
	}
	if toInt64(userRow["role"]) == 100 {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("操作已阻止：root 用户 %d 受保护", userIDVal),
		}
	}

	currentGroup := normalizeAutoGroupName(toString(userRow["user_group"]))

	if currentGroup != newGroup {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("用户当前分组 (%s) 与日志记录不符 (%s)，无法恢复", currentGroup, newGroup),
		}
	}

	revertLogs, err := prepareAutoGroupAuditLogs(ctx, store, "revert", []autoGroupLogUser{{
		ID:       userIDVal,
		Username: username,
		Source:   source,
	}}, newGroup, oldGroup, "admin")
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("prepare revert audit log failed: %v", err),
		}
	}

	tx, err := s.db.DB.Beginx()
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("开始恢复事务失败: %v", err),
		}
	}
	defer func() { _ = tx.Rollback() }()

	// Revert only if the current group still matches the state captured in the log.
	affected, err := s.updateUserGroupIfCurrentWith(tx, userIDVal, oldGroup, newGroup)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("恢复用户分组失败: %v", err),
		}
	}
	if affected == 0 {
		_ = tx.Rollback()
		current, checkErr := s.db.QueryOne(userSQL, userIDVal)
		if checkErr != nil || current == nil {
			return map[string]interface{}{
				"success": false,
				"message": "用户状态已变化，分组未恢复",
			}
		}
		if toInt64(current["role"]) == 100 {
			return map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("操作已阻止：root 用户 %d 受保护", userIDVal),
			}
		}
		checkGroup := normalizeAutoGroupName(toString(current["user_group"]))
		if checkGroup != newGroup {
			return map[string]interface{}{
				"success": false,
				"message": "用户状态已并发变化，分组未恢复",
			}
		}
		return map[string]interface{}{
			"success": false,
			"message": "用户分组未恢复，请重试",
		}
	}

	if err := store.StageLogs(ctx, revertLogs); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("revert audit staging failed; user group was rolled back: %v", err),
		}
	}
	if err := tx.Commit(); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("提交恢复事务失败: %v", err),
		}
	}
	if err := commitAutoGroupAuditLogs(ctx, store, revertLogs); err != nil {
		return map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("用户分组已恢复，但审计记录最终化失败；pending 记录保留在 Redis %s 且不可恢复，请人工核对: %v", autoGroupPendingLogsKey, err),
		}
	}

	logger.L.Business(fmt.Sprintf("自动分组: 用户恢复 user_id=%d username=%s %s -> %s", userIDVal, username, newGroup, oldGroup))

	return map[string]interface{}{
		"success":   true,
		"message":   fmt.Sprintf("用户 %s 已恢复到 %s", username, oldGroup),
		"user_id":   userIDVal,
		"username":  username,
		"old_group": newGroup,
		"new_group": oldGroup,
	}
}
