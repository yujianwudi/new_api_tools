package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
)

type batchDeleteUser struct {
	id           int64
	username     string
	requestCount int64
}

type batchDeleteSnapshot struct {
	IDs            []int64 `json:"ids"`
	RequestCounts  []int64 `json:"request_counts"`
	ActivityLevel  string  `json:"activity_level"`
	HardDelete     bool    `json:"hard_delete"`
	ActivityCutoff int64   `json:"activity_cutoff"`
	CreatedAt      int64   `json:"created_at"`
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
	if len(snapshot.IDs) != len(snapshot.RequestCounts) {
		return nil, fmt.Errorf("%w: malformed batch-delete snapshot", ErrInvalidOrExpiredSnapshot)
	}
	return &snapshot, nil
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

// Cached OAuth column existence checks
var (
	oauthColumnsOnce   sync.Once
	availableOAuthCols []string // columns that actually exist in the users table
)

// allOAuthColumns lists all possible OAuth ID columns in New API users table
var allOAuthColumns = []string{"github_id", "wechat_id", "telegram_id", "discord_id", "oidc_id", "linux_do_id"}

// NewUserManagementService creates a new UserManagementService
func NewUserManagementService() *UserManagementService {
	return &UserManagementService{db: database.Get(), logDB: database.GetLog()}
}

// activeUserIDsSince returns the set of user_ids that have at least one billable
// log entry (type 2/5) since `since`. It queries the log DB directly, so it stays
// correct when logs live in a separate database (LOG_SQL_DSN) — a cross-DB
// EXISTS(...) subquery against the users table is impossible there.
func (s *UserManagementService) activeUserIDsSince(since int64) (map[int64]bool, error) {
	rows, err := s.logDB.QueryWithTimeout(60*time.Second, s.logDB.RebindQuery(
		"SELECT DISTINCT user_id FROM logs WHERE type IN (2,5) AND created_at >= ? AND user_id > 0"), since)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]bool, len(rows))
	for _, r := range rows {
		set[toInt64(r["user_id"])] = true
	}
	return set, nil
}

// activeCandidateUserIDsSince limits the log lookup to deletion candidates.
// This is especially important for ActivityNever, where a created_at >= 0 scan
// over the complete lifetime of a large logs table would otherwise be costly.
func (s *UserManagementService) activeCandidateUserIDsSince(candidateIDs []int64, since int64) (map[int64]bool, error) {
	return s.activeCandidateUserIDsSinceWithTimeout(candidateIDs, since, 60*time.Second)
}

func (s *UserManagementService) activeCandidateUserIDsSinceWithTimeout(candidateIDs []int64, since int64, timeout time.Duration) (map[int64]bool, error) {
	set := make(map[int64]bool)
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
		args = append(args, since)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}

		query := s.logDB.RebindQuery(fmt.Sprintf(
			"SELECT DISTINCT user_id FROM logs WHERE type IN (2,5) AND created_at >= ? AND user_id IN (%s)",
			strings.Join(placeholders, ",")))
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		rows, err := s.logDB.QueryWithTimeout(remaining, query, args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if id := toInt64(row["user_id"]); id > 0 {
				set[id] = true
			}
		}
	}
	return set, nil
}

