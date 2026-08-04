package service

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func installUserQueryFixture(t *testing.T, includeLinuxDO bool) (*sqlx.DB, *sqlx.DB, *UserManagementService) {
	t.Helper()
	mainDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open main sqlite: %v", err)
	}
	logDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open log sqlite: %v", err)
	}
	mainDB.SetMaxOpenConns(1)
	logDB.SetMaxOpenConns(1)
	linuxColumn := ""
	if includeLinuxDO {
		linuxColumn = ", linux_do_id TEXT"
	}
	mainDB.MustExec(fmt.Sprintf(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
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
			oidc_id TEXT%s,
			deleted_at INTEGER
		)`, linuxColumn))
	logDB.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		type INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`)
	mainManager := &database.Manager{DB: mainDB, IsPG: false}
	logManager := &database.Manager{DB: logDB, IsPG: false}
	database.SetForTesting(mainManager)
	database.SetLogForTesting(logManager, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = logDB.Close()
		_ = mainDB.Close()
	})
	return mainDB, logDB, &UserManagementService{db: mainManager, logDB: logManager}
}

func userListIDs(t *testing.T, result map[string]interface{}) []int64 {
	t.Helper()
	rows, ok := result["items"].([]map[string]interface{})
	if !ok {
		t.Fatalf("items type = %T", result["items"])
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, toInt64(row["id"]))
	}
	return ids
}

func TestGetUsersActivityFiltersShareStatisticsTruth(t *testing.T) {
	mainDB, logDB, svc := installUserQueryFixture(t, true)
	now := time.Now().Unix()
	mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES
		(1, 'one-day', 1), (2, 'ten-day', 1), (3, 'thirty-one-day', 1), (4, 'never', 0)`)
	logDB.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES
		(1, 2, ?), (2, 2, ?), (3, 5, ?)`, now-24*3600, now-10*24*3600, now-31*24*3600)

	cases := []struct {
		activity string
		wantID   int64
		wantLast *int64
	}{
		{ActivityActive, 1, int64Pointer(now - 24*3600)},
		{ActivityInactive, 2, int64Pointer(now - 10*24*3600)},
		{ActivityVeryInactive, 3, int64Pointer(now - 31*24*3600)},
		{ActivityNever, 4, nil},
	}
	for _, test := range cases {
		t.Run(test.activity, func(t *testing.T) {
			result, err := svc.GetUsers(ListUsersParams{
				Page: 1, PageSize: 20, ActivityFilter: test.activity, OrderBy: "id", OrderDir: "ASC",
			})
			if err != nil {
				t.Fatalf("GetUsers: %v", err)
			}
			ids := userListIDs(t, result)
			if len(ids) != 1 || ids[0] != test.wantID {
				t.Fatalf("ids = %v, want [%d]", ids, test.wantID)
			}
			if got := toInt64(result["total"]); got != 1 {
				t.Fatalf("total = %d, want 1", got)
			}
			rows := result["items"].([]map[string]interface{})
			if got := toString(rows[0]["activity_level"]); got != test.activity {
				t.Fatalf("activity_level = %q, want %q", got, test.activity)
			}
			if test.wantLast == nil {
				if rows[0]["last_request_time"] != nil {
					t.Fatalf("last_request_time = %#v, want nil", rows[0]["last_request_time"])
				}
			} else if got := toInt64(rows[0]["last_request_time"]); got != *test.wantLast {
				t.Fatalf("last_request_time = %d, want %d", got, *test.wantLast)
			}
		})
	}

	stats, err := svc.GetActivityStats(false)
	if err != nil {
		t.Fatalf("GetActivityStats: %v", err)
	}
	for key, want := range map[string]int64{
		"active_users": 1, "inactive_users": 1, "very_inactive_users": 1, "never_requested": 1,
	} {
		if got := toInt64(stats[key]); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
}

