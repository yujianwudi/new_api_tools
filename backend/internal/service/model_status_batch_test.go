package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	modernsqlite "modernc.org/sqlite"
)

var modelStatusBatchDriverSequence atomic.Uint64

type modelStatusBatchQueryRecord struct {
	SQL  string
	Args []any
}

type modelStatusBatchQueryObserver struct {
	mu        sync.Mutex
	enabled   bool
	failOn    int
	records   []modelStatusBatchQueryRecord
	failure   error
	querySeen int
}

func (o *modelStatusBatchQueryObserver) start(failOn int) {
	o.mu.Lock()
	o.enabled = true
	o.failOn = failOn
	o.records = nil
	o.querySeen = 0
	o.failure = errors.New("injected model status batch query failure")
	o.mu.Unlock()
}

func (o *modelStatusBatchQueryObserver) stop() []modelStatusBatchQueryRecord {
	o.mu.Lock()
	o.enabled = false
	records := append([]modelStatusBatchQueryRecord(nil), o.records...)
	o.mu.Unlock()
	return records
}

func (o *modelStatusBatchQueryObserver) observe(query string, args []driver.NamedValue) error {
	if !isModelStatusBatchSlotQuery(query) {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.enabled {
		return nil
	}
	values := make([]any, len(args))
	for index := range args {
		values[index] = args[index].Value
	}
	o.records = append(o.records, modelStatusBatchQueryRecord{SQL: query, Args: values})
	o.querySeen++
	if o.failOn > 0 && o.querySeen == o.failOn {
		return o.failure
	}
	return nil
}

func isModelStatusBatchSlotQuery(query string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(query), " "))
	return strings.Contains(normalized, "select model_name, floor(") &&
		strings.Contains(normalized, "from logs where model_name in (") &&
		strings.Contains(normalized, "group by model_name, floor(")
}

// modelStatusBatchCountingDriver observes the exact database/sql statements
// emitted by the production service and can deterministically fail a selected
// batch. No production hook is needed.
type modelStatusBatchCountingDriver struct {
	inner    driver.Driver
	observer *modelStatusBatchQueryObserver
}

func (d *modelStatusBatchCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &modelStatusBatchCountingConn{Conn: conn, observer: d.observer}, nil
}

type modelStatusBatchCountingConn struct {
	driver.Conn
	observer *modelStatusBatchQueryObserver
}

func (c *modelStatusBatchCountingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *modelStatusBatchCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.Conn.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) || opts.ReadOnly {
		return nil, errors.New("wrapped SQLite driver does not support requested transaction options")
	}
	return c.Conn.Begin()
}

func (c *modelStatusBatchCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	conn, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.ExecContext(ctx, query, args)
}

func (c *modelStatusBatchCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.observer.observe(query, args); err != nil {
		return nil, err
	}
	conn, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return conn.QueryContext(ctx, query, args)
}

func (c *modelStatusBatchCountingConn) Ping(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return nil
}

