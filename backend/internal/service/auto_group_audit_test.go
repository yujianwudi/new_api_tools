package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

type fakeAutoGroupAuditStore struct {
	mu sync.Mutex

	nextID     int64
	logs       []string
	allIDs     []int64
	pending    map[int64]string
	pingErr    error
	reserveErr error
	appendErr  error
	commitErr  error
	markerErr  error
	readErr    error
	committed  map[int64]struct{}
}

func (s *fakeAutoGroupAuditStore) Ping(context.Context) error {
	return s.pingErr
}

func (s *fakeAutoGroupAuditStore) ReserveIDs(_ context.Context, count int) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	if s.nextID == 0 {
		s.nextID = 1 << 40
	}
	ids := make([]int64, count)
	for i := range ids {
		s.nextID++
		ids[i] = s.nextID
	}
	return ids, nil
}

func (s *fakeAutoGroupAuditStore) StageLogs(_ context.Context, logs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	if s.pending == nil {
		s.pending = make(map[int64]string, len(logs))
	}
	for _, raw := range logs {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return err
		}
		id, ok := parseAutoGroupLogID(entry["id"])
		if !ok {
			return errors.New("invalid staged audit ID")
		}
		s.pending[id] = raw
	}
	return nil
}

func (s *fakeAutoGroupAuditStore) CommitLogs(_ context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return s.commitErr
	}
	staged := make([]string, len(ids))
	for i, id := range ids {
		raw, ok := s.pending[id]
		if !ok {
			return fmt.Errorf("missing pending audit log %d", id)
		}
		staged[i] = raw
	}
	if s.committed == nil {
		s.committed = make(map[int64]struct{}, len(ids))
	}
	newHead := make([]string, len(staged))
	for i, id := range ids {
		// Redis LPUSHes each command in order, so the last command becomes the
		// first list item.
		newHead[len(staged)-1-i] = staged[i]
		s.committed[id] = struct{}{}
		delete(s.pending, id)
		s.allIDs = append(s.allIDs, id)
	}
	s.logs = append(newHead, s.logs...)
	if len(s.logs) > int(autoGroupMaxLogs) {
		s.logs = s.logs[:autoGroupMaxLogs]
	}
	return nil
}

func (s *fakeAutoGroupAuditStore) IsCommitted(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markerErr != nil {
		return false, s.markerErr
	}
	_, ok := s.committed[id]
	return ok, nil
}

func (s *fakeAutoGroupAuditStore) ReadLogs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	return append([]string(nil), s.logs...), nil
}

func (s *fakeAutoGroupAuditStore) snapshot() ([]string, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...), append([]int64(nil), s.allIDs...)
}

func (s *fakeAutoGroupAuditStore) pendingSnapshot() map[int64]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[int64]string, len(s.pending))
	for id, raw := range s.pending {
		result[id] = raw
	}
	return result
}

func newAutoGroupAuditTestService(t *testing.T, store autoGroupAuditStore) (*sqlx.DB, *AutoGroupService) {
	t.Helper()
	setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})

	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// In-memory SQLite databases are connection-local. One connection also
	// makes transaction rollback assertions deterministic.
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		role INTEGER NOT NULL DEFAULT 1,
		"group" TEXT,
		status INTEGER NOT NULL DEFAULT 1,
		deleted_at INTEGER
	)`)
	t.Cleanup(func() { _ = db.Close() })

	manager := &database.Manager{DB: db, IsPG: false}
	return db, &AutoGroupService{db: manager, auditStore: store}
}

func insertAutoGroupUser(t *testing.T, db *sqlx.DB, id int64, username, group string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users(id, username, "group") VALUES (?, ?, ?)`, id, username, group); err != nil {
		t.Fatalf("insert user %d: %v", id, err)
	}
}

func autoGroupForUser(t *testing.T, db *sqlx.DB, id int64) string {
	t.Helper()
	var group string
	if err := db.Get(&group, `SELECT COALESCE(NULLIF("group", ''), 'default') FROM users WHERE id = ?`, id); err != nil {
		t.Fatalf("read group for user %d: %v", id, err)
	}
	return group
}

func TestAutoGroupFailsClosedWithoutRedisAndReadPathsDoNotPanic(t *testing.T) {
	db, svc := newAutoGroupAuditTestService(t, nil)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly succeeded without Redis: %#v", result)
	}
	if message := toString(result["message"]); !strings.Contains(message, "audit storage unavailable") {
		t.Fatalf("expected audit storage failure, got %q", message)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("group changed without audit storage: %q", got)
	}

	if logs := svc.GetLogs(1, 20, "", nil); toInt64(logs["total"]) != 0 {
		t.Fatalf("nil Redis log read should degrade to an empty result: %#v", logs)
	}
	stats := svc.GetStats()
	if toInt64(stats["total_assigned"]) != 0 {
		t.Fatalf("nil Redis stats should report zero audited assignments: %#v", stats)
	}
	if svc.SaveConfig(map[string]interface{}{"enabled": true}) {
		t.Fatal("SaveConfig unexpectedly reported success without Redis")
	}
}

