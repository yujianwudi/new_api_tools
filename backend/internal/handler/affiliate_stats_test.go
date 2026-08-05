package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/service"
	_ "modernc.org/sqlite"
)

func installInviteTopUpHandlerFixture(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
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
		INSERT INTO users VALUES
			(1, 'Alice', NULL, 1, NULL, NULL),
			(10, 'invitee', NULL, 0, 1, NULL);
		INSERT INTO top_ups VALUES (1, 10, 10, 1.0, 'success', 1, 2);
	`)
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db
}

func inviteTopUpHandlerParent(t *testing.T) *service.PaginatedAffiliateStats {
	t.Helper()
	parent, err := service.ListAffiliateStats(service.AffiliateStatsParams{Page: 1, PageSize: 20, AsOf: 10})
	if err != nil {
		t.Fatalf("load parent contract: %v", err)
	}
	if len(parent.Items) != 1 {
		t.Fatalf("parent items = %d, want 1", len(parent.Items))
	}
	return parent
}

func TestInviteTopUpAnalysisRoutesUseTruthfulNameAndKeepReadCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAffiliateStatsRoutes(router.Group("/api"))
	routes := map[string]bool{}
	for _, route := range router.Routes() {
		routes[route.Path] = true
	}
	for _, path := range []string{
		"/api/users/invite-topup-analysis",
		"/api/users/invite-topup-analysis/summary",
		"/api/users/invite-topup-analysis/:inviter_id/details",
		"/api/users/affiliate-stats",
		"/api/users/affiliate-stats/summary",
	} {
		if !routes[path] {
			t.Fatalf("missing invite top-up analysis route %q: %#v", path, routes)
		}
	}
}

func TestInviteTopUpAnalysisRoutesRequireOperatorRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	paths := []string{
		"/api/users/invite-topup-analysis?page=0",
		"/api/users/invite-topup-analysis/summary?page=0",
		"/api/users/invite-topup-analysis/0/details",
		"/api/users/affiliate-stats?page=0",
		"/api/users/affiliate-stats/summary?page=0",
	}
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleOperator} {
		t.Run(string(role), func(t *testing.T) {
			router := gin.New()
			api := router.Group("/api")
			api.Use(func(c *gin.Context) {
				auth.SetRole(c, role)
				c.Next()
			})
			RegisterAffiliateStatsRoutes(api)
			for _, path := range paths {
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				want := http.StatusBadRequest
				if role == auth.RoleViewer {
					want = http.StatusForbidden
				}
				if recorder.Code != want {
					t.Fatalf("%s %s status = %d, want %d; body=%s", role, path, recorder.Code, want, recorder.Body.String())
				}
			}
		})
	}
}

func TestInviteTopUpAnalysisDatabaseFailureIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	t.Cleanup(func() { database.SetForTesting(nil) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/users/invite-topup-analysis?as_of=10", nil)
	ListAffiliateStats(ctx)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "INVITE_TOPUP_ANALYSIS_UNAVAILABLE") || strings.Contains(strings.ToLower(body), "database is closed") {
		t.Fatalf("unsafe or ambiguous unavailable response: %s", body)
	}
}

func TestInviteTopUpAnalysisHandlersRejectInvalidQueryBeforeDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		target  string
		handler gin.HandlerFunc
		params  gin.Params
	}{
		{name: "page zero", target: "/api/users/invite-topup-analysis?page=0", handler: ListAffiliateStats},
		{name: "page malformed", target: "/api/users/invite-topup-analysis?page=nope", handler: ListAffiliateStats},
		{name: "page too large", target: "/api/users/invite-topup-analysis?page=100001", handler: ListAffiliateStats},
		{name: "page size too large", target: "/api/users/invite-topup-analysis?page_size=101", handler: ListAffiliateStats},
		{name: "invalid date", target: "/api/users/invite-topup-analysis?start_date=2026-02-30", handler: ListAffiliateStats},
		{name: "unknown sort", target: "/api/users/invite-topup-analysis?sort_by=success_money", handler: ListAffiliateStats},
		{name: "invalid as of", target: "/api/users/invite-topup-analysis?as_of=0", handler: ListAffiliateStats},
		{name: "invalid expected total", target: "/api/users/invite-topup-analysis?expected_total=-1", handler: ListAffiliateStats},
		{name: "invalid inviter", target: "/api/users/invite-topup-analysis/0/details", handler: ListAffiliateTopUpDetails, params: gin.Params{{Key: "inviter_id", Value: "0"}}},
		{name: "missing detail contract", target: "/api/users/invite-topup-analysis/1/details?as_of=10", handler: ListAffiliateTopUpDetails, params: gin.Params{{Key: "inviter_id", Value: "1"}}},
		{name: "missing detail evidence hash", target: "/api/users/invite-topup-analysis/1/details?as_of=10&expected_total=0&query_fingerprint=" + strings.Repeat("0", 64), handler: ListAffiliateTopUpDetails, params: gin.Params{{Key: "inviter_id", Value: "1"}}},
		{name: "invalid detail evidence hash", target: "/api/users/invite-topup-analysis/1/details?as_of=10&expected_total=0&query_fingerprint=" + strings.Repeat("0", 64) + "&expected_evidence_hash=not-a-sha256", handler: ListAffiliateTopUpDetails, params: gin.Params{{Key: "inviter_id", Value: "1"}}},
		{name: "missing detail as of", target: "/api/users/invite-topup-analysis/1/details?expected_total=0&query_fingerprint=" + strings.Repeat("0", 64) + "&expected_evidence_hash=" + strings.Repeat("a", 64), handler: ListAffiliateTopUpDetails, params: gin.Params{{Key: "inviter_id", Value: "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, tt.target, nil)
			ctx.Params = tt.params
			tt.handler(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "INVALID_PARAMS") {
				t.Fatalf("missing stable invalid-params code: %s", recorder.Body.String())
			}
		})
	}
}

func TestInviteTopUpDetailSameCountMutationReturnsConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := installInviteTopUpHandlerFixture(t)
	parent := inviteTopUpHandlerParent(t)
	row := parent.Items[0]
	db.MustExec(`UPDATE top_ups SET amount = amount + 1 WHERE id = 1`)

	query := url.Values{
		"as_of":                  {"10"},
		"expected_total":         {"1"},
		"query_fingerprint":      {parent.QueryFingerprint},
		"expected_evidence_hash": {row.DetailEvidenceHash},
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/users/invite-topup-analysis/1/details?"+query.Encode(), nil)
	ctx.Params = gin.Params{{Key: "inviter_id", Value: "1"}}
	ListAffiliateTopUpDetails(ctx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "QUERY_SNAPSHOT_CHANGED") || strings.Contains(body, "untrusted-detail") {
		t.Fatalf("unsafe conflict response: %s", body)
	}
}

func TestInviteTopUpDetailMissingInviterReturnsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installInviteTopUpHandlerFixture(t)
	parent := inviteTopUpHandlerParent(t)
	query := url.Values{
		"as_of":                  {"10"},
		"expected_total":         {"0"},
		"query_fingerprint":      {parent.QueryFingerprint},
		"expected_evidence_hash": {strings.Repeat("a", 64)},
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/users/invite-topup-analysis/999/details?"+query.Encode(), nil)
	ctx.Params = gin.Params{{Key: "inviter_id", Value: "999"}}
	ListAffiliateTopUpDetails(ctx)

	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "INVITER_NOT_FOUND") {
		t.Fatalf("missing inviter response = %d %s, want 404 INVITER_NOT_FOUND", recorder.Code, recorder.Body.String())
	}
}
