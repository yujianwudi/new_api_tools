package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/redis/go-redis/v9"
)

// Activity level constants
const (
	ActivityActive       = "active"
	ActivityInactive     = "inactive"
	ActivityVeryInactive = "very_inactive"
	ActivityNever        = "never"
	ActivityUnknown      = "unknown"

	ActiveThreshold   = 7 * 24 * 3600  // 7 days
	InactiveThreshold = 30 * 24 * 3600 // 30 days

	destructiveLogMaxAge            = 24 * time.Hour
	destructiveLogFutureSkew        = 5 * time.Minute
	destructiveMutationTimeout      = 30 * time.Second
	batchDeleteSnapshotTTL          = 10 * time.Minute
	destructiveSnapshotRedisTimeout = 5 * time.Second
	maxBatchDeleteUsers             = 1000
	batchDeleteCandidatePageSize    = 250
	maxBatchDeletePreviewScanRows   = 50000
	batchDeletePreviewScanTimeout   = 30 * time.Second
	protectedAdminRole              = 10
	userListMaxPageSize             = 100
	activityFilterMaxIDs            = 100000
	activityFilterMaxCandidates     = 100000
	activityCandidateBatchSize      = 10000
	activityLogQueryTimeout         = 15 * time.Second
)

var userActivityNow = time.Now

type batchDeleteUser struct {
	id               int64
	username         string
	requestCount     int64
	lastRequest      int64
	billableLogCount int64
}

type batchDeleteSnapshot struct {
	IDs               []int64 `json:"ids"`
	RequestCounts     []int64 `json:"request_counts"`
	LastRequestTimes  []int64 `json:"last_request_times"`
	BillableLogCounts []int64 `json:"billable_log_counts"`
	ActivityLevel     string  `json:"activity_level"`
	HardDelete        bool    `json:"hard_delete"`
	ActivityAsOf      int64   `json:"activity_as_of"`
	ActivityCutoff    int64   `json:"activity_cutoff"`
	CreatedAt         int64   `json:"created_at"`
}

type batchDeleteActivityEvidence struct {
	lastRequest      int64
	billableLogCount int64
}

type purgeSoftDeletedSnapshot struct {
	IDs       []int64  `json:"ids"`
	DeletedAt []string `json:"deleted_at"`
	CreatedAt int64    `json:"created_at"`
}

func canonicalSnapshotDBValue(value interface{}) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", errors.New("snapshot database value is null")
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano), nil
	case []byte:
		return string(typed), nil
	case string:
		return typed, nil
	default:
		return fmt.Sprint(typed), nil
	}
}

var claimDestructiveSnapshotScript = redis.NewScript(`
local value = redis.call('GET', KEYS[1])
if not value then
  return false
end
redis.call('DEL', KEYS[1])
return value
`)

var storeDestructiveSnapshot = storeDestructiveSnapshotInRedis
var claimDestructiveSnapshot = claimDestructiveSnapshotFromRedis

func destructiveSnapshotRedisClient() (*redis.Client, error) {
	rdb := cache.Get().RedisClient()
	if rdb == nil {
		return nil, fmt.Errorf("%w: Redis is required for one-time destructive-operation snapshots", ErrDestructiveSnapshotStoreUnavailable)
	}
	return rdb, nil
}

func storeDestructiveSnapshotInRedis(key string, value interface{}, ttl time.Duration) error {
	rdb, err := destructiveSnapshotRedisClient()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("serialize destructive-operation snapshot: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), destructiveSnapshotRedisTimeout)
	defer cancel()
	if err := rdb.Set(ctx, key, raw, ttl).Err(); err != nil {
		return fmt.Errorf("%w: write snapshot to Redis: %v", ErrDestructiveSnapshotStoreUnavailable, err)
	}
	return nil
}

func claimDestructiveSnapshotFromRedis(key string) ([]byte, error) {
	rdb, err := destructiveSnapshotRedisClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), destructiveSnapshotRedisTimeout)
	defer cancel()
	result, err := claimDestructiveSnapshotScript.Run(ctx, rdb, []string{key}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: atomically claim snapshot: %v", ErrDestructiveSnapshotStoreUnavailable, err)
	}
	if result == nil {
		return nil, nil
	}
	raw, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("atomically claim destructive-operation snapshot: unexpected Redis result %T", result)
	}
	return []byte(raw), nil
}

// ErrBulkTokenReactivationDisabled rejects the legacy enable_tokens contract.
// Without a durable snapshot of exactly which tokens a ban changed, bulk
// restoration could reactivate credentials disabled for an unrelated incident.
var ErrBulkTokenReactivationDisabled = errors.New("bulk token reactivation is disabled")

// ErrInvalidOrExpiredSnapshot identifies malformed, expired, missing, or
// already-consumed destructive-operation previews. Handlers should return a
// client error and ask the operator to preview again.
var ErrInvalidOrExpiredSnapshot = errors.New("invalid or expired operation snapshot")

// ErrSnapshotInvalidated identifies a valid preview whose protected rows have
// changed since it was created.
var ErrSnapshotInvalidated = errors.New("operation snapshot invalidated")

// ErrDestructiveSnapshotStoreUnavailable means Redis could not provide the
// cross-instance atomic one-time claim required by destructive previews.
var ErrDestructiveSnapshotStoreUnavailable = errors.New("destructive-operation snapshot store unavailable")

// ErrSeparateLogDBBatchDeleteBlocked prevents pretending that a users-row lock
// can fence billable-log inserts made through an independent database.
var ErrSeparateLogDBBatchDeleteBlocked = errors.New("batch delete is blocked when logs use a separate database")

var (
	ErrInvalidActivityFilter        = errors.New("invalid user activity filter")
	ErrInvalidSourceFilter          = errors.New("invalid user source filter")
	ErrUnsupportedSourceFilter      = errors.New("user source filter is unsupported by the current schema")
	ErrInvalidGroupFilter           = errors.New("invalid user group filter")
	ErrActivityLogUnavailable       = errors.New("user activity log source is unavailable")
	ErrActivityFilterScaleExceeded  = errors.New("user activity filter exceeds the safe query limit")
	ErrOAuthCapabilitiesUnavailable = errors.New("user OAuth capabilities are unavailable")
	ErrInviterNotFound              = errors.New("inviter was not found")
	ErrInvitedUsersSnapshotMismatch = errors.New("invited-user snapshot identity mismatch")
)

func newBatchDeleteSnapshotID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate batch-delete snapshot id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func batchDeleteSnapshotKey(id string) (string, error) {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("%w: batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	return "user_management:batch_delete_snapshot:" + id, nil
}

func storeBatchDeleteSnapshot(snapshot batchDeleteSnapshot) (string, error) {
	id, err := newBatchDeleteSnapshotID()
	if err != nil {
		return "", err
	}
	key, _ := batchDeleteSnapshotKey(id)
	if err := storeDestructiveSnapshot(key, snapshot, batchDeleteSnapshotTTL); err != nil {
		return "", fmt.Errorf("store batch-delete snapshot: %w", err)
	}
	return id, nil
}

func consumeBatchDeleteSnapshot(id, activityLevel string, hardDelete bool) (*batchDeleteSnapshot, error) {
	key, err := batchDeleteSnapshotKey(id)
	if err != nil {
		return nil, err
	}
	raw, err := claimDestructiveSnapshot(key)
	if err != nil {
		return nil, fmt.Errorf("claim batch-delete snapshot: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	var snapshot batchDeleteSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("%w: malformed batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	if time.Since(time.Unix(snapshot.CreatedAt, 0)) > batchDeleteSnapshotTTL {
		return nil, fmt.Errorf("%w: batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	if snapshot.ActivityLevel != activityLevel || snapshot.HardDelete != hardDelete {
		return nil, fmt.Errorf("%w: batch-delete snapshot does not match the confirmed operation", ErrInvalidOrExpiredSnapshot)
	}
	if err := validateBatchDeleteSnapshotEvidence(snapshot); err != nil {
		return nil, fmt.Errorf("%w: malformed batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	return &snapshot, nil
}

func batchDeleteActivityCutoff(activityLevel string, asOf int64) (int64, error) {
	switch activityLevel {
	case ActivityNever:
		return 0, nil
	case ActivityVeryInactive:
		return asOf - InactiveThreshold, nil
	case ActivityInactive:
		return asOf - ActiveThreshold, nil
	default:
		return 0, fmt.Errorf("invalid activity level: %s", activityLevel)
	}
}

func validateBatchDeleteSnapshotEvidence(snapshot batchDeleteSnapshot) error {
	length := len(snapshot.IDs)
	if length != len(snapshot.RequestCounts) ||
		length != len(snapshot.LastRequestTimes) ||
		length != len(snapshot.BillableLogCounts) ||
		snapshot.ActivityAsOf <= 0 || snapshot.CreatedAt != snapshot.ActivityAsOf {
		return errors.New("batch-delete snapshot evidence dimensions are invalid")
	}
	expectedCutoff, err := batchDeleteActivityCutoff(snapshot.ActivityLevel, snapshot.ActivityAsOf)
	if err != nil || snapshot.ActivityCutoff != expectedCutoff {
		return errors.New("batch-delete snapshot cutoff is invalid")
	}
	seen := make(map[int64]struct{}, length)
	for index, id := range snapshot.IDs {
		if id <= 0 {
			return errors.New("batch-delete snapshot user identity is invalid")
		}
		if _, exists := seen[id]; exists {
			return errors.New("batch-delete snapshot contains duplicate users")
		}
		seen[id] = struct{}{}

		lastRequest := snapshot.LastRequestTimes[index]
		logCount := snapshot.BillableLogCounts[index]
		if logCount < 0 || (logCount == 0 && lastRequest != 0) ||
			(logCount > 0 && (lastRequest <= 0 || lastRequest > snapshot.ActivityAsOf)) {
			return errors.New("batch-delete snapshot log evidence is invalid")
		}
		if classifyUserActivity(snapshot.RequestCounts[index], lastRequest, snapshot.ActivityAsOf) != snapshot.ActivityLevel {
			return errors.New("batch-delete snapshot activity classification is invalid")
		}
	}
	return nil
}

func purgeSoftDeletedSnapshotKey(id string) (string, error) {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("%w: purge snapshot", ErrInvalidOrExpiredSnapshot)
	}
	return "user_management:purge_soft_deleted_snapshot:" + id, nil
}

func storePurgeSoftDeletedSnapshot(snapshot purgeSoftDeletedSnapshot) (string, error) {
	id, err := newBatchDeleteSnapshotID()
	if err != nil {
		return "", err
	}
	key, _ := purgeSoftDeletedSnapshotKey(id)
	if err := storeDestructiveSnapshot(key, snapshot, batchDeleteSnapshotTTL); err != nil {
		return "", fmt.Errorf("store purge snapshot: %w", err)
	}
	return id, nil
}

func consumePurgeSoftDeletedSnapshot(id string) (*purgeSoftDeletedSnapshot, error) {
	key, err := purgeSoftDeletedSnapshotKey(id)
	if err != nil {
		return nil, err
	}
	raw, err := claimDestructiveSnapshot(key)
	if err != nil {
		return nil, fmt.Errorf("claim purge snapshot: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: purge snapshot", ErrInvalidOrExpiredSnapshot)
	}
	var snapshot purgeSoftDeletedSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("%w: malformed purge snapshot", ErrInvalidOrExpiredSnapshot)
	}
	if time.Since(time.Unix(snapshot.CreatedAt, 0)) > batchDeleteSnapshotTTL {
		return nil, fmt.Errorf("%w: purge snapshot", ErrInvalidOrExpiredSnapshot)
	}
	if len(snapshot.IDs) == 0 || len(snapshot.IDs) > maxBatchDeleteUsers || len(snapshot.IDs) != len(snapshot.DeletedAt) {
		return nil, fmt.Errorf("%w: invalid purge snapshot size", ErrInvalidOrExpiredSnapshot)
	}
	return &snapshot, nil
}

func batchDeleteEligibilityClause(ids, requestCounts []int64) (string, []interface{}, error) {
	if len(ids) == 0 || len(ids) != len(requestCounts) {
		return "", nil, fmt.Errorf("invalid batch-delete eligibility snapshot")
	}
	clauses := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		if id <= 0 || requestCounts[i] < 0 {
			return "", nil, fmt.Errorf("invalid batch-delete eligibility value")
		}
		clauses[i] = "(id = ? AND request_count = ?)"
		args = append(args, id, requestCounts[i])
	}
	return strings.Join(clauses, " OR "), args, nil
}

// UserManagementService handles user queries and operations
type UserManagementService struct {
	db    *database.Manager
	logDB *database.Manager
}

type userSourceColumn struct {
	Source string
	Column string
}

// Keep source filtering and response decoration on one authoritative priority
// order. A multi-bound account belongs to exactly the first matching source.
var userSourcePriority = []userSourceColumn{
	{Source: "linux_do", Column: "linux_do_id"},
	{Source: "github", Column: "github_id"},
	{Source: "wechat", Column: "wechat_id"},
	{Source: "telegram", Column: "telegram_id"},
	{Source: "discord", Column: "discord_id"},
	{Source: "oidc", Column: "oidc_id"},
}

type oauthColumnCacheKey struct {
	DB            *sqlx.DB
	IsPG          bool
	CurrentSchema string
	SearchPath    string
	TableSchema   string
}

type oauthColumnCacheEntry struct {
	Columns   []string
	ExpiresAt time.Time
}

const oauthColumnCacheTTL = time.Minute

const postgresOAuthSchemaIdentitySQL = `SELECT
	COALESCE(current_schema(), '') AS current_schema,
	current_setting('search_path') AS search_path,
	COALESCE((
		SELECT n.nspname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass('users')
	), '') AS table_schema`

const postgresOAuthColumnsSQL = `SELECT column_name
	FROM information_schema.columns
	WHERE table_schema = $1 AND table_name = $2
	ORDER BY ordinal_position`

// Only successful, complete capability probes are cached. The key is scoped to
// the concrete database pool so tests, reconnects and different schemas cannot
// inherit another database's capabilities. A short TTL detects additive
// migrations, while a dependent query failure invalidates removed or renamed
// columns immediately for the next request.
var oauthColumnCache sync.Map

func (s *UserManagementService) oauthColumnCacheIdentity() (oauthColumnCacheKey, error) {
	if s == nil || s.db == nil || s.db.DB == nil {
		return oauthColumnCacheKey{}, fmt.Errorf("%w: main database is not initialized", ErrOAuthCapabilitiesUnavailable)
	}

	key := oauthColumnCacheKey{DB: s.db.DB, IsPG: s.db.IsPG}
	driverName := strings.ToLower(s.db.DB.DriverName())
	switch {
	case driverName == "sqlite" || driverName == "sqlite3":
		key.CurrentSchema = "main"
		key.TableSchema = "main"
	case s.db.IsPG:
		row, err := s.db.QueryOneWithTimeout(activityLogQueryTimeout, postgresOAuthSchemaIdentitySQL)
		if err != nil {
			return oauthColumnCacheKey{}, fmt.Errorf("%w: resolve PostgreSQL users schema: %v", ErrOAuthCapabilitiesUnavailable, err)
		}
		if row == nil {
			return oauthColumnCacheKey{}, fmt.Errorf("%w: PostgreSQL schema identity returned no row", ErrOAuthCapabilitiesUnavailable)
		}
		key.CurrentSchema = strings.TrimSpace(toString(row["current_schema"]))
		key.SearchPath = strings.TrimSpace(toString(row["search_path"]))
		key.TableSchema = strings.TrimSpace(toString(row["table_schema"]))
		if key.CurrentSchema == "" || key.SearchPath == "" || key.TableSchema == "" {
			return oauthColumnCacheKey{}, fmt.Errorf(
				"%w: PostgreSQL users schema is unresolved (current_schema=%q search_path=%q table_schema=%q)",
				ErrOAuthCapabilitiesUnavailable, key.CurrentSchema, key.SearchPath, key.TableSchema,
			)
		}
	default:
		row, err := s.db.QueryOneWithTimeout(activityLogQueryTimeout, "SELECT DATABASE() AS table_schema")
		if err != nil {
			return oauthColumnCacheKey{}, fmt.Errorf("%w: resolve users schema: %v", ErrOAuthCapabilitiesUnavailable, err)
		}
		key.TableSchema = strings.TrimSpace(toString(row["table_schema"]))
		key.CurrentSchema = key.TableSchema
		if key.TableSchema == "" {
			return oauthColumnCacheKey{}, fmt.Errorf("%w: users schema is unresolved", ErrOAuthCapabilitiesUnavailable)
		}
	}
	return key, nil
}

func (s *UserManagementService) invalidateOAuthColumnCache() {
	if s == nil || s.db == nil || s.db.DB == nil {
		return
	}
	oauthColumnCache.Range(func(rawKey, _ interface{}) bool {
		key, ok := rawKey.(oauthColumnCacheKey)
		if ok && key.DB == s.db.DB {
			oauthColumnCache.Delete(key)
		}
		return true
	})
}

// NewUserManagementService creates a new UserManagementService
func NewUserManagementService() *UserManagementService {
	return &UserManagementService{db: database.Get(), logDB: database.GetLog()}
}

func normalizeActivityFilter(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "all":
		return "", nil
	case ActivityActive, ActivityInactive, ActivityVeryInactive, ActivityNever:
		return value, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidActivityFilter, value)
	}
}

func normalizeSourceFilter(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "all" || value == "password" {
		if value == "all" {
			return "", nil
		}
		return value, nil
	}
	for _, item := range userSourcePriority {
		if value == item.Source {
			return value, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidSourceFilter, value)
}

func normalizeGroupFilter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 64 || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: group names must be at most 64 characters and contain no control delimiters", ErrInvalidGroupFilter)
	}
	return value, nil
}

