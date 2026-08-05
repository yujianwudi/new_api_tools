package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	modernsqlite "modernc.org/sqlite"
)

func scanAffiliateEvidenceDecimalForTest(t *testing.T, source any) affiliateEvidenceDecimal {
	t.Helper()
	var value affiliateEvidenceDecimal
	if err := value.Scan(source); err != nil {
		t.Fatalf("scan affiliate evidence decimal from %T: %v", source, err)
	}
	return value
}

func hashAffiliateEvidenceDecimalForTest(t *testing.T, source any) [sha256.Size]byte {
	t.Helper()
	value := scanAffiliateEvidenceDecimalForTest(t, source)
	digest := sha256.New()
	writeAffiliateEvidenceDecimal(digest, value)
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func TestAffiliateEvidenceDecimalCanonicalizesEquivalentForms(t *testing.T) {
	tests := map[string]string{
		"1.2500":       "125e-2",
		"125e-2":       "125e-2",
		"+001.25":      "125e-2",
		"0.0125e2":     "125e-2",
		"100":          "1e2",
		"00100.000e+0": "1e2",
		".5":           "5e-1",
		"1.":           "1e0",
		"-0.000":       "0",
	}
	for source, want := range tests {
		source, want := source, want
		t.Run(source, func(t *testing.T) {
			got := scanAffiliateEvidenceDecimalForTest(t, source)
			if !got.Valid || got.Canonical != want {
				t.Fatalf("canonical decimal = %+v, want valid %q", got, want)
			}
		})
	}

	wantHash := hashAffiliateEvidenceDecimalForTest(t, "1.2500")
	for _, equivalent := range []any{"125e-2", []byte("+001.25"), float64(1.25)} {
		if got := hashAffiliateEvidenceDecimalForTest(t, equivalent); got != wantHash {
			t.Fatalf("equivalent %T value produced hash %x, want %x", equivalent, got, wantHash)
		}
	}
}

func TestAffiliateEvidenceDecimalDriverFormsHashIdentically(t *testing.T) {
	sources := []any{"125.0", []byte("1.25e2"), int64(125), float64(125)}
	want := hashAffiliateEvidenceDecimalForTest(t, sources[0])
	for _, source := range sources {
		value := scanAffiliateEvidenceDecimalForTest(t, source)
		if value.Canonical != "125e0" {
			t.Fatalf("canonical %T value = %q, want 125e0", source, value.Canonical)
		}
		if got := hashAffiliateEvidenceDecimalForTest(t, source); got != want {
			t.Fatalf("%T hash = %x, want %x", source, got, want)
		}
	}
}

func TestAffiliateEvidenceDecimalNullAndZeroRemainDistinct(t *testing.T) {
	nullValue := scanAffiliateEvidenceDecimalForTest(t, nil)
	if nullValue.Valid || nullValue.Canonical != "" {
		t.Fatalf("NULL decimal = %+v, want invalid empty value", nullValue)
	}
	if nullHash, zeroHash := hashAffiliateEvidenceDecimalForTest(t, nil), hashAffiliateEvidenceDecimalForTest(t, "0"); nullHash == zeroHash {
		t.Fatalf("NULL and numeric zero produced the same evidence hash %x", nullHash)
	}

	want := hashAffiliateEvidenceDecimalForTest(t, "0")
	for _, negativeZero := range []any{"-0", "-0.000e+999", math.Copysign(0, -1)} {
		value := scanAffiliateEvidenceDecimalForTest(t, negativeZero)
		if value.Canonical != "0" {
			t.Fatalf("negative zero %T canonicalized as %q, want 0", negativeZero, value.Canonical)
		}
		if got := hashAffiliateEvidenceDecimalForTest(t, negativeZero); got != want {
			t.Fatalf("negative zero %T hash = %x, want %x", negativeZero, got, want)
		}
	}
}

func TestAffiliateEvidenceDecimalRejectsUnsafeDriverValues(t *testing.T) {
	invalid := []any{
		"",
		".",
		"+",
		"1.2.3",
		"1e",
		"1e+",
		" 1",
		"1 ",
		"NaN",
		"Infinity",
		"0x10",
		strings.Repeat("1", maximumAffiliateEvidenceDecimalInputBytes+1),
		strings.Repeat("1", maximumAffiliateEvidenceDecimalCoefficientDigits+1),
		fmt.Sprintf("1e%d", maximumAffiliateEvidenceDecimalExponentMagnitude+1),
		fmt.Sprintf("10e%d", maximumAffiliateEvidenceDecimalExponentMagnitude),
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		int32(1),
	}
	for index, source := range invalid {
		t.Run(fmt.Sprintf("case-%02d-%T", index, source), func(t *testing.T) {
			value := affiliateEvidenceDecimal{Canonical: "stale", Valid: true}
			if err := value.Scan(source); err == nil {
				t.Fatalf("Scan(%T) succeeded with %+v, want fail-closed error", source, value)
			}
			if value.Valid || value.Canonical != "" {
				t.Fatalf("failed Scan(%T) retained stale state %+v", source, value)
			}
		})
	}
}

func useAffiliateStatsGuardrailsForTest(t *testing.T, rowCap int64, timeout time.Duration, concurrency int) {
	t.Helper()
	if active := affiliateQueryLimiter.activeCount(); active != 0 {
		t.Fatalf("cannot replace affiliate guardrails with %d active queries", active)
	}
	previous := currentAffiliateStatsGuardrails()
	affiliateStatsGuardrailConfig.Store(&affiliateStatsGuardrails{
		evidenceRowCap:      rowCap,
		queryTimeout:        timeout,
		queryMaxConcurrency: concurrency,
	})
	t.Cleanup(func() {
		if active := affiliateQueryLimiter.activeCount(); active != 0 {
			t.Fatalf("affiliate guardrail test leaked %d active queries", active)
		}
		affiliateStatsGuardrailConfig.Store(&previous)
	})
}

func TestAffiliateEvidenceCapMinusOneAndCapHashCompletely(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)
	useAffiliateStatsGuardrailsForTest(t, 5, 5*time.Second, 2)

	assertCompleteEvidence := func(name, endDate string, wantRows int64) *PaginatedAffiliateStats {
		t.Helper()
		params := AffiliateStatsParams{
			Page: 1, PageSize: 20, Search: "Alice",
			StartDate: "2026-01-02", EndDate: endDate,
			AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
		}
		parent, err := ListAffiliateStats(params)
		if err != nil {
			t.Fatalf("%s parent query: %v", name, err)
		}
		if len(parent.Items) != 1 || parent.Items[0].SuccessTopUpCount != wantRows || len(parent.Items[0].DetailEvidenceHash) != 64 {
			t.Fatalf("%s parent evidence = %+v, want %d rows and full SHA-256", name, parent.Items, wantRows)
		}
		detailParams := params
		detailParams.ExpectedFingerprint = parent.QueryFingerprint
		detailParams.ExpectedDetailTotal = int64Pointer(wantRows)
		detailParams.ExpectedDetailEvidenceHash = parent.Items[0].DetailEvidenceHash
		detail, err := ListAffiliateTopUpDetails(parent.Items[0].InviterID, detailParams)
		if err != nil {
			t.Fatalf("%s detail query: %v", name, err)
		}
		if detail.Total != wantRows || detail.DetailEvidenceHash != parent.Items[0].DetailEvidenceHash || len(detail.DetailEvidenceHash) != 64 {
			t.Fatalf("%s detail evidence = %+v, want complete parent-bound hash", name, detail)
		}
		return parent
	}

	assertCompleteEvidence("cap-minus-one", "2026-01-02", 4)
	parentAtCap := assertCompleteEvidence("cap", "2026-01-03", 5)

	database.Get().DB.MustExec(`INSERT INTO top_ups
		(id, user_id, amount, money, status, create_time, complete_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		8, 10, 80, 8.0, "success",
		inviteTopUpTestTimestamp(t, "2026-01-03 12:00:00"),
		inviteTopUpTestTimestamp(t, "2026-01-03 12:05:00"),
	)
	capPlusOneParams := AffiliateStatsParams{
		Page: 1, PageSize: 20, Search: "Alice",
		StartDate: "2026-01-02", EndDate: "2026-01-03",
		AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
	}
	parent, err := ListAffiliateStats(capPlusOneParams)
	if parent != nil || !errors.Is(err, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("cap+1 parent result/error = %#v/%v, want nil scale-exceeded", parent, err)
	}
	if err != nil && strings.Contains(err.Error(), parentAtCap.Items[0].DetailEvidenceHash) {
		t.Fatalf("cap+1 parent error leaked a prior evidence hash: %v", err)
	}

	detailParams := capPlusOneParams
	detailParams.ExpectedFingerprint = parentAtCap.QueryFingerprint
	detailParams.ExpectedDetailTotal = int64Pointer(5)
	detailParams.ExpectedDetailEvidenceHash = parentAtCap.Items[0].DetailEvidenceHash
	detail, err := ListAffiliateTopUpDetails(1, detailParams)
	if detail != nil || !errors.Is(err, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("cap+1 detail result/error = %#v/%v, want nil scale-exceeded", detail, err)
	}
}

func TestAffiliatePageEvidencePreflightRejectsNegativeAndInt64Overflow(t *testing.T) {
	if _, err := preflightAffiliatePageEvidenceRows(
		[]AffiliateStatsRow{{InviterID: 1, SuccessTopUpCount: -1}},
		defaultAffiliateEvidenceRowCap,
	); err == nil || errors.Is(err, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("negative aggregate error = %v, want unavailable data-integrity failure", err)
	}
	if _, err := preflightAffiliatePageEvidenceRows(
		[]AffiliateStatsRow{
			{InviterID: 1, SuccessTopUpCount: math.MaxInt64},
			{InviterID: 2, SuccessTopUpCount: 1},
		},
		math.MaxInt64,
	); !errors.Is(err, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("overflow aggregate error = %v, want scale-exceeded", err)
	}
}

func installAffiliateCountingSQLiteFixture(t *testing.T, postgresPlaceholders bool) (*sqlx.DB, *affiliatePerfQueryCounter) {
	t.Helper()
	counter := &affiliatePerfQueryCounter{}
	driverName := fmt.Sprintf("affiliate-evidence-limit-sqlite-%d", time.Now().UnixNano())
	sql.Register(driverName, &affiliatePerfCountingDriver{inner: &modernsqlite.Driver{}, counter: counter})
	rawDB, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("open SQLite evidence fixture: %v", err)
	}
	db := sqlx.NewDb(rawDB, "sqlite")
	db.SetMaxOpenConns(1)
	database.SetForTesting(&database.Manager{DB: db, IsPG: postgresPlaceholders})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db, counter
}

func TestAffiliateParentCapPreflightSkipsEvidenceSQL(t *testing.T) {
	db, counter := installAffiliateCountingSQLiteFixture(t, false)
	useAffiliateStatsGuardrailsForTest(t, 1, 5*time.Second, 2)
	db.MustExec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY, username TEXT, display_name TEXT, aff_count INTEGER,
			inviter_id INTEGER, deleted_at INTEGER
		);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER, money REAL,
			status TEXT, create_time INTEGER, complete_time INTEGER
		);
		INSERT INTO users VALUES (1, 'inviter', NULL, 0, NULL, NULL), (10, 'invitee', NULL, 0, 1, NULL);
		INSERT INTO top_ups VALUES
			(1, 10, 1, 1.0, 'success', 1, 1),
			(2, 10, 2, 2.0, 'success', 2, 2);
	`)
	counter.start()
	result, listErr := ListAffiliateStats(AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
	records := counter.stop()
	if result != nil || !errors.Is(listErr, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("parent cap preflight result/error = %#v/%v, want nil scale-exceeded", result, listErr)
	}
	counts := map[string]int{}
	for _, record := range records {
		counts[affiliatePerfQueryLabel(record.SQL)]++
	}
	if counts["parent_count"] != 1 || counts["summary"] != 1 || counts["parent_list"] != 1 {
		t.Fatalf("parent cap preflight query counts = %v, want count+summary+list", counts)
	}
	if counts["batch_evidence"] != 0 {
		t.Fatalf("parent cap preflight executed %d evidence queries, want zero", counts["batch_evidence"])
	}

	detailParams := AffiliateStatsParams{
		Page: 1, PageSize: 20, AsOf: 10,
		ExpectedDetailTotal:        int64Pointer(1),
		ExpectedDetailEvidenceHash: strings.Repeat("a", 64),
	}
	normalized, err := normalizeAffiliateParams(detailParams)
	if err != nil {
		t.Fatal(err)
	}
	detailParams.ExpectedFingerprint = normalized.fingerprint
	counter.start()
	detail, detailErr := ListAffiliateTopUpDetails(1, detailParams)
	detailRecords := counter.stop()
	if detail != nil || !errors.Is(detailErr, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("detail cap preflight result/error = %#v/%v, want nil scale-exceeded", detail, detailErr)
	}
	detailCounts := map[string]int{}
	for _, record := range detailRecords {
		detailCounts[affiliatePerfQueryLabel(record.SQL)]++
	}
	if detailCounts["detail_inviter"] != 1 || detailCounts["detail_count"] != 1 {
		t.Fatalf("detail cap preflight query counts = %v, want inviter+count", detailCounts)
	}
	if detailCounts["batch_evidence"] != 0 || detailCounts["detail_page"] != 0 {
		t.Fatalf("detail cap preflight ran evidence/page queries: %v", detailCounts)
	}
}

func TestAffiliateEvidenceSQLiteQueryUsesCapPlusOneLimitPlaceholder(t *testing.T) {
	db, counter := installAffiliateCountingSQLiteFixture(t, false)
	db.MustExec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, inviter_id INTEGER);
		CREATE TABLE top_ups (id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER, money REAL, status TEXT, complete_time INTEGER);
		INSERT INTO users VALUES (1, 'inviter', NULL), (10, 'invitee', 1);
		INSERT INTO top_ups VALUES (1, 10, 1, 1.0, 'success', 1), (2, 10, 2, 2.0, 'success', 2);
	`)
	query, err := normalizeAffiliateParams(AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTxx(context.Background(), affiliateReadTxOptions(database.Get()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	counter.start()
	hashes, evidenceErr := affiliateDetailEvidenceHashes(context.Background(), tx, query, []int64{1}, 1, 1)
	records := counter.stop()
	if hashes != nil || !errors.Is(evidenceErr, ErrAffiliateEvidenceScaleExceeded) {
		t.Fatalf("defensive cap result/error = %#v/%v, want nil scale-exceeded", hashes, evidenceErr)
	}
	if len(records) != 1 {
		t.Fatalf("evidence query count = %d, want 1", len(records))
	}
	record := records[0]
	if !strings.Contains(strings.ToUpper(record.SQL), "LIMIT ?") {
		t.Fatalf("SQLite evidence SQL does not use a LIMIT placeholder: %s", record.SQL)
	}
	if placeholders := strings.Count(record.SQL, "?"); placeholders != len(record.Args) {
		t.Fatalf("SQLite placeholders/args = %d/%d; sql=%s args=%v", placeholders, len(record.Args), record.SQL, record.Args)
	}
	if len(record.Args) == 0 || record.Args[len(record.Args)-1] != int64(2) {
		t.Fatalf("SQLite LIMIT arg = %#v, want cap+1 = 2", record.Args)
	}
}

func TestAffiliateEvidencePostgreSQLLimitUsesNextNumberedPlaceholder(t *testing.T) {
	db, counter := installAffiliateCountingSQLiteFixture(t, true)
	db.MustExec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, inviter_id INTEGER);
		CREATE TABLE top_ups (id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER, money REAL, status TEXT, complete_time INTEGER);
		INSERT INTO users VALUES (1, 'inviter', NULL), (10, 'invitee', 1);
		INSERT INTO top_ups VALUES (1, 10, 1, 1.0, 'success', 1), (2, 10, 2, 2.0, 'success', 2);
	`)
	query, err := normalizeAffiliateParams(AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTxx(context.Background(), affiliateReadTxOptions(database.Get()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	counter.start()
	hashes, evidenceErr := affiliateDetailEvidenceHashes(context.Background(), tx, query, []int64{1}, 2, 2)
	records := counter.stop()
	if evidenceErr != nil || len(hashes[1]) != 64 {
		t.Fatalf("numbered-placeholder evidence result/error = %#v/%v", hashes, evidenceErr)
	}
	if len(records) != 1 {
		t.Fatalf("numbered-placeholder evidence query count = %d, want 1", len(records))
	}
	record := records[0]
	for _, placeholder := range []string{"$1", "$2", "$3", "LIMIT $4"} {
		if !strings.Contains(record.SQL, placeholder) {
			t.Fatalf("PostgreSQL-style evidence SQL missing %q: %s", placeholder, record.SQL)
		}
	}
	if len(record.Args) != 4 || record.Args[len(record.Args)-1] != int64(3) {
		t.Fatalf("numbered-placeholder args = %#v, want four args ending in cap+1 = 3", record.Args)
	}
}

type affiliateBlockingQueryState struct {
	enabled atomic.Bool
	active  atomic.Int32
	maximum atomic.Int32
	started chan struct{}
}

func (s *affiliateBlockingQueryState) enter() {
	current := s.active.Add(1)
	for {
		maximum := s.maximum.Load()
		if current <= maximum || s.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	s.started <- struct{}{}
}

type affiliateBlockingDriver struct {
	inner driver.Driver
	state *affiliateBlockingQueryState
}

func (d *affiliateBlockingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &affiliateBlockingConn{Conn: conn, state: d.state}, nil
}

type affiliateBlockingConn struct {
	driver.Conn
	state *affiliateBlockingQueryState
}

func (c *affiliateBlockingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *affiliateBlockingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.Conn.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *affiliateBlockingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	conn, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.ExecContext(ctx, query, args)
}

func (c *affiliateBlockingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.state.enabled.Load() {
		c.state.enter()
		defer c.state.active.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	conn, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.QueryContext(ctx, query, args)
}

func (c *affiliateBlockingConn) Ping(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return nil
}

func (c *affiliateBlockingConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *affiliateBlockingConn) IsValid() bool {
	if conn, ok := c.Conn.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

func installAffiliateBlockingQueryFixture(t *testing.T, maxOpen int) (*sqlx.DB, *affiliateBlockingQueryState) {
	t.Helper()
	state := &affiliateBlockingQueryState{started: make(chan struct{}, 32)}
	state.enabled.Store(true)
	driverName := fmt.Sprintf("affiliate-blocking-sqlite-%d", time.Now().UnixNano())
	sql.Register(driverName, &affiliateBlockingDriver{inner: &modernsqlite.Driver{}, state: state})
	rawDB, err := sql.Open(driverName, "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open blocking SQLite fixture: %v", err)
	}
	db := sqlx.NewDb(rawDB, "sqlite")
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping blocking SQLite fixture: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		state.enabled.Store(false)
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db, state
}

func waitForAffiliateConnectionsReleased(t *testing.T, db *sqlx.DB) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for db.Stats().InUse != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("affiliate query leaked %d database connections", inUse)
	}
}

func TestAffiliateQueryDeadlineAndCallerCancellationReleaseConnections(t *testing.T) {
	tests := []struct {
		name          string
		serverTimeout time.Duration
		callerTimeout time.Duration
	}{
		{name: "server deadline", serverTimeout: 30 * time.Millisecond},
		{name: "earlier caller deadline", serverTimeout: time.Second, callerTimeout: 20 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, state := installAffiliateBlockingQueryFixture(t, 1)
			useAffiliateStatsGuardrailsForTest(t, 100, test.serverTimeout, 1)
			ctx := context.Background()
			if test.callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.callerTimeout)
				defer cancel()
			}
			started := time.Now()
			result, err := ListAffiliateStatsContext(ctx, AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
			if result != nil || !errors.Is(err, ErrAffiliateStatsUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline result/error = %#v/%v, want nil unavailable+deadline", result, err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("deadline took %s, want bounded early return", elapsed)
			}
			state.enabled.Store(false)
			waitForAffiliateConnectionsReleased(t, db)
			var one int
			if err := db.Get(&one, "SELECT 1"); err != nil || one != 1 {
				t.Fatalf("database connection was not reusable after cancellation: one=%d err=%v", one, err)
			}
		})
	}
}

