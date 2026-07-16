package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

type fakeAutoGroupAuditStore struct {
	mu sync.Mutex

	nextID       int64
	logs         []string
	allIDs       []int64
	pending      map[int64]string
	pendingAt    map[int64]time.Time
	pendingState map[int64]string
	pingErr      error
	reserveErr   error
	appendErr    error
	commitErr    error
	markerErr    error
	readErr      error
	pendingErr   error
	resolveErr   error
	committed    map[int64]struct{}
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
	if int64(len(s.pending)+len(logs)) > autoGroupMaxPendingLogs {
		return errors.New("pending auto-group audit capacity exceeded")
	}
	if s.pending == nil {
		s.pending = make(map[int64]string, len(logs))
	}
	if s.pendingAt == nil {
		s.pendingAt = make(map[int64]time.Time, len(logs))
	}
	if s.pendingState == nil {
		s.pendingState = make(map[int64]string, len(logs))
	}
	stagedAt := time.Now()
	for _, raw := range logs {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return err
		}
		id, ok := parseAutoGroupLogID(entry["id"])
		if !ok {
			return errors.New("invalid staged audit ID")
		}
		if _, exists := s.pending[id]; exists {
			return fmt.Errorf("duplicate pending audit log %d", id)
		}
		s.pending[id] = raw
		s.pendingAt[id] = stagedAt
		s.pendingState[id] = autoGroupPendingStateUnknown
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
		delete(s.pendingAt, id)
		delete(s.pendingState, id)
		s.allIDs = append(s.allIDs, id)
	}
	s.logs = append(newHead, s.logs...)
	if len(s.logs) > int(autoGroupMaxLogs) {
		s.logs = s.logs[:autoGroupMaxLogs]
	}
	return nil
}

func (s *fakeAutoGroupAuditStore) SetPendingState(_ context.Context, ids []int64, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state != autoGroupPendingStateAmbiguous && state != autoGroupPendingStateSQLCommitted {
		return errors.New("invalid pending audit state")
	}
	for _, id := range ids {
		if _, ok := s.pending[id]; !ok {
			return fmt.Errorf("missing pending audit log %d", id)
		}
	}
	if s.pendingState == nil {
		s.pendingState = make(map[int64]string, len(ids))
	}
	for _, id := range ids {
		s.pendingState[id] = state
	}
	return nil
}