// classifyUserActivity is the single activity truth table used by statistics,
// filtering and response decoration. A positive request_count is not proof of
// when a request happened. Without a billable log at or before asOf, the only
// safe classification is unknown; treating missing or future-only history as
// very_inactive could make a user eligible for destructive follow-up actions.
func classifyUserActivity(requestCount, lastRequest, asOf int64) string {
	if requestCount == 0 {
		if lastRequest != 0 {
			return ActivityUnknown
		}
		return ActivityNever
	}
	if requestCount < 0 || lastRequest <= 0 || lastRequest > asOf {
		return ActivityUnknown
	}
	if lastRequest > asOf-ActiveThreshold {
		return ActivityActive
	}
	if lastRequest >= asOf-InactiveThreshold {
		return ActivityInactive
	}
	return ActivityVeryInactive
}

func activitySetsFromLastRequestTimes(lastRequestTimes map[int64]int64, asOf int64) (map[int64]bool, map[int64]bool) {
	active := make(map[int64]bool)
	recent := make(map[int64]bool)
	activeCutoff := asOf - ActiveThreshold
	recentCutoff := asOf - InactiveThreshold
	for userID, lastRequest := range lastRequestTimes {
		// The contract is active for age < 7d, inactive for age 7d..30d,
		// and very_inactive for age > 30d. Therefore the 7-day boundary is
		// intentionally exclusive while the 30-day boundary is inclusive.
		if lastRequest > activeCutoff {
			active[userID] = true
		}
		if lastRequest >= recentCutoff {
			recent[userID] = true
		}
	}
	return active, recent
}

func (s *UserManagementService) ensureActivityLogSourceReady() error {
	if s.logDB == nil || s.logDB.DB == nil {
		return fmt.Errorf("%w: log database is not initialized", ErrActivityLogUnavailable)
	}
	status := database.GetLogSourceStatus()
	if status.Configured && (!status.Healthy || status.UsingFallback) {
		return fmt.Errorf("%w: mode=%s fallback=%t: %s", ErrActivityLogUnavailable, status.Mode, status.UsingFallback, status.LastError)
	}
	return nil
}

func (s *UserManagementService) activityUserIDsBetween(since, asOf int64) (map[int64]bool, error) {
	if err := s.ensureActivityLogSourceReady(); err != nil {
		return nil, err
	}
	if asOf < since {
		return nil, fmt.Errorf("%w: invalid activity evidence window", ErrActivityLogUnavailable)
	}
	rows, err := s.logDB.QueryWithTimeout(activityLogQueryTimeout, s.logDB.RebindQuery(
		"SELECT DISTINCT user_id FROM logs WHERE type IN (2,5) AND created_at >= ? AND created_at <= ? AND user_id > 0"), since, asOf)
	if err != nil {
		return nil, fmt.Errorf("%w: query recent user IDs: %v", ErrActivityLogUnavailable, err)
	}
	if len(rows) > activityFilterMaxIDs {
		return nil, fmt.Errorf("%w: %d IDs exceeds %d", ErrActivityFilterScaleExceeded, len(rows), activityFilterMaxIDs)
	}
	set := make(map[int64]bool, len(rows))
	for _, row := range rows {
		if id := toInt64(row["user_id"]); id > 0 {
			set[id] = true
		}
	}
	return set, nil
}

func (s *UserManagementService) activitySets(asOf int64) (map[int64]bool, map[int64]bool, error) {
	lastRequestTimes, err := s.activityEvidenceSnapshot(asOf)
	if err != nil {
		return nil, nil, err
	}
	active, recent := activitySetsFromLastRequestTimes(lastRequestTimes, asOf)
	return active, recent, nil
}

// activityEvidenceSnapshot obtains every user's latest billable log in one
// statement. Filtering, statistics and response decoration can then reuse the
// same immutable in-memory evidence instead of observing three different log
// database snapshots. Late/backfilled rows written after this query are not
// allowed to change the classification of the response already in flight.
func (s *UserManagementService) activityEvidenceSnapshot(asOf int64) (map[int64]int64, error) {
	if err := s.ensureActivityLogSourceReady(); err != nil {
		return nil, err
	}
	query := s.logDB.RebindQuery(`SELECT user_id, MAX(created_at) AS last_request_time
		FROM logs
		WHERE type IN (2,5) AND created_at <= ? AND user_id > 0
		GROUP BY user_id
		ORDER BY user_id
		LIMIT ?`)
	rows, err := s.logDB.QueryWithTimeout(activityLogQueryTimeout, query, asOf, activityFilterMaxIDs+1)
	if err != nil {
		return nil, fmt.Errorf("%w: query activity evidence snapshot: %v", ErrActivityLogUnavailable, err)
	}
	if len(rows) > activityFilterMaxIDs {
		return nil, fmt.Errorf("%w: %d users exceeds %d", ErrActivityFilterScaleExceeded, len(rows), activityFilterMaxIDs)
	}
	result := make(map[int64]int64, len(rows))
	for _, row := range rows {
		userID := toInt64(row["user_id"])
		lastRequest := toInt64(row["last_request_time"])
		if userID > 0 && lastRequest > 0 {
			result[userID] = lastRequest
		}
	}
	return result, nil
}

// activeUserIDsSince returns the set of user_ids that have at least one billable
// log entry (type 2/5) since `since`. It queries the log DB directly, so it stays
// correct when logs live in a separate database (LOG_SQL_DSN) — a cross-DB
// EXISTS(...) subquery against the users table is impossible there.
func (s *UserManagementService) activeUserIDsSince(since int64) (map[int64]bool, error) {
	return s.activityUserIDsBetween(since, time.Now().Unix())
}

// candidateBillableEvidenceWithTimeout returns bounded evidence for only the
// deletion candidates. A positive asOf fixes the preview boundary; zero reads
// all currently visible evidence for execution-time drift detection.
func (s *UserManagementService) candidateBillableEvidenceWithTimeout(candidateIDs []int64, asOf int64, timeout time.Duration) (map[int64]batchDeleteActivityEvidence, error) {
	evidence := make(map[int64]batchDeleteActivityEvidence)
	if len(candidateIDs) == 0 {
		return evidence, nil
	}
	if err := s.ensureActivityLogSourceReady(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	deadline := time.Now().Add(timeout)
	const lookupBatchSize = 500
	for start := 0; start < len(candidateIDs); start += lookupBatchSize {
		end := start + lookupBatchSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		batch := candidateIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		asOfClause := ""
		if asOf > 0 {
			asOfClause = " AND created_at <= ?"
			args = append(args, asOf)
		}
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}

		query := s.logDB.RebindQuery(fmt.Sprintf(
			`SELECT user_id, MAX(created_at) AS last_request_time, COUNT(*) AS billable_log_count
			FROM logs WHERE type IN (2,5)%s AND user_id IN (%s)
			GROUP BY user_id`,
			asOfClause, strings.Join(placeholders, ",")))
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		rows, err := s.logDB.QueryWithTimeout(remaining, query, args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id := toInt64(row["user_id"])
			if id > 0 {
				evidence[id] = batchDeleteActivityEvidence{
					lastRequest:      toInt64(row["last_request_time"]),
					billableLogCount: toInt64(row["billable_log_count"]),
				}
			}
		}
	}
	return evidence, nil
}

