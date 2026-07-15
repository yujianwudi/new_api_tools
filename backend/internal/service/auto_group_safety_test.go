package service

import (
	"testing"

	"github.com/new-api-tools/backend/internal/database"
)

func TestUpdateUserGroupIfCurrentUsesCompareAndSwap(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		role INTEGER NOT NULL,
		"group" TEXT
	)`)
	db.MustExec(`INSERT INTO users(id, role, "group") VALUES (1, 1, ''), (2, 100, 'default')`)

	svc := &AutoGroupService{db: database.Get()}
	affected, err := svc.updateUserGroupIfCurrent(1, "vip", "default")
	if err != nil || affected != 1 {
		t.Fatalf("expected normalized default group update to affect one row, affected=%d err=%v", affected, err)
	}

	db.MustExec(`UPDATE users SET "group" = 'manual' WHERE id = 1`)
	affected, err = svc.updateUserGroupIfCurrent(1, "premium", "vip")
	if err != nil {
		t.Fatalf("compare-and-swap update returned error: %v", err)
	}
	if affected != 0 {
		t.Fatalf("concurrent group change was overwritten, affected=%d", affected)
	}
	var current string
	if err := db.Get(&current, `SELECT "group" FROM users WHERE id = 1`); err != nil {
		t.Fatalf("read current group: %v", err)
	}
	if current != "manual" {
		t.Fatalf("concurrent group was not preserved: %q", current)
	}

	affected, err = svc.updateUserGroupIfCurrent(2, "vip", "default")
	if err != nil || affected != 0 {
		t.Fatalf("root user guard failed, affected=%d err=%v", affected, err)
	}
}