func TestAutoGroupAssignmentRollsBackOnRuntimeAuditWriteError(t *testing.T) {
	store := &fakeAutoGroupAuditStore{appendErr: errors.New("simulated Redis write failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly succeeded: %#v", result)
	}
	if message := toString(result["message"]); !strings.Contains(message, "rolled back") {
		t.Fatalf("expected explicit rollback result, got %q", message)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("audit write failure left group changed: %q", got)
	}
	logs, _ := store.snapshot()
	if len(logs) != 0 {
		t.Fatalf("failed audit append unexpectedly stored logs: %d", len(logs))
	}
}

func TestAutoGroupFinalizeFailureKeepsCommittedMutationPendingAndNonActionable(t *testing.T) {
	store := &fakeAutoGroupAuditStore{commitErr: errors.New("simulated Redis finalize failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly reported full success: %#v", result)
	}
	if message := toString(result["message"]); !strings.Contains(message, "pending") || !strings.Contains(message, "人工核对") {
		t.Fatalf("expected explicit pending recovery error, got %q", message)
	}
	if got := autoGroupForUser(t, db, 1); got != "vip" {
		t.Fatalf("SQL commit should remain durable after finalize failure, group=%q", got)
	}
	logs, ids := store.snapshot()
	if len(logs) != 0 || len(ids) != 0 {
		t.Fatalf("failed finalize exposed an uncommitted audit log: logs=%d ids=%d", len(logs), len(ids))
	}
	pending := store.pendingSnapshot()
	if len(pending) != 1 {
		t.Fatalf("finalize failure did not retain one pending record: %d", len(pending))
	}
	for id := range pending {
		revert := svc.RevertUser(int(id))
		if success, _ := revert["success"].(bool); success {
			t.Fatalf("pending finalize record unexpectedly reverted the user: %#v", revert)
		}
	}
}

func TestAutoGroupCommitFailureLeavesOnlyPendingNonActionableAudit(t *testing.T) {
	setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})
	store := &fakeAutoGroupAuditStore{}
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	db.MustExec(`PRAGMA foreign_keys = ON`)
	db.MustExec(`
		CREATE TABLE allowed_groups (name TEXT PRIMARY KEY);
		INSERT INTO allowed_groups(name) VALUES ('default');
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			role INTEGER NOT NULL DEFAULT 1,
			"group" TEXT REFERENCES allowed_groups(name) DEFERRABLE INITIALLY DEFERRED,
			status INTEGER NOT NULL DEFAULT 1,
			deleted_at INTEGER
		);
		INSERT INTO users(id, username, "group") VALUES (1, 'alice', 'default');
	`)
	svc := &AutoGroupService{
		db:         &database.Manager{DB: db, IsPG: false},
		auditStore: store,
	}

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly survived deferred commit failure: %#v", result)
	}
	if message := toString(result["message"]); !strings.Contains(message, "提交用户分组事务失败") {
		t.Fatalf("expected commit failure, got %q", message)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("failed commit changed the durable group to %q", got)
	}

	logs, ids := store.snapshot()
	if len(logs) != 0 || len(ids) != 0 {
		t.Fatalf("failed SQL commit polluted committed history: logs=%d ids=%d", len(logs), len(ids))
	}
	pending := store.pendingSnapshot()
	if len(pending) != 1 {
		t.Fatalf("expected one pending recovery audit, got %d", len(pending))
	}
	var ghostID int64
	for id := range pending {
		ghostID = id
	}
	revert := svc.RevertUser(int(ghostID))
	if success, _ := revert["success"].(bool); success {
		t.Fatalf("ghost audit unexpectedly became actionable: %#v", revert)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("ghost audit revert changed user group to %q", got)
	}

	// The current-group CAS alone has an ABA hole: the failed record would
	// become actionable if a later legitimate operation moved the user into
	// the same target group. The missing commit marker must keep it blocked.
	db.MustExec(`INSERT INTO allowed_groups(name) VALUES ('vip')`)
	legitimate := svc.assignUser(1, "vip", "admin")
	if success, _ := legitimate["success"].(bool); !success {
		t.Fatalf("later legitimate assignment failed: %#v", legitimate)
	}
	if got := autoGroupForUser(t, db, 1); got != "vip" {
		t.Fatalf("later assignment left group %q, want vip", got)
	}

	revert = svc.RevertUser(int(ghostID))
	if success, _ := revert["success"].(bool); success {
		t.Fatalf("old ghost audit became actionable after an ABA group change: %#v", revert)
	}
	if message := toString(revert["message"]); !strings.Contains(message, "不存在") {
		t.Fatalf("expected pending-log rejection, got %q", message)
	}
	if got := autoGroupForUser(t, db, 1); got != "vip" {
		t.Fatalf("ghost audit reverted the later legitimate assignment to %q", got)
	}
}