// previewBatchDeleteCandidates walks the main user table with keyset
// pagination and checks one bounded page against the log database at a time.
// It stops as soon as maxBatchDeleteUsers+1 eligible rows are found, which is
// enough to reject an oversized operation without scanning the full population.
func (s *UserManagementService) previewBatchDeleteCandidates(activityLevel string, asOf int64) ([]batchDeleteUser, error) {
	requestCountPredicate := "request_count > 0"
	if activityLevel == ActivityNever {
		requestCountPredicate = "request_count = 0"
	}
	maxRow, err := s.db.QueryOneWithTimeout(10*time.Second, s.db.RebindQuery(fmt.Sprintf(`SELECT COALESCE(MAX(id), 0) AS max_id
		FROM users WHERE deleted_at IS NULL AND role < 10 AND %s`, requestCountPredicate)))
	if err != nil {
		return nil, fmt.Errorf("read batch-delete scan ceiling: %w", err)
	}
	scanCeiling := toInt64(maxRow["max_id"])
	if scanCeiling <= 0 {
		return []batchDeleteUser{}, nil
	}
	query := s.db.RebindQuery(fmt.Sprintf(`SELECT id, username, request_count
		FROM users
		WHERE deleted_at IS NULL AND role < 10 AND %s AND id > ? AND id <= ?
		ORDER BY id ASC LIMIT ?`, requestCountPredicate))

	eligible := make([]batchDeleteUser, 0, maxBatchDeleteUsers+1)
	lastID := int64(0)
	scannedRows := 0
	deadline := time.Now().Add(batchDeletePreviewScanTimeout)
	for len(eligible) <= maxBatchDeleteUsers {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("batch-delete preview exceeded its %s scan budget", batchDeletePreviewScanTimeout)
		}
		rows, err := s.db.QueryWithTimeout(remaining, query, lastID, scanCeiling, batchDeleteCandidatePageSize)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		scannedRows += len(rows)
		if scannedRows > maxBatchDeletePreviewScanRows {
			return nil, fmt.Errorf("batch-delete preview exceeded the safe scan limit of %d candidate rows", maxBatchDeletePreviewScanRows)
		}

		pageIDs := make([]int64, 0, len(rows))
		pageLastID := lastID
		for _, row := range rows {
			id := toInt64(row["id"])
			if id > pageLastID {
				pageLastID = id
			}
			if id > 0 {
				pageIDs = append(pageIDs, id)
			}
		}
		if pageLastID <= lastID {
			return nil, errors.New("batch-delete candidate pagination made no progress")
		}

		activityEvidence, err := s.candidateBillableEvidenceWithTimeout(pageIDs, asOf, time.Until(deadline))
		if err != nil {
			return nil, fmt.Errorf("无法从日志库判定用户活跃度，已中止删除以防误删: %w", err)
		}
		for _, row := range rows {
			uid := toInt64(row["id"])
			if uid <= 0 {
				continue
			}
			requestCount := toInt64(row["request_count"])
			evidence := activityEvidence[uid]
			activity := ActivityUnknown
			if evidence.billableLogCount == 0 || evidence.lastRequest > 0 {
				activity = classifyUserActivity(requestCount, evidence.lastRequest, asOf)
			}
			if activity != activityLevel {
				continue
			}
			eligible = append(eligible, batchDeleteUser{
				id:               uid,
				username:         toString(row["username"]),
				requestCount:     requestCount,
				lastRequest:      evidence.lastRequest,
				billableLogCount: evidence.billableLogCount,
			})
			if len(eligible) > maxBatchDeleteUsers {
				return eligible, nil
			}
		}
		lastID = pageLastID
		if len(rows) < batchDeleteCandidatePageSize {
			break
		}
	}
	return eligible, nil
}

func (s *UserManagementService) candidateBillableEvidenceLocked(ctx context.Context, tx *sqlx.Tx, candidateIDs []int64, asOf int64) (map[int64]batchDeleteActivityEvidence, error) {
	if s.logDB != s.db {
		return nil, ErrSeparateLogDBBatchDeleteBlocked
	}

	evidence := make(map[int64]batchDeleteActivityEvidence)
	const lookupBatchSize = 500
	for start := 0; start < len(candidateIDs); start += lookupBatchSize {
		end := start + lookupBatchSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		batch := candidateIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		asOfClause := ""
		if asOf > 0 {
			asOfClause = " AND created_at <= ?"
			args = append(args, asOf)
		}
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := tx.Rebind(fmt.Sprintf(
			`SELECT user_id, MAX(created_at) AS last_request_time, COUNT(*) AS billable_log_count
			FROM logs WHERE type IN (2,5)%s AND user_id IN (%s)
			GROUP BY user_id`,
			asOfClause, strings.Join(placeholders, ",")))
		var rows []struct {
			UserID           int64 `db:"user_id"`
			LastRequest      int64 `db:"last_request_time"`
			BillableLogCount int64 `db:"billable_log_count"`
		}
		if err := tx.SelectContext(ctx, &rows, query, args...); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.UserID > 0 {
				evidence[row.UserID] = batchDeleteActivityEvidence{
					lastRequest:      row.LastRequest,
					billableLogCount: row.BillableLogCount,
				}
			}
		}
	}
	return evidence, nil
}

func validateBatchDeleteEvidenceUnchanged(snapshot batchDeleteSnapshot, current map[int64]batchDeleteActivityEvidence, currentAsOf int64) error {
	if currentAsOf < snapshot.ActivityAsOf {
		return fmt.Errorf("%w: system time precedes the preview evidence boundary", ErrSnapshotInvalidated)
	}
	for index, id := range snapshot.IDs {
		expected := batchDeleteActivityEvidence{
			lastRequest:      snapshot.LastRequestTimes[index],
			billableLogCount: snapshot.BillableLogCounts[index],
		}
		observed := current[id]
		if observed != expected {
			return fmt.Errorf(
				"%w: user %d billable-log evidence changed (last_request %d->%d, count %d->%d); preview again",
				ErrSnapshotInvalidated, id,
				expected.lastRequest, observed.lastRequest,
				expected.billableLogCount, observed.billableLogCount,
			)
		}
		if classifyUserActivity(snapshot.RequestCounts[index], observed.lastRequest, currentAsOf) != snapshot.ActivityLevel {
			return fmt.Errorf("%w: user %d activity classification changed as time advanced; preview again", ErrSnapshotInvalidated, id)
		}
	}
	return nil
}

// ensureDestructiveLogSourceReady fails closed when a batch deletion cannot
// prove that it is reading the intended, current log stream. Read-only pages
// may use database.GetLog()'s fallback, but destructive activity decisions may
// not use a configured-but-unavailable LOG_SQL_DSN or a stale/empty log table.
func (s *UserManagementService) ensureDestructiveLogSourceReady(now time.Time) error {
	status := database.GetLogSourceStatus()
	if !status.SafeForDestructiveReads() {
		detail := status.LastError
		if detail == "" {
			detail = "log source is not initialized or healthy"
		}
		return fmt.Errorf("destructive operation blocked: log source mode=%s fallback=%t: %s",
			status.Mode, status.UsingFallback, detail)
	}
	if s.logDB == nil || s.logDB.DB == nil {
		return fmt.Errorf("destructive operation blocked: log database is unavailable")
	}

	row, err := s.logDB.QueryOneWithTimeout(10*time.Second,
		"SELECT MAX(created_at) AS max_created_at FROM logs WHERE type IN (2,5)")
	if err != nil {
		return fmt.Errorf("destructive operation blocked: cannot verify log freshness: %w", err)
	}
	if row == nil {
		return fmt.Errorf("destructive operation blocked: log table returned no freshness data")
	}

	latest := toInt64(row["max_created_at"])
	if latest <= 0 {
		return fmt.Errorf("destructive operation blocked: log table is empty")
	}
	if latest > now.Add(destructiveLogFutureSkew).Unix() {
		return fmt.Errorf("destructive operation blocked: newest log timestamp %d is in the future", latest)
	}
	if age := now.Sub(time.Unix(latest, 0)); age > destructiveLogMaxAge {
		return fmt.Errorf("destructive operation blocked: newest log is stale (%s old, maximum %s)",
			age.Round(time.Second), destructiveLogMaxAge)
	}
	return nil
}

// withMutationTransaction keeps destructive writes atomic and ensures every
// statement issued by the callback shares the same timeout context.
func (s *UserManagementService) withMutationTransaction(fn func(context.Context, *sqlx.Tx) error) error {
	if s.db == nil || s.db.DB == nil {
		return fmt.Errorf("database is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), destructiveMutationTimeout)
	defer cancel()

	tx, err := s.db.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mutation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mutation transaction: %w", err)
	}
	return nil
}

func (s *UserManagementService) mutationRowLockClause() string {
	// Production managers always carry their MySQL/PostgreSQL config. SQLite
	// managers used by unit tests intentionally leave Config nil because SQLite
	// does not support SELECT ... FOR UPDATE.
	if s.db != nil && s.db.Config != nil {
		return " FOR UPDATE"
	}
	return ""
}

func (s *UserManagementService) ensureNonRootUserMutation(ctx context.Context, tx *sqlx.Tx, userID int64) error {
	var role int64
	query := tx.Rebind("SELECT role FROM users WHERE id = ?" + s.mutationRowLockClause())
	if err := tx.GetContext(ctx, &role, query, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %d not found", userID)
		}
		return fmt.Errorf("check user role: %w", err)
	}
	if role == 100 {
		return fmt.Errorf("operation blocked: root user %d is protected", userID)
	}
	return nil
}

func (s *UserManagementService) ensureNonRootTokenMutation(ctx context.Context, tx *sqlx.Tx, tokenID int64) error {
	var role int64
	query := tx.Rebind(`SELECT u.role
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = ?` + s.mutationRowLockClause())
	if err := tx.GetContext(ctx, &role, query, tokenID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("token %d not found", tokenID)
		}
		return fmt.Errorf("check token owner role: %w", err)
	}
	if role == 100 {
		return fmt.Errorf("operation blocked: token %d belongs to a protected root user", tokenID)
	}
	return nil
}

func verifyUserStatusMutation(ctx context.Context, tx *sqlx.Tx, userID, targetStatus, affected int64, action string) error {
	if affected > 0 {
		return nil
	}
	var state struct {
		Role   int64 `db:"role"`
		Status int64 `db:"status"`
	}
	if err := tx.GetContext(ctx, &state, tx.Rebind("SELECT role, status FROM users WHERE id = ?"), userID); err != nil {
		return fmt.Errorf("%s user state check: %w", action, err)
	}
	if state.Role == 100 {
		return fmt.Errorf("%s blocked: user became a protected root", action)
	}
	if state.Status == targetStatus {
		return nil
	}
	return fmt.Errorf("%s blocked: user changed concurrently", action)
}

// probeAvailableOAuthColumns returns OAuth columns that exist in the users
// table. Probe failures are returned and deliberately not cached.
func (s *UserManagementService) probeAvailableOAuthColumns() ([]string, error) {
	if s.db == nil || s.db.DB == nil {
		return nil, fmt.Errorf("%w: main database is not initialized", ErrOAuthCapabilitiesUnavailable)
	}
	key, err := s.oauthColumnCacheIdentity()
	if err != nil {
		return nil, err
	}
	if cached, ok := oauthColumnCache.Load(key); ok {
		entry, valid := cached.(oauthColumnCacheEntry)
		if valid && time.Now().Before(entry.ExpiresAt) {
			return append([]string{}, entry.Columns...), nil
		}
		oauthColumnCache.Delete(key)
	}

	var rows []map[string]interface{}
	switch strings.ToLower(s.db.DB.DriverName()) {
	case "sqlite", "sqlite3":
		rows, err = s.db.Query("PRAGMA table_info(users)")
	default:
		if s.db.IsPG {
			rows, err = s.db.QueryWithTimeout(
				activityLogQueryTimeout, postgresOAuthColumnsSQL, key.TableSchema, "users")
		} else {
			rows, err = s.db.QueryWithTimeout(activityLogQueryTimeout,
				"SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position",
				key.TableSchema, "users")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect users columns: %v", ErrOAuthCapabilitiesUnavailable, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: users table was not visible during capability inspection", ErrOAuthCapabilitiesUnavailable)
	}

	existing := make(map[string]bool, len(rows))
	for _, row := range rows {
		name := toString(row["column_name"])
		if name == "" { // SQLite PRAGMA uses `name`.
			name = toString(row["name"])
		}
		existing[name] = true
	}
	columns := make([]string, 0, len(userSourcePriority))
	for _, item := range userSourcePriority {
		if existing[item.Column] {
			columns = append(columns, item.Column)
		}
	}
	oauthColumnCache.Store(key, oauthColumnCacheEntry{
		Columns: append([]string{}, columns...), ExpiresAt: time.Now().Add(oauthColumnCacheTTL),
	})
	logger.L.Business(fmt.Sprintf("detected users OAuth columns: %v", columns))
	return columns, nil
}

