//go:build performance

package service

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

const (
	userManagementPerfUserCount = 100_000
	userManagementPerfLogDays   = 30
	userManagementPerfWarmups   = 3
	userManagementPerfSamples   = 20

	userListP95Limit     = 800 * time.Millisecond
	invitedUsersP95Limit = 500 * time.Millisecond
	secondsPerFixtureDay = int64(24 * time.Hour / time.Second)
	performancePageSize  = 100
	performanceInviterID = int64(1)
)

type userManagementPerformanceFixture struct {
	mainDB  *sqlx.DB
	logDB   *sqlx.DB
	service *UserManagementService
	asOf    int64
}

// TestUserManagementPerformanceSLO is an explicit, threshold-bearing
// acceptance test rather than a Benchmark. Run it with:
//
//	go test -tags=performance -run '^TestUserManagementPerformanceSLO$' -count=1 -v ./internal/service
//
// It uses file-backed SQLite databases so query planning and timings are taken
// against a real 100,000-user fixture whose billable logs span 30 distinct days.
func TestUserManagementPerformanceSLO(t *testing.T) {
	t.Logf("PERF_ENV go=%s os=%s arch=%s cpus=%d",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fixtureStarted := time.Now()
	fixture := installUserManagementPerformanceFixture(t)
	t.Logf("PERF_FIXTURE users=%d billable_logs=%d log_days=%d build=%s",
		userManagementPerfUserCount, userManagementPerfUserCount,
		userManagementPerfLogDays, time.Since(fixtureStarted).Round(time.Millisecond))

	assertUserManagementPerformanceFixture(t, fixture)
	explainUserManagementPerformanceQueries(t, fixture)

	previousNow := userActivityNow
	// Keep activity-bucket membership deterministic. The newest log is one hour
	// before as_of, so the active bucket contains fixture day offsets 0..6.
	userActivityNow = func() time.Time { return time.Unix(fixture.asOf+3600, 0) }
	t.Cleanup(func() { userActivityNow = previousNow })

	defaultListP95 := measureWarmQueryP95(t, "user_list_default",
		userManagementPerfWarmups, userManagementPerfSamples, func() error {
			result, err := fixture.service.GetUsers(ListUsersParams{
				Page: 1, PageSize: performancePageSize,
				OrderBy: "request_count", OrderDir: "DESC",
			})
			if err != nil {
				return err
			}
			if total := toInt64(result["total"]); total != userManagementPerfUserCount {
				return fmt.Errorf("default user total = %d, want %d",
					total, userManagementPerfUserCount)
			}
			items, ok := result["items"].([]map[string]interface{})
			if !ok || len(items) != performancePageSize {
				return fmt.Errorf("default user page has type %T and length %d, want %d rows",
					result["items"], len(items), performancePageSize)
			}
			for _, item := range items {
				if item["last_request_time"] == nil {
					return fmt.Errorf("user %v does not carry billable-log evidence", item["id"])
				}
			}
			return nil
		})

	wantActiveUsers := usersInLeadingFixtureDays(
		userManagementPerfUserCount, userManagementPerfLogDays, 7)
	activeListP95 := measureWarmQueryP95(t, "user_list_active_filter",
		userManagementPerfWarmups, userManagementPerfSamples, func() error {
			result, err := fixture.service.GetUsers(ListUsersParams{
				Page: 1, PageSize: performancePageSize,
				ActivityFilter: ActivityActive,
				OrderBy:        "request_count", OrderDir: "DESC",
			})
			if err != nil {
				return err
			}
			if total := toInt64(result["total"]); total != int64(wantActiveUsers) {
				return fmt.Errorf("active user total = %d, want %d", total, wantActiveUsers)
			}
			items, ok := result["items"].([]map[string]interface{})
			if !ok || len(items) != performancePageSize {
				return fmt.Errorf("active user page has type %T and length %d, want %d rows",
					result["items"], len(items), performancePageSize)
			}
			for _, item := range items {
				if toString(item["activity_level"]) != ActivityActive || item["last_request_time"] == nil {
					return fmt.Errorf("user %v does not carry active billable-log evidence", item["id"])
				}
			}
			return nil
		})

	invitedP95 := measureWarmQueryP95(t, "invited_user_details",
		userManagementPerfWarmups, userManagementPerfSamples, func() error {
			result, err := fixture.service.GetInvitedUsers(
				performanceInviterID, 1, performancePageSize, nil)
			if err != nil {
				return err
			}
			wantTotal := int64(userManagementPerfUserCount - 1)
			if result.Total != wantTotal || result.Stats.TotalInvited != wantTotal {
				return fmt.Errorf("invited totals = %d/%d, want %d",
					result.Total, result.Stats.TotalInvited, wantTotal)
			}
			if len(result.Items) != performancePageSize || result.AsOf <= 0 {
				return fmt.Errorf("invited page rows/as_of = %d/%d", len(result.Items), result.AsOf)
			}
			return nil
		})

	if defaultListP95 > userListP95Limit {
		t.Errorf("default user list p95 = %s, limit = %s",
			defaultListP95.Round(time.Microsecond), userListP95Limit)
	}
	if activeListP95 > userListP95Limit {
		t.Errorf("activity-filtered user list p95 = %s, limit = %s",
			activeListP95.Round(time.Microsecond), userListP95Limit)
	}
	if invitedP95 > invitedUsersP95Limit {
		t.Errorf("invited-user details p95 = %s, limit = %s",
			invitedP95.Round(time.Microsecond), invitedUsersP95Limit)
	}
}

func installUserManagementPerformanceFixture(t *testing.T) userManagementPerformanceFixture {
	t.Helper()
	dir := t.TempDir()
	mainDB := openPerformanceSQLite(t, filepath.Join(dir, "users.sqlite"))
	logDB := openPerformanceSQLite(t, filepath.Join(dir, "logs.sqlite"))

	mainDB.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		display_name TEXT,
		email TEXT,
		role INTEGER NOT NULL DEFAULT 1,
		status INTEGER NOT NULL DEFAULT 1,
		quota INTEGER NOT NULL DEFAULT 0,
		used_quota INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		"group" TEXT DEFAULT 'default',
		aff_code TEXT,
		aff_count INTEGER NOT NULL DEFAULT 0,
		aff_quota INTEGER NOT NULL DEFAULT 0,
		aff_history INTEGER NOT NULL DEFAULT 0,
		inviter_id INTEGER,
		remark TEXT,
		github_id TEXT,
		wechat_id TEXT,
		telegram_id TEXT,
		discord_id TEXT,
		oidc_id TEXT,
		linux_do_id TEXT,
		deleted_at INTEGER
	)`)
	logDB.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		type INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`)

	insertPerformanceUsers(t, mainDB)
	asOf := time.Now().UTC().Truncate(24 * time.Hour).Unix()
	insertPerformanceLogs(t, logDB, asOf)

	// Match the relevant indexes present in the exported upstream schema. The
	// fixture deliberately does not introduce test-only covering indexes.
	mainDB.MustExec(`
		CREATE INDEX idx_users_deleted_at ON users(deleted_at);
		CREATE INDEX idx_users_request_count ON users(request_count);
		CREATE INDEX idx_users_inviter_id ON users(inviter_id);
		ANALYZE;
	`)
	logDB.MustExec(`
		CREATE INDEX idx_logs_type_created_user ON logs(type, created_at, user_id);
		CREATE INDEX idx_logs_user_type_created ON logs(user_id, type, created_at);
		ANALYZE;
	`)

	mainManager := &database.Manager{DB: mainDB, IsPG: false}
	logManager := &database.Manager{DB: logDB, IsPG: false}
	database.SetForTesting(mainManager)
	database.SetLogForTesting(logManager, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	t.Cleanup(func() {
		(&UserManagementService{db: mainManager, logDB: logManager}).invalidateOAuthColumnCache()
		database.SetForTesting(nil)
		_ = logDB.Close()
		_ = mainDB.Close()
	})

	return userManagementPerformanceFixture{
		mainDB:  mainDB,
		logDB:   logDB,
		service: &UserManagementService{db: mainManager, logDB: logManager},
		asOf:    asOf,
	}
}

