package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/database"
)

func inviteTopUpTestTimestamp(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.Local)
	if err != nil {
		t.Fatalf("parse fixture timestamp %q: %v", value, err)
	}
	return parsed.Unix()
}

func installInviteTopUpFixture(t *testing.T) {
	t.Helper()
	db := installSQLiteForTests(t)
	db.SetMaxOpenConns(1)
	db.MustExec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT,
			display_name TEXT,
			aff_count INTEGER,
			inviter_id INTEGER,
			deleted_at INTEGER
		);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			amount INTEGER,
			money REAL,
			status TEXT,
			create_time INTEGER,
			complete_time INTEGER
		);
		INSERT INTO users(id, username, display_name, aff_count, inviter_id, deleted_at) VALUES
			(1, 'Alice', 'ALICE Display', 7, NULL, NULL),
			(2, 'Bob', 'Bob Display', 3, NULL, NULL),
			(10, 'invitee-10', NULL, 0, 1, NULL),
			(11, 'invitee-11', NULL, 0, 1, NULL),
			(12, 'invitee-deleted', NULL, 0, 1, 1),
			(20, 'invitee-20', NULL, 0, 2, NULL);
	`)

	insert := `INSERT INTO top_ups(id, user_id, amount, money, status, create_time, complete_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	rows := [][]interface{}{
		{1, 10, 10, 1.0, "success", inviteTopUpTestTimestamp(t, "2026-01-01 23:55:00"), inviteTopUpTestTimestamp(t, "2026-01-02 00:05:00")},
		{2, 10, 20, 2.0, "completed", inviteTopUpTestTimestamp(t, "2026-01-03 02:00:00"), inviteTopUpTestTimestamp(t, "2026-01-02 12:00:00")},
		{3, 11, 30, 3.0, "1", inviteTopUpTestTimestamp(t, "2026-01-01 01:00:00"), inviteTopUpTestTimestamp(t, "2026-01-02 12:00:00")},
		{4, 11, 40, 4.0, "success", inviteTopUpTestTimestamp(t, "2026-01-02 23:00:00"), inviteTopUpTestTimestamp(t, "2026-01-03 00:00:00")},
		{5, 11, 50, 5.0, "pending", inviteTopUpTestTimestamp(t, "2026-01-02 02:00:00"), inviteTopUpTestTimestamp(t, "2026-01-02 02:05:00")},
		{6, 20, 60, 6.0, "success", inviteTopUpTestTimestamp(t, "2026-01-02 03:00:00"), inviteTopUpTestTimestamp(t, "2026-01-02 03:05:00")},
		{7, 12, 70, 7.0, "SUCCESS", inviteTopUpTestTimestamp(t, "2026-01-01 05:00:00"), inviteTopUpTestTimestamp(t, "2026-01-02 06:00:00")},
	}
	for _, row := range rows {
		if _, err := db.Exec(insert, row...); err != nil {
			t.Fatalf("insert top-up fixture: %v", err)
		}
	}
}

func withInviteTopUpTestTimezone(t *testing.T) {
	t.Helper()
	previous := time.Local
	time.Local = time.FixedZone("Asia/Shanghai", 8*60*60)
	t.Cleanup(func() { time.Local = previous })
}