func (s *fakeAutoGroupAuditStore) ReadPending(_ context.Context, limit int64) ([]autoGroupPendingAuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	if int64(len(s.pending)) > limit {
		return nil, fmt.Errorf("pending audit log count %d exceeds limit %d", len(s.pending), limit)
	}
	records := make([]autoGroupPendingAuditRecord, 0, len(s.pending))
	for id, raw := range s.pending {
		state := s.pendingState[id]
		if state == "" {
			state = autoGroupPendingStateUnknown
		}
		records = append(records, autoGroupPendingAuditRecord{
			ID:       id,
			Raw:      raw,
			State:    state,
			StagedAt: s.pendingAt[id],
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func (s *fakeAutoGroupAuditStore) ResolvePending(
	_ context.Context,
	operationID string,
	ids []int64,
	resolution, actor string,
	resolvedAt time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolveErr != nil {
		return s.resolveErr
	}
	entries := make([]map[string]interface{}, len(ids))
	for i, id := range ids {
		raw, ok := s.pending[id]
		if !ok {
			return fmt.Errorf("missing pending audit log %d", id)
		}
		state := s.pendingState[id]
		if state == "" {
			state = autoGroupPendingStateUnknown
		}
		if state == autoGroupPendingStateUnknown {
			return errors.New("commit-unknown pending auto-group audit operations cannot be resolved online")
		}
		if resolution == autoGroupPendingResolutionDiscard && state == autoGroupPendingStateSQLCommitted {
			return errors.New("sql-committed pending auto-group audit operations must be finalized")
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return err
		}
		if toString(entry["operation_id"]) != operationID || toInt64(entry["operation_size"]) != int64(len(ids)) {
			return errors.New("pending audit operation mismatch")
		}
		entries[i] = entry
	}
	if resolution == autoGroupPendingResolutionFinalize {
		if s.committed == nil {
			s.committed = make(map[int64]struct{}, len(ids))
		}
		newHead := make([]string, len(ids))
		for i, id := range ids {
			if s.pendingState[id] == autoGroupPendingStateSQLCommitted {
				entries[i]["revertible"] = true
				entries[i]["recovery_state"] = "manually_finalized_sql_committed"
			} else {
				entries[i]["revertible"] = false
				entries[i]["recovery_state"] = "manually_finalized_ambiguous"
			}
			entries[i]["resolved_by"] = actor
			entries[i]["resolved_at"] = resolvedAt.Unix()
			raw, err := json.Marshal(entries[i])
			if err != nil {
				return err
			}
			newHead[len(ids)-1-i] = string(raw)
			s.committed[id] = struct{}{}
			s.allIDs = append(s.allIDs, id)
		}
		s.logs = append(newHead, s.logs...)
		if len(s.logs) > int(autoGroupMaxLogs) {
			s.logs = s.logs[:autoGroupMaxLogs]
		}
	} else if resolution != autoGroupPendingResolutionDiscard {
		return errors.New("invalid pending audit resolution")
	}
	for _, id := range ids {
		delete(s.pending, id)
		delete(s.pendingAt, id)
		delete(s.pendingState, id)
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

func (s *fakeAutoGroupAuditStore) pendingStateSnapshot() map[int64]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[int64]string, len(s.pendingState))
	for id, state := range s.pendingState {
		result[id] = state
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
	if message := toString(result["message"]); !strings.Contains(message, "pending") || !strings.Contains(message, "人工") {
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
	for id, state := range store.pendingStateSnapshot() {
		if state != autoGroupPendingStateSQLCommitted {
			t.Fatalf("pending audit %d state=%q, want %q", id, state, autoGroupPendingStateSQLCommitted)
		}
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
	for id, state := range store.pendingStateSnapshot() {
		if state != autoGroupPendingStateAmbiguous {
			t.Fatalf("pending audit %d state=%q, want %q", id, state, autoGroupPendingStateAmbiguous)
		}
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

func TestPendingAuditManualFinalizeKeepsSQLCommittedEntryRevertible(t *testing.T) {
	store := &fakeAutoGroupAuditStore{commitErr: errors.New("simulated Redis finalize failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly reported full success: %#v", result)
	}
	pending, err := svc.GetPendingAudits()
	if err != nil {
		t.Fatalf("list pending audits: %v", err)
	}
	items, ok := pending["items"].([]map[string]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected pending operation list: %#v", pending)
	}
	operationID := toString(items[0]["operation_id"])
	if operationID == "" || toString(items[0]["state"]) != autoGroupPendingStateSQLCommitted {
		t.Fatalf("unexpected pending operation metadata: %#v", items[0])
	}
	if _, err := svc.ResolvePendingAudit(operationID, autoGroupPendingResolutionFinalize, "FINALIZE wrong", "admin"); err == nil {
		t.Fatal("manual finalize accepted an incorrect confirmation")
	}
	resolved, err := svc.ResolvePendingAudit(
		operationID,
		autoGroupPendingResolutionFinalize,
		"FINALIZE "+operationID,
		"security-admin",
	)
	if err != nil {
		t.Fatalf("manual finalize: %v", err)
	}
	if revertible, _ := resolved["revertible"].(bool); !revertible {
		t.Fatalf("SQL-committed manual finalize was not revertible: %#v", resolved)
	}
	if got := len(store.pendingSnapshot()); got != 0 {
		t.Fatalf("manual finalize retained %d pending records", got)
	}
	logs, ids := store.snapshot()
	if len(logs) != 1 || len(ids) != 1 {
		t.Fatalf("manual finalize did not preserve one committed audit record: logs=%d ids=%d", len(logs), len(ids))
	}
	var archived map[string]interface{}
	if err := json.Unmarshal([]byte(logs[0]), &archived); err != nil {
		t.Fatalf("decode manually finalized record: %v", err)
	}
	if archived["revertible"] != true || toString(archived["recovery_state"]) != "manually_finalized_sql_committed" {
		t.Fatalf("manual finalize did not preserve SQL-committed recovery state: %#v", archived)
	}
	store.mu.Lock()
	store.commitErr = nil
	store.mu.Unlock()
	reverted := svc.RevertUser(int(ids[0]))
	if success, _ := reverted["success"].(bool); !success {
		t.Fatalf("manually finalized SQL commit was not revertible: %#v", reverted)
	}
	if got := autoGroupForUser(t, db, 1); got != "default" {
		t.Fatalf("manual finalize revert restored group %q, want default", got)
	}
}

func TestPendingAuditManualFinalizeKeepsAmbiguousEntryNonRevertible(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "vip")

	ctx := context.Background()
	logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
		ID: 1, Username: "alice", Source: "password",
	}}, "default", "vip", "admin")
	if err != nil {
		t.Fatalf("prepare pending audit: %v", err)
	}
	if err := store.StageLogs(ctx, logs); err != nil {
		t.Fatalf("stage pending audit: %v", err)
	}
	ids, err := autoGroupAuditLogIDs(logs)
	if err != nil {
		t.Fatalf("parse pending audit IDs: %v", err)
	}
	if err := store.SetPendingState(ctx, ids, autoGroupPendingStateAmbiguous); err != nil {
		t.Fatalf("mark pending audit ambiguous: %v", err)
	}
	var entry autoGroupAuditEntry
	if err := json.Unmarshal([]byte(logs[0]), &entry); err != nil {
		t.Fatalf("decode pending audit: %v", err)
	}

	resolved, err := svc.ResolvePendingAudit(
		entry.OperationID,
		autoGroupPendingResolutionFinalize,
		"FINALIZE "+entry.OperationID,
		"security-admin",
	)
	if err != nil {
		t.Fatalf("manual finalize ambiguous audit: %v", err)
	}
	if revertible, _ := resolved["revertible"].(bool); revertible {
		t.Fatalf("ambiguous manual finalize unexpectedly became revertible: %#v", resolved)
	}
	archivedLogs, archivedIDs := store.snapshot()
	if len(archivedLogs) != 1 || len(archivedIDs) != 1 {
		t.Fatalf("manual finalize did not archive ambiguous evidence: logs=%d ids=%d", len(archivedLogs), len(archivedIDs))
	}
	var archived map[string]interface{}
	if err := json.Unmarshal([]byte(archivedLogs[0]), &archived); err != nil {
		t.Fatalf("decode manually finalized ambiguous record: %v", err)
	}
	if archived["revertible"] != false || toString(archived["recovery_state"]) != "manually_finalized_ambiguous" {
		t.Fatalf("ambiguous finalize did not disable automatic recovery: %#v", archived)
	}
	reverted := svc.RevertUser(int(archivedIDs[0]))
	if success, _ := reverted["success"].(bool); success {
		t.Fatalf("manually finalized ambiguous audit unexpectedly reverted the user: %#v", reverted)
	}
	if got := autoGroupForUser(t, db, 1); got != "vip" {
		t.Fatalf("ambiguous audit changed user group to %q", got)
	}
}

func TestPendingAuditSQLCommittedCannotBeDiscarded(t *testing.T) {
	store := &fakeAutoGroupAuditStore{commitErr: errors.New("simulated Redis finalize failure")}
	db, svc := newAutoGroupAuditTestService(t, store)
	insertAutoGroupUser(t, db, 1, "alice", "default")

	result := svc.assignUser(1, "vip", "admin")
	if success, _ := result["success"].(bool); success {
		t.Fatalf("assignment unexpectedly reported full success: %#v", result)
	}
	pending, err := svc.GetPendingAudits()
	if err != nil {
		t.Fatalf("list pending audits: %v", err)
	}
	items, ok := pending["items"].([]map[string]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected pending operation list: %#v", pending)
	}
	operationID := toString(items[0]["operation_id"])
	if toString(items[0]["state"]) != autoGroupPendingStateSQLCommitted {
		t.Fatalf("unexpected pending operation state: %#v", items[0])
	}

	if _, err := svc.ResolvePendingAudit(
		operationID,
		autoGroupPendingResolutionDiscard,
		"DISCARD "+operationID,
		"security-admin",
	); err == nil || !strings.Contains(err.Error(), "must be finalized") {
		t.Fatalf("SQL-committed pending audit was not protected from discard: %v", err)
	}
	if got := len(store.pendingSnapshot()); got != 1 {
		t.Fatalf("rejected discard removed pending audit evidence: got %d records", got)
	}
	if got := autoGroupForUser(t, db, 1); got != "vip" {
		t.Fatalf("SQL-committed mutation unexpectedly changed after rejected discard: %q", got)
	}
}

func TestPendingAuditCommitUnknownCannotBeResolvedOnline(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	_, svc := newAutoGroupAuditTestService(t, store)
	ctx := context.Background()
	logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
		ID: 1, Username: "alice", Source: "password",
	}}, "default", "vip", "admin")
	if err != nil {
		t.Fatalf("prepare pending audit: %v", err)
	}
	if err := store.StageLogs(ctx, logs); err != nil {
		t.Fatalf("stage pending audit: %v", err)
	}
	var entry autoGroupAuditEntry
	if err := json.Unmarshal([]byte(logs[0]), &entry); err != nil {
		t.Fatalf("decode pending audit: %v", err)
	}

	for _, resolution := range []string{autoGroupPendingResolutionFinalize, autoGroupPendingResolutionDiscard} {
		confirmation := strings.ToUpper(resolution) + " " + entry.OperationID
		if _, err := svc.ResolvePendingAudit(entry.OperationID, resolution, confirmation, "admin"); err == nil || !strings.Contains(err.Error(), "cannot be resolved online") {
			t.Fatalf("commit-unknown operation accepted %s: %v", resolution, err)
		}
	}
	if got := len(store.pendingSnapshot()); got != 1 {
		t.Fatalf("rejected online recovery removed pending evidence: got %d records", got)
	}
}

func TestPendingAuditManualDiscardRequiresExactOperationConfirmation(t *testing.T) {
	store := &fakeAutoGroupAuditStore{}
	_, svc := newAutoGroupAuditTestService(t, store)
	ctx := context.Background()
	logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
		ID: 1, Username: "alice", Source: "password",
	}}, "default", "vip", "admin")
	if err != nil {
		t.Fatalf("prepare pending audit: %v", err)
	}
	if err := store.StageLogs(ctx, logs); err != nil {
		t.Fatalf("stage pending audit: %v", err)
	}
	ids, err := autoGroupAuditLogIDs(logs)
	if err != nil {
		t.Fatalf("decode staged audit IDs: %v", err)
	}
	if err := store.SetPendingState(ctx, ids, autoGroupPendingStateAmbiguous); err != nil {
		t.Fatalf("mark pending audit ambiguous: %v", err)
	}
	var entry autoGroupAuditEntry
	if err := json.Unmarshal([]byte(logs[0]), &entry); err != nil {
		t.Fatalf("decode pending audit: %v", err)
	}
	if _, err := svc.ResolvePendingAudit(entry.OperationID, autoGroupPendingResolutionDiscard, "DISCARD", "admin"); err == nil {
		t.Fatal("manual discard accepted an incomplete confirmation")
	}
	if _, err := svc.ResolvePendingAudit(
		entry.OperationID,
		autoGroupPendingResolutionDiscard,
		"DISCARD "+entry.OperationID,
		"admin",
	); err != nil {
		t.Fatalf("manual discard: %v", err)
	}
	if got := len(store.pendingSnapshot()); got != 0 {
		t.Fatalf("manual discard retained %d pending records", got)
	}
	if committed, _ := store.snapshot(); len(committed) != 0 {
		t.Fatalf("manual discard unexpectedly committed %d records", len(committed))
	}
}