func (c *modelStatusBatchCountingConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *modelStatusBatchCountingConn) IsValid() bool {
	if conn, ok := c.Conn.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

type modelStatusBatchFixture struct {
	db       *sqlx.DB
	service  *ModelStatusService
	observer *modelStatusBatchQueryObserver
}

func newModelStatusBatchFixture(t *testing.T) modelStatusBatchFixture {
	t.Helper()
	observer := &modelStatusBatchQueryObserver{}
	driverName := fmt.Sprintf("model-status-batch-sqlite-%d", modelStatusBatchDriverSequence.Add(1))
	sql.Register(driverName, &modelStatusBatchCountingDriver{
		inner: &modernsqlite.Driver{}, observer: observer,
	})
	rawDB, err := sql.Open(driverName, filepath.Join(t.TempDir(), "model-status-batch.sqlite"))
	if err != nil {
		t.Fatalf("open model status batch fixture: %v", err)
	}
	db := sqlx.NewDb(rawDB, "sqlite")
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping model status batch fixture: %v", err)
	}
	db.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model_name TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		type INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL
	)`)
	db.MustExec(`CREATE INDEX idx_logs_model_name ON logs(model_name)`)
	db.MustExec(`CREATE INDEX idx_logs_type_created_model ON logs(type, created_at, model_name)`)
	manager := &database.Manager{DB: db, IsPG: false}
	fixture := modelStatusBatchFixture{
		db: db, observer: observer,
		service: &ModelStatusService{db: manager, logDB: manager},
	}
	cache.Get().ClearLocal()
	t.Cleanup(func() {
		cache.Get().ClearLocal()
		_ = db.Close()
	})
	return fixture
}

func insertModelStatusBatchLogs(t *testing.T, db *sqlx.DB, rows []modelStatusBatchLogRow) {
	t.Helper()
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin model status batch fixture: %v", err)
	}
	statement, err := tx.Preparex(`INSERT INTO logs(model_name, created_at, type, completion_tokens)
		VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare model status batch fixture: %v", err)
	}
	defer statement.Close()
	for index, row := range rows {
		if _, err := statement.Exec(row.model, row.createdAt, row.logType, row.completionTokens); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert model status batch row %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit model status batch fixture: %v", err)
	}
	db.MustExec(`ANALYZE`)
}

type modelStatusBatchLogRow struct {
	model            string
	createdAt        int64
	logType          int
	completionTokens int
}

func modelStatusBatchNames(prefix string, count int) []string {
	names := make([]string, count)
	for index := range names {
		names[index] = fmt.Sprintf("%s-%04d", prefix, index)
	}
	return names
}

func insertOneFreshLogPerModel(t *testing.T, fixture modelStatusBatchFixture, names []string) {
	t.Helper()
	now := time.Now().Unix() - 1
	rows := make([]modelStatusBatchLogRow, len(names))
	for index, name := range names {
		rows[index] = modelStatusBatchLogRow{
			model: name, createdAt: now - int64(index%60), logType: 2, completionTokens: 1,
		}
	}
	insertModelStatusBatchLogs(t, fixture.db, rows)
}

func TestGetMultipleModelsStatusUsesAtMostTwoQueriesFor1000ColdModelsAndZeroWhenCached(t *testing.T) {
	fixture := newModelStatusBatchFixture(t)
	names := modelStatusBatchNames("counted-model", 1000)
	insertOneFreshLogPerModel(t, fixture, names)

	fixture.observer.start(0)
	results, err := fixture.service.GetMultipleModelsStatus(names, "24h")
	records := fixture.observer.stop()
	if err != nil {
		t.Fatalf("query 1000 cold model statuses: %v", err)
	}
	if len(results) != len(names) {
		t.Fatalf("cold batch returned %d models, want %d", len(results), len(names))
	}
	if len(records) != 2 {
		t.Fatalf("1000 cold models emitted %d aggregation queries, want exactly 2 (and at most 2)", len(records))
	}
	for index, record := range records {
		if len(record.Args) != modelStatusBatchChunkSize+2 {
			t.Fatalf("batch query %d has %d args, want %d model/time args", index+1,
				len(record.Args), modelStatusBatchChunkSize+2)
		}
		if placeholders := strings.Count(record.SQL, "?"); placeholders != modelStatusBatchChunkSize+2 {
			t.Fatalf("batch query %d has %d placeholders, want %d", index+1,
				placeholders, modelStatusBatchChunkSize+2)
		}
		wantFirst := names[index*modelStatusBatchChunkSize]
		wantLast := names[(index+1)*modelStatusBatchChunkSize-1]
		if record.Args[0] != wantFirst || record.Args[modelStatusBatchChunkSize-1] != wantLast {
			t.Fatalf("batch query %d model bounds = %v..%v, want %q..%q", index+1,
				record.Args[0], record.Args[modelStatusBatchChunkSize-1], wantFirst, wantLast)
		}
	}
	explainExactModelStatusBatchQuery(t, fixture.db, records[0])

	fixture.observer.start(0)
	cached, err := fixture.service.GetMultipleModelsStatus(names, "24h")
	cachedRecords := fixture.observer.stop()
	if err != nil || len(cached) != len(names) {
		t.Fatalf("read fully cached model statuses: models=%d err=%v", len(cached), err)
	}
	if len(cachedRecords) != 0 {
		t.Fatalf("fully cached model status batch emitted %d aggregation queries, want 0", len(cachedRecords))
	}
}