func TestAutoGroupAssignmentAndRevertUseDistinctLogIDs(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	assigned := svc.assignUser(1, "vip", "admin")
	if success, _ := assigned["success"].(bool); !success {
		t.Fatalf("assignment failed: %#v", assigned)
	}
	logs, ids := store.snapshot()
	if len(logs) != 1 || len(ids) != 1 {
		t.Fatalf("expected one audit log, logs=%d ids=%d", len(logs), len(ids))
	}
	if ids[0] <= 1000 {
		t.Fatalf("new audit ID should not overlap legacy LLEN IDs: %d", ids[0])
	}

	reverted := svc.RevertUser(int(ids[0]))
	if success, _ := reverted["success"].(bool); !success {
		t.Fatalf("revert failed: %#v", reverted)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("revert left unexpected group %q", got)
	}
	_, ids = store.snapshot()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("assignment and revert IDs are not unique: %v", ids)
	}
}

func TestAutoGroupLogIDsRemainUniqueAfterTrim(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	ctx := context.Background()
	for i := 0; i < 1005; i++ {
		logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
			ID:       int64(i + 1),
			Username: fmt.Sprintf("user-%d", i+1),
			Source:   "password",
		}}, "default", "vip", "system")
		if err != nil {
			t.Fatalf("prepare log %d: %v", i, err)
		}
		if err := store.StageLogs(ctx, logs); err != nil {
			t.Fatalf("stage log %d: %v", i, err)
		}
		ids, err := autoGroupAuditLogIDs(logs)
		if err != nil {
			t.Fatalf("read prepared log ID %d: %v", i, err)
		}
		if err := store.CommitLogs(ctx, ids); err != nil {
			t.Fatalf("commit log %d: %v", i, err)
		}
	}

	logs, ids := store.snapshot()
	if len(logs) != int(autoGroupMaxLogs) {
		t.Fatalf("retained log count = %d, want %d", len(logs), autoGroupMaxLogs)
	}
	if len(ids) != 1005 {
		t.Fatalf("recorded ID count = %d, want 1005", len(ids))
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate audit ID after trim: %d", id)
		}
		seen[id] = struct{}{}
	}
}

func TestPendingAuditBatchDoesNotTrimCommittedHistory(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	ctx := context.Background()
	users := make([]autoGroupLogUser, int(autoGroupMaxLogs))
	for i := range users {
		users[i] = autoGroupLogUser{
			ID:       int64(i + 1),
			Username: fmt.Sprintf("committed-%d", i+1),
			Source:   "password",
		}
	}
	committedLogs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", users, "default", "vip", "system")
	if err != nil {
		t.Fatalf("prepare committed history: %v", err)
	}
	if err := store.StageLogs(ctx, committedLogs); err != nil {
		t.Fatalf("stage committed history: %v", err)
	}
	committedIDs, err := autoGroupAuditLogIDs(committedLogs)
	if err != nil {
		t.Fatalf("read committed IDs: %v", err)
	}
	if err := store.CommitLogs(ctx, committedIDs); err != nil {
		t.Fatalf("commit history: %v", err)
	}
	before, _ := store.snapshot()
	if len(before) != int(autoGroupMaxLogs) {
		t.Fatalf("committed history length = %d, want %d", len(before), autoGroupMaxLogs)
	}

	for i := range users {
		users[i].ID += autoGroupMaxLogs
		users[i].Username = fmt.Sprintf("pending-%d", i+1)
	}
	pendingLogs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", users, "default", "blocked", "system")
	if err != nil {
		t.Fatalf("prepare failed SQL batch: %v", err)
	}
	if err := store.StageLogs(ctx, pendingLogs); err != nil {
		t.Fatalf("stage failed SQL batch: %v", err)
	}
	// Simulate the SQL transaction failing: CommitLogs is intentionally never
	// called for this batch.
	after, _ := store.snapshot()
	if len(after) != len(before) {
		t.Fatalf("pending batch changed committed history length: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("pending batch changed committed history at index %d", i)
		}
	}
	if pending := store.pendingSnapshot(); len(pending) != int(autoGroupMaxLogs) {
		t.Fatalf("pending recovery record count = %d, want %d", len(pending), autoGroupMaxLogs)
	}
}

