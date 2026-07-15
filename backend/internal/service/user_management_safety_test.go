package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func installUserManagementSafetyDB(t *testing.T) (*sqlx.DB, *UserManagementService) {
	t.Helper()
	setMutationSafetyConfigForTest(t, &config.Config{
		NewAPIRedisDisabled:   true,
		AllowUnsafeHardDelete: true,
	})
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite in-memory databases are connection-local. Keeping a single pooled
	// connection also makes transaction rollback assertions deterministic.
	db.SetMaxOpenConns(1)
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	db.MustExec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 1,
			role INTEGER NOT NULL DEFAULT 1,
			request_count INTEGER NOT NULL DEFAULT 0,
			deleted_at INTEGER
		);
		CREATE TABLE tokens (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			status INTEGER NOT NULL DEFAULT 1,
			expired_time INTEGER,
			remain_quota INTEGER,
			unlimited_quota BOOLEAN,
			deleted_at INTEGER
		);
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			type INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
	`)
	return db, NewUserManagementService()
}

func TestPermanentDeleteEntrypointsRequireUnsafeOptIn(t *testing.T) {
	t.Run("single hard delete", func(t *testing.T) {
		db, svc := installUserManagementSafetyDB(t)
		setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})
		db.MustExec(`INSERT INTO users (id, username) VALUES (1, 'alice')`)

		if _, err := svc.DeleteUser(1, true); err == nil || !strings.Contains(err.Error(), "ALLOW_UNSAFE_HARD_DELETE=true") {
			t.Fatalf("expected unsafe hard-delete opt-in error, got %v", err)
		}
		if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
			t.Fatal("blocked hard delete changed the user")
		}
	})

	t.Run("permanent purge", func(t *testing.T) {
		db, svc := installUserManagementSafetyDB(t)
		setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})
		db.MustExec(`INSERT INTO users (id, username, deleted_at) VALUES (1, 'alice', 1)`)

		if _, err := svc.PurgeSoftDeleted(""); err == nil || !strings.Contains(err.Error(), "ALLOW_UNSAFE_HARD_DELETE=true") {
			t.Fatalf("expected unsafe purge opt-in error, got %v", err)
		}
		if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
			t.Fatal("blocked purge changed the user")
		}
	})

	t.Run("batch hard delete execution", func(t *testing.T) {
		db, svc := installUserManagementSafetyDB(t)
		db.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'alice', 0)`)
		db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`, time.Now().Unix())

		preview, err := svc.BatchDeleteInactiveUsers(ActivityNever, true, true, "")
		if err != nil {
			t.Fatalf("preview hard delete: %v", err)
		}
		setMutationSafetyConfigForTest(t, &config.Config{NewAPIRedisDisabled: true})
		_, err = svc.BatchDeleteInactiveUsers(ActivityNever, false, true, toString(preview["snapshot_id"]))
		if err == nil || !strings.Contains(err.Error(), "ALLOW_UNSAFE_HARD_DELETE=true") {
			t.Fatalf("expected unsafe batch hard-delete opt-in error, got %v", err)
		}
		if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
			t.Fatal("blocked batch hard delete changed the user")
		}
	})
}

func queryInt64(t *testing.T, db *sqlx.DB, query string, args ...interface{}) int64 {
	t.Helper()
	var value int64
	if err := db.Get(&value, query, args...); err != nil {
		t.Fatalf("query scalar: %v", err)
	}
	return value
}

func previewAndExecuteBatchDelete(t *testing.T, svc *UserManagementService, activityLevel string, hardDelete bool) (map[string]interface{}, error) {
	t.Helper()
	preview, err := svc.BatchDeleteInactiveUsers(activityLevel, true, hardDelete, "")
	if err != nil {
		return nil, err
	}
	snapshotID := toString(preview["snapshot_id"])
	if snapshotID == "" && toInt64(preview["affected_count"]) > 0 {
		t.Fatal("batch-delete preview did not return a snapshot id")
	}
	return svc.BatchDeleteInactiveUsers(activityLevel, false, hardDelete, snapshotID)
}

func previewAndExecutePurge(t *testing.T, svc *UserManagementService) (int64, error) {
	t.Helper()
	preview, err := svc.PreviewSoftDeletedUsers()
	if err != nil {
		return 0, err
	}
	snapshotID := toString(preview["snapshot_id"])
	if snapshotID == "" && toInt64(preview["affected_count"]) > 0 {
		t.Fatal("purge preview did not return a snapshot id")
	}
	return svc.PurgeSoftDeleted(snapshotID)
}

func TestBatchDeleteActivityNeverRechecksBillableLogs(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO users (id, username, request_count) VALUES
		(1, 'counter-stale', 0), (2, 'actually-never', 0)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (?, 2, ?)`, 1, now)

	result, err := previewAndExecuteBatchDelete(t, svc, ActivityNever, false)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if got := toInt64(result["affected_count"]); got != 1 {
		t.Fatalf("expected one soft-deleted user, got %d", got)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("user with a billable log was deleted despite ActivityNever recheck")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 2 AND deleted_at IS NOT NULL"); got != 1 {
		t.Fatal("true never-requested user was not soft-deleted")
	}
}