func TestQuickActivityStatsExposePartialNullsInsteadOfZero(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'never', 0)`)
	stats, err := svc.GetActivityStats(true)
	if err != nil {
		t.Fatalf("GetActivityStats quick: %v", err)
	}
	for _, key := range []string{"active_users", "inactive_users", "very_inactive_users"} {
		if stats[key] != nil {
			t.Fatalf("%s = %#v, want nil until log evidence is calculated", key, stats[key])
		}
	}
	if stats["source_state"] != "partial" || toInt64(stats["never_requested"]) != 1 {
		t.Fatalf("quick stats truth metadata = %#v", stats)
	}
}

func TestUserActivityTreatsMissingAndFutureOnlyEvidenceAsUnknown(t *testing.T) {
	mainDB, logDB, svc := installUserQueryFixture(t, true)
	now := time.Now().Unix()
	previousNow := userActivityNow
	userActivityNow = func() time.Time { return time.Unix(now, 0) }
	t.Cleanup(func() { userActivityNow = previousNow })
	mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES
		(1, 'future-only', 1), (2, 'missing-history', 1), (3, 'old-evidence', 1)`)
	logDB.MustExec(`INSERT INTO logs (user_id, type, created_at) VALUES
		(1, 2, ?), (3, 2, ?)`, now+3600, now-31*24*3600)

	result, err := svc.GetUsers(ListUsersParams{
		Page: 1, PageSize: 20, ActivityFilter: ActivityVeryInactive, OrderBy: "id", OrderDir: "ASC",
	})
	if err != nil {
		t.Fatalf("GetUsers: %v", err)
	}
	rows := result["items"].([]map[string]interface{})
	if len(rows) != 1 || toInt64(rows[0]["id"]) != 3 || toString(rows[0]["activity_level"]) != ActivityVeryInactive {
		t.Fatalf("rows = %#v, want only the user with explicit old evidence", rows)
	}
	if result["source_state"] != "partial" {
		t.Fatalf("filtered source_state = %#v, want partial because two candidates lack historical evidence", result["source_state"])
	}
	asOf := toInt64(result["as_of"])
	if asOf != now {
		t.Fatalf("as_of = %d, want %d", asOf, now)
	}

	all, err := svc.GetUsers(ListUsersParams{Page: 1, PageSize: 20, OrderBy: "id", OrderDir: "ASC"})
	if err != nil {
		t.Fatalf("unfiltered GetUsers: %v", err)
	}
	allRows := all["items"].([]map[string]interface{})
	if len(allRows) != 3 || toString(allRows[0]["activity_level"]) != ActivityUnknown ||
		toString(allRows[1]["activity_level"]) != ActivityUnknown ||
		toString(allRows[2]["activity_level"]) != ActivityVeryInactive {
		t.Fatalf("unfiltered activity truth = %#v", allRows)
	}
	if allRows[0]["last_request_time"] != nil || allRows[1]["last_request_time"] != nil {
		t.Fatalf("missing/future evidence leaked into last_request_time: %#v", allRows)
	}
	if all["source_state"] != "partial" {
		t.Fatalf("unfiltered source_state = %#v, want partial", all["source_state"])
	}

	stats, err := svc.GetActivityStats(false)
	if err != nil {
		t.Fatalf("GetActivityStats: %v", err)
	}
	for _, key := range []string{"active_users", "inactive_users", "very_inactive_users"} {
		if stats[key] != nil {
			t.Fatalf("%s = %#v, want nil while activity history is incomplete", key, stats[key])
		}
	}
	if stats["source_state"] != "partial" || toInt64(stats["unknown_users"]) != 2 {
		t.Fatalf("stats truth metadata = %#v", stats)
	}
	if got := toInt64(stats["as_of"]); got != now {
		t.Fatalf("stats as_of = %d, want %d", got, now)
	}
}

func TestActivityClassifierUsesExactSevenAndThirtyDayBoundaries(t *testing.T) {
	asOf := time.Unix(1_800_000_000, 0).Unix()
	lastRequestTimes := map[int64]int64{
		1: asOf - ActiveThreshold + 1,
		2: asOf - ActiveThreshold,
		3: asOf - InactiveThreshold,
		4: asOf - InactiveThreshold - 1,
	}
	active, recent := activitySetsFromLastRequestTimes(lastRequestTimes, asOf)
	if !active[1] || active[2] {
		t.Fatalf("7-day boundary classified incorrectly: active=%v", active)
	}
	if !recent[2] || !recent[3] || recent[4] {
		t.Fatalf("30-day boundary classified incorrectly: recent=%v", recent)
	}
	if got := classifyUserActivity(1, lastRequestTimes[2], asOf); got != ActivityInactive {
		t.Fatalf("exactly 7d = %q, want inactive", got)
	}
	if got := classifyUserActivity(1, lastRequestTimes[3], asOf); got != ActivityInactive {
		t.Fatalf("exactly 30d = %q, want inactive", got)
	}
	if got := classifyUserActivity(1, lastRequestTimes[4], asOf); got != ActivityVeryInactive {
		t.Fatalf("older than 30d = %q, want very_inactive", got)
	}
	if got := classifyUserActivity(1, 0, asOf); got != ActivityUnknown {
		t.Fatalf("missing evidence = %q, want unknown", got)
	}
}