func TestInviteTopUpAnalysisUnifiesStatusCompletionWindowAndFingerprint(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)

	params := AffiliateStatsParams{
		Page: 1, PageSize: 10, Search: "aLiCe",
		StartDate: "2026-01-02", EndDate: "2026-01-02",
		SortBy: "success_topup_count", SortDir: "desc",
		AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
	}
	list, err := ListAffiliateStats(params)
	if err != nil {
		t.Fatalf("ListAffiliateStats returned error: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("list total/items = %d/%d, want 1/1: %+v", list.Total, len(list.Items), list)
	}
	row := list.Items[0]
	if row.InviterID != 1 || row.SuccessTopUpCount != 4 || row.WindowPayingInviteeCount != 3 {
		t.Fatalf("unexpected window counts: %+v", row)
	}
	if len(row.DetailEvidenceHash) != 64 {
		t.Fatalf("detail evidence hash = %q, want SHA-256 hex digest", row.DetailEvidenceHash)
	}
	if row.CurrentInviteeCount != 2 || row.RewardedInviteCount != 7 {
		t.Fatalf("invite count scopes were mixed: %+v", row)
	}
	if list.Summary.SuccessTopUpCount != row.SuccessTopUpCount ||
		list.Summary.WindowPayingInviteeCount != row.WindowPayingInviteeCount ||
		list.Summary.QueryFingerprint != list.QueryFingerprint || list.Summary.AsOf != list.AsOf {
		t.Fatalf("bundled summary and list do not share one snapshot: row=%+v summary=%+v", row, list.Summary)
	}
	if row.SuccessAmount != nil || row.SuccessMoney != nil {
		t.Fatalf("unknown-unit money was aggregated: %+v", row)
	}
	if list.SourceState != inviteTopUpStateUnreconciled || list.Currency != "XXX" || list.Unit != inviteTopUpSourceUnit {
		t.Fatalf("unsafe source metadata: %+v", list.AffiliateAnalysisMetadata)
	}
	if list.Timezone != inviteTopUpTimezone {
		t.Fatalf("analysis timezone = %q, want %q", list.Timezone, inviteTopUpTimezone)
	}
	if list.SnapshotConsistency != "repeatable_read_bundle" || list.Summary.SnapshotConsistency != "repeatable_read_bundle" {
		t.Fatalf("bundled snapshot consistency is not explicit: list=%+v summary=%+v", list.AffiliateAnalysisMetadata, list.Summary.AffiliateAnalysisMetadata)
	}

	summary, err := GetAffiliateStatsSummary(params)
	if err != nil {
		t.Fatalf("GetAffiliateStatsSummary returned error: %v", err)
	}
	if summary.QueryFingerprint != list.QueryFingerprint || summary.AsOf != list.AsOf {
		t.Fatalf("parent fingerprints/as_of differ: list=%+v summary=%+v", list.AffiliateAnalysisMetadata, summary.AffiliateAnalysisMetadata)
	}
	if summary.SnapshotConsistency != "live_unbound" {
		t.Fatalf("standalone compatibility summary must disclose live_unbound, got %q", summary.SnapshotConsistency)
	}
	if summary.WindowActiveInviterCount != 1 || summary.SuccessTopUpCount != row.SuccessTopUpCount ||
		summary.WindowPayingInviteeCount != row.WindowPayingInviteeCount || summary.CurrentInviteeCount != row.CurrentInviteeCount ||
		summary.RewardedInviteCount != row.RewardedInviteCount {
		t.Fatalf("summary and list counts differ: row=%+v summary=%+v", row, summary)
	}
	if summary.TotalAmount != nil || summary.TotalMoney != nil {
		t.Fatalf("summary fabricated unknown-unit totals: %+v", summary)
	}
	wildcardParams := params
	wildcardParams.Search = "Alice%"
	wildcardList, err := ListAffiliateStats(wildcardParams)
	if err != nil {
		t.Fatalf("literal wildcard search returned error: %v", err)
	}
	if wildcardList.Total != 0 {
		t.Fatalf("search wildcard was not escaped: %+v", wildcardList)
	}

	detailParams := params
	detailParams.PageSize = 2
	detailParams.ExpectedFingerprint = list.QueryFingerprint
	expectedDetailTotal := row.SuccessTopUpCount
	detailParams.ExpectedDetailTotal = &expectedDetailTotal
	detailParams.ExpectedDetailEvidenceHash = row.DetailEvidenceHash
	// Page size is pagination, not part of the parent query fingerprint.
	details, err := ListAffiliateTopUpDetails(1, detailParams)
	if err != nil {
		t.Fatalf("ListAffiliateTopUpDetails returned error: %v", err)
	}
	if details.QueryFingerprint != list.QueryFingerprint || details.Total != row.SuccessTopUpCount || details.TotalPages != 2 {
		t.Fatalf("detail contract differs from parent: %+v", details)
	}
	if details.SnapshotConsistency != "parent_evidence_bound" {
		t.Fatalf("detail snapshot consistency = %q", details.SnapshotConsistency)
	}
	if details.DetailEvidenceHash != row.DetailEvidenceHash {
		t.Fatalf("detail evidence hash = %q, want parent hash %q", details.DetailEvidenceHash, row.DetailEvidenceHash)
	}
	gotPageOne := []int64{details.Items[0].ID, details.Items[1].ID}
	if want := []int64{3, 2}; !reflect.DeepEqual(gotPageOne, want) {
		t.Fatalf("stable detail order page 1 = %v, want %v", gotPageOne, want)
	}
	detailParams.Page = 2
	pageTwo, err := ListAffiliateTopUpDetails(1, detailParams)
	if err != nil {
		t.Fatalf("detail page 2 returned error: %v", err)
	}
	gotPageTwo := []int64{pageTwo.Items[0].ID, pageTwo.Items[1].ID}
	if want := []int64{7, 1}; !reflect.DeepEqual(gotPageTwo, want) {
		t.Fatalf("stable detail order page 2 = %v, want %v", gotPageTwo, want)
	}
}