func TestBatchDeleteRejectsStaleLogsBeforeMutation(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'candidate', 0)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`,
		time.Now().Add(-destructiveLogMaxAge-time.Hour).Unix())

	_, err := svc.BatchDeleteInactiveUsers(ActivityNever, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale-log rejection, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("stale log source allowed a mutation")
	}
}

func TestBatchDeleteRejectsConfiguredLogFallback(t *testing.T) {
	db, _ := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'candidate', 0)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`, time.Now().Unix())
	mgr := &database.Manager{DB: db, IsPG: false}
	database.SetLogForTesting(mgr, database.LogSourceStatus{
		Mode:          database.LogSourceModeFallback,
		Configured:    true,
		Healthy:       false,
		UsingFallback: true,
		LastError:     "dedicated log database dial failed",
	})
	svc := NewUserManagementService()

	_, err := svc.BatchDeleteInactiveUsers(ActivityNever, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "fallback=true") {
		t.Fatalf("expected configured fallback rejection, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("configured log fallback allowed a mutation")
	}
}

func TestHardDeleteRollsBackWhenTokenDeleteFails(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username) VALUES (1, 'alice')`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status, expired_time, remain_quota, unlimited_quota) VALUES (10, 1, 1, 0, 10, 0)`)
	db.MustExec(`CREATE TRIGGER reject_token_delete BEFORE DELETE ON tokens
		BEGIN SELECT RAISE(ABORT, 'token delete rejected'); END`)

	if _, err := svc.DeleteUser(1, true); err == nil {
		t.Fatal("expected hard-delete token error")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
		t.Fatal("hard delete committed the user after token deletion failed")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM tokens WHERE id = 10"); got != 1 {
		t.Fatal("hard delete unexpectedly removed the token")
	}
}

func TestPurgeSoftDeletedRollsBackWhenUserDeleteFails(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, deleted_at) VALUES (1, 'alice', 1)`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status, expired_time, remain_quota, unlimited_quota) VALUES (10, 1, 1, 0, 10, 0)`)
	db.MustExec(`CREATE TRIGGER reject_user_delete BEFORE DELETE ON users
		BEGIN SELECT RAISE(ABORT, 'user delete rejected'); END`)

	if _, err := previewAndExecutePurge(t, svc); err == nil {
		t.Fatal("expected purge user-delete error")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
		t.Fatal("purge unexpectedly removed the user")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM tokens WHERE id = 10"); got != 1 {
		t.Fatal("purge committed token deletion before the user-delete failure")
	}
}