func openPerformanceSQLite(t *testing.T, path string) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite", path)
	if err != nil {
		t.Fatalf("open performance SQLite %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -65536",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("configure performance SQLite with %q: %v", statement, err)
		}
	}
	return db
}

func insertPerformanceUsers(t *testing.T, db *sqlx.DB) {
	t.Helper()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin user fixture transaction: %v", err)
	}
	statement, err := tx.Preparex(`INSERT INTO users (
		id, username, display_name, email, role, status, quota, used_quota,
		request_count, "group", aff_code, aff_count, aff_quota, aff_history,
		inviter_id, remark
	) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare user fixture insert: %v", err)
	}
	defer statement.Close()

	for id := 1; id <= userManagementPerfUserCount; id++ {
		var inviterID interface{}
		if id > 1 {
			inviterID = performanceInviterID
		}
		affCount := 0
		if id == int(performanceInviterID) {
			affCount = userManagementPerfUserCount - 1
		}
		status := 1
		if id%97 == 0 {
			status = 2
		}
		if _, err := statement.Exec(
			id,
			fmt.Sprintf("perf-user-%06d", id),
			fmt.Sprintf("Performance User %06d", id),
			fmt.Sprintf("perf-%06d@example.invalid", id),
			status,
			1_000_000+id,
			id%10_000,
			id%17+1,
			fmt.Sprintf("group-%02d", id%8),
			fmt.Sprintf("AFF%06d", id),
			affCount,
			inviterID,
			"performance fixture",
		); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert performance user %d: %v", id, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close user fixture statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit user fixture: %v", err)
	}
}

func insertPerformanceLogs(t *testing.T, db *sqlx.DB, newestLog int64) {
	t.Helper()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin log fixture transaction: %v", err)
	}
	statement, err := tx.Preparex(
		"INSERT INTO logs (id, user_id, type, created_at) VALUES (?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare log fixture insert: %v", err)
	}
	defer statement.Close()

	for id := 1; id <= userManagementPerfUserCount; id++ {
		logType := 2
		if id%2 == 0 {
			logType = 5
		}
		dayOffset := int64((id - 1) % userManagementPerfLogDays)
		createdAt := newestLog - dayOffset*secondsPerFixtureDay
		if _, err := statement.Exec(id, id, logType, createdAt); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert performance log %d: %v", id, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close log fixture statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit log fixture: %v", err)
	}
}

func assertUserManagementPerformanceFixture(t *testing.T, fixture userManagementPerformanceFixture) {
	t.Helper()
	var userCount int64
	if err := fixture.mainDB.Get(&userCount, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatalf("count performance users: %v", err)
	}
	if userCount != userManagementPerfUserCount {
		t.Fatalf("performance user count = %d, want %d", userCount, userManagementPerfUserCount)
	}

	var invitedCount int64
	if err := fixture.mainDB.Get(&invitedCount,
		"SELECT COUNT(*) FROM users WHERE inviter_id = ?", performanceInviterID); err != nil {
		t.Fatalf("count performance invitees: %v", err)
	}
	if invitedCount != userManagementPerfUserCount-1 {
		t.Fatalf("performance invitee count = %d, want %d",
			invitedCount, userManagementPerfUserCount-1)
	}

	var logs struct {
		Count         int64 `db:"count"`
		DistinctUsers int64 `db:"distinct_users"`
		DistinctDays  int64 `db:"distinct_days"`
		Oldest        int64 `db:"oldest"`
		Newest        int64 `db:"newest"`
	}
	if err := fixture.logDB.Get(&logs, `SELECT
		COUNT(*) AS count,
		COUNT(DISTINCT user_id) AS distinct_users,
		COUNT(DISTINCT created_at) AS distinct_days,
		MIN(created_at) AS oldest,
		MAX(created_at) AS newest
		FROM logs WHERE type IN (2,5)`); err != nil {
		t.Fatalf("summarize performance logs: %v", err)
	}
	if logs.Count != userManagementPerfUserCount ||
		logs.DistinctUsers != userManagementPerfUserCount ||
		logs.DistinctDays != userManagementPerfLogDays {
		t.Fatalf("performance log cardinality = rows:%d users:%d days:%d, want %d/%d/%d",
			logs.Count, logs.DistinctUsers, logs.DistinctDays,
			userManagementPerfUserCount, userManagementPerfUserCount, userManagementPerfLogDays)
	}
	wantSpan := int64(userManagementPerfLogDays-1) * secondsPerFixtureDay
	if logs.Newest != fixture.asOf || logs.Newest-logs.Oldest != wantSpan {
		t.Fatalf("performance logs cover [%d,%d] span=%d, want newest=%d span=%d",
			logs.Oldest, logs.Newest, logs.Newest-logs.Oldest, fixture.asOf, wantSpan)
	}
	t.Logf("PERF_FIXTURE_VALIDATED users=%d invitees=%d logs=%d distinct_log_users=%d distinct_log_days=%d span=%s",
		userCount, invitedCount, logs.Count, logs.DistinctUsers, logs.DistinctDays,
		time.Duration(logs.Newest-logs.Oldest)*time.Second)
}

func explainUserManagementPerformanceQueries(t *testing.T, fixture userManagementPerformanceFixture) {
	t.Helper()
	activityEvidencePlan := explainSQLiteQuery(t, fixture.logDB, "activity_evidence_snapshot", `
		SELECT user_id, MAX(created_at) AS last_request_time
		FROM logs
		WHERE type IN (2,5) AND created_at <= ? AND user_id > 0
		GROUP BY user_id
		ORDER BY user_id
		LIMIT ?`, fixture.asOf+3600, activityFilterMaxIDs+1)
	requirePlanIndex(t, "activity_evidence_snapshot", activityEvidencePlan,
		"idx_logs_user_type_created", "idx_logs_type_created_user")

	activityCandidatePlan := explainSQLiteQuery(t, fixture.mainDB, "activity_ordered_candidate_scan", `
		SELECT u.id, u.request_count, u.request_count AS activity_sort_value
		FROM users u
		WHERE u.deleted_at IS NULL AND u.request_count > 0
		ORDER BY u.request_count DESC, u.id DESC
		LIMIT ?`, activityCandidateBatchSize)
	requirePlanIndex(t, "activity_ordered_candidate_scan", activityCandidatePlan,
		"idx_users_request_count", "idx_users_deleted_at")

	pagePlaceholders := make([]string, performancePageSize)
	pageIDArgs := make([]interface{}, performancePageSize)
	for index := range pagePlaceholders {
		pagePlaceholders[index] = "?"
		pageIDArgs[index] = userManagementPerfUserCount - index
	}
	activityPagePlan := explainSQLiteQuery(t, fixture.mainDB, "activity_bounded_page_fetch", fmt.Sprintf(`
		SELECT u.id, u.username, u.display_name, u.email, u.role, u.status,
			u.quota, u.used_quota, u.request_count, u."group", u.aff_code, u.remark,
			u.linux_do_id, u.github_id, u.wechat_id, u.telegram_id, u.discord_id, u.oidc_id
		FROM users u
		WHERE u.deleted_at IS NULL AND u.request_count > 0 AND u.id IN (%s)`, strings.Join(pagePlaceholders, ",")), pageIDArgs...)
	requirePlanStrategy(t, "activity_bounded_page_fetch", activityPagePlan,
		"integer primary key", "primary key", "users_pkey")

	listCountPlan := explainSQLiteQuery(t, fixture.mainDB, "user_list_count",
		"SELECT COUNT(*) AS count FROM users u WHERE u.deleted_at IS NULL")
	requirePlanIndex(t, "user_list_count", listCountPlan, "idx_users_deleted_at")

	listPagePlan := explainSQLiteQuery(t, fixture.mainDB, "user_list_page", `
		SELECT u.id, u.username, u.display_name, u.email, u.role, u.status,
			u.quota, u.used_quota, u.request_count, u."group", u.aff_code, u.remark,
			u.linux_do_id, u.github_id, u.wechat_id, u.telegram_id, u.discord_id, u.oidc_id
		FROM users u
		WHERE u.deleted_at IS NULL
		ORDER BY u.request_count DESC, u.id DESC
		LIMIT ? OFFSET ?`, performancePageSize, 0)
	requirePlanIndex(t, "user_list_page", listPagePlan,
		"idx_users_deleted_at", "idx_users_request_count")

	logPlan := explainSQLiteQuery(t, fixture.logDB, "user_list_last_billable_log", `
		SELECT user_id, MAX(created_at) AS last_request_time
		FROM logs
		WHERE type IN (2,5) AND created_at <= ? AND user_id IN (?,?,?)
		GROUP BY user_id`, fixture.asOf+3600, 99_999, 99_998, 99_997)
	requirePlanIndex(t, "user_list_last_billable_log", logPlan,
		"idx_logs_user_type_created", "idx_logs_type_created_user")

	invitedStatsPlan := explainSQLiteQuery(t, fixture.mainDB, "invited_user_stats", `
		SELECT COUNT(*) AS total_invited,
			COALESCE(SUM(CASE WHEN request_count > 0 THEN 1 ELSE 0 END), 0) AS requested_count,
			COALESCE(SUM(CASE WHEN status = 2 THEN 1 ELSE 0 END), 0) AS banned_count,
			COALESCE(SUM(used_quota), 0) AS total_used_quota,
			COALESCE(SUM(request_count), 0) AS total_requests
		FROM users WHERE inviter_id = ? AND deleted_at IS NULL`, performanceInviterID)
	// The fixture intentionally gives one inviter 99,999 of 100,000 users. With
	// ANALYZE statistics, SQLite can correctly choose a full table aggregation
	// instead of 99,999 index-to-table lookups. Pin either legitimate strategy
	// and let the latency threshold catch an expensive execution regression.
	requirePlanStrategy(t, "invited_user_stats", invitedStatsPlan,
		"idx_users_inviter_id", "scan users")

	invitedPagePlan := explainSQLiteQuery(t, fixture.mainDB, "invited_user_page", `
		SELECT id AS user_id, username, display_name, email, status,
			quota, used_quota, request_count,
			COALESCE(NULLIF("group", ''), 'default') AS user_group, role
		FROM users
		WHERE inviter_id = ? AND deleted_at IS NULL
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, performanceInviterID, performancePageSize, 0)
	requirePlanStrategy(t, "invited_user_page", invitedPagePlan,
		"idx_users_inviter_id", "scan users")
}