func TestGetMultipleModelsStatusSecondChunkFailureDoesNotPublishFirstChunkCache(t *testing.T) {
	fixture := newModelStatusBatchFixture(t)
	names := modelStatusBatchNames("failure-model", 1000)
	insertOneFreshLogPerModel(t, fixture, names)

	fixture.observer.start(2)
	results, err := fixture.service.GetMultipleModelsStatus(names, "24h")
	records := fixture.observer.stop()
	if err == nil || !strings.Contains(err.Error(), "chunk 500-1000") ||
		!strings.Contains(err.Error(), "injected model status batch query failure") {
		t.Fatalf("second chunk failure error = %v", err)
	}
	if results != nil {
		t.Fatalf("second chunk failure returned partial result with %d models", len(results))
	}
	if len(records) != 2 {
		t.Fatalf("second chunk failure observed %d batch queries, want 2", len(records))
	}

	// Manager.Set always publishes L1 before optional Redis. If none of the
	// requested keys exists in L1, the post-success publication loop never ran,
	// so no Redis write could have been initiated either.
	manager := cache.Get()
	for _, name := range names {
		var cached map[string]interface{}
		found, cacheErr := manager.GetJSON(fmt.Sprintf("model_status:%s:24h", name), &cached)
		if cacheErr != nil {
			t.Fatalf("inspect cache after failed batch for %q: %v", name, cacheErr)
		}
		if found {
			t.Fatalf("failed batch published model %q into cache: %+v", name, cached)
		}
	}
}