func TestGetUsersRejectsInvalidFiltersAndUnavailableSources(t *testing.T) {
	_, _, svc := installUserQueryFixture(t, false)
	cases := []struct {
		name   string
		params ListUsersParams
		want   error
	}{
		{"activity", ListUsersParams{ActivityFilter: "bogus"}, ErrInvalidActivityFilter},
		{"source", ListUsersParams{SourceFilter: "bogus"}, ErrInvalidSourceFilter},
		{"unsupported source", ListUsersParams{SourceFilter: "linux_do"}, ErrUnsupportedSourceFilter},
		{"group", ListUsersParams{GroupFilter: "bad\nvalue"}, ErrInvalidGroupFilter},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := svc.GetUsers(test.params)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestGetUsersCanonicalizesDefaultGroups(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users (id, username, "group") VALUES
		(1, 'explicit', 'default'), (2, 'null-group', NULL), (3, 'empty-group', '')`)
	result, err := svc.GetUsers(ListUsersParams{
		Page: 1, PageSize: 20, GroupFilter: "default", OrderBy: "id", OrderDir: "ASC",
	})
	if err != nil {
		t.Fatalf("GetUsers: %v", err)
	}
	if ids := userListIDs(t, result); fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("ids = %v, want [1 2 3]", ids)
	}
	for _, row := range result["items"].([]map[string]interface{}) {
		if got := toString(row["group"]); got != "default" {
			t.Fatalf("group = %q, want default", got)
		}
	}
}

func TestGetUsersUsesOneSourcePriorityForFilterAndDisplay(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users (id, username, github_id, linux_do_id) VALUES
		(1, 'multi', 'gh-1', 'linux-1'), (2, 'github-only', 'gh-2', NULL)`)
	linuxResult, err := svc.GetUsers(ListUsersParams{Page: 1, PageSize: 20, SourceFilter: "linux_do", OrderBy: "id", OrderDir: "ASC"})
	if err != nil {
		t.Fatalf("linux_do GetUsers: %v", err)
	}
	if ids := userListIDs(t, linuxResult); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("linux_do ids = %v, want [1]", ids)
	}
	if got := toString(linuxResult["items"].([]map[string]interface{})[0]["source"]); got != "linux_do" {
		t.Fatalf("display source = %q, want linux_do", got)
	}
	githubResult, err := svc.GetUsers(ListUsersParams{Page: 1, PageSize: 20, SourceFilter: "github", OrderBy: "id", OrderDir: "ASC"})
	if err != nil {
		t.Fatalf("github GetUsers: %v", err)
	}
	if ids := userListIDs(t, githubResult); fmt.Sprint(ids) != "[2]" {
		t.Fatalf("github ids = %v, want [2]", ids)
	}
}

func TestOAuthCapabilityFailureIsRetryable(t *testing.T) {
	mainDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mainDB.SetMaxOpenConns(1)
	manager := &database.Manager{DB: mainDB, IsPG: false}
	database.SetForTesting(manager)
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = mainDB.Close()
	})
	svc := &UserManagementService{db: manager, logDB: manager}
	if _, err := svc.probeAvailableOAuthColumns(); !errors.Is(err, ErrOAuthCapabilitiesUnavailable) {
		t.Fatalf("first probe error = %v, want ErrOAuthCapabilitiesUnavailable", err)
	}
	mainDB.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, github_id TEXT)`)
	columns, err := svc.probeAvailableOAuthColumns()
	if err != nil {
		t.Fatalf("retry probe: %v", err)
	}
	if fmt.Sprint(columns) != "[github_id]" {
		t.Fatalf("columns = %v, want [github_id]", columns)
	}
}

func TestGetUsersStableTieBreakAndPageSizeCap(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	for id := 1; id <= 41; id++ {
		mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES (?, ?, 0)`, id, fmt.Sprintf("user-%02d", id))
	}
	var all []int64
	for page := 1; page <= 3; page++ {
		result, err := svc.GetUsers(ListUsersParams{Page: page, PageSize: 20, OrderBy: "request_count", OrderDir: "DESC"})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		all = append(all, userListIDs(t, result)...)
	}
	if len(all) != 41 {
		t.Fatalf("combined rows = %d, want 41", len(all))
	}
	for index, id := range all {
		want := int64(41 - index)
		if id != want {
			t.Fatalf("id[%d] = %d, want %d; order=%v", index, id, want, all)
		}
	}
	result, err := svc.GetUsers(ListUsersParams{Page: 1, PageSize: 101, OrderBy: "id"})
	if err != nil {
		t.Fatalf("page-size cap: %v", err)
	}
	if got := result["page_size"]; got != userListMaxPageSize {
		t.Fatalf("page_size = %#v, want %d", got, userListMaxPageSize)
	}
}

