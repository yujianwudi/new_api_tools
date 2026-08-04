package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	modernsqlite "modernc.org/sqlite"
)

const (
	affiliatePerfUserCount    = int64(100_000)
	affiliatePerfInviterCount = int64(1_000)
	affiliatePerfTopUpCount   = affiliatePerfUserCount - affiliatePerfInviterCount
	affiliatePerfWarmups      = 3
	affiliatePerfSamples      = 20
	affiliatePerfListLimit    = 1500 * time.Millisecond
	affiliatePerfSummaryLimit = 1500 * time.Millisecond
	affiliatePerfDetailLimit  = 500 * time.Millisecond
)

type affiliatePerfQueryRecord struct {
	SQL  string
	Args []any
}

type affiliatePerfQueryCounter struct {
	mu      sync.Mutex
	enabled bool
	records []affiliatePerfQueryRecord
}

func (c *affiliatePerfQueryCounter) start() {
	c.mu.Lock()
	c.records = nil
	c.enabled = true
	c.mu.Unlock()
}

func (c *affiliatePerfQueryCounter) stop() []affiliatePerfQueryRecord {
	c.mu.Lock()
	c.enabled = false
	records := append([]affiliatePerfQueryRecord(nil), c.records...)
	c.mu.Unlock()
	return records
}

func (c *affiliatePerfQueryCounter) record(query string, args []driver.NamedValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return
	}
	values := make([]any, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	c.records = append(c.records, affiliatePerfQueryRecord{SQL: query, Args: values})
}

// affiliatePerfCountingDriver keeps the production service path intact while
// recording the exact statements database/sql sends to SQLite. The wrapper is
// deliberately test-only; it is used to prove that parent-page evidence is one
// batched query instead of one query per inviter.
type affiliatePerfCountingDriver struct {
	inner   driver.Driver
	counter *affiliatePerfQueryCounter
}

func (d *affiliatePerfCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &affiliatePerfCountingConn{Conn: conn, counter: d.counter}, nil
}

type affiliatePerfCountingConn struct {
	driver.Conn
	counter *affiliatePerfQueryCounter
}

func (c *affiliatePerfCountingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *affiliatePerfCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.Conn.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) || opts.ReadOnly {
		return nil, fmt.Errorf("wrapped SQLite driver does not support requested transaction options")
	}
	return c.Conn.Begin()
}

func (c *affiliatePerfCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	conn, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.ExecContext(ctx, query, args)
}

func (c *affiliatePerfCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.counter.record(query, args)
	conn, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.QueryContext(ctx, query, args)
}

func (c *affiliatePerfCountingConn) Ping(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return nil
}