func userSourceSQLExpression(alias string, available map[string]bool) string {
	parts := make([]string, 0, len(userSourcePriority))
	for _, item := range userSourcePriority {
		if available[item.Column] {
			parts = append(parts, fmt.Sprintf(
				"WHEN %s.%s IS NOT NULL AND %s.%s <> '' THEN '%s'",
				alias, item.Column, alias, item.Column, item.Source))
		}
	}
	if len(parts) == 0 {
		return "'password'"
	}
	return "CASE " + strings.Join(parts, " ") + " ELSE 'password' END"
}

func userSourceFromRow(row map[string]interface{}, available map[string]bool) string {
	for _, item := range userSourcePriority {
		if available[item.Column] && strings.TrimSpace(toString(row[item.Column])) != "" {
			return item.Source
		}
	}
	return "password"
}

// GetActivityStats returns user activity statistics
func (s *UserManagementService) GetActivityStats(quick bool) (map[string]interface{}, error) {
	now := userActivityNow().Unix()

	// Total users (not deleted)
	totalRow, err := s.db.QueryOne("SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL")
	if err != nil {
		return nil, err
	}
	totalUsers := toInt64(totalRow["count"])

	if quick {
		// Quick mode: only total + never requested
		neverRow, err := s.db.QueryOne(
			"SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL AND request_count = 0")
		if err != nil {
			return nil, err
		}
		neverCount := int64(0)
		if neverRow != nil {
			neverCount = toInt64(neverRow["count"])
		}
		return map[string]interface{}{
			"total_users":         totalUsers,
			"active_users":        nil,
			"inactive_users":      nil,
			"very_inactive_users": nil,
			"never_requested":     neverCount,
			"quick_mode":          true,
			"source_state":        "partial",
			"as_of":               now,
		}, nil
	}

	// Full stats: classify users by their most recent billable log.
	// Logs may live in a separate DB, so we can't use a cross-DB EXISTS subquery.
	// Instead: pull the active/recent user-id sets from the log DB, then count
	// against the users table in Go.
	lastRequestTimes, err := s.activityEvidenceSnapshot(now)
	if err != nil {
		return nil, err
	}

	// All non-deleted users that have ever made a request.
	requestedRows, err := s.db.Query("SELECT id FROM users WHERE deleted_at IS NULL AND request_count > 0")
	if err != nil {
		return nil, err
	}

	var activeCount, inactiveCount, veryInactiveCount, unknownCount int64
	for _, r := range requestedRows {
		uid := toInt64(r["id"])
		switch classifyUserActivity(1, lastRequestTimes[uid], now) {
		case ActivityActive:
			activeCount++
		case ActivityInactive:
			inactiveCount++
		case ActivityVeryInactive:
			veryInactiveCount++
		case ActivityUnknown:
			unknownCount++
		}
	}

	// Never requested
	neverRow, err := s.db.QueryOne("SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL AND request_count = 0")
	if err != nil {
		return nil, err
	}
	neverCount := int64(0)
	if neverRow != nil {
		neverCount = toInt64(neverRow["count"])
	}

	var activeValue interface{} = activeCount
	var inactiveValue interface{} = inactiveCount
	var veryInactiveValue interface{} = veryInactiveCount
	sourceState := "fresh"
	if unknownCount > 0 {
		// Bucket counts are incomplete lower bounds when any requested user
		// lacks trustworthy historical evidence. Do not publish them as
		// authoritative numbers; expose the degraded state and unknown count.
		activeValue = nil
		inactiveValue = nil
		veryInactiveValue = nil
		sourceState = "partial"
	}

	return map[string]interface{}{
		"total_users":         totalUsers,
		"active_users":        activeValue,
		"inactive_users":      inactiveValue,
		"very_inactive_users": veryInactiveValue,
		"never_requested":     neverCount,
		"unknown_users":       unknownCount,
		"as_of":               now,
		"source_state":        sourceState,
	}, nil
}

// ListUsersParams defines parameters for listing users
type ListUsersParams struct {
	Page           int    `json:"page"`
	PageSize       int    `json:"page_size"`
	ActivityFilter string `json:"activity_filter"`
	GroupFilter    string `json:"group_filter"`
	SourceFilter   string `json:"source_filter"`
	Search         string `json:"search"`
	OrderBy        string `json:"order_by"`
	OrderDir       string `json:"order_dir"`
}

func userListOrderColumn(orderBy string) string {
	switch orderBy {
	case "id":
		return "id"
	case "username":
		return "username"
	case "quota":
		return "quota"
	case "used_quota":
		return "used_quota"
	case "request_count":
		return "request_count"
	default:
		return "request_count"
	}
}

func userListOrderDirection(orderDir string) string {
	if strings.EqualFold(orderDir, "ASC") {
		return "ASC"
	}
	return "DESC"
}

func userMatchesActivityFilter(activity string, requestCount, lastRequest, asOf int64) bool {
	return classifyUserActivity(requestCount, lastRequest, asOf) == activity
}

type activityCandidateSnapshot struct {
	RequestCount string
	SortValue    string
}

func userManagementReadTxOptions(db *database.Manager) *sql.TxOptions {
	isolation := sql.LevelRepeatableRead
	if db != nil && db.DB != nil {
		switch strings.ToLower(db.DB.DriverName()) {
		case "sqlite", "sqlite3":
			isolation = sql.LevelSerializable
		}
	}
	return &sql.TxOptions{Isolation: isolation, ReadOnly: true}
}