func TestActivityCandidatePopulationLimitFailsClosed(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	asOf := time.Now().Unix()
	for id := int64(1); id <= 3; id++ {
		mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES (?, ?, 1)`, id, fmt.Sprintf("candidate-%d", id))
	}
	lastRequestTimes := map[int64]int64{
		1: asOf - InactiveThreshold - 1,
		2: asOf - InactiveThreshold - 1,
		3: asOf - InactiveThreshold - 1,
	}

	_, _, _, err := svc.activityFilteredUserPageWithLimit(
		"u.id, u.request_count",
		"u.deleted_at IS NULL AND u.request_count > 0",
		"id", "ASC", nil,
		ActivityVeryInactive, lastRequestTimes, asOf,
		1, 20, 2,
	)
	if !errors.Is(err, ErrActivityFilterScaleExceeded) {
		t.Fatalf("error = %v, want ErrActivityFilterScaleExceeded", err)
	}
}

func TestGetUsersFailsClosedWhenLogQueryFails(t *testing.T) {
	mainDB, logDB, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users (id, username, request_count) VALUES (1, 'requested', 1)`)
	if err := logDB.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := svc.GetUsers(ListUsersParams{Page: 1, PageSize: 20, ActivityFilter: ActivityActive})
	if !errors.Is(err, ErrActivityLogUnavailable) {
		t.Fatalf("error = %v, want ErrActivityLogUnavailable", err)
	}
}

