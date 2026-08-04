package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
)

type userManagementStaticRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *userManagementStaticRows) Columns() []string { return r.columns }
func (r *userManagementStaticRows) Close() error      { return nil }
func (r *userManagementStaticRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

type activityMutationDriverState struct {
	mu                  sync.Mutex
	currentRequestCount int64
	beginOptions        []driver.TxOptions
}

type activityMutationDriver struct {
	state *activityMutationDriverState
}

func (d *activityMutationDriver) Open(string) (driver.Conn, error) {
	return &activityMutationConn{state: d.state}, nil
}

type activityMutationConn struct {
	state                *activityMutationDriverState
	inTx                 bool
	snapshotRequestCount int64
}

func (c *activityMutationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("activity mutation test driver does not prepare statements")
}
func (c *activityMutationConn) Close() error { return nil }
func (c *activityMutationConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *activityMutationConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	c.snapshotRequestCount = c.state.currentRequestCount
	c.state.beginOptions = append(c.state.beginOptions, opts)
	c.state.mu.Unlock()
	c.inTx = true
	return &activityMutationTx{conn: c}, nil
}
func (c *activityMutationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	if strings.Contains(normalized, "as activity_sort_value") {
		if !c.inTx {
			return nil, errors.New("candidate query executed outside transaction")
		}
		// Simulate a concurrent commit after the lightweight candidate was
		// observed and before the full row is fetched.
		c.state.mu.Lock()
		c.state.currentRequestCount = 9
		c.state.mu.Unlock()
		return &userManagementStaticRows{
			columns: []string{"id", "request_count", "activity_sort_value"},
			values:  [][]driver.Value{{int64(1), c.snapshotRequestCount, c.snapshotRequestCount}},
		}, nil
	}
	if strings.Contains(normalized, " in (") {
		if !c.inTx {
			return nil, errors.New("page query executed outside transaction")
		}
		// Deliberately model a target driver that failed to preserve its read
		// snapshot. Production code must detect this mixed row and fail closed.
		c.state.mu.Lock()
		current := c.state.currentRequestCount
		c.state.mu.Unlock()
		return &userManagementStaticRows{
			columns: []string{"id", "request_count"},
			values:  [][]driver.Value{{int64(1), current}},
		}, nil
	}
	return nil, fmt.Errorf("unexpected activity query: %s", query)
}

type activityMutationTx struct {
	conn *activityMutationConn
}

func (tx *activityMutationTx) Commit() error {
	tx.conn.inTx = false
	return nil
}
func (tx *activityMutationTx) Rollback() error {
	tx.conn.inTx = false
	return nil
}

func TestActivityFilteredPageFailsClosedOnConcurrentRequestCountChange(t *testing.T) {
	state := &activityMutationDriverState{currentRequestCount: 1}
	driverName := fmt.Sprintf("user-activity-mutation-%d", time.Now().UnixNano())
	sql.Register(driverName, &activityMutationDriver{state: state})
	rawDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open activity mutation database: %v", err)
	}
	db := sqlx.NewDb(rawDB, driverName)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	svc := &UserManagementService{db: &database.Manager{DB: db}}
	asOf := int64(1_800_000_000)

	_, _, _, err = svc.activityFilteredUserPageWithLimit(
		"u.id, u.request_count",
		"u.deleted_at IS NULL AND u.request_count > 0",
		"request_count", "ASC", nil,
		ActivityVeryInactive,
		map[int64]int64{1: asOf - InactiveThreshold - 1},
		asOf, 1, 20, 10,
	)
	if !errors.Is(err, ErrActivityLogUnavailable) || !strings.Contains(err.Error(), "request_count changed") {
		t.Fatalf("error = %v, want fail-closed concurrent request_count error", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.currentRequestCount != 9 {
		t.Fatalf("concurrent mutation was not simulated: request_count=%d", state.currentRequestCount)
	}
	if len(state.beginOptions) != 1 || !state.beginOptions[0].ReadOnly ||
		state.beginOptions[0].Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
		t.Fatalf("read snapshot options = %#v", state.beginOptions)
	}
}

type oauthSchemaProbeRecord struct {
	query       string
	tableSchema string
	tableName   string
}

type oauthSchemaDriverState struct {
	mu            sync.Mutex
	currentSchema string
	searchPath    string
	tableSchema   string
	columns       map[string][]string
	identities    []string
	probes        []oauthSchemaProbeRecord
}

type oauthSchemaDriver struct {
	state *oauthSchemaDriverState
}

func (d *oauthSchemaDriver) Open(string) (driver.Conn, error) {
	return &oauthSchemaConn{state: d.state}, nil
}

type oauthSchemaConn struct {
	state *oauthSchemaDriverState
}

func (c *oauthSchemaConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("OAuth schema test driver does not prepare statements")
}
func (c *oauthSchemaConn) Close() error { return nil }
func (c *oauthSchemaConn) Begin() (driver.Tx, error) {
	return nil, errors.New("OAuth schema test driver does not begin transactions")
}
func (c *oauthSchemaConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if strings.Contains(normalized, "current_schema()") {
		c.state.identities = append(c.state.identities, query)
		return &userManagementStaticRows{
			columns: []string{"current_schema", "search_path", "table_schema"},
			values: [][]driver.Value{{
				c.state.currentSchema,
				c.state.searchPath,
				c.state.tableSchema,
			}},
		}, nil
	}
	if strings.Contains(normalized, "information_schema.columns") {
		if len(args) != 2 {
			return nil, fmt.Errorf("OAuth column probe args = %v", args)
		}
		schema, _ := args[0].Value.(string)
		table, _ := args[1].Value.(string)
		c.state.probes = append(c.state.probes, oauthSchemaProbeRecord{
			query: query, tableSchema: schema, tableName: table,
		})
		values := make([][]driver.Value, 0, len(c.state.columns[schema]))
		for _, column := range c.state.columns[schema] {
			values = append(values, []driver.Value{column})
		}
		return &userManagementStaticRows{columns: []string{"column_name"}, values: values}, nil
	}
	return nil, fmt.Errorf("unexpected OAuth schema query: %s", query)
}

func TestPostgresOAuthCapabilitiesAreScopedToResolvedSchema(t *testing.T) {
	state := &oauthSchemaDriverState{
		currentSchema: "tenant_a",
		searchPath:    `"tenant_a", public`,
		tableSchema:   "tenant_a",
		columns: map[string][]string{
			"tenant_a": {"id", "github_id"},
			"tenant_b": {"id", "oidc_id"},
		},
	}
	driverName := fmt.Sprintf("user-oauth-schema-%d", time.Now().UnixNano())
	sql.Register(driverName, &oauthSchemaDriver{state: state})
	rawDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open OAuth schema database: %v", err)
	}
	db := sqlx.NewDb(rawDB, "pgx")
	db.SetMaxOpenConns(1)
	svc := &UserManagementService{db: &database.Manager{DB: db, IsPG: true}}
	t.Cleanup(func() {
		svc.invalidateOAuthColumnCache()
		_ = db.Close()
	})

	columns, err := svc.probeAvailableOAuthColumns()
	if err != nil || fmt.Sprint(columns) != "[github_id]" {
		t.Fatalf("tenant_a OAuth columns = %v, %v", columns, err)
	}
	state.mu.Lock()
	state.currentSchema = "tenant_b"
	state.searchPath = `"tenant_b", public`
	state.tableSchema = "tenant_b"
	state.mu.Unlock()
	columns, err = svc.probeAvailableOAuthColumns()
	if err != nil || fmt.Sprint(columns) != "[oidc_id]" {
		t.Fatalf("tenant_b OAuth columns = %v, %v", columns, err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.identities) != 2 || len(state.probes) != 2 {
		t.Fatalf("schema identity/probe calls = %d/%d, want 2/2", len(state.identities), len(state.probes))
	}
	if !strings.Contains(strings.ToLower(state.identities[0]), "current_setting('search_path')") ||
		!strings.Contains(strings.ToLower(state.identities[0]), "to_regclass('users')") {
		t.Fatalf("schema identity SQL is not search-path aware: %s", state.identities[0])
	}
	for index, wantSchema := range []string{"tenant_a", "tenant_b"} {
		probe := state.probes[index]
		if probe.tableSchema != wantSchema || probe.tableName != "users" {
			t.Fatalf("probe[%d] identity = %q/%q", index, probe.tableSchema, probe.tableName)
		}
		normalized := strings.ToLower(probe.query)
		if !strings.Contains(normalized, "table_schema = $1") || !strings.Contains(normalized, "table_name = $2") {
			t.Fatalf("probe[%d] is not schema constrained: %s", index, probe.query)
		}
	}
}