func TestInviteTopUpDatesUseExplicitShanghaiTimezone(t *testing.T) {
	previous := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = previous })

	query, err := normalizeAffiliateParams(AffiliateStatsParams{
		Page: 1, PageSize: 20, StartDate: "2026-01-02", EndDate: "2026-01-02", AsOf: 1,
	})
	if err != nil {
		t.Fatalf("normalizeAffiliateParams: %v", err)
	}
	wantStart := time.Date(2026, 1, 2, 0, 0, 0, 0, inviteTopUpLocation).Unix()
	wantEnd := time.Date(2026, 1, 3, 0, 0, 0, 0, inviteTopUpLocation).Unix()
	if query.start == nil || *query.start != wantStart || query.endExclusive == nil || *query.endExclusive != wantEnd {
		t.Fatalf("Shanghai bounds = %v/%v, want %d/%d", query.start, query.endExclusive, wantStart, wantEnd)
	}
}

func TestInviteTopUpFingerprintV2MatchesCrossLanguageFixture(t *testing.T) {
	query, err := normalizeAffiliateParams(AffiliateStatsParams{
		Page: 1, PageSize: 20,
		Search: " Alice|Bob\nx=1 ", StartDate: " 2026-01-02 ", EndDate: "2026-01-03",
		SortBy: "LAST_TOPUP_AT", SortDir: "ASC", AsOf: 1767225600,
	})
	if err != nil {
		t.Fatalf("normalizeAffiliateParams: %v", err)
	}
	const want = "7bfec800b7080f1b3ebdcab2414fdb0969f69822ae4ca99791182cd8c8707208"
	if query.fingerprint != want {
		t.Fatalf("v2 fingerprint = %q, want %q", query.fingerprint, want)
	}
}

func TestInviteTopUpAnalysisRejectsInvalidFilters(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	base := AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00")}
	tests := []struct {
		name   string
		mutate func(*AffiliateStatsParams)
	}{
		{name: "bad start date", mutate: func(p *AffiliateStatsParams) { p.StartDate = "2026-02-30" }},
		{name: "bad end date", mutate: func(p *AffiliateStatsParams) { p.EndDate = "not-a-date" }},
		{name: "reversed dates", mutate: func(p *AffiliateStatsParams) { p.StartDate, p.EndDate = "2026-01-03", "2026-01-01" }},
		{name: "unknown sort", mutate: func(p *AffiliateStatsParams) { p.SortBy = "success_money" }},
		{name: "bad sort direction", mutate: func(p *AffiliateStatsParams) { p.SortDir = "sideways" }},
		{name: "oversized page", mutate: func(p *AffiliateStatsParams) { p.PageSize = 101 }},
		{name: "negative page", mutate: func(p *AffiliateStatsParams) { p.Page = -1 }},
		{name: "bad as of", mutate: func(p *AffiliateStatsParams) { p.AsOf = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := base
			tt.mutate(&params)
			if _, err := normalizeAffiliateParams(params); !errors.Is(err, ErrInvalidAffiliateStatsParams) {
				t.Fatalf("error = %v, want ErrInvalidAffiliateStatsParams", err)
			}
		})
	}
}

func TestInviteTopUpDetailRejectsParentFingerprintMismatch(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)
	_, err := ListAffiliateTopUpDetails(1, AffiliateStatsParams{
		Page: 1, PageSize: 20, StartDate: "2026-01-02", EndDate: "2026-01-02",
		AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"), ExpectedFingerprint: strings.Repeat("0", 64),
		ExpectedDetailTotal: int64Pointer(4), ExpectedDetailEvidenceHash: strings.Repeat("a", 64),
	})
	if !errors.Is(err, ErrInvalidAffiliateStatsParams) {
		t.Fatalf("fingerprint mismatch error = %v, want invalid parameters", err)
	}
}

func TestInviteTopUpDetailRejectsChangedParentTotal(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)
	expected := int64(99)
	params := AffiliateStatsParams{
		Page: 1, PageSize: 20, AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
		ExpectedDetailTotal: &expected, ExpectedDetailEvidenceHash: strings.Repeat("a", 64),
	}
	normalized, err := normalizeAffiliateParams(params)
	if err != nil {
		t.Fatal(err)
	}
	params.ExpectedFingerprint = normalized.fingerprint
	_, err = ListAffiliateTopUpDetails(1, params)
	if !errors.Is(err, ErrAffiliateSnapshotChanged) {
		t.Fatalf("error = %v, want ErrAffiliateSnapshotChanged", err)
	}
}