func queryUserManagementTxMaps(ctx context.Context, tx *sqlx.Tx, query string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := tx.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]map[string]interface{}, 0)
	for rows.Next() {
		row := make(map[string]interface{})
		if err := rows.MapScan(row); err != nil {
			return nil, err
		}
		for key, value := range row {
			if bytes, ok := value.([]byte); ok {
				row[key] = string(bytes)
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// activityFilteredUserPage avoids serializing tens of thousands of log-derived
// IDs into one IN (...) predicate. Logs may live in a separate database, so a
// cross-database join is not portable. The main-database candidate scan and
// bounded row fetch run in one repeatable read-only snapshot. Candidates are
// keyset-scanned in bounded batches, and the scan fails closed above the hard
// population limit instead of materializing an unbounded result in memory.
func (s *UserManagementService) activityFilteredUserPage(
	selectCols, whereClause, orderBy, orderDir string,
	args []interface{},
	activity string,
	lastRequestTimes map[int64]int64,
	asOf int64,
	page, pageSize int,
) ([]map[string]interface{}, int64, int64, error) {
	return s.activityFilteredUserPageWithLimit(
		selectCols, whereClause, orderBy, orderDir, args, activity,
		lastRequestTimes, asOf, page, pageSize, activityFilterMaxCandidates,
	)
}

func (s *UserManagementService) activityFilteredUserPageWithLimit(
	selectCols, whereClause, orderBy, orderDir string,
	args []interface{},
	activity string,
	lastRequestTimes map[int64]int64,
	asOf int64,
	page, pageSize int,
	maxCandidates int64,
) ([]map[string]interface{}, int64, int64, error) {
	if s == nil || s.db == nil || s.db.DB == nil {
		return nil, 0, 0, fmt.Errorf("%w: main database is not initialized", ErrActivityLogUnavailable)
	}
	if maxCandidates < 1 {
		return nil, 0, 0, fmt.Errorf("%w: candidate limit must be positive", ErrActivityFilterScaleExceeded)
	}

	ctx, cancel := context.WithTimeout(context.Background(), activityLogQueryTimeout)
	defer cancel()
	tx, err := s.db.DB.BeginTxx(ctx, userManagementReadTxOptions(s.db))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: begin activity-filtered user snapshot: %v", ErrActivityLogUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	orderClause := fmt.Sprintf("u.%s %s", orderBy, orderDir)
	if orderBy != "id" {
		orderClause += fmt.Sprintf(", u.id %s", orderDir)
	}
	comparison := ">"
	if orderDir == "DESC" {
		comparison = "<"
	}

	offset := int64(page-1) * int64(pageSize)
	pageIDs := make([]int64, 0, pageSize)
	pageSnapshots := make(map[int64]activityCandidateSnapshot, pageSize)
	var total int64
	var unknownCount int64
	var scanned int64
	var cursorID int64
	var cursorSortValue interface{}
	hasCursor := false

	for {
		candidateArgs := append([]interface{}{}, args...)
		keysetClause := ""
		if hasCursor {
			if orderBy == "id" {
				keysetClause = fmt.Sprintf(" AND u.id %s %s", comparison, s.db.Placeholder(len(candidateArgs)+1))
				candidateArgs = append(candidateArgs, cursorID)
			} else {
				firstPlaceholder := s.db.Placeholder(len(candidateArgs) + 1)
				secondPlaceholder := s.db.Placeholder(len(candidateArgs) + 2)
				idPlaceholder := s.db.Placeholder(len(candidateArgs) + 3)
				keysetClause = fmt.Sprintf(
					" AND (u.%s %s %s OR (u.%s = %s AND u.id %s %s))",
					orderBy, comparison, firstPlaceholder,
					orderBy, secondPlaceholder, comparison, idPlaceholder,
				)
				candidateArgs = append(candidateArgs, cursorSortValue, cursorSortValue, cursorID)
			}
		}

		queryLimit := int64(activityCandidateBatchSize)
		remainingWithOverflowProbe := maxCandidates - scanned + 1
		if remainingWithOverflowProbe < queryLimit {
			queryLimit = remainingWithOverflowProbe
		}
		limitPlaceholder := s.db.Placeholder(len(candidateArgs) + 1)
		candidateArgs = append(candidateArgs, queryLimit)
		candidateQuery := fmt.Sprintf(
			"SELECT u.id, u.request_count, u.%s AS activity_sort_value FROM users u WHERE %s%s ORDER BY %s LIMIT %s",
			orderBy, whereClause, keysetClause, orderClause, limitPlaceholder,
		)
		candidates, queryErr := queryUserManagementTxMaps(ctx, tx, candidateQuery, candidateArgs...)
		if queryErr != nil {
			return nil, 0, 0, fmt.Errorf("%w: query activity-filtered user candidates: %v", ErrActivityLogUnavailable, queryErr)
		}
		if len(candidates) == 0 {
			break
		}

		for _, candidate := range candidates {
			scanned++
			if scanned > maxCandidates {
				return nil, 0, 0, fmt.Errorf(
					"%w: more than %d candidate users matched the non-activity filters",
					ErrActivityFilterScaleExceeded, maxCandidates,
				)
			}
			userID := toInt64(candidate["id"])
			if userID < 1 {
				return nil, 0, 0, fmt.Errorf("%w: invalid activity candidate identity", ErrActivityLogUnavailable)
			}
			requestCount := toInt64(candidate["request_count"])
			level := classifyUserActivity(requestCount, lastRequestTimes[userID], asOf)
			if level == ActivityUnknown {
				unknownCount++
			}
			if level != activity {
				continue
			}
			if total >= offset && len(pageIDs) < pageSize {
				requestCountSnapshot, snapshotErr := canonicalSnapshotDBValue(candidate["request_count"])
				if snapshotErr != nil {
					return nil, 0, 0, fmt.Errorf("%w: snapshot candidate request_count: %v", ErrActivityLogUnavailable, snapshotErr)
				}
				sortSnapshot, snapshotErr := canonicalSnapshotDBValue(candidate["activity_sort_value"])
				if snapshotErr != nil {
					return nil, 0, 0, fmt.Errorf("%w: snapshot candidate sort value: %v", ErrActivityLogUnavailable, snapshotErr)
				}
				pageIDs = append(pageIDs, userID)
				pageSnapshots[userID] = activityCandidateSnapshot{
					RequestCount: requestCountSnapshot,
					SortValue:    sortSnapshot,
				}
			}
			total++
		}

		lastCandidate := candidates[len(candidates)-1]
		cursorID = toInt64(lastCandidate["id"])
		cursorSortValue = lastCandidate["activity_sort_value"]
		if cursorID < 1 || cursorSortValue == nil {
			return nil, 0, 0, fmt.Errorf("%w: activity candidate cursor is invalid", ErrActivityLogUnavailable)
		}
		hasCursor = true
		if int64(len(candidates)) < queryLimit {
			break
		}
	}

	orderedRows := make([]map[string]interface{}, 0, len(pageIDs))
	if len(pageIDs) > 0 {
		placeholders := make([]string, len(pageIDs))
		pageArgs := append([]interface{}{}, args...)
		for index, userID := range pageIDs {
			placeholders[index] = s.db.Placeholder(len(pageArgs) + 1)
			pageArgs = append(pageArgs, userID)
		}
		pageQuery := fmt.Sprintf(
			"SELECT %s FROM users u WHERE %s AND u.id IN (%s)",
			selectCols, whereClause, strings.Join(placeholders, ","),
		)
		pageRows, queryErr := queryUserManagementTxMaps(ctx, tx, pageQuery, pageArgs...)
		if queryErr != nil {
			return nil, 0, 0, fmt.Errorf("%w: fetch activity-filtered user page: %v", ErrActivityLogUnavailable, queryErr)
		}
		rowsByID := make(map[int64]map[string]interface{}, len(pageRows))
		for _, row := range pageRows {
			userID := toInt64(row["id"])
			if userID < 1 || rowsByID[userID] != nil {
				return nil, 0, 0, fmt.Errorf("%w: activity-filtered user page identity is inconsistent", ErrActivityLogUnavailable)
			}
			rowsByID[userID] = row
		}
		for _, userID := range pageIDs {
			row := rowsByID[userID]
			if row == nil {
				return nil, 0, 0, fmt.Errorf("%w: activity-filtered user page changed during the query", ErrActivityLogUnavailable)
			}
			snapshot := pageSnapshots[userID]
			requestCountNow, snapshotErr := canonicalSnapshotDBValue(row["request_count"])
			if snapshotErr != nil || requestCountNow != snapshot.RequestCount {
				return nil, 0, 0, fmt.Errorf("%w: activity-filtered request_count changed during the query", ErrActivityLogUnavailable)
			}
			sortValueNow, snapshotErr := canonicalSnapshotDBValue(row[orderBy])
			if snapshotErr != nil || sortValueNow != snapshot.SortValue {
				return nil, 0, 0, fmt.Errorf("%w: activity-filtered sort key changed during the query", ErrActivityLogUnavailable)
			}
			if !userMatchesActivityFilter(activity, toInt64(row["request_count"]), lastRequestTimes[userID], asOf) {
				return nil, 0, 0, fmt.Errorf("%w: activity-filtered page eligibility changed during the query", ErrActivityLogUnavailable)
			}
			orderedRows = append(orderedRows, row)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: commit activity-filtered user snapshot: %v", ErrActivityLogUnavailable, err)
	}
	return orderedRows, total, unknownCount, nil
}

func (s *UserManagementService) lastBillableRequestTimes(userIDs []int64, asOf int64) (map[int64]int64, error) {
	result := make(map[int64]int64)
	if len(userIDs) == 0 {
		return result, nil
	}
	if err := s.ensureActivityLogSourceReady(); err != nil {
		return nil, err
	}
	const batchSize = 500
	deadline := time.Now().Add(activityLogQueryTimeout)
	for start := 0; start < len(userIDs); start += batchSize {
		end := start + batchSize
		if end > len(userIDs) {
			end = len(userIDs)
		}
		batch := userIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: last-request lookup timed out", ErrActivityLogUnavailable)
		}
		query := s.logDB.RebindQuery(fmt.Sprintf(
			"SELECT user_id, MAX(created_at) AS last_request_time FROM logs WHERE type IN (2,5) AND created_at <= ? AND user_id IN (%s) GROUP BY user_id",
			strings.Join(placeholders, ",")))
		boundedArgs := append([]interface{}{asOf}, args...)
		rows, err := s.logDB.QueryWithTimeout(remaining, query, boundedArgs...)
		if err != nil {
			return nil, fmt.Errorf("%w: query last request times: %v", ErrActivityLogUnavailable, err)
		}
		for _, row := range rows {
			id := toInt64(row["user_id"])
			if id > 0 {
				result[id] = toInt64(row["last_request_time"])
			}
		}
	}
	return result, nil
}

// GetUsers returns paginated user list
func (s *UserManagementService) GetUsers(params ListUsersParams) (map[string]interface{}, error) {
	asOf := userActivityNow().Unix()
	if params.Page < 1 {
		params.Page = 1
	}
	if params.PageSize < 1 {
		params.PageSize = 20
	} else if params.PageSize > userListMaxPageSize {
		params.PageSize = userListMaxPageSize
	}
	activityFilter, err := normalizeActivityFilter(params.ActivityFilter)
	if err != nil {
		return nil, err
	}
	groupFilter, err := normalizeGroupFilter(params.GroupFilter)
	if err != nil {
		return nil, err
	}
	sourceFilter, err := normalizeSourceFilter(params.SourceFilter)
	if err != nil {
		return nil, err
	}
	params.Search = strings.TrimSpace(params.Search)
	// ORDER BY identifiers cannot be parameterized. Map all external values to
	// fixed literals before composing the query so no request data reaches SQL.
	orderBy := userListOrderColumn(params.OrderBy)
	orderDir := userListOrderDirection(params.OrderDir)

	groupCol := "`group`"
	if s.db.IsPG {
		groupCol = `"group"`
	}

	// Detect which OAuth columns exist in the database. Capability errors are
	// unavailable, never an instruction to silently drop a requested filter.
	oauthCols, err := s.probeAvailableOAuthColumns()
	if err != nil {
		return nil, err
	}
	oauthColSet := make(map[string]bool)
	for _, col := range oauthCols {
		oauthColSet[col] = true
	}
	if sourceFilter != "" && sourceFilter != "password" {
		supported := false
		for _, item := range userSourcePriority {
			if item.Source == sourceFilter && oauthColSet[item.Column] {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceFilter, sourceFilter)
		}
	}

	offset := (params.Page - 1) * params.PageSize
	where := []string{"u.deleted_at IS NULL"}
	args := []interface{}{}
	argIdx := 1

	if params.Search != "" {
		// Build search fields: always include username, display_name, email, aff_code
		// Conditionally include linux_do_id if it exists
		if s.db.IsPG {
			searchFields := []string{
				fmt.Sprintf("u.username ILIKE $%d", argIdx),
				fmt.Sprintf("COALESCE(u.display_name,'') ILIKE $%d", argIdx+1),
				fmt.Sprintf("COALESCE(u.email,'') ILIKE $%d", argIdx+2),
			}
			searchPattern := "%" + params.Search + "%"
			args = append(args, searchPattern, searchPattern, searchPattern)
			nextIdx := argIdx + 3

			if oauthColSet["linux_do_id"] {
				searchFields = append(searchFields, fmt.Sprintf("COALESCE(u.linux_do_id,'') ILIKE $%d", nextIdx))
				args = append(args, searchPattern)
				nextIdx++
			}
			searchFields = append(searchFields, fmt.Sprintf("COALESCE(u.aff_code,'') ILIKE $%d", nextIdx))
			args = append(args, searchPattern)
			nextIdx++

			where = append(where, "("+strings.Join(searchFields, " OR ")+")")
			argIdx = nextIdx
		} else {
			searchFields := []string{
				"u.username LIKE ?",
				"COALESCE(u.display_name,'') LIKE ?",
				"COALESCE(u.email,'') LIKE ?",
			}
			searchPattern := "%" + params.Search + "%"
			args = append(args, searchPattern, searchPattern, searchPattern)

			if oauthColSet["linux_do_id"] {
				searchFields = append(searchFields, "COALESCE(u.linux_do_id,'') LIKE ?")
				args = append(args, searchPattern)
			}
			searchFields = append(searchFields, "COALESCE(u.aff_code,'') LIKE ?")
			args = append(args, searchPattern)

			where = append(where, "("+strings.Join(searchFields, " OR ")+")")
		}
	}
	if groupFilter != "" {
		groupExpression := fmt.Sprintf("COALESCE(NULLIF(u.%s, ''), 'default')", groupCol)
		if s.db.IsPG {
			where = append(where, fmt.Sprintf("%s = $%d", groupExpression, argIdx))
			argIdx++
		} else {
			where = append(where, groupExpression+" = ?")
		}
		args = append(args, groupFilter)
	}
	var activityLastRequestTimes map[int64]int64
	if activityFilter != "" && activityFilter != ActivityNever {
		activityLastRequestTimes, err = s.activityEvidenceSnapshot(asOf)
		if err != nil {
			return nil, err
		}
	}
	if activityFilter == ActivityNever {
		where = append(where, "u.request_count = 0")
	} else if activityFilter != "" {
		where = append(where, "u.request_count > 0")
	}

	// Source filter — only apply if the relevant column exists
	if sourceFilter != "" {
		sourceExpression := userSourceSQLExpression("u", oauthColSet)
		if s.db.IsPG {
			where = append(where, fmt.Sprintf("(%s) = $%d", sourceExpression, argIdx))
			argIdx++
		} else {
			where = append(where, "("+sourceExpression+") = ?")
		}
		args = append(args, sourceFilter)
	}

	whereClause := strings.Join(where, " AND ")

	// Build SELECT columns dynamically based on available OAuth columns
	// NOTE: users table does NOT have created_at — do not select it
	selectCols := fmt.Sprintf("u.id, u.username, u.display_name, u.email, u.role, u.status, u.quota, u.used_quota, u.request_count, u.%s, u.aff_code, u.remark", groupCol)
	for _, col := range oauthCols {
		selectCols += fmt.Sprintf(", u.%s", col)
	}
	orderClause := fmt.Sprintf("u.%s %s", orderBy, orderDir)
	if orderBy != "id" {
		orderClause += fmt.Sprintf(", u.id %s", orderDir)
	}

	var total int64
	var unknownActivityCount int64
	var rows []map[string]interface{}
	if activityFilter != "" && activityFilter != ActivityNever {
		rows, total, unknownActivityCount, err = s.activityFilteredUserPage(
			selectCols, whereClause, orderBy, orderDir, args,
			activityFilter, activityLastRequestTimes, asOf,
			params.Page, params.PageSize,
		)
		if err != nil {
			s.invalidateOAuthColumnCache()
			return nil, err
		}
	} else {
		countQuery := fmt.Sprintf("SELECT COUNT(*) as count FROM users u WHERE %s", whereClause)
		if !s.db.IsPG {
			countQuery = s.db.RebindQuery(countQuery)
		}
		countRow, countErr := s.db.QueryOne(countQuery, args...)
		if countErr != nil {
			s.invalidateOAuthColumnCache()
			return nil, countErr
		}
		total = toInt64(countRow["count"])

		selectArgs := append([]interface{}{}, args...)
		var selectQuery string
		if s.db.IsPG {
			selectQuery = fmt.Sprintf(
				"SELECT %s FROM users u WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d",
				selectCols, whereClause, orderClause, argIdx, argIdx+1)
			selectArgs = append(selectArgs, params.PageSize, offset)
		} else {
			selectQuery = fmt.Sprintf(
				"SELECT %s FROM users u WHERE %s ORDER BY %s LIMIT ? OFFSET ?",
				selectCols, whereClause, orderClause)
			selectArgs = append(selectArgs, params.PageSize, offset)
			selectQuery = s.db.RebindQuery(selectQuery)
		}

		rows, err = s.db.Query(selectQuery, selectArgs...)
		if err != nil {
			s.invalidateOAuthColumnCache()
			logger.L.Error(fmt.Sprintf("GetUsers 查询失败: %v, SQL: %s, args: %v", err, selectQuery, selectArgs))
			return nil, err
		}
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}

	requestedIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if toInt64(row["request_count"]) > 0 {
			requestedIDs = append(requestedIDs, toInt64(row["id"]))
		}
	}
	lastRequestTimes := activityLastRequestTimes
	if lastRequestTimes == nil {
		lastRequestTimes, err = s.lastBillableRequestTimes(requestedIDs, asOf)
		if err != nil {
			return nil, err
		}
	}
	// Enrich rows with the same activity/source truth used by filters.
	for _, row := range rows {
		reqCount := toInt64(row["request_count"])
		lastRequest := lastRequestTimes[toInt64(row["id"])]
		activityLevel := classifyUserActivity(reqCount, lastRequest, asOf)
		row["activity_level"] = activityLevel
		if (activityFilter == "" || activityFilter == ActivityNever) && activityLevel == ActivityUnknown {
			unknownActivityCount++
		}
		if lastRequest > 0 {
			row["last_request_time"] = lastRequest
		} else {
			row["last_request_time"] = nil
		}
		groupName := strings.TrimSpace(toString(row["group"]))
		if groupName == "" {
			groupName = "default"
		}
		row["group"] = groupName

		// Preserve linux_do_id for frontend display
		linuxDoID := ""
		if oauthColSet["linux_do_id"] {
			linuxDoID = toString(row["linux_do_id"])
		}
		row["linux_do_id"] = linuxDoID

		row["source"] = userSourceFromRow(row, oauthColSet)

		// Clean up internal OAuth fields (except linux_do_id which is kept)
		for _, col := range oauthCols {
			if col != "linux_do_id" {
				delete(row, col)
			}
		}
	}

	totalPages := int((total + int64(params.PageSize) - 1) / int64(params.PageSize))
	sourceState := "fresh"
	if unknownActivityCount > 0 {
		sourceState = "partial"
	}

	return map[string]interface{}{
		"items":        rows,
		"total":        total,
		"page":         params.Page,
		"page_size":    params.PageSize,
		"total_pages":  totalPages,
		"as_of":        asOf,
		"source_state": sourceState,
	}, nil
}

// GetBannedUsers returns banned users list
func (s *UserManagementService) GetBannedUsers(page, pageSize int, search string) (map[string]interface{}, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}

	offset := (page - 1) * pageSize
	where := "u.status = 2 AND u.deleted_at IS NULL"
	args := []interface{}{}

	if search != "" {
		if s.db.IsPG {
			where += " AND u.username ILIKE $1"
		} else {
			where += " AND u.username LIKE ?"
		}
		args = append(args, "%"+search+"%")
	}

	// Count
	countQuery := s.db.RebindQuery(fmt.Sprintf("SELECT COUNT(*) as count FROM users u WHERE %s", where))
	countRow, _ := s.db.QueryOne(countQuery, args...)
	total := int64(0)
	if countRow != nil {
		total = toInt64(countRow["count"])
	}

	// Query
	query := fmt.Sprintf(
		"SELECT u.id, u.username, u.display_name, u.email, u.status, u.role, "+
			"u.quota, u.used_quota, u.request_count "+
			"FROM users u WHERE %s ORDER BY u.id DESC LIMIT %d OFFSET %d",
		where, pageSize, offset)
	if !s.db.IsPG {
		query = s.db.RebindQuery(query)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}

	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))

	return map[string]interface{}{
		"items":       rows,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
	}, nil
}