func TestAffiliateInvalidParamsRemainInvalidWhileQueryLimiterIsSaturated(t *testing.T) {
	useAffiliateStatsGuardrailsForTest(t, 100, time.Second, 1)
	if err := affiliateQueryLimiter.acquire(context.Background(), 1); err != nil {
		t.Fatalf("saturate affiliate query limiter: %v", err)
	}
	defer affiliateQueryLimiter.release()

	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "list",
			call: func(ctx context.Context) error {
				_, err := ListAffiliateStatsContext(ctx, AffiliateStatsParams{Page: -1, PageSize: 20, AsOf: 10})
				return err
			},
		},
		{
			name: "summary",
			call: func(ctx context.Context) error {
				_, err := GetAffiliateStatsSummaryContext(ctx, AffiliateStatsParams{Page: -1, PageSize: 20, AsOf: 10})
				return err
			},
		},
		{
			name: "detail",
			call: func(ctx context.Context) error {
				_, err := ListAffiliateTopUpDetailsContext(ctx, 0, AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := test.call(ctx)
			if !errors.Is(err, ErrInvalidAffiliateStatsParams) || errors.Is(err, ErrAffiliateStatsUnavailable) {
				t.Fatalf("saturated invalid request error = %v, want stable invalid-params", err)
			}
			if elapsed := time.Since(started); elapsed >= 20*time.Millisecond {
				t.Fatalf("invalid request waited on the saturated limiter for %s", elapsed)
			}
		})
	}
}