func TestInviteTopUpDetailRejectsSameCountEvidenceMutationOutsideRequestedPage(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)
	params := AffiliateStatsParams{
		Page: 1, PageSize: 20, Search: "Alice",
		StartDate: "2026-01-02", EndDate: "2026-01-02",
		AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
	}
	parent, err := ListAffiliateStats(params)
	if err != nil {
		t.Fatalf("ListAffiliateStats returned error: %v", err)
	}
	if len(parent.Items) != 1 {
		t.Fatalf("parent items = %d, want 1", len(parent.Items))
	}
	row := parent.Items[0]
	if len(row.DetailEvidenceHash) != 64 {
		t.Fatalf("parent evidence hash = %q, want SHA-256 hex digest", row.DetailEvidenceHash)
	}

	// Mutating an exposed field keeps COUNT(*) unchanged. The detail contract
	// must still reject the old parent snapshot instead of showing replacement
	// content under the old aggregate row.
	database.Get().DB.MustExec(`UPDATE top_ups SET amount = amount + 1 WHERE id = 1`)
	detailParams := params
	// ID 1 sorts outside the requested first detail page. The evidence hash
	// must still cover it instead of hashing only the displayed page.
	detailParams.PageSize = 1
	detailParams.ExpectedFingerprint = parent.QueryFingerprint
	detailParams.ExpectedDetailTotal = int64Pointer(row.SuccessTopUpCount)
	detailParams.ExpectedDetailEvidenceHash = row.DetailEvidenceHash
	_, err = ListAffiliateTopUpDetails(row.InviterID, detailParams)
	if !errors.Is(err, ErrAffiliateSnapshotChanged) {
		t.Fatalf("same-count evidence mutation error = %v, want ErrAffiliateSnapshotChanged", err)
	}
	if !strings.Contains(err.Error(), "detail evidence hash changed") {
		t.Fatalf("same-count mutation did not reach the evidence-hash guard: %v", err)
	}
}

func TestInviteTopUpDetailMissingInviterReturnsNotFound(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	installInviteTopUpFixture(t)
	params := AffiliateStatsParams{
		Page: 1, PageSize: 20, AsOf: inviteTopUpTestTimestamp(t, "2026-01-04 00:00:00"),
		ExpectedDetailTotal: int64Pointer(0), ExpectedDetailEvidenceHash: strings.Repeat("a", 64),
	}
	normalized, err := normalizeAffiliateParams(params)
	if err != nil {
		t.Fatal(err)
	}
	params.ExpectedFingerprint = normalized.fingerprint
	_, err = ListAffiliateTopUpDetails(999, params)
	if !errors.Is(err, ErrAffiliateInviterNotFound) {
		t.Fatalf("missing inviter error = %v, want ErrAffiliateInviterNotFound", err)
	}
}

func TestInviteTopUpDetailScanFailureIsUnavailable(t *testing.T) {
	withInviteTopUpTestTimezone(t)
	db := installSQLiteForTests(t)
	db.SetMaxOpenConns(1)
	db.MustExec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, display_name TEXT, aff_count INTEGER, inviter_id INTEGER, deleted_at INTEGER);
		CREATE TABLE top_ups (id INTEGER PRIMARY KEY, user_id INTEGER, amount TEXT, money REAL, status TEXT, create_time INTEGER, complete_time INTEGER);
		INSERT INTO users VALUES (1, 'Alice', NULL, 1, NULL, NULL), (10, 'invitee', NULL, 0, 1, NULL);
		INSERT INTO top_ups VALUES (1, 10, 'not-an-integer', 1.0, 'success', 1, 2);
	`)
	params := AffiliateStatsParams{
		Page: 1, PageSize: 20, AsOf: 10, ExpectedDetailTotal: int64Pointer(1),
		ExpectedDetailEvidenceHash: strings.Repeat("a", 64),
	}
	normalized, normalizeErr := normalizeAffiliateParams(params)
	if normalizeErr != nil {
		t.Fatal(normalizeErr)
	}
	params.ExpectedFingerprint = normalized.fingerprint
	_, err := ListAffiliateTopUpDetails(1, params)
	if err == nil || !strings.Contains(err.Error(), "scan invite top-up detail evidence") {
		t.Fatalf("scan failure was not surfaced as unavailable: %v", err)
	}
}