func explainSQLiteQuery(t *testing.T, db *sqlx.DB, name, query string, args ...interface{}) []string {
	t.Helper()
	rows, err := db.Queryx("EXPLAIN QUERY PLAN "+strings.TrimSpace(query), args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %s: %v", name, err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan EXPLAIN QUERY PLAN %s: %v", name, err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read EXPLAIN QUERY PLAN %s: %v", name, err)
	}
	if len(plan) == 0 {
		t.Fatalf("EXPLAIN QUERY PLAN %s returned no rows", name)
	}
	t.Logf("PERF_EXPLAIN name=%s plan=%q", name, strings.Join(plan, " | "))
	return plan
}

func requirePlanIndex(t *testing.T, name string, plan []string, indexes ...string) {
	t.Helper()
	joined := strings.ToLower(strings.Join(plan, "\n"))
	for _, index := range indexes {
		if strings.Contains(joined, strings.ToLower(index)) {
			return
		}
	}
	t.Fatalf("EXPLAIN QUERY PLAN %s does not use any expected index %v: %s",
		name, indexes, strings.Join(plan, " | "))
}

func requirePlanStrategy(t *testing.T, name string, plan []string, fragments ...string) {
	t.Helper()
	joined := strings.ToLower(strings.Join(plan, "\n"))
	for _, fragment := range fragments {
		if strings.Contains(joined, strings.ToLower(fragment)) {
			return
		}
	}
	t.Fatalf("EXPLAIN QUERY PLAN %s does not match an expected strategy %v: %s",
		name, fragments, strings.Join(plan, " | "))
}