func TestGetInvitedUsersUsesFullPopulationStatisticsAndUserIDContract(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users
		(id, username, display_name, aff_code, aff_count, aff_quota, aff_history)
		VALUES (100, 'inviter', 'Inviter', 'AFF', 25, 50, 60)`)
	for id := 1; id <= 25; id++ {
		status := 1
		if id%5 == 0 {
			status = 2
		}
		mainDB.MustExec(`INSERT INTO users
			(id, username, email, status, quota, used_quota, request_count, "group", role, inviter_id)
			VALUES (?, ?, ?, ?, 100, 10, ?, ?, 1, 100)`,
			id, fmt.Sprintf("invitee-%02d", id), fmt.Sprintf("u%d@example.com", id), status, id, "")
	}

	var firstStats InvitedUserStats
	var identity *InvitedUsersSnapshotIdentity
	for page := 1; page <= 3; page++ {
		result, err := svc.GetInvitedUsers(100, page, 10, identity)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if result.Total != 25 || result.Stats.TotalInvited != 25 || result.Stats.RequestedCount != 25 || result.Stats.BannedCount != 5 {
			t.Fatalf("page %d stats = %+v total=%d", page, result.Stats, result.Total)
		}
		if page == 1 {
			firstStats = result.Stats
			identity = &InvitedUsersSnapshotIdentity{AsOf: result.AsOf, QueryFingerprint: result.QueryFingerprint}
			decoded, decodeErr := hex.DecodeString(result.QueryFingerprint)
			if result.AsOf <= 0 || decodeErr != nil || len(decoded) != 32 || strings.ToLower(result.QueryFingerprint) != result.QueryFingerprint {
				t.Fatalf("invalid first-page snapshot identity: as_of=%d fingerprint=%q err=%v", result.AsOf, result.QueryFingerprint, decodeErr)
			}
		} else if result.Stats.TotalRequests != firstStats.TotalRequests || *result.Stats.TotalUsedQuota != *firstStats.TotalUsedQuota {
			t.Fatalf("stats changed across pages: first=%+v page%d=%+v", firstStats, page, result.Stats)
		}
		if identity != nil && (result.AsOf != identity.AsOf || result.QueryFingerprint != identity.QueryFingerprint) {
			t.Fatalf("page %d identity changed: got as_of=%d fingerprint=%q want %+v", page, result.AsOf, result.QueryFingerprint, identity)
		}
		for _, item := range result.Items {
			if item.UserID <= 0 {
				t.Fatalf("invalid user_id in item: %+v", item)
			}
			if item.Group != "default" {
				t.Fatalf("group = %q, want default", item.Group)
			}
		}
	}
	if _, err := svc.GetInvitedUsers(999, 1, 10, nil); !errors.Is(err, ErrInviterNotFound) {
		t.Fatalf("missing inviter error = %v, want ErrInviterNotFound", err)
	}
}

func TestGetInvitedUsersRejectsCrossPageIdentityAndContentDrift(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*sqlx.DB, *InvitedUsersSnapshotIdentity)
		pageSize int
	}{
		{name: "as_of drift", mutate: func(_ *sqlx.DB, identity *InvitedUsersSnapshotIdentity) { identity.AsOf++ }, pageSize: 10},
		{name: "well-formed wrong fingerprint", mutate: func(_ *sqlx.DB, identity *InvitedUsersSnapshotIdentity) {
			identity.QueryFingerprint = strings.Repeat("f", 64)
		}, pageSize: 10},
		{name: "invitee added", mutate: func(db *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {
			db.MustExec(`INSERT INTO users (id, username, inviter_id) VALUES (99, 'added', 100)`)
		}, pageSize: 10},
		{name: "invitee deleted", mutate: func(db *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {
			db.MustExec(`UPDATE users SET deleted_at = 1 WHERE id = 1`)
		}, pageSize: 10},
		{name: "display field changed", mutate: func(db *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {
			db.MustExec(`UPDATE users SET display_name = 'changed' WHERE id = 1`)
		}, pageSize: 10},
		{name: "statistics evidence changed", mutate: func(db *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {
			db.MustExec(`UPDATE users SET request_count = request_count + 1 WHERE id = 1`)
		}, pageSize: 10},
		{name: "inviter changed", mutate: func(db *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {
			db.MustExec(`UPDATE users SET display_name = 'changed inviter' WHERE id = 100`)
		}, pageSize: 10},
		{name: "page size contract changed", mutate: func(_ *sqlx.DB, _ *InvitedUsersSnapshotIdentity) {}, pageSize: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mainDB, _, svc := installUserQueryFixture(t, true)
			mainDB.MustExec(`INSERT INTO users (id, username, display_name) VALUES (100, 'inviter', 'Inviter')`)
			for id := 1; id <= 12; id++ {
				mainDB.MustExec(`INSERT INTO users
					(id, username, display_name, status, used_quota, request_count, "group", inviter_id)
					VALUES (?, ?, ?, 1, ?, ?, 'default', 100)`, id, fmt.Sprintf("invitee-%02d", id), fmt.Sprintf("User %02d", id), id, id)
			}

			first, err := svc.GetInvitedUsers(100, 1, 10, nil)
			if err != nil {
				t.Fatalf("first page: %v", err)
			}
			identity := &InvitedUsersSnapshotIdentity{AsOf: first.AsOf, QueryFingerprint: first.QueryFingerprint}
			tt.mutate(mainDB, identity)
			if _, err := svc.GetInvitedUsers(100, 2, tt.pageSize, identity); !errors.Is(err, ErrInvitedUsersSnapshotMismatch) {
				t.Fatalf("error = %v, want ErrInvitedUsersSnapshotMismatch", err)
			}
		})
	}
}

func TestGetInvitedUsersRequiresBothIdentityFieldsAfterPageOne(t *testing.T) {
	mainDB, _, svc := installUserQueryFixture(t, true)
	mainDB.MustExec(`INSERT INTO users (id, username) VALUES (100, 'inviter')`)
	for id := 1; id <= 11; id++ {
		mainDB.MustExec(`INSERT INTO users (id, username, inviter_id) VALUES (?, ?, 100)`, id, fmt.Sprintf("invitee-%02d", id))
	}
	if _, err := svc.GetInvitedUsers(100, 2, 10, nil); !errors.Is(err, ErrInvitedUsersSnapshotMismatch) {
		t.Fatalf("missing identity error = %v", err)
	}
	if _, err := svc.GetInvitedUsers(100, 2, 10, &InvitedUsersSnapshotIdentity{AsOf: time.Now().Unix()}); !errors.Is(err, ErrInvitedUsersSnapshotMismatch) {
		t.Fatalf("as_of-only error = %v", err)
	}
}

func TestInvitedUsersViewerRedactionRemovesPII(t *testing.T) {
	email, affCode := "private@example.com", "SECRET"
	result := InvitedUsersResult{
		Inviter: InviterInfo{AffCode: &affCode, AffQuota: int64Pointer(1), AffHistory: int64Pointer(2)},
		Items:   []InvitedUserItem{{Email: &email, Quota: int64Pointer(3), UsedQuota: int64Pointer(4), Role: int64Pointer(10)}},
		Stats:   InvitedUserStats{TotalUsedQuota: int64Pointer(4)},
	}.RedactForViewer()
	if result.Inviter.AffCode != nil || result.Inviter.AffQuota != nil || result.Inviter.AffHistory != nil || result.Stats.TotalUsedQuota != nil {
		t.Fatalf("inviter/stats PII remained: %+v", result)
	}
	item := result.Items[0]
	if item.Email != nil || item.Quota != nil || item.UsedQuota != nil || item.Role != nil {
		t.Fatalf("item PII remained: %+v", item)
	}
}