// DeleteUser soft-deletes a user
func (s *UserManagementService) DeleteUser(userID int64, hardDelete bool) (int64, error) {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return 0, err
	}
	if hardDelete {
		if err := ensureUnsafeHardDeleteAllowed(); err != nil {
			return 0, err
		}
		// Hard delete the user and tokens atomically. A token-delete failure must
		// never leave the user row deleted (or vice versa).
		var affected int64
		err := s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
			if err := s.ensureNonRootUserMutation(ctx, tx, userID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, tx.Rebind(`DELETE FROM tokens WHERE user_id IN (
				SELECT id FROM users WHERE id = ? AND role != 100)`), userID); err != nil {
				return fmt.Errorf("delete user tokens: %w", err)
			}
			result, err := tx.ExecContext(ctx, tx.Rebind("DELETE FROM users WHERE id = ? AND role != 100"), userID)
			if err != nil {
				return fmt.Errorf("delete user: %w", err)
			}
			affected, err = result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read deleted user count: %w", err)
			}
			if affected == 0 {
				return fmt.Errorf("delete user blocked: user changed or became protected")
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		logger.L.Business(fmt.Sprintf("用户 %d 已彻底删除", userID))
		return affected, nil
	}

	// Soft delete with the same root-user guard as hard deletion.
	now := time.Now()
	var affected int64
	err := s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		if err := s.ensureNonRootUserMutation(ctx, tx, userID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, tx.Rebind(
			"UPDATE users SET deleted_at = ? WHERE id = ? AND role != 100 AND deleted_at IS NULL"), now, userID)
		if err != nil {
			return fmt.Errorf("soft-delete user: %w", err)
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read soft-deleted user count: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("soft-delete blocked: user changed, is already deleted, or became protected")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		logger.L.Business(fmt.Sprintf("用户 %d 已注销", userID))
	}
	return affected, nil
}