func TestAffiliateQueryConcurrencyNeverExceedsConfiguredLimit(t *testing.T) {
	db, state := installAffiliateBlockingQueryFixture(t, 8)
	useAffiliateStatsGuardrailsForTest(t, 100, 5*time.Second, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10}
	normalized, err := normalizeAffiliateParams(base)
	if err != nil {
		t.Fatal(err)
	}
	detail := base
	detail.ExpectedFingerprint = normalized.fingerprint
	detail.ExpectedDetailTotal = int64Pointer(0)
	detail.ExpectedDetailEvidenceHash = strings.Repeat("a", 64)
	operations := []func(context.Context) error{
		func(ctx context.Context) error { _, err := ListAffiliateStatsContext(ctx, base); return err },
		func(ctx context.Context) error { _, err := GetAffiliateStatsSummaryContext(ctx, base); return err },
		func(ctx context.Context) error {
			_, err := ListAffiliateTopUpDetailsContext(ctx, 1, detail)
			return err
		},
		func(ctx context.Context) error { _, err := ListAffiliateStatsContext(ctx, base); return err },
		func(ctx context.Context) error { _, err := GetAffiliateStatsSummaryContext(ctx, base); return err },
		func(ctx context.Context) error {
			_, err := ListAffiliateTopUpDetailsContext(ctx, 1, detail)
			return err
		},
	}
	errorsCh := make(chan error, len(operations))
	for _, operation := range operations {
		operation := operation
		go func() { errorsCh <- operation(ctx) }()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-state.started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for configured affiliate query slots")
		}
	}
	select {
	case <-state.started:
		t.Fatal("a third affiliate query reached the database beyond the configured limit")
	case <-time.After(50 * time.Millisecond):
	}
	if active := affiliateQueryLimiter.activeCount(); active != 2 {
		t.Fatalf("active affiliate operations = %d, want 2", active)
	}
	if inUse := db.Stats().InUse; inUse > 2 {
		t.Fatalf("database connections in use = %d, want at most 2 before cancellation", inUse)
	}

	cancel()
	for range operations {
		select {
		case err := <-errorsCh:
			if !errors.Is(err, ErrAffiliateStatsUnavailable) || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled affiliate operation error = %v, want unavailable+canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled affiliate operation did not return")
		}
	}
	if maximum := state.maximum.Load(); maximum > 2 {
		t.Fatalf("concurrent database queries reached %d, configured maximum is 2", maximum)
	}
	state.enabled.Store(false)
	waitForAffiliateConnectionsReleased(t, db)
}