func measureWarmQueryP95(
	t *testing.T,
	name string,
	warmups int,
	samples int,
	query func() error,
) time.Duration {
	t.Helper()
	if warmups < 1 || samples < 2 {
		t.Fatalf("%s needs at least one warmup and two measured samples", name)
	}
	for run := 1; run <= warmups; run++ {
		if err := query(); err != nil {
			t.Fatalf("%s warmup %d/%d failed: %v", name, run, warmups, err)
		}
	}

	durations := make([]time.Duration, 0, samples)
	for run := 1; run <= samples; run++ {
		started := time.Now()
		err := query()
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("%s measured run %d/%d failed after %s: %v",
				name, run, samples, elapsed.Round(time.Microsecond), err)
		}
		durations = append(durations, elapsed)
	}

	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[percentileIndex(len(sorted), 50)]
	p95 := sorted[percentileIndex(len(sorted), 95)]
	t.Logf("PERF_RESULT name=%s warmups=%d samples=%d min=%s p50=%s p95=%s max=%s",
		name, warmups, samples,
		sorted[0].Round(time.Microsecond), p50.Round(time.Microsecond),
		p95.Round(time.Microsecond), sorted[len(sorted)-1].Round(time.Microsecond))
	return p95
}

func percentileIndex(samples, percentile int) int {
	// Nearest-rank percentile: ceil(percentile*samples/100)-1.
	index := (percentile*samples+99)/100 - 1
	if index < 0 {
		return 0
	}
	if index >= samples {
		return samples - 1
	}
	return index
}

func usersInLeadingFixtureDays(users, totalDays, leadingDays int) int {
	fullCycles := users / totalDays
	remainder := users % totalDays
	extra := remainder
	if extra > leadingDays {
		extra = leadingDays
	}
	return fullCycles*leadingDays + extra
}