// BanUser sets user status to banned (2)
func (s *UserManagementService) BanUser(userID int64, disableTokens bool) error {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return err
	}
	now := time.Now().Unix()
	err := s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		if err := s.ensureNonRootUserMutation(ctx, tx, userID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, tx.Rebind("UPDATE users SET status = 2 WHERE id = ? AND role != 100"), userID)
		if err != nil {
			return fmt.Errorf("ban user: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read banned user count: %w", err)
		}
		if err := verifyUserStatusMutation(ctx, tx, userID, 2, affected, "ban"); err != nil {
			return err
		}
		if !disableTokens {
			return nil
		}

		// Preserve expired/exhausted/otherwise non-active token states. Only
		// active tokens are disabled as part of the ban.
		query := fmt.Sprintf(
			"UPDATE tokens SET status = 2 WHERE user_id = ? AND deleted_at IS NULL AND %s",
			tokenEffectiveActiveCondition("tokens", now))
		if _, err := tx.ExecContext(ctx, tx.Rebind(query), userID); err != nil {
			return fmt.Errorf("disable user tokens: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.L.Security(fmt.Sprintf("用户 %d 已封禁", userID))
	return nil
}

// UnbanUser sets user status to active (1). Legacy callers requesting bulk token
// restoration are rejected before any database mutation.
func (s *UserManagementService) UnbanUser(userID int64, enableTokens bool) error {
	if enableTokens {
		return ErrBulkTokenReactivationDisabled
	}
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return err
	}
	err := s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		if err := s.ensureNonRootUserMutation(ctx, tx, userID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, tx.Rebind("UPDATE users SET status = 1 WHERE id = ? AND role != 100"), userID)
		if err != nil {
			return fmt.Errorf("unban user: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read unbanned user count: %w", err)
		}
		if err := verifyUserStatusMutation(ctx, tx, userID, 1, affected, "unban"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.L.Security(fmt.Sprintf("用户 %d 已解封", userID))
	return nil
}

// DisableToken disables a single token
func (s *UserManagementService) DisableToken(tokenID int64) error {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return err
	}
	err := s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		if err := s.ensureNonRootTokenMutation(ctx, tx, tokenID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, tx.Rebind(`UPDATE tokens SET status = 2
			WHERE id = ? AND user_id IN (SELECT id FROM users WHERE role != 100)`), tokenID)
		if err != nil {
			return fmt.Errorf("disable token: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.L.Security(fmt.Sprintf("Token %d 已禁用", tokenID))
	return nil
}

// GetSoftDeletedCount returns count of soft-deleted users
func (s *UserManagementService) GetSoftDeletedCount() (int64, error) {
	row, err := s.db.QueryOne("SELECT COUNT(*) as count FROM users WHERE deleted_at IS NOT NULL AND role < 10")
	if err != nil {
		return 0, err
	}
	return toInt64(row["count"]), nil
}

// PurgeSoftDeleted permanently deletes only the exact soft-deleted user IDs
// captured by a recent, single-use preview snapshot.
func (s *UserManagementService) PurgeSoftDeleted(snapshotID string) (int64, error) {
	if err := ensureNewAPIDirectMutationSafe(); err != nil {
		return 0, err
	}
	if err := ensureUnsafeHardDeleteAllowed(); err != nil {
		return 0, err
	}
	snapshot, err := consumePurgeSoftDeletedSnapshot(snapshotID)
	if err != nil {
		return 0, err
	}
	ids := append([]int64(nil), snapshot.IDs...)
	expectedDeletedAt := make(map[int64]string, len(ids))
	for i, id := range ids {
		if id <= 0 {
			return 0, fmt.Errorf("%w: invalid purge snapshot user ID", ErrInvalidOrExpiredSnapshot)
		}
		if _, duplicate := expectedDeletedAt[id]; duplicate {
			return 0, fmt.Errorf("%w: duplicate purge snapshot user ID", ErrInvalidOrExpiredSnapshot)
		}
		expectedDeletedAt[id] = snapshot.DeletedAt[i]
	}

	var affected int64
	err = s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")
		lockQuery := fmt.Sprintf(
			"SELECT id, deleted_at FROM users WHERE id IN (%s) AND deleted_at IS NOT NULL AND role < 10 ORDER BY id%s",
			inClause, s.mutationRowLockClause())
		rows, err := tx.QueryxContext(ctx, tx.Rebind(lockQuery), args...)
		if err != nil {
			return fmt.Errorf("lock purge candidates: %w", err)
		}
		lockedCount := 0
		for rows.Next() {
			row := make(map[string]interface{})
			if err := rows.MapScan(row); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan locked purge candidate: %w", err)
			}
			id := toInt64(row["id"])
			deletedAt, err := canonicalSnapshotDBValue(row["deleted_at"])
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("read locked purge candidate %d: %w", id, err)
			}
			expected, exists := expectedDeletedAt[id]
			if !exists || deletedAt != expected {
				_ = rows.Close()
				return fmt.Errorf("%w: soft-deleted user %d changed after preview", ErrSnapshotInvalidated, id)
			}
			lockedCount++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read locked purge candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close locked purge candidates: %w", err)
		}
		if lockedCount != len(ids) {
			return fmt.Errorf("%w: purge expected %d eligible users, found %d", ErrSnapshotInvalidated, len(ids), lockedCount)
		}

		const purgeBatchSize = 500
		for start := 0; start < len(ids); start += purgeBatchSize {
			end := start + purgeBatchSize
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[start:end]
			placeholders := make([]string, len(batch))
			args := make([]interface{}, len(batch))
			for i, id := range batch {
				placeholders[i] = "?"
				args[i] = id
			}
			inClause := strings.Join(placeholders, ",")
			deleteTokens := fmt.Sprintf(`DELETE FROM tokens WHERE user_id IN (
				SELECT id FROM users WHERE id IN (%s) AND deleted_at IS NOT NULL AND role < 10)`, inClause)
			if _, err := tx.ExecContext(ctx, tx.Rebind(deleteTokens), args...); err != nil {
				return fmt.Errorf("purge soft-deleted user tokens: %w", err)
			}
			deleteUsers := fmt.Sprintf(
				"DELETE FROM users WHERE id IN (%s) AND deleted_at IS NOT NULL AND role < 10", inClause)
			result, err := tx.ExecContext(ctx, tx.Rebind(deleteUsers), args...)
			if err != nil {
				return fmt.Errorf("purge soft-deleted users: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read purged user count: %w", err)
			}
			affected += count
		}
		if affected != int64(len(ids)) {
			return fmt.Errorf("%w: purge candidates changed concurrently; expected %d users, deleted %d; no changes committed", ErrSnapshotInvalidated, len(ids), affected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	logger.L.Business(fmt.Sprintf("已清理 %d 个软删除用户", affected))
	return affected, nil
}

// PreviewSoftDeletedUsers returns count and sample usernames for the purge dialog.
func (s *UserManagementService) PreviewSoftDeletedUsers() (map[string]interface{}, error) {
	rows, err := s.db.Query(`SELECT id, username, deleted_at FROM users
		WHERE deleted_at IS NOT NULL AND role < 10
		ORDER BY deleted_at DESC, id DESC LIMIT 1001`)
	if err != nil {
		return nil, err
	}
	if len(rows) > maxBatchDeleteUsers {
		return nil, fmt.Errorf("purge blocked: more than %d eligible users; purge in smaller reviewed batches", maxBatchDeleteUsers)
	}
	ids := make([]int64, 0, len(rows))
	deletedAtValues := make([]string, 0, len(rows))
	users := make([]string, 0, 20)
	for _, row := range rows {
		id := toInt64(row["id"])
		if id <= 0 {
			continue
		}
		deletedAt, err := canonicalSnapshotDBValue(row["deleted_at"])
		if err != nil {
			return nil, fmt.Errorf("snapshot soft-deleted user %d: %w", id, err)
		}
		ids = append(ids, id)
		deletedAtValues = append(deletedAtValues, deletedAt)
		if len(users) < 20 {
			users = append(users, toString(row["username"]))
		}
	}
	snapshotID := ""
	if len(ids) > 0 {
		snapshotID, err = storePurgeSoftDeletedSnapshot(purgeSoftDeletedSnapshot{
			IDs:       ids,
			DeletedAt: deletedAtValues,
			CreatedAt: time.Now().Unix(),
		})
		if err != nil {
			return nil, err
		}
	}
	count := int64(len(ids))

	return map[string]interface{}{
		"dry_run":        true,
		"count":          count,
		"affected":       count,
		"affected_count": count,
		"users":          users,
		"snapshot_id":    snapshotID,
		"snapshot_ttl":   int64(batchDeleteSnapshotTTL / time.Second),
	}, nil
}

// BatchDeleteInactiveUsers deletes inactive users
func (s *UserManagementService) BatchDeleteInactiveUsers(activityLevel string, dryRun, hardDelete bool, snapshotID string) (map[string]interface{}, error) {
	now := time.Now()
	nowUnix := now.Unix()

	// Every activity level, including "never", is verified against the log DB.
	// This check also rejects a configured LOG_SQL_DSN fallback and stale logs.
	if err := s.ensureDestructiveLogSourceReady(now); err != nil {
		return nil, err
	}

	threshold, err := batchDeleteActivityCutoff(activityLevel, nowUnix)
	if err != nil {
		return nil, err
	}

	if dryRun {
		toDelete, err := s.previewBatchDeleteCandidates(activityLevel, nowUnix)
		if err != nil {
			return nil, err
		}
		if len(toDelete) > maxBatchDeleteUsers {
			return nil, fmt.Errorf("batch delete blocked: %d users exceed the per-operation limit of %d", len(toDelete), maxBatchDeleteUsers)
		}

		preview := make([]string, 0, 20)
		ids := make([]int64, len(toDelete))
		requestCounts := make([]int64, len(toDelete))
		lastRequestTimes := make([]int64, len(toDelete))
		billableLogCounts := make([]int64, len(toDelete))
		for i, u := range toDelete {
			ids[i] = u.id
			requestCounts[i] = u.requestCount
			lastRequestTimes[i] = u.lastRequest
			billableLogCounts[i] = u.billableLogCount
			if i >= 20 {
				continue
			}
			preview = append(preview, u.username)
		}
		storedSnapshotID := ""
		if len(ids) > 0 {
			storedSnapshotID, err = storeBatchDeleteSnapshot(batchDeleteSnapshot{
				IDs:               ids,
				RequestCounts:     requestCounts,
				LastRequestTimes:  lastRequestTimes,
				BillableLogCounts: billableLogCounts,
				ActivityLevel:     activityLevel,
				HardDelete:        hardDelete,
				ActivityAsOf:      nowUnix,
				ActivityCutoff:    threshold,
				CreatedAt:         nowUnix,
			})
			if err != nil {
				return nil, err
			}
		}
		return map[string]interface{}{
			"dry_run":        true,
			"count":          int64(len(ids)),
			"affected_count": int64(len(ids)),
			"activity_level": activityLevel,
			"users":          preview,
			"snapshot_id":    storedSnapshotID,
			"snapshot_ttl":   int64(batchDeleteSnapshotTTL / time.Second),
		}, nil
	}
	if s.logDB != s.db {
		return nil, fmt.Errorf("%w: billable-log inserts cannot be atomically fenced with the user deletion transaction; use the NewAPI admin API or co-locate logs in the main database", ErrSeparateLogDBBatchDeleteBlocked)
	}
	if err := ensureUnsafeBatchDeleteAllowed(); err != nil {
		return nil, err
	}
	if hardDelete {
		if err := ensureUnsafeHardDeleteAllowed(); err != nil {
			return nil, err
		}
	}

	snapshot, err := consumeBatchDeleteSnapshot(snapshotID, activityLevel, hardDelete)
	if err != nil {
		return nil, err
	}
	ids := append([]int64(nil), snapshot.IDs...)
	requestCounts := append([]int64(nil), snapshot.RequestCounts...)
	if len(ids) == 0 {
		return map[string]interface{}{
			"dry_run":        false,
			"count":          int64(0),
			"affected_count": int64(0),
			"activity_level": activityLevel,
			"hard_delete":    hardDelete,
		}, nil
	}
	if len(ids) > maxBatchDeleteUsers {
		return nil, fmt.Errorf("batch-delete snapshot exceeds the per-operation limit")
	}
	for _, requestCount := range requestCounts {
		if (activityLevel == ActivityNever && requestCount != 0) ||
			(activityLevel != ActivityNever && requestCount <= 0) {
			return nil, fmt.Errorf("batch-delete snapshot contains an invalid activity counter")
		}
	}

	// Bind execution to the preview's exact MAX/count billable-log evidence and
	// re-read all currently visible evidence before entering the write
	// transaction. Missing-history users cannot be smuggled in as very inactive,
	// and any evidence drift invalidates the whole snapshot.
	currentEvidence, err := s.candidateBillableEvidenceWithTimeout(ids, 0, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("无法重新验证预览用户的活跃度，已中止删除: %w", err)
	}
	if err := validateBatchDeleteEvidenceUnchanged(*snapshot, currentEvidence, nowUnix); err != nil {
		return nil, err
	}

	// Lock and compare the exact previewed request counters before the final log
	// recheck. A concurrent request-count change, role promotion, soft deletion,
	// or newly visible billable log invalidates the whole operation.
	const batchSize = 400
	actualAffected := int64(0)
	err = s.withMutationTransaction(func(ctx context.Context, tx *sqlx.Tx) error {
		lockedCount := 0
		for start := 0; start < len(ids); start += batchSize {
			end := start + batchSize
			if end > len(ids) {
				end = len(ids)
			}
			eligibility, args, err := batchDeleteEligibilityClause(ids[start:end], requestCounts[start:end])
			if err != nil {
				return err
			}
			var lockedIDs []int64
			lockQuery := fmt.Sprintf(
				"SELECT id FROM users WHERE (%s) AND deleted_at IS NULL AND role < 10%s",
				eligibility, s.mutationRowLockClause())
			if err := tx.SelectContext(ctx, &lockedIDs, tx.Rebind(lockQuery), args...); err != nil {
				return fmt.Errorf("lock batch-delete candidates: %w", err)
			}
			lockedCount += len(lockedIDs)
		}
		if lockedCount != len(ids) {
			return fmt.Errorf("%w: expected %d eligible users, found %d", ErrSnapshotInvalidated, len(ids), lockedCount)
		}

		lockedEvidence, err := s.candidateBillableEvidenceLocked(ctx, tx, ids, 0)
		if err != nil {
			return fmt.Errorf("无法在锁定候选后重新验证用户活跃度，已中止删除: %w", err)
		}
		if err := validateBatchDeleteEvidenceUnchanged(*snapshot, lockedEvidence, nowUnix); err != nil {
			return err
		}

		for start := 0; start < len(ids); start += batchSize {
			end := start + batchSize
			if end > len(ids) {
				end = len(ids)
			}
			eligibility, args, err := batchDeleteEligibilityClause(ids[start:end], requestCounts[start:end])
			if err != nil {
				return err
			}
			candidateGuard := fmt.Sprintf("(%s) AND deleted_at IS NULL AND role < 10", eligibility)

			if hardDelete {
				deleteTokens := fmt.Sprintf(`DELETE FROM tokens WHERE user_id IN (
					SELECT id FROM users WHERE %s)`, candidateGuard)
				if _, err := tx.ExecContext(ctx, tx.Rebind(deleteTokens), args...); err != nil {
					return fmt.Errorf("batch delete user tokens: %w", err)
				}
				deleteUsers := fmt.Sprintf("DELETE FROM users WHERE %s", candidateGuard)
				result, err := tx.ExecContext(ctx, tx.Rebind(deleteUsers), args...)
				if err != nil {
					return fmt.Errorf("batch delete users: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("read batch deleted user count: %w", err)
				}
				actualAffected += count
			} else {
				softArgs := append([]interface{}{now}, args...)
				q := fmt.Sprintf("UPDATE users SET deleted_at = ? WHERE %s", candidateGuard)
				result, err := tx.ExecContext(ctx, tx.Rebind(q), softArgs...)
				if err != nil {
					return fmt.Errorf("batch soft-delete users: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("read batch soft-deleted user count: %w", err)
				}
				actualAffected += count
			}
		}
		if actualAffected != int64(len(ids)) {
			return fmt.Errorf("%w: expected %d eligible users, found %d; no changes committed", ErrSnapshotInvalidated, len(ids), actualAffected)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.L.Business(fmt.Sprintf("批量删除 %s 用户: %d 个", activityLevel, actualAffected))

	return map[string]interface{}{
		"dry_run":        false,
		"count":          actualAffected,
		"affected_count": actualAffected,
		"activity_level": activityLevel,
		"hard_delete":    hardDelete,
	}, nil
}

func (s *UserManagementService) previewUsers(query string) ([]string, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}

	users := make([]string, 0, len(rows))
	for _, row := range rows {
		users = append(users, userPreviewName(row))
	}
	return users, nil
}

func userPreviewName(row map[string]interface{}) string {
	username := strings.TrimSpace(toString(row["username"]))
	if username != "" {
		return username
	}
	if id := toInt64(row["id"]); id > 0 {
		return fmt.Sprintf("用户#%d", id)
	}
	return "未知用户"
}

// toInt64 safely converts interface{} to int64
func toInt64(v interface{}) int64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int64:
		return val
	case int:
		return int64(val)
	case int32:
		return int64(val)
	case float64:
		return int64(val)
	case float32:
		return int64(val)
	case string:
		var n int64
		fmt.Sscanf(val, "%d", &n)
		return n
	case []byte:
		var n int64
		fmt.Sscanf(string(val), "%d", &n)
		return n
	default:
		return 0
	}
}

// toString safely converts interface{} to string
func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

type InviterInfo struct {
	UserID      int64   `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AffCode     *string `json:"aff_code,omitempty"`
	AffCount    int64   `json:"aff_count"`
	AffQuota    *int64  `json:"aff_quota,omitempty"`
	AffHistory  *int64  `json:"aff_history,omitempty"`
}

type InvitedUserItem struct {
	UserID       int64   `json:"user_id"`
	Username     string  `json:"username"`
	DisplayName  *string `json:"display_name,omitempty"`
	Email        *string `json:"email,omitempty"`
	Status       int64   `json:"status"`
	Quota        *int64  `json:"quota,omitempty"`
	UsedQuota    *int64  `json:"used_quota,omitempty"`
	RequestCount int64   `json:"request_count"`
	Group        string  `json:"group"`
	Role         *int64  `json:"role,omitempty"`
}

type InvitedUserStats struct {
	TotalInvited   int64  `json:"total_invited"`
	RequestedCount int64  `json:"requested_count"`
	BannedCount    int64  `json:"banned_count"`
	TotalUsedQuota *int64 `json:"total_used_quota,omitempty"`
	TotalRequests  int64  `json:"total_requests"`
}

type InvitedUsersResult struct {
	Inviter          InviterInfo       `json:"inviter"`
	Items            []InvitedUserItem `json:"items"`
	Total            int64             `json:"total"`
	Page             int               `json:"page"`
	PageSize         int               `json:"page_size"`
	TotalPages       int               `json:"total_pages"`
	AsOf             int64             `json:"as_of"`
	QueryFingerprint string            `json:"query_fingerprint"`
	Stats            InvitedUserStats  `json:"stats"`
}

type InvitedUsersSnapshotIdentity struct {
	AsOf             int64
	QueryFingerprint string
}

func validInvitedUsersFingerprint(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

type invitedUsersFingerprintEncoder struct {
	digest hash.Hash
	buffer []byte
}

func newInvitedUsersFingerprintEncoder(asOf int64, inviter InviterInfo, pageSize int) *invitedUsersFingerprintEncoder {
	encoder := &invitedUsersFingerprintEncoder{digest: sha256.New(), buffer: make([]byte, 0, 256*1024)}
	encoder.writeString("invited-users-v3")
	encoder.writeInt64(asOf)
	encoder.writeInt64(int64(pageSize))
	encoder.writeString("user_id_desc")
	encoder.writeInt64(inviter.UserID)
	encoder.writeString(inviter.Username)
	encoder.writeOptionalString(inviter.DisplayName)
	encoder.writeOptionalString(inviter.AffCode)
	encoder.writeInt64(inviter.AffCount)
	encoder.writeOptionalInt64(inviter.AffQuota)
	encoder.writeOptionalInt64(inviter.AffHistory)
	return encoder
}

func (encoder *invitedUsersFingerprintEncoder) writeInt64(value int64) {
	encoder.buffer = binary.BigEndian.AppendUint64(encoder.buffer, uint64(value))
}

func (encoder *invitedUsersFingerprintEncoder) writeString(value string) {
	encoder.writeInt64(int64(len(value)))
	encoder.buffer = append(encoder.buffer, value...)
}

func (encoder *invitedUsersFingerprintEncoder) writeOptionalString(value *string) {
	if value == nil {
		encoder.buffer = append(encoder.buffer, 0)
		return
	}
	encoder.buffer = append(encoder.buffer, 1)
	encoder.writeString(*value)
}

func (encoder *invitedUsersFingerprintEncoder) writeOptionalInt64(value *int64) {
	if value == nil {
		encoder.buffer = append(encoder.buffer, 0)
		return
	}
	encoder.buffer = append(encoder.buffer, 1)
	encoder.writeInt64(*value)
}

func (encoder *invitedUsersFingerprintEncoder) writeNullableString(value sql.NullString) {
	if !value.Valid {
		encoder.buffer = append(encoder.buffer, 0)
		return
	}
	encoder.buffer = append(encoder.buffer, 1)
	encoder.writeString(value.String)
}

func (encoder *invitedUsersFingerprintEncoder) writeInvitedUser(row invitedEvidenceDBRow) {
	encoder.buffer = append(encoder.buffer, 1)
	encoder.writeInt64(row.UserID)
	encoder.writeString(row.Username)
	encoder.writeNullableString(row.DisplayName)
	encoder.writeInt64(row.Status)
	encoder.writeInt64(row.UsedQuota)
	encoder.writeInt64(row.RequestCount)
	encoder.writeString(row.Group)
	if len(encoder.buffer) >= 256*1024 {
		encoder.flush()
	}
}

func (encoder *invitedUsersFingerprintEncoder) flush() {
	if len(encoder.buffer) == 0 {
		return
	}
	_, _ = encoder.digest.Write(encoder.buffer)
	encoder.buffer = encoder.buffer[:0]
}

func (encoder *invitedUsersFingerprintEncoder) finish(total int64, stats InvitedUserStats) string {
	encoder.buffer = append(encoder.buffer, 0)
	encoder.writeInt64(total)
	encoder.writeInt64(stats.TotalInvited)
	encoder.writeInt64(stats.RequestedCount)
	encoder.writeInt64(stats.BannedCount)
	encoder.writeOptionalInt64(stats.TotalUsedQuota)
	encoder.writeInt64(stats.TotalRequests)
	encoder.flush()
	return hex.EncodeToString(encoder.digest.Sum(nil))
}

// RedactForViewer removes fields that are unnecessary for read-only viewer
// workflows. Authorization is enforced by the handler; this method makes the
// response transformation explicit and independently testable.
func (result InvitedUsersResult) RedactForViewer() InvitedUsersResult {
	result.Inviter.AffCode = nil
	result.Inviter.AffQuota = nil
	result.Inviter.AffHistory = nil
	result.Stats.TotalUsedQuota = nil
	for i := range result.Items {
		result.Items[i].Email = nil
		result.Items[i].Quota = nil
		result.Items[i].UsedQuota = nil
		result.Items[i].Role = nil
	}
	return result
}

type inviterDBRow struct {
	UserID      int64          `db:"user_id"`
	Username    string         `db:"username"`
	DisplayName sql.NullString `db:"display_name"`
	AffCode     sql.NullString `db:"aff_code"`
	AffCount    int64          `db:"aff_count"`
	AffQuota    int64          `db:"aff_quota"`
	AffHistory  int64          `db:"aff_history"`
}

type invitedUserDBRow struct {
	UserID       int64          `db:"user_id"`
	Username     string         `db:"username"`
	DisplayName  sql.NullString `db:"display_name"`
	Email        sql.NullString `db:"email"`
	Status       int64          `db:"status"`
	Quota        int64          `db:"quota"`
	UsedQuota    int64          `db:"used_quota"`
	RequestCount int64          `db:"request_count"`
	Group        string         `db:"user_group"`
	Role         int64          `db:"role"`
}

type invitedEvidenceDBRow struct {
	UserID       int64          `db:"user_id"`
	Username     string         `db:"username"`
	DisplayName  sql.NullString `db:"display_name"`
	Status       int64          `db:"status"`
	UsedQuota    int64          `db:"used_quota"`
	RequestCount int64          `db:"request_count"`
	Group        string         `db:"user_group"`
}

type invitedStatsDBRow struct {
	TotalInvited   int64 `db:"total_invited"`
	RequestedCount int64 `db:"requested_count"`
	BannedCount    int64 `db:"banned_count"`
	TotalUsedQuota int64 `db:"total_used_quota"`
	TotalRequests  int64 `db:"total_requests"`
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func int64Pointer(value int64) *int64 {
	copy := value
	return &copy
}

// GetInvitedUsers returns one repeatable-read snapshot containing the requested
// page and evidence for the complete invited-user population. Page one may
// start a new snapshot without an identity; every other request must replay the
// original as_of and SHA-256 fingerprint. The full evidence is recomputed in
// the same read-only transaction before a later page is released.
func (s *UserManagementService) GetInvitedUsers(userID int64, page, pageSize int, expected *InvitedUsersSnapshotIdentity) (InvitedUsersResult, error) {
	if userID <= 0 || page < 1 || pageSize < 1 || pageSize > userListMaxPageSize {
		return InvitedUsersResult{}, fmt.Errorf("%w: invalid inviter or pagination parameters", ErrInvitedUsersSnapshotMismatch)
	}
	if expected == nil && page != 1 {
		return InvitedUsersResult{}, fmt.Errorf("%w: later pages require as_of and query_fingerprint", ErrInvitedUsersSnapshotMismatch)
	}

	asOf := time.Now().Unix()
	if expected != nil {
		if expected.AsOf <= 0 || !validInvitedUsersFingerprint(expected.QueryFingerprint) {
			return InvitedUsersResult{}, fmt.Errorf("%w: invalid as_of or query_fingerprint", ErrInvitedUsersSnapshotMismatch)
		}
		asOf = expected.AsOf
	}
	if asOf <= 0 {
		return InvitedUsersResult{}, errors.New("invited-user snapshot time is unavailable")
	}
	if s.db == nil || s.db.DB == nil {
		return InvitedUsersResult{}, errors.New("main database is unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.DB.BeginTxx(ctx, userManagementReadTxOptions(s.db))
	if err != nil {
		return InvitedUsersResult{}, fmt.Errorf("begin invited-user snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var inviterRow inviterDBRow
	err = tx.GetContext(ctx, &inviterRow, tx.Rebind(
		"SELECT id AS user_id, username, display_name, aff_code, aff_count, aff_quota, aff_history FROM users WHERE id = ? AND deleted_at IS NULL"), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return InvitedUsersResult{}, fmt.Errorf("%w: user_id=%d", ErrInviterNotFound, userID)
	}
	if err != nil {
		return InvitedUsersResult{}, fmt.Errorf("query inviter: %w", err)
	}

	groupCol := "`group`"
	if s.db.IsPG {
		groupCol = `"group"`
	}
	evidenceQuery := tx.Rebind(fmt.Sprintf(`
		SELECT id AS user_id, username, display_name, status,
			used_quota, request_count,
			COALESCE(NULLIF(%s, ''), 'default') AS user_group
		FROM users
		WHERE inviter_id = ? AND deleted_at IS NULL
		ORDER BY id DESC`, groupCol))
	rows, err := tx.QueryxContext(ctx, evidenceQuery, userID)
	if err != nil {
		return InvitedUsersResult{}, fmt.Errorf("query complete invited-user population: %w", err)
	}
	defer func() { _ = rows.Close() }()

	inviter := InviterInfo{
		UserID: inviterRow.UserID, Username: inviterRow.Username,
		DisplayName: nullableStringPointer(inviterRow.DisplayName), AffCode: nullableStringPointer(inviterRow.AffCode),
		AffCount: inviterRow.AffCount, AffQuota: int64Pointer(inviterRow.AffQuota), AffHistory: int64Pointer(inviterRow.AffHistory),
	}
	encoder := newInvitedUsersFingerprintEncoder(asOf, inviter, pageSize)
	stats := InvitedUserStats{}
	var totalUsedQuota int64
	rowIndex := 0
	for rows.Next() {
		var row invitedEvidenceDBRow
		if err := rows.Scan(
			&row.UserID, &row.Username, &row.DisplayName, &row.Status,
			&row.UsedQuota, &row.RequestCount, &row.Group,
		); err != nil {
			return InvitedUsersResult{}, fmt.Errorf("scan complete invited-user population: %w", err)
		}
		encoder.writeInvitedUser(row)
		if row.RequestCount > 0 {
			stats.RequestedCount++
		}
		if row.Status == 2 {
			stats.BannedCount++
		}
		totalUsedQuota += row.UsedQuota
		stats.TotalRequests += row.RequestCount
		rowIndex++
	}
	if err := rows.Err(); err != nil {
		return InvitedUsersResult{}, fmt.Errorf("iterate complete invited-user population: %w", err)
	}
	if err := rows.Close(); err != nil {
		return InvitedUsersResult{}, fmt.Errorf("close complete invited-user population: %w", err)
	}
	stats.TotalInvited = int64(rowIndex)
	stats.TotalUsedQuota = int64Pointer(totalUsedQuota)
	total := int64(rowIndex)
	fingerprint := encoder.finish(total, stats)
	if expected != nil && expected.QueryFingerprint != fingerprint {
		return InvitedUsersResult{}, fmt.Errorf("%w: invited-user population or query contract changed", ErrInvitedUsersSnapshotMismatch)
	}

	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if page > 1 && (totalPages == 0 || page > totalPages) {
		return InvitedUsersResult{}, fmt.Errorf("%w: requested page is outside the snapshot", ErrInvitedUsersSnapshotMismatch)
	}
	start := (page - 1) * pageSize
	if start > rowIndex {
		return InvitedUsersResult{}, fmt.Errorf("%w: requested page offset is outside the snapshot", ErrInvitedUsersSnapshotMismatch)
	}

	pageQuery := tx.Rebind(fmt.Sprintf(`
		SELECT id AS user_id, username, display_name, email, status,
			quota, used_quota, request_count,
			COALESCE(NULLIF(%s, ''), 'default') AS user_group, role
		FROM users
		WHERE inviter_id = ? AND deleted_at IS NULL
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, groupCol))
	var pageRows []invitedUserDBRow
	if err := tx.SelectContext(ctx, &pageRows, pageQuery, userID, pageSize, start); err != nil {
		return InvitedUsersResult{}, fmt.Errorf("query invited-user page: %w", err)
	}
	items := make([]InvitedUserItem, 0, len(pageRows))
	for _, row := range pageRows {
		items = append(items, InvitedUserItem{
			UserID: row.UserID, Username: row.Username,
			DisplayName: nullableStringPointer(row.DisplayName), Email: nullableStringPointer(row.Email),
			Status: row.Status, Quota: int64Pointer(row.Quota), UsedQuota: int64Pointer(row.UsedQuota),
			RequestCount: row.RequestCount, Group: row.Group, Role: int64Pointer(row.Role),
		})
	}

	if err := tx.Commit(); err != nil {
		return InvitedUsersResult{}, fmt.Errorf("commit invited-user snapshot: %w", err)
	}
	return InvitedUsersResult{
		Inviter: inviter, Items: items, Total: total, Page: page, PageSize: pageSize,
		TotalPages: totalPages, AsOf: asOf, QueryFingerprint: fingerprint, Stats: stats,
	}, nil
}