// previewBatchDeleteCandidates walks the main user table with keyset
// pagination and checks one bounded page against the log database at a time.
// It stops as soon as maxBatchDeleteUsers+1 eligible rows are found, which is
// enough to reject an oversized operation without scanning the full population.
func (s *UserManagementService) previewBatchDeleteCandidates(activityLevel string, threshold int64) ([]batchDeleteUser, error) {
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

		activeSet, err := s.activeCandidateUserIDsSinceWithTimeout(pageIDs, threshold, time.Until(deadline))
		if err != nil {
			return nil, fmt.Errorf("无法从日志库判定用户活跃度，已中止删除以防误删: %w", err)
		}
		for _, row := range rows {
			uid := toInt64(row["id"])
			if uid <= 0 || activeSet[uid] {
				continue
			}
			eligible = append(eligible, batchDeleteUser{
				id:           uid,
				username:     toString(row["username"]),
				requestCount: toInt64(row["request_count"]),
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

func (s *UserManagementService) activeCandidateUserIDsSinceLocked(ctx context.Context, tx *sqlx.Tx, candidateIDs []int64, since int64) (map[int64]bool, error) {
	if s.logDB != s.db {
		return nil, ErrSeparateLogDBBatchDeleteBlocked
	}

	set := make(map[int64]bool)
	const lookupBatchSize = 500
	for start := 0; start < len(candidateIDs); start += lookupBatchSize {
		end := start + lookupBatchSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		batch := candidateIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		args = append(args, since)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := tx.Rebind(fmt.Sprintf(
			"SELECT DISTINCT user_id FROM logs WHERE type IN (2,5) AND created_at >= ? AND user_id IN (%s)",
			strings.Join(placeholders, ",")))
		var activeIDs []int64
		if err := tx.SelectContext(ctx, &activeIDs, query, args...); err != nil {
			return nil, err
		}
		for _, id := range activeIDs {
			if id > 0 {
				set[id] = true
			}
		}
	}
	return set, nil
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

// getAvailableOAuthColumns returns OAuth columns that exist in the users table (cached)
func (s *UserManagementService) getAvailableOAuthColumns() []string {
	oauthColumnsOnce.Do(func() {
		availableOAuthCols = make([]string, 0)
		for _, col := range allOAuthColumns {
			if s.db.ColumnExists("users", col) {
				availableOAuthCols = append(availableOAuthCols, col)
			}
		}
		logger.L.Business(fmt.Sprintf("检测到 users 表 OAuth 字段: %v", availableOAuthCols))
	})
	return availableOAuthCols
}

// GetActivityStats returns user activity statistics
func (s *UserManagementService) GetActivityStats(quick bool) (map[string]interface{}, error) {
	now := time.Now().Unix()
	activeThreshold := now - ActiveThreshold
	inactiveThreshold := now - InactiveThreshold

	// Total users (not deleted)
	totalRow, err := s.db.QueryOne("SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL")
	if err != nil {
		return nil, err
	}
	totalUsers := totalRow["count"]

	if quick {
		// Quick mode: only total + never requested
		neverRow, _ := s.db.QueryOne(
			"SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL AND request_count = 0")
		neverCount := int64(0)
		if neverRow != nil {
			neverCount = toInt64(neverRow["count"])
		}
		return map[string]interface{}{
			"total_users":         totalUsers,
			"active_users":        0,
			"inactive_users":      0,
			"very_inactive_users": 0,
			"never_requested":     neverCount,
			"quick_mode":          true,
		}, nil
	}

	// Full stats: classify users by their most recent billable log.
	// Logs may live in a separate DB, so we can't use a cross-DB EXISTS subquery.
	// Instead: pull the active/recent user-id sets from the log DB, then count
	// against the users table in Go.
	activeSet, err := s.activeUserIDsSince(activeThreshold) // active in last 7d
	if err != nil {
		return nil, err
	}
	recentSet, err := s.activeUserIDsSince(inactiveThreshold) // active in last 30d
	if err != nil {
		return nil, err
	}

	// All non-deleted users that have ever made a request.
	requestedRows, err := s.db.Query("SELECT id FROM users WHERE deleted_at IS NULL AND request_count > 0")
	if err != nil {
		return nil, err
	}

	var activeCount, inactiveCount int64
	for _, r := range requestedRows {
		uid := toInt64(r["id"])
		switch {
		case activeSet[uid]:
			// last request within 7d
			activeCount++
		case recentSet[uid]:
			// last request within 7-30d (in recent set but not active set)
			inactiveCount++
		}
	}

	// Never requested
	neverRow, _ := s.db.QueryOne("SELECT COUNT(*) as count FROM users WHERE deleted_at IS NULL AND request_count = 0")
	neverCount := int64(0)
	if neverRow != nil {
		neverCount = toInt64(neverRow["count"])
	}

	total := toInt64(totalUsers)
	veryInactive := total - activeCount - inactiveCount - neverCount

	return map[string]interface{}{
		"total_users":         total,
		"active_users":        activeCount,
		"inactive_users":      inactiveCount,
		"very_inactive_users": veryInactive,
		"never_requested":     neverCount,
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

// GetUsers returns paginated user list
func (s *UserManagementService) GetUsers(params ListUsersParams) (map[string]interface{}, error) {
	if params.Page < 1 {
		params.Page = 1
	}
	if params.PageSize < 1 || params.PageSize > 100 {
		params.PageSize = 20
	}
	// ORDER BY identifiers cannot be parameterized. Map all external values to
	// fixed literals before composing the query so no request data reaches SQL.
	orderBy := userListOrderColumn(params.OrderBy)
	orderDir := userListOrderDirection(params.OrderDir)

	groupCol := "`group`"
	if s.db.IsPG {
		groupCol = `"group"`
	}

	// Detect which OAuth columns exist in the database
	oauthCols := s.getAvailableOAuthColumns()
	oauthColSet := make(map[string]bool)
	for _, col := range oauthCols {
		oauthColSet[col] = true
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
	if params.GroupFilter != "" {
		if s.db.IsPG {
			where = append(where, fmt.Sprintf("u.%s = $%d", groupCol, argIdx))
			argIdx++
		} else {
			where = append(where, fmt.Sprintf("u.%s = ?", groupCol))
		}
		args = append(args, params.GroupFilter)
	}
	if params.ActivityFilter == ActivityNever {
		where = append(where, "u.request_count = 0")
	}

	// Source filter — only apply if the relevant column exists
	if params.SourceFilter != "" {
		var sourceCond string
		switch params.SourceFilter {
		case "password":
			// Password means none of the OAuth columns are set
			condParts := make([]string, 0)
			for _, col := range oauthCols {
				condParts = append(condParts, fmt.Sprintf("(u.%s IS NULL OR u.%s = '')", col, col))
			}
			if len(condParts) > 0 {
				sourceCond = strings.Join(condParts, " AND ")
			}
		default:
			// Map filter name to column name
			colMap := map[string]string{
				"github": "github_id", "wechat": "wechat_id", "telegram": "telegram_id",
				"discord": "discord_id", "oidc": "oidc_id", "linux_do": "linux_do_id",
			}
			if colName, ok := colMap[params.SourceFilter]; ok && oauthColSet[colName] {
				sourceCond = fmt.Sprintf("u.%s IS NOT NULL AND u.%s <> ''", colName, colName)
			}
		}
		if sourceCond != "" {
			where = append(where, "("+sourceCond+")")
		}
	}

	whereClause := strings.Join(where, " AND ")

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) as count FROM users u WHERE %s", whereClause)
	if !s.db.IsPG {
		countQuery = s.db.RebindQuery(countQuery)
	}
	countRow, err := s.db.QueryOne(countQuery, args...)
	if err != nil {
		return nil, err
	}
	total := toInt64(countRow["count"])

	// Build SELECT columns dynamically based on available OAuth columns
	// NOTE: users table does NOT have created_at — do not select it
	selectCols := fmt.Sprintf("u.id, u.username, u.display_name, u.email, u.role, u.status, u.quota, u.used_quota, u.request_count, u.%s, u.aff_code, u.remark", groupCol)
	for _, col := range oauthCols {
		selectCols += fmt.Sprintf(", u.%s", col)
	}

	var selectQuery string
	if s.db.IsPG {
		selectQuery = fmt.Sprintf(
			"SELECT %s FROM users u WHERE %s ORDER BY u.%s %s LIMIT $%d OFFSET $%d",
			selectCols, whereClause, orderBy, orderDir, argIdx, argIdx+1)
		args = append(args, params.PageSize, offset)
	} else {
		selectQuery = fmt.Sprintf(
			"SELECT %s FROM users u WHERE %s ORDER BY u.%s %s LIMIT ? OFFSET ?",
			selectCols, whereClause, orderBy, orderDir)
		args = append(args, params.PageSize, offset)
		selectQuery = s.db.RebindQuery(selectQuery)
	}

	rows, err := s.db.Query(selectQuery, args...)
	if err != nil {
		logger.L.Error(fmt.Sprintf("GetUsers 查询失败: %v, SQL: %s, args: %v", err, selectQuery, args))
		return nil, err
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}

	// Enrich rows with computed fields (activity_level, source, linux_do_id)
	for _, row := range rows {
		reqCount := toInt64(row["request_count"])
		if reqCount == 0 {
			row["activity_level"] = ActivityNever
		} else {
			row["activity_level"] = ActivityActive
		}
		row["last_request_time"] = nil

		// Preserve linux_do_id for frontend display
		linuxDoID := ""
		if oauthColSet["linux_do_id"] {
			linuxDoID = toString(row["linux_do_id"])
		}
		row["linux_do_id"] = linuxDoID

		// Compute source from OAuth ID fields (only check existing columns)
		source := "password"
		if oauthColSet["linux_do_id"] && toString(row["linux_do_id"]) != "" {
			source = "linux_do"
		} else if oauthColSet["github_id"] && toString(row["github_id"]) != "" {
			source = "github"
		} else if oauthColSet["wechat_id"] && toString(row["wechat_id"]) != "" {
			source = "wechat"
		} else if oauthColSet["telegram_id"] && toString(row["telegram_id"]) != "" {
			source = "telegram"
		} else if oauthColSet["discord_id"] && toString(row["discord_id"]) != "" {
			source = "discord"
		} else if oauthColSet["oidc_id"] && toString(row["oidc_id"]) != "" {
			source = "oidc"
		}
		row["source"] = source

		// Clean up internal OAuth fields (except linux_do_id which is kept)
		for _, col := range oauthCols {
			if col != "linux_do_id" {
				delete(row, col)
			}
		}
	}

	totalPages := int((total + int64(params.PageSize) - 1) / int64(params.PageSize))

	return map[string]interface{}{
		"items":       rows,
		"total":       total,
		"page":        params.Page,
		"page_size":   params.PageSize,
		"total_pages": totalPages,
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

	// A zero threshold means "has any billable log", which is the required
	// cross-check for users whose denormalized request_count says zero.
	var threshold int64
	switch activityLevel {
	case ActivityNever:
		threshold = 0
	case ActivityVeryInactive:
		threshold = nowUnix - InactiveThreshold
	case ActivityInactive:
		threshold = nowUnix - ActiveThreshold
	default:
		return nil, fmt.Errorf("invalid activity level: %s", activityLevel)
	}

	if dryRun {
		toDelete, err := s.previewBatchDeleteCandidates(activityLevel, threshold)
		if err != nil {
			return nil, err
		}
		if len(toDelete) > maxBatchDeleteUsers {
			return nil, fmt.Errorf("batch delete blocked: %d users exceed the per-operation limit of %d", len(toDelete), maxBatchDeleteUsers)
		}

		preview := make([]string, 0, 20)
		ids := make([]int64, len(toDelete))
		requestCounts := make([]int64, len(toDelete))
		for i, u := range toDelete {
			ids[i] = u.id
			requestCounts[i] = u.requestCount
			if i >= 20 {
				continue
			}
			preview = append(preview, u.username)
		}
		storedSnapshotID := ""
		if len(ids) > 0 {
			storedSnapshotID, err = storeBatchDeleteSnapshot(batchDeleteSnapshot{
				IDs:            ids,
				RequestCounts:  requestCounts,
				ActivityLevel:  activityLevel,
				HardDelete:     hardDelete,
				ActivityCutoff: threshold,
				CreatedAt:      nowUnix,
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

	// Bind execution to the exact previewed IDs and re-read their logs immediately
	// before entering the write transaction. Any newly active user invalidates the
	// entire snapshot; the operator must preview again.
	activeSet, err := s.activeCandidateUserIDsSince(ids, snapshot.ActivityCutoff)
	if err != nil {
		return nil, fmt.Errorf("无法重新验证预览用户的活跃度，已中止删除: %w", err)
	}
	if len(activeSet) > 0 {
		return nil, fmt.Errorf("%w: %d previewed users became active; preview again", ErrSnapshotInvalidated, len(activeSet))
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

		activeSet, err := s.activeCandidateUserIDsSinceLocked(ctx, tx, ids, snapshot.ActivityCutoff)
		if err != nil {
			return fmt.Errorf("无法在锁定候选后重新验证用户活跃度，已中止删除: %w", err)
		}
		if len(activeSet) > 0 {
			return fmt.Errorf("%w: %d locked users became active; preview again", ErrSnapshotInvalidated, len(activeSet))
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

// GetInvitedUsers returns users invited by the specified user
func (s *UserManagementService) GetInvitedUsers(userID int64, page, pageSize int) (map[string]interface{}, error) {
	offset := (page - 1) * pageSize

	// Get inviter info
	inviterRow, err := s.db.QueryOne(s.db.RebindQuery(
		"SELECT id, username, display_name, aff_code, aff_count, aff_quota, aff_history FROM users WHERE id = ? AND deleted_at IS NULL"), userID)
	if err != nil || inviterRow == nil {
		return map[string]interface{}{
			"inviter":   nil,
			"items":     []interface{}{},
			"total":     0,
			"page":      page,
			"page_size": pageSize,
			"stats":     map[string]interface{}{},
		}, nil
	}

	inviter := map[string]interface{}{
		"user_id":      inviterRow["id"],
		"username":     inviterRow["username"],
		"display_name": inviterRow["display_name"],
		"aff_code":     inviterRow["aff_code"],
		"aff_count":    inviterRow["aff_count"],
		"aff_quota":    inviterRow["aff_quota"],
		"aff_history":  inviterRow["aff_history"],
	}

	// Count total invited
	countRow, _ := s.db.QueryOne(s.db.RebindQuery(
		"SELECT COUNT(*) as total FROM users WHERE inviter_id = ? AND deleted_at IS NULL"), userID)
	total := int64(0)
	if countRow != nil {
		total = toInt64(countRow["total"])
	}

	// Get invited users list
	groupCol := "`group`"
	if s.db.IsPG {
		groupCol = `"group"`
	}
	query := s.db.RebindQuery(fmt.Sprintf(`
		SELECT id, username, display_name, email, status,
			quota, used_quota, request_count, %s, role
		FROM users
		WHERE inviter_id = ? AND deleted_at IS NULL
		ORDER BY id DESC
		LIMIT ? OFFSET ?`,
		groupCol))

	rows, err := s.db.Query(query, userID, pageSize, offset)
	if err != nil {
		return nil, err
	}

	// Compute stats
	activeCount := 0
	bannedCount := 0
	totalUsedQuota := int64(0)
	totalRequests := int64(0)
	for _, row := range rows {
		if toInt64(row["request_count"]) > 0 {
			activeCount++
		}
		if toInt64(row["status"]) == 2 {
			bannedCount++
		}
		totalUsedQuota += toInt64(row["used_quota"])
		totalRequests += toInt64(row["request_count"])
	}

	return map[string]interface{}{
		"inviter":   inviter,
		"items":     rows,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"stats": map[string]interface{}{
			"total_invited":    total,
			"active_count":     activeCount,
			"banned_count":     bannedCount,
			"total_used_quota": totalUsedQuota,
			"total_requests":   totalRequests,
		},
	}, nil
}