func TestGetMultipleModelsStatusMatchesSingleModelSemanticsAndSharesFetchedAt(t *testing.T) {
	fixture := newModelStatusBatchFixture(t)
	if cfg := config.GetOptional(); cfg != nil {
		previous := cfg.LogFreshnessMaxAge
		cfg.LogFreshnessMaxAge = 15 * time.Minute
		t.Cleanup(func() { cfg.LogFreshnessMaxAge = previous })
	}
	now := time.Now().UTC().Truncate(time.Second)
	names := []string{"fresh-success", "fresh-failure", "stale-success", "empty-model", "quoted'model"}
	insertModelStatusBatchLogs(t, fixture.db, []modelStatusBatchLogRow{
		{model: names[0], createdAt: now.Add(-2 * time.Second).Unix(), logType: 2, completionTokens: 1},
		{model: names[1], createdAt: now.Add(-3 * time.Second).Unix(), logType: 5, completionTokens: 0},
		{model: names[2], createdAt: now.Add(-2 * time.Hour).Unix(), logType: 2, completionTokens: 1},
		{model: names[4], createdAt: now.Add(-4 * time.Second).Unix(), logType: 2, completionTokens: 0},
	})

	singles := make([]map[string]interface{}, len(names))
	for index, name := range names {
		status, err := fixture.service.GetModelStatus(name, "24h")
		if err != nil {
			t.Fatalf("query single status for %q: %v", name, err)
		}
		singles[index] = status
	}
	cache.Get().ClearLocal()

	fixture.observer.start(0)
	batch, err := fixture.service.GetMultipleModelsStatus(names, "24h")
	records := fixture.observer.stop()
	if err != nil {
		t.Fatalf("query semantic model status batch: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("five cold semantic models emitted %d batch queries, want 1", len(records))
	}
	if len(batch) != len(names) {
		t.Fatalf("semantic batch returned %d models, want %d", len(batch), len(names))
	}

	sharedFetchedAt, ok := batch[0]["fetched_at"].(string)
	if !ok || sharedFetchedAt == "" {
		t.Fatalf("first batch miss lacks fetched_at: %+v", batch[0])
	}
	for index := range batch {
		if batch[index]["model_name"] != names[index] {
			t.Fatalf("batch order[%d] model=%v, want %q", index, batch[index]["model_name"], names[index])
		}
		if batch[index]["fetched_at"] != sharedFetchedAt {
			t.Fatalf("batch misses do not share fetched_at: first=%q model[%d]=%v",
				sharedFetchedAt, index, batch[index]["fetched_at"])
		}
		assertModelStatusSemanticEquivalent(t, batch[index], singles[index])
	}

	assertModelStatusState(t, batch[0], "fresh", "green", float64(100))
	assertModelStatusState(t, batch[1], "fresh", "red", float64(0))
	assertModelStatusState(t, batch[2], "stale", "unknown", nil)
	assertModelStatusState(t, batch[3], "empty", "unknown", nil)
	assertModelStatusState(t, batch[4], "fresh", "green", float64(100))
	if batch[4]["empty_count"] != int64(1) {
		t.Fatalf("quoted zero-completion model lost diagnostic empty count: %+v", batch[4])
	}
}

func assertModelStatusSemanticEquivalent(t *testing.T, batch, single map[string]interface{}) {
	t.Helper()
	for _, key := range []string{
		"model_name", "display_name", "time_window", "total_requests", "success_count",
		"failure_count", "empty_count", "success_rate", "current_status", "traffic_health",
		"source_state", "observed_status", "observed_rate", "last_traffic_at",
	} {
		if !reflect.DeepEqual(batch[key], single[key]) {
			t.Fatalf("batch/single field %q differs: batch=%#v single=%#v", key, batch[key], single[key])
		}
	}
	batchSlots, batchOK := batch["slot_data"].([]map[string]interface{})
	singleSlots, singleOK := single["slot_data"].([]map[string]interface{})
	if !batchOK || !singleOK || len(batchSlots) != len(singleSlots) {
		t.Fatalf("batch/single slot shapes differ: batch=%T/%d single=%T/%d",
			batch["slot_data"], len(batchSlots), single["slot_data"], len(singleSlots))
	}
	for index := range batchSlots {
		for _, key := range []string{
			"slot", "total_requests", "success_count", "failure_count", "empty_count", "success_rate", "status",
		} {
			if !reflect.DeepEqual(batchSlots[index][key], singleSlots[index][key]) {
				t.Fatalf("batch/single slot %d field %q differs: batch=%#v single=%#v",
					index, key, batchSlots[index][key], singleSlots[index][key])
			}
		}
	}
}

func assertModelStatusState(t *testing.T, status map[string]interface{}, source, current string, rate any) {
	t.Helper()
	if status["source_state"] != source || status["current_status"] != current ||
		!reflect.DeepEqual(status["success_rate"], rate) {
		t.Fatalf("model %v state/rate = %v/%v/%#v, want %s/%s/%#v", status["model_name"],
			status["source_state"], status["current_status"], status["success_rate"], source, current, rate)
	}
}

func explainExactModelStatusBatchQuery(t *testing.T, db *sqlx.DB, record modelStatusBatchQueryRecord) {
	t.Helper()
	rows, err := db.Queryx("EXPLAIN QUERY PLAN "+record.SQL, record.Args...)
	if err != nil {
		t.Fatalf("EXPLAIN exact model status batch SQL: %v", err)
	}
	defer rows.Close()
	details := make([]string, 0, 4)
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan model status batch EXPLAIN: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate model status batch EXPLAIN: %v", err)
	}
	plan := strings.ToLower(strings.Join(details, " | "))
	t.Logf("exact 500-model batch EXPLAIN: %s", strings.Join(details, " | "))
	if !strings.Contains(plan, "idx_logs_model_name") && !strings.Contains(plan, "idx_logs_type_created_model") {
		t.Fatalf("exact batch query did not use a model traffic index: %s", strings.Join(details, " | "))
	}
}