func TestPendingAuditCapacityFailsClosedWithoutEvictingEvidence(t *testing.T) {
	store := &fakeAutoGroupAuditStore{
		pending:      make(map[int64]string, autoGroupMaxPendingLogs),
		pendingAt:    make(map[int64]time.Time, autoGroupMaxPendingLogs),
		pendingState: make(map[int64]string, autoGroupMaxPendingLogs),
	}
	for id := int64(1); id <= autoGroupMaxPendingLogs; id++ {
		store.pending[id] = "preserved"
		store.pendingAt[id] = time.Now()
		store.pendingState[id] = autoGroupPendingStateAmbiguous
	}
	ctx := context.Background()
	logs, err := prepareAutoGroupAuditLogs(ctx, store, "assign", []autoGroupLogUser{{
		ID: 9999, Username: "blocked", Source: "password",
	}}, "default", "vip", "admin")
	if err != nil {
		t.Fatalf("prepare audit at capacity: %v", err)
	}
	if err := store.StageLogs(ctx, logs); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("expected pending capacity rejection, got %v", err)
	}
	if got := len(store.pendingSnapshot()); got != int(autoGroupMaxPendingLogs) {
		t.Fatalf("capacity rejection evicted pending evidence: got %d records", got)
	}
}

func TestRedisAuditStoreRejectsOversizedBatchesBeforeRedis(t *testing.T) {
	store := &redisAutoGroupAuditStore{}
	oversized := make([]string, int(autoGroupMaxPendingLogs)+1)

	if _, err := store.ReserveIDs(context.Background(), len(oversized)); err == nil || !strings.Contains(err.Error(), "supported limit") {
		t.Fatalf("oversized ID reservation was not rejected before Redis access: %v", err)
	}
	if err := store.StageLogs(context.Background(), oversized); err == nil || !strings.Contains(err.Error(), "supported limit") {
		t.Fatalf("oversized staging batch was not rejected before Redis access: %v", err)
	}
}

func TestAutoGroupRejectsFutureAuditVersion(t *testing.T) {
	entry := map[string]interface{}{
		"audit_version": float64(autoGroupAuditVersion + 1),
	}
	if requiresMarker, err := autoGroupLogRequiresCommitMarker(entry); err == nil || requiresMarker {
		t.Fatalf("future audit version was accepted: marker=%t err=%v", requiresMarker, err)
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
