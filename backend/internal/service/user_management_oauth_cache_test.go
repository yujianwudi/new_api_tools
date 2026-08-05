package service

import (
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func TestOAuthCapabilityCacheExpiresAndInvalidatesAcrossSchemaChanges(t *testing.T) {
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		display_name TEXT,
		email TEXT,
		role INTEGER NOT NULL DEFAULT 1,
		status INTEGER NOT NULL DEFAULT 1,
		quota INTEGER NOT NULL DEFAULT 0,
		used_quota INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		"group" TEXT,
		aff_code TEXT,
		remark TEXT,
		deleted_at INTEGER,
		github_id TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users(id, username, github_id) VALUES (1, 'cache-user', 'github-1')`)
	if err != nil {
		t.Fatal(err)
	}

	service := &UserManagementService{db: &database.Manager{DB: db, IsPG: false}}
	columns, err := service.probeAvailableOAuthColumns()
	if err != nil || !containsOAuthColumn(columns, "github_id") {
		t.Fatalf("initial OAuth columns = %v, %v", columns, err)
	}
	if _, err := db.Exec(`ALTER TABLE users DROP COLUMN github_id`); err != nil {
		t.Fatalf("drop cached OAuth column: %v", err)
	}
	if _, err := service.GetUsers(ListUsersParams{Page: 1, PageSize: 20}); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "github_id") {
		t.Fatalf("stale cached column did not fail visibly and trigger invalidation: %v", err)
	}
	columns, err = service.probeAvailableOAuthColumns()
	if err != nil || containsOAuthColumn(columns, "github_id") {
		t.Fatalf("removed OAuth column remained cached: %v, %v", columns, err)
	}

	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN oidc_id TEXT`); err != nil {
		t.Fatalf("add OAuth column: %v", err)
	}
	columns, err = service.probeAvailableOAuthColumns()
	if err != nil || containsOAuthColumn(columns, "oidc_id") {
		t.Fatalf("unexpired cache unexpectedly changed before TTL: %v, %v", columns, err)
	}
	key, err := service.oauthColumnCacheIdentity()
	if err != nil {
		t.Fatalf("OAuth cache key unavailable: %v", err)
	}
	value, ok := oauthColumnCache.Load(key)
	if !ok {
		t.Fatal("OAuth cache entry missing")
	}
	entry := value.(oauthColumnCacheEntry)
	entry.ExpiresAt = time.Now().Add(-time.Second)
	oauthColumnCache.Store(key, entry)
	columns, err = service.probeAvailableOAuthColumns()
	if err != nil || !containsOAuthColumn(columns, "oidc_id") {
		t.Fatalf("expired OAuth cache did not re-probe additive migration: %v, %v", columns, err)
	}
}

func containsOAuthColumn(columns []string, target string) bool {
	for _, column := range columns {
		if column == target {
			return true
		}
	}
	return false
}