func TestRevertUserRejectsAmbiguousLegacyLogID(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "vip")
	insertAutoGroupUser(t, db, 2, "bob", "vip")

	for _, entry := range []autoGroupAuditEntry{
		{ID: 7, Action: "assign", UserID: 1, Username: "alice", OldGroup: "default", NewGroup: "vip", Source: "password", Affected: 1},
		{ID: 7, Action: "assign", UserID: 2, Username: "bob", OldGroup: "default", NewGroup: "vip", Source: "password", Affected: 1},
	} {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal legacy log: %v", err)
		}
		store.logs = append(store.logs, string(raw))
	}

	result := svc.RevertUser(7)
	if success, _ := result["success"].(bool); success {
		t.Fatalf("ambiguous legacy log ID unexpectedly reverted a user: %#v", result)
	}
	if message := toString(result["message"]); !strings.Contains(message, "多条匹配") {
		t.Fatalf("expected ambiguity error, got %q", message)
	}
	for id := int64(1); id <= 2; id++ {
		if got := autoGroupForUser(t, db, id); got != "vip" {
			t.Fatalf("ambiguous ID changed user %d to %q", id, got)
		}
	}
}

func TestBatchMoveUsersRollsBackEachUserWhenAuditWriteFails(t *testing.T) {
	store := &fakeAutoGroupAuditStore{appendErr: errors.New("simulated Redis write failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")
	insertAutoGroupUser(t, db, 2, "bob", "default")

	result := svc.BatchMoveUsers([]int64{1, 2}, "vip")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("batch move unexpectedly succeeded: %#v", result)
	}
	if toInt64(result["success_count"]) != 0 || toInt64(result["failed_count"]) != 2 {
		t.Fatalf("unexpected batch counts: %#v", result)
	}
	for id := int64(1); id <= 2; id++ {
		if got := autoGroupForUser(t, db, id); got != "default" {
			t.Fatalf("batch audit failure changed user %d to %q", id, got)
		}
	}
}

func TestSimpleScanOnlyChangesAndAuditsLoadedUsers(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	db, svc := newAutoGroupAuditTestService(t, store)

	tx := db.MustBegin()
	for id := int64(1); id <= 1001; id++ {
		tx.MustExec(`INSERT INTO users(id, username, "group") VALUES (?, ?, 'default')`, id, fmt.Sprintf("user-%d", id))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	svc.cachedConfig = map[string]interface{}{
		"mode":                  "simple",
		"target_group":          "vip",
		"whitelist_ids":         []interface{}{},
		"scan_interval_minutes": 60,
		"auto_scan_enabled":     false,
		"last_scan_time":        0,
	}
	result := svc.RunScan(false)
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("simple scan failed: %#v", result)
	}
	stats, _ := result["stats"].(map[string]interface{})
	if toInt64(stats["assigned"]) != 1000 {
		t.Fatalf("scan assigned unexpected count: %#v", stats)
	}

	var vipCount int64
	if err := db.Get(&vipCount, `SELECT COUNT(*) FROM users WHERE "group" = 'vip'`); err != nil {
		t.Fatalf("count vip users: %v", err)
	}
	if vipCount != 1000 {
		t.Fatalf("scan changed %d users; expected exactly the 1000 loaded candidates", vipCount)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("user outside the loaded page was changed without an audit log: %q", got)
	}

	logs, ids := store.snapshot()
	if len(logs) != 1000 || len(ids) != 1000 {
		t.Fatalf("scan audit cardinality mismatch: logs=%d ids=%d", len(logs), len(ids))
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate scan audit ID %d", id)
		}
		seen[id] = struct{}{}
	}
}

func TestSimpleScanRollsBackWholeTransactionOnAuditWriteError(t *testing.T) {
	store := &fakeAutoGroupAuditStore{appendErr: errors.New("simulated Redis write failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")
	insertAutoGroupUser(t, db, 2, "bob", "default")
	svc.cachedConfig = map[string]interface{}{
		"mode":          "simple",
		"target_group":  "vip",
		"whitelist_ids": []interface{}{},
	}

	result := svc.RunScan(false)
	if success, _ := result["success"].(bool); success {
		t.Fatalf("scan unexpectedly succeeded: %#v", result)
	}
	for id := int64(1); id <= 2; id++ {
		if got := autoGroupForUser(t, db, id); got != "default" {
			t.Fatalf("scan audit failure changed user %d to %q", id, got)
		}
	}
}