func TestBatchHardDeleteRollsBackEarlierBatches(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	tx := db.MustBegin()
	for id := 1; id <= 501; id++ {
		if _, err := tx.Exec(`INSERT INTO users (id, username, request_count) VALUES (?, ?, 0)`,
			id, fmt.Sprintf("user-%d", id)); err != nil {
			t.Fatalf("seed user %d: %v", id, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO tokens (id, user_id, status) VALUES (501, 501, 1)`); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO logs (user_id, type, created_at) VALUES (9999, 2, ?)`, time.Now().Unix()); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	db.MustExec(`CREATE TRIGGER reject_last_batch_token BEFORE DELETE ON tokens
		WHEN OLD.user_id = 501 BEGIN SELECT RAISE(ABORT, 'last batch rejected'); END`)

	if _, err := previewAndExecuteBatchDelete(t, svc, ActivityNever, true); err == nil {
		t.Fatal("expected second-batch token-delete error")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users"); got != 501 {
		t.Fatalf("expected all 501 users after rollback, got %d", got)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
		t.Fatal("first batch committed before the later batch failed")
	}
}

func TestBatchDeleteSnapshotIsInvalidatedByNewActivity(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'candidate', 0)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`, now)

	preview, err := svc.BatchDeleteInactiveUsers(ActivityNever, true, false, "")
	if err != nil {
		t.Fatalf("preview batch delete: %v", err)
	}
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (1, 2, ?)`, time.Now().Unix())

	_, err = svc.BatchDeleteInactiveUsers(ActivityNever, false, false, toString(preview["snapshot_id"]))
	if err == nil || !strings.Contains(err.Error(), "became active") {
		t.Fatalf("expected newly active user to invalidate snapshot, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("newly active user was deleted")
	}
}

func TestBatchDeleteExcludesAdministrators(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, role, request_count) VALUES
		(1, 'admin', 10, 0), (2, 'ordinary', 1, 0)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`, time.Now().Unix())

	result, err := previewAndExecuteBatchDelete(t, svc, ActivityNever, false)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if got := toInt64(result["affected_count"]); got != 1 {
		t.Fatalf("expected one ordinary user deletion, got %d", got)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("batch delete included an administrator")
	}
}

func TestBatchDeleteSnapshotIsInvalidatedByRequestCountChange(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'candidate', 5)`)
	db.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES (99, 2, ?)`, time.Now().Unix())

	preview, err := svc.BatchDeleteInactiveUsers(ActivityVeryInactive, true, false, "")
	if err != nil {
		t.Fatalf("preview batch delete: %v", err)
	}
	db.MustExec(`UPDATE users SET request_count = request_count + 1 WHERE id = 1`)

	_, err = svc.BatchDeleteInactiveUsers(ActivityVeryInactive, false, false, toString(preview["snapshot_id"]))
	if err == nil || !strings.Contains(err.Error(), "snapshot invalidated") {
		t.Fatalf("expected request-count snapshot invalidation, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1 AND deleted_at IS NULL"); got != 1 {
		t.Fatal("request-count change did not protect the user")
	}
}

func TestPurgeSoftDeletedUsesExactPreviewSnapshot(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, deleted_at) VALUES (1, 'previewed', 1)`)
	preview, err := svc.PreviewSoftDeletedUsers()
	if err != nil {
		t.Fatalf("preview purge: %v", err)
	}
	db.MustExec(`INSERT INTO users (id, username, deleted_at) VALUES (2, 'later', 2)`)

	affected, err := svc.PurgeSoftDeleted(toString(preview["snapshot_id"]))
	if err != nil {
		t.Fatalf("execute purge: %v", err)
	}
	if affected != 1 {
		t.Fatalf("purged users = %d, want 1", affected)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 2"); got != 1 {
		t.Fatal("purge deleted a user that was not in the preview snapshot")
	}
}

func TestBanUserRollsBackAndPropagatesTokenUpdateError(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, status) VALUES (1, 'alice', 1)`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status, expired_time, remain_quota, unlimited_quota) VALUES (10, 1, 1, 0, 10, 0)`)
	db.MustExec(`CREATE TRIGGER reject_token_disable BEFORE UPDATE OF status ON tokens
		WHEN NEW.status = 2 BEGIN SELECT RAISE(ABORT, 'token disable rejected'); END`)

	if err := svc.BanUser(1, true); err == nil || !strings.Contains(err.Error(), "disable user tokens") {
		t.Fatalf("expected propagated token update error, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT status FROM users WHERE id = 1"); got != 1 {
		t.Fatalf("expected user ban rollback, status=%d", got)
	}
}

func TestBanUserPreservesNonActiveTokenStates(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, status) VALUES (1, 'alice', 1)`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status, expired_time, remain_quota, unlimited_quota) VALUES
		(1, 1, 1, 0, 10, 0), (2, 1, 3, 0, 0, 0), (3, 1, 4, 0, 0, 0)`)

	if err := svc.BanUser(1, true); err != nil {
		t.Fatalf("ban user: %v", err)
	}
	for id, want := range map[int]int64{1: 2, 2: 3, 3: 4} {
		if got := queryInt64(t, db, "SELECT status FROM tokens WHERE id = ?", id); got != want {
			t.Errorf("token %d status=%d, want %d", id, got, want)
		}
	}
}