func (c *affiliatePerfCountingConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *affiliatePerfCountingConn) IsValid() bool {
	if conn, ok := c.Conn.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

type affiliatePerfFixture struct {
	db          *sqlx.DB
	dbPath      string
	counter     *affiliatePerfQueryCounter
	params      AffiliateStatsParams
	successRows int64
}

func installAffiliatePerfFixture(t *testing.T) affiliatePerfFixture {
	t.Helper()
	started := time.Now()
	dbPath := filepath.Join(t.TempDir(), "affiliate-performance.sqlite")
	counter := &affiliatePerfQueryCounter{}
	driverName := fmt.Sprintf("affiliate-performance-sqlite-%d", time.Now().UnixNano())
	sql.Register(driverName, &affiliatePerfCountingDriver{
		inner:   &modernsqlite.Driver{},
		counter: counter,
	})

	rawDB, err := sql.Open(driverName, dbPath)
	if err != nil {
		t.Fatalf("open affiliate performance SQLite fixture: %v", err)
	}
	db := sqlx.NewDb(rawDB, "sqlite")
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping affiliate performance SQLite fixture: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = OFF",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -65536",
		"PRAGMA mmap_size = 268435456",
	} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("apply fixture pragma %q: %v", pragma, err)
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			display_name TEXT,
			aff_count INTEGER NOT NULL DEFAULT 0,
			inviter_id INTEGER,
			deleted_at INTEGER
		);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			amount INTEGER,
			money REAL,
			trade_no TEXT NOT NULL UNIQUE,
			payment_method TEXT,
			status TEXT,
			create_time INTEGER,
			complete_time INTEGER
		);
	`); err != nil {
		t.Fatalf("create affiliate performance schema: %v", err)
	}

	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin affiliate performance fixture transaction: %v", err)
	}
	userStmt, err := tx.Prepare(`INSERT INTO users
		(id, username, display_name, aff_count, inviter_id, deleted_at)
		VALUES (?, ?, ?, ?, ?, NULL)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare performance user insert: %v", err)
	}
	topUpStmt, err := tx.Prepare(`INSERT INTO top_ups
		(id, user_id, amount, money, trade_no, payment_method, status, create_time, complete_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = userStmt.Close()
		_ = tx.Rollback()
		t.Fatalf("prepare performance top-up insert: %v", err)
	}

	windowStart := time.Date(2026, 6, 1, 0, 0, 0, 0, inviteTopUpLocation).Unix()
	statuses := [...]string{"success", "completed", "1", "pending"}
	for userID := int64(1); userID <= affiliatePerfUserCount; userID++ {
		var inviterID any
		if userID > affiliatePerfInviterCount {
			ordinal := userID - affiliatePerfInviterCount - 1
			if ordinal < 9_000 {
				inviterID = int64(1)
			} else {
				inviterID = int64(2) + (ordinal-9_000)%(affiliatePerfInviterCount-1)
			}
		}
		if _, err := userStmt.Exec(
			userID,
			fmt.Sprintf("perf-user-%06d", userID),
			fmt.Sprintf("Performance User %06d", userID),
			(userID*17)%1_000,
			inviterID,
		); err != nil {
			_ = topUpStmt.Close()
			_ = userStmt.Close()
			_ = tx.Rollback()
			t.Fatalf("insert performance user %d: %v", userID, err)
		}
		if userID <= affiliatePerfInviterCount {
			continue
		}

		ordinal := userID - affiliatePerfInviterCount - 1
		topUpID := ordinal + 1
		completeTime := windowStart + (ordinal%30)*24*60*60 + (ordinal*37)%(24*60*60)
		amount := int64(1_000) + ordinal%10_000
		paymentMethod := "alipay"
		if ordinal%2 == 1 {
			paymentMethod = "wechat"
		}
		if _, err := topUpStmt.Exec(
			topUpID,
			userID,
			amount,
			float64(amount)/100,
			fmt.Sprintf("perf-trade-%09d", topUpID),
			paymentMethod,
			statuses[ordinal%int64(len(statuses))],
			completeTime-300,
			completeTime,
		); err != nil {
			_ = topUpStmt.Close()
			_ = userStmt.Close()
			_ = tx.Rollback()
			t.Fatalf("insert performance top-up %d: %v", topUpID, err)
		}
	}
	if err := topUpStmt.Close(); err != nil {
		_ = userStmt.Close()
		_ = tx.Rollback()
		t.Fatalf("close performance top-up statement: %v", err)
	}
	if err := userStmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close performance user statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit affiliate performance fixture: %v", err)
	}

	// These indexes mirror the relevant indexes in the checked-in NewAPI
	// PostgreSQL schema export. In particular, the fixture does not invent a
	// complete_time/status index that production does not currently have.
	if _, err := db.Exec(`
		CREATE INDEX idx_users_deleted_at ON users(deleted_at);
		CREATE INDEX idx_users_display_name ON users(display_name);
		CREATE INDEX idx_users_inviter_id ON users(inviter_id);
		CREATE INDEX idx_users_username ON users(username);
		CREATE INDEX idx_top_ups_trade_no ON top_ups(trade_no);
		CREATE INDEX idx_top_ups_user_id ON top_ups(user_id);
		CREATE INDEX idx_topups_create_time ON top_ups(create_time);
		ANALYZE;
	`); err != nil {
		t.Fatalf("create/analyze affiliate performance indexes: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint affiliate performance fixture: %v", err)
	}
	if _, err := db.Exec("PRAGMA synchronous = NORMAL"); err != nil {
		t.Fatalf("restore SQLite synchronous mode: %v", err)
	}
	if _, err := db.Exec("PRAGMA optimize"); err != nil {
		t.Fatalf("optimize affiliate performance fixture: %v", err)
	}

	var facts struct {
		Users        int64 `db:"users"`
		TopUps       int64 `db:"top_ups"`
		SuccessRows  int64 `db:"success_rows"`
		CoveredDays  int64 `db:"covered_days"`
		MinCompleted int64 `db:"min_completed"`
		MaxCompleted int64 `db:"max_completed"`
	}
	if err := db.Get(&facts, `SELECT
		(SELECT COUNT(*) FROM users) AS users,
		(SELECT COUNT(*) FROM top_ups) AS top_ups,
		(SELECT COUNT(*) FROM top_ups WHERE LOWER(TRIM(status)) IN ('success', 'completed') OR TRIM(status) = '1') AS success_rows,
		(SELECT COUNT(DISTINCT date(complete_time, 'unixepoch', '+8 hours')) FROM top_ups) AS covered_days,
		(SELECT MIN(complete_time) FROM top_ups) AS min_completed,
		(SELECT MAX(complete_time) FROM top_ups) AS max_completed`); err != nil {
		t.Fatalf("inspect affiliate performance fixture: %v", err)
	}
	windowEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, inviteTopUpLocation).Unix()
	if facts.Users != affiliatePerfUserCount || facts.TopUps != affiliatePerfTopUpCount || facts.CoveredDays != 30 {
		t.Fatalf("fixture scale users/top_ups/days = %d/%d/%d, want %d/%d/30",
			facts.Users, facts.TopUps, facts.CoveredDays, affiliatePerfUserCount, affiliatePerfTopUpCount)
	}
	if facts.MinCompleted < windowStart || facts.MaxCompleted >= windowEnd {
		t.Fatalf("fixture complete_time range [%d,%d] is outside [%d,%d)",
			facts.MinCompleted, facts.MaxCompleted, windowStart, windowEnd)
	}
	stat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat affiliate performance fixture: %v", err)
	}
	t.Logf("AFFILIATE_PERF fixture go=%s platform=%s/%s users=%d inviters=%d top_ups=%d success_rows=%d covered_days=%d db_bytes=%d setup=%s",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, facts.Users, affiliatePerfInviterCount, facts.TopUps, facts.SuccessRows, facts.CoveredDays, stat.Size(), time.Since(started))

	return affiliatePerfFixture{
		db: db, dbPath: dbPath, counter: counter,
		params: AffiliateStatsParams{
			Page: 1, PageSize: 100,
			StartDate: "2026-06-01", EndDate: "2026-06-30",
			SortBy: "success_topup_count", SortDir: "desc",
			AsOf: windowEnd,
		},
		successRows: facts.SuccessRows,
	}
}

func affiliatePerfQueryLabel(query string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(query), " "))
	switch {
	case strings.Contains(normalized, "select u.inviter_id, t.id, t.user_id"):
		return "batch_evidence"
	case strings.Contains(normalized, "select count(*) as window_active_inviter_count"):
		return "summary"
	case strings.Contains(normalized, "select a.inviter_id, iu.username as inviter_username"):
		return "parent_list"
	case strings.Contains(normalized, "select count(*) from (") && strings.Contains(normalized, "group by u.inviter_id"):
		return "parent_count"
	case strings.HasPrefix(normalized, "select 1 from users"):
		return "detail_inviter"
	case strings.HasPrefix(normalized, "select count(*) from top_ups"):
		return "detail_count"
	case strings.HasPrefix(normalized, "select t.id, t.user_id"):
		return "detail_page"
	default:
		return "query"
	}
}

func assertAffiliateParentQueryShape(
	t *testing.T,
	params AffiliateStatsParams,
	parent *PaginatedAffiliateStats,
	records []affiliatePerfQueryRecord,
) {
	t.Helper()
	if len(records) != 4 {
		labels := make([]string, len(records))
		for i := range records {
			labels[i] = affiliatePerfQueryLabel(records[i].SQL)
		}
		t.Fatalf("parent page executed %d queries (%v), want exactly count+summary+list+one batch evidence query", len(records), labels)
	}
	counts := make(map[string]int)
	var evidence affiliatePerfQueryRecord
	for _, record := range records {
		label := affiliatePerfQueryLabel(record.SQL)
		counts[label]++
		if label == "batch_evidence" {
			evidence = record
		}
	}
	for _, label := range []string{"parent_count", "summary", "parent_list", "batch_evidence"} {
		if counts[label] != 1 {
			t.Fatalf("parent query count for %s = %d, want 1 (all counts: %v)", label, counts[label], counts)
		}
	}
	query, err := normalizeAffiliateParams(params)
	if err != nil {
		t.Fatalf("normalize parent params for query-count proof: %v", err)
	}
	_, baseArgs, _ := buildAffiliateAggWhere(query)
	batchedInviters := len(evidence.Args) - len(baseArgs)
	if batchedInviters != len(parent.Items) || batchedInviters != params.PageSize {
		t.Fatalf("batch evidence inviter args = %d, parent items/page_size = %d/%d; evidence is not one full-page batch",
			batchedInviters, len(parent.Items), params.PageSize)
	}
	if !strings.Contains(strings.ToUpper(evidence.SQL), " IN (") {
		t.Fatalf("evidence statement is not an IN batch: %s", evidence.SQL)
	}
	t.Logf("AFFILIATE_PERF query_count parent_total=%d heavy_aggregates=3 batch_evidence=1 batched_inviters=%d",
		len(records), batchedInviters)
}

func explainAffiliatePerfQueries(t *testing.T, db *sqlx.DB, scope string, records []affiliatePerfQueryRecord) {
	t.Helper()
	for i, record := range records {
		rows, err := db.Queryx("EXPLAIN QUERY PLAN "+record.SQL, record.Args...)
		if err != nil {
			t.Fatalf("EXPLAIN %s query %d (%s): %v", scope, i+1, affiliatePerfQueryLabel(record.SQL), err)
		}
		plan := make([]string, 0, 8)
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatalf("scan EXPLAIN %s query %d: %v", scope, i+1, err)
			}
			plan = append(plan, fmt.Sprintf("%d/%d %s", id, parent, detail))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterate EXPLAIN %s query %d: %v", scope, i+1, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close EXPLAIN %s query %d: %v", scope, i+1, err)
		}
		if len(plan) == 0 {
			t.Fatalf("EXPLAIN %s query %d returned no plan rows", scope, i+1)
		}
		normalizedSQL := strings.Join(strings.Fields(record.SQL), " ")
		t.Logf("AFFILIATE_PERF EXPLAIN scope=%s label=%s args=%d sql=%q plan=%q",
			scope, affiliatePerfQueryLabel(record.SQL), len(record.Args), normalizedSQL, plan)
	}
}

type affiliatePerfTiming struct {
	min    time.Duration
	median time.Duration
	p95    time.Duration
	max    time.Duration
}

func measureAffiliatePerfP95(
	t *testing.T,
	name string,
	limit time.Duration,
	fn func() error,
) affiliatePerfTiming {
	t.Helper()
	for i := 0; i < affiliatePerfWarmups; i++ {
		if err := fn(); err != nil {
			t.Fatalf("%s warmup %d/%d: %v", name, i+1, affiliatePerfWarmups, err)
		}
	}
	durations := make([]time.Duration, affiliatePerfSamples)
	for i := range durations {
		started := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s measured run %d/%d: %v", name, i+1, affiliatePerfSamples, err)
		}
		durations[i] = time.Since(started)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95Index := int(math.Ceil(0.95*float64(len(durations)))) - 1
	timing := affiliatePerfTiming{
		min: durations[0], median: durations[len(durations)/2],
		p95: durations[p95Index], max: durations[len(durations)-1],
	}
	t.Logf("AFFILIATE_PERF timing name=%s warmups=%d samples=%d min=%s median=%s p95=%s max=%s limit=%s",
		name, affiliatePerfWarmups, affiliatePerfSamples, timing.min, timing.median, timing.p95, timing.max, limit)
	return timing
}

func TestAffiliateStatsPerformanceAcceptance(t *testing.T) {
	if os.Getenv("AFFILIATE_PERF") != "1" {
		t.Skip("set AFFILIATE_PERF=1 to run the 100000-user affiliate performance acceptance")
	}
	fixture := installAffiliatePerfFixture(t)

	fixture.counter.start()
	parent, parentErr := ListAffiliateStatsContext(context.Background(), fixture.params)
	parentRecords := fixture.counter.stop()
	if parentErr != nil {
		t.Fatalf("capture parent query path: %v", parentErr)
	}
	if parent.Total != affiliatePerfInviterCount || len(parent.Items) != fixture.params.PageSize {
		t.Fatalf("parent result total/items = %d/%d, want %d/%d", parent.Total, len(parent.Items), affiliatePerfInviterCount, fixture.params.PageSize)
	}
	if parent.Summary.SuccessTopUpCount != fixture.successRows {
		t.Fatalf("parent bundled summary success rows = %d, want %d", parent.Summary.SuccessTopUpCount, fixture.successRows)
	}
	assertAffiliateParentQueryShape(t, fixture.params, parent, parentRecords)

	target := parent.Items[0]
	detailTotal := target.SuccessTopUpCount
	detailParams := fixture.params
	detailParams.ExpectedFingerprint = parent.QueryFingerprint
	detailParams.ExpectedDetailTotal = &detailTotal
	detailParams.ExpectedDetailEvidenceHash = target.DetailEvidenceHash
	fixture.counter.start()
	detail, detailErr := ListAffiliateTopUpDetailsContext(context.Background(), target.InviterID, detailParams)
	detailRecords := fixture.counter.stop()
	if detailErr != nil {
		t.Fatalf("capture detail query path: %v", detailErr)
	}
	if detail.Total != detailTotal || len(detail.Items) != detailParams.PageSize {
		t.Fatalf("detail result total/items = %d/%d, want %d/%d", detail.Total, len(detail.Items), detailTotal, detailParams.PageSize)
	}
	if len(detailRecords) != 4 {
		t.Fatalf("detail path executed %d queries, want inviter check+count+evidence+page", len(detailRecords))
	}

	explainAffiliatePerfQueries(t, fixture.db, "parent", parentRecords)
	explainAffiliatePerfQueries(t, fixture.db, "detail", detailRecords)

	callWithTimeout := func(fn func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return fn(ctx)
	}
	listTiming := measureAffiliatePerfP95(t, "affiliate_list_with_bundled_summary", affiliatePerfListLimit, func() error {
		return callWithTimeout(func(ctx context.Context) error {
			result, err := ListAffiliateStatsContext(ctx, fixture.params)
			if err != nil {
				return err
			}
			if result.Total != affiliatePerfInviterCount || len(result.Items) != fixture.params.PageSize || result.Summary.SuccessTopUpCount != fixture.successRows {
				return fmt.Errorf("unexpected list result total/items/success = %d/%d/%d", result.Total, len(result.Items), result.Summary.SuccessTopUpCount)
			}
			return nil
		})
	})
	summaryTiming := measureAffiliatePerfP95(t, "affiliate_standalone_summary", affiliatePerfSummaryLimit, func() error {
		return callWithTimeout(func(ctx context.Context) error {
			result, err := GetAffiliateStatsSummaryContext(ctx, fixture.params)
			if err != nil {
				return err
			}
			if result.WindowActiveInviterCount != affiliatePerfInviterCount || result.SuccessTopUpCount != fixture.successRows {
				return fmt.Errorf("unexpected summary active/success = %d/%d", result.WindowActiveInviterCount, result.SuccessTopUpCount)
			}
			return nil
		})
	})
	detailTiming := measureAffiliatePerfP95(t, "affiliate_detail", affiliatePerfDetailLimit, func() error {
		return callWithTimeout(func(ctx context.Context) error {
			result, err := ListAffiliateTopUpDetailsContext(ctx, target.InviterID, detailParams)
			if err != nil {
				return err
			}
			if result.Total != detailTotal || len(result.Items) != detailParams.PageSize || result.DetailEvidenceHash != target.DetailEvidenceHash {
				return fmt.Errorf("unexpected detail total/items/evidence = %d/%d/%t", result.Total, len(result.Items), result.DetailEvidenceHash == target.DetailEvidenceHash)
			}
			return nil
		})
	})

	var sloFailures []string
	if listTiming.p95 > affiliatePerfListLimit {
		sloFailures = append(sloFailures, fmt.Sprintf("list p95 %s > %s", listTiming.p95, affiliatePerfListLimit))
	}
	if summaryTiming.p95 > affiliatePerfSummaryLimit {
		sloFailures = append(sloFailures, fmt.Sprintf("summary p95 %s > %s", summaryTiming.p95, affiliatePerfSummaryLimit))
	}
	if detailTiming.p95 > affiliatePerfDetailLimit {
		sloFailures = append(sloFailures, fmt.Sprintf("detail p95 %s > %s", detailTiming.p95, affiliatePerfDetailLimit))
	}
	if len(sloFailures) > 0 {
		t.Fatalf("affiliate performance SLO failure: %s", strings.Join(sloFailures, "; "))
	}
}