func TestUnbanUserNeverReactivatesPreviouslyDisabledTokens(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	now := time.Now().Unix()
	db.MustExec(`INSERT INTO users (id, username, status) VALUES (1, 'alice', 2)`)
	db.MustExec(`INSERT INTO tokens
		(id, user_id, status, expired_time, remain_quota, unlimited_quota, deleted_at) VALUES
		(1, 1, 2, 0,    10, 0, NULL),
		(2, 1, 3, ?,    10, 0, NULL),
		(3, 1, 4, 0,     0, 0, NULL),
		(4, 1, 2, ?,    10, 0, NULL),
		(5, 1, 2, 0,     0, 0, NULL),
		(6, 1, 2, 0,     0, 1, NULL),
		(7, 1, 2, 0,    10, 0, 1)`, now-1, now-1)

	if err := svc.UnbanUser(1, false); err != nil {
		t.Fatalf("unban user: %v", err)
	}
	for id, want := range map[int]int64{
		1: 2, // pre-existing disabled token remains disabled
		2: 3, // expired status is preserved
		3: 4, // exhausted status is preserved
		4: 2, // disabled but expired remains disabled
		5: 2, // disabled and exhausted remains disabled
		6: 2, // unlimited quota does not make restoration safe
		7: 2, // soft-deleted token remains disabled
	} {
		if got := queryInt64(t, db, "SELECT status FROM tokens WHERE id = ?", id); got != want {
			t.Errorf("token %d status=%d, want %d", id, got, want)
		}
	}
	if got := queryInt64(t, db, "SELECT status FROM users WHERE id = 1"); got != 1 {
		t.Fatalf("user status=%d, want active", got)
	}
}

func TestUnbanUserRejectsLegacyTokenReactivationBeforeMutation(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, status) VALUES (1, 'alice', 2)`)
	db.MustExec(`INSERT INTO tokens
		(id, user_id, status, expired_time, remain_quota, unlimited_quota)
		VALUES (10, 1, 2, 0, 10, 0)`)
	db.MustExec(`CREATE TRIGGER reject_token_enable BEFORE UPDATE OF status ON tokens
		WHEN NEW.status = 1 BEGIN SELECT RAISE(ABORT, 'token enable rejected'); END`)

	if err := svc.UnbanUser(1, true); !errors.Is(err, ErrBulkTokenReactivationDisabled) {
		t.Fatalf("expected ErrBulkTokenReactivationDisabled, got %v", err)
	}
	if got := queryInt64(t, db, "SELECT status FROM users WHERE id = 1"); got != 2 {
		t.Fatalf("legacy request mutated user before rejection, status=%d", got)
	}
	if got := queryInt64(t, db, "SELECT status FROM tokens WHERE id = 10"); got != 2 {
		t.Fatalf("disabled token was reactivated, status=%d", got)
	}
}

func TestRootUserMutationsAreBlocked(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, status, role) VALUES (1, 'root', 2, 100)`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status, expired_time, remain_quota, unlimited_quota)
		VALUES (10, 1, 1, 0, 10, 0)`)

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "soft delete", run: func() error { _, err := svc.DeleteUser(1, false); return err }},
		{name: "hard delete", run: func() error { _, err := svc.DeleteUser(1, true); return err }},
		{name: "ban", run: func() error { return svc.BanUser(1, true) }},
		{name: "unban", run: func() error { return svc.UnbanUser(1, false) }},
		{name: "disable token", run: func() error { return svc.DisableToken(10) }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil || !strings.Contains(err.Error(), "root user") {
				t.Fatalf("expected protected-root error, got %v", err)
			}
		})
	}

	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
		t.Fatal("root user was deleted")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM tokens WHERE user_id = 1"); got != 1 {
		t.Fatal("root token was deleted")
	}
}

func TestPurgeSoftDeletedPreservesRootUsers(t *testing.T) {
	db, svc := installUserManagementSafetyDB(t)
	db.MustExec(`INSERT INTO users (id, username, role, deleted_at) VALUES
		(1, 'root', 100, 1), (2, 'alice', 1, 1), (3, 'admin', 10, 1)`)
	db.MustExec(`INSERT INTO tokens (id, user_id, status) VALUES (10, 1, 1), (20, 2, 1), (30, 3, 1)`)

	affected, err := previewAndExecutePurge(t, svc)
	if err != nil {
		t.Fatalf("PurgeSoftDeleted returned error: %v", err)
	}
	if affected != 1 {
		t.Fatalf("purged users = %d, want 1", affected)
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 1"); got != 1 {
		t.Fatal("purge deleted the protected root user")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM tokens WHERE user_id = 1"); got != 1 {
		t.Fatal("purge deleted the protected root token")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM users WHERE id = 3"); got != 1 {
		t.Fatal("purge deleted an administrator")
	}
	if got := queryInt64(t, db, "SELECT COUNT(*) FROM tokens WHERE user_id = 3"); got != 1 {
		t.Fatal("purge deleted an administrator token")
	}
}
