package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

func TestParseTopUpFiltersIncludesUserAndInviterIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		"GET",
		"/api/top-ups/export?status=success&user_id=12&inviter_id=34&start_date=2026-01-01&end_date=2026-01-31",
		nil,
	)

	params, err := parseTopUpFilters(ctx)
	if err != nil {
		t.Fatalf("parseTopUpFilters returned error: %v", err)
	}
	if params.UserID == nil || *params.UserID != 12 {
		t.Fatalf("user_id = %v, want 12", params.UserID)
	}
	if params.InviterID == nil || *params.InviterID != 34 {
		t.Fatalf("inviter_id = %v, want 34", params.InviterID)
	}
	if params.Status != "success" || params.StartDate != "2026-01-01" || params.EndDate != "2026-01-31" {
		t.Fatalf("unexpected filters: %+v", params)
	}
}

func TestParseTopUpFiltersRejectsInvalidIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []string{
		"/api/top-ups/export?user_id=not-a-number",
		"/api/top-ups/export?inviter_id=also-invalid",
		"/api/top-ups/export?user_id=0",
		"/api/top-ups/export?inviter_id=-1",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", target, nil)

			if _, err := parseTopUpFilters(ctx); err == nil {
				t.Fatalf("invalid ID in %q was accepted", target)
			}
		})
	}
}

func TestOptionalTopUpFilterIDLogsValuesNotPointers(t *testing.T) {
	id := int64(34)
	if got := optionalTopUpFilterID(&id); got != int64(34) {
		t.Fatalf("optionalTopUpFilterID = %#v, want 34", got)
	}
	if got := optionalTopUpFilterID(nil); got != nil {
		t.Fatalf("optionalTopUpFilterID(nil) = %#v, want nil", got)
	}
}

func TestTopUpHandlersReturnBadRequestForMalformedIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		target  string
		handler gin.HandlerFunc
	}{
		{name: "list user id", target: "/api/top-ups?user_id=bad", handler: ListTopUps},
		{name: "list inviter id", target: "/api/top-ups?inviter_id=bad", handler: ListTopUps},
		{name: "export user id", target: "/api/top-ups/export?user_id=bad", handler: ExportTopUps},
		{name: "export inviter id", target: "/api/top-ups/export?inviter_id=bad", handler: ExportTopUps},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", tt.target, nil)

			tt.handler(ctx)
			if recorder.Code != 400 {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestTopUpQueryHandlersHideDatabaseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite test database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		ATTACH DATABASE ':memory:' AS information_schema;
		CREATE TABLE information_schema.columns (table_name TEXT, column_name TEXT);
		INSERT INTO information_schema.columns (table_name, column_name)
		VALUES ('top_ups', 'payment_provider');
	`); err != nil {
		_ = db.Close()
		t.Fatalf("install information_schema fixture: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: true})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	tests := []struct {
		name        string
		target      string
		handler     gin.HandlerFunc
		wantStatus  int
		wantMessage string
	}{
		{name: "list", target: "/api/top-ups", handler: ListTopUps, wantStatus: http.StatusInternalServerError, wantMessage: "Top-up data is temporarily unavailable"},
		{name: "statistics", target: "/api/top-ups/statistics", handler: GetTopUpStatistics, wantStatus: http.StatusInternalServerError, wantMessage: "Top-up data is temporarily unavailable"},
		{name: "payment methods", target: "/api/top-ups/payment-methods", handler: GetPaymentMethods, wantStatus: http.StatusInternalServerError, wantMessage: "Top-up data is temporarily unavailable"},
		{name: "payment providers", target: "/api/top-ups/payment-providers", handler: GetPaymentProviders, wantStatus: http.StatusInternalServerError, wantMessage: "Top-up data is temporarily unavailable"},
		{name: "export", target: "/api/top-ups/export", handler: ExportTopUps, wantStatus: http.StatusServiceUnavailable, wantMessage: "Export was not started because the audit store is unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, tt.target, nil)
			ctx.Request.RemoteAddr = "192.0.2.30:12345"

			tt.handler(ctx)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, tt.wantMessage) {
				t.Fatalf("generic top-up error missing: %s", body)
			}
			for _, sensitive := range []string{"no such table", "SQL logic error", "top_ups", "topups"} {
				if strings.Contains(body, sensitive) {
					t.Fatalf("response leaked database detail %q: %s", sensitive, body)
				}
			}
		})
	}
}

func TestTopUpExportRequiresAdminAndWritesIntentOutcomeAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, _ := installTopUpExportFixture(t)

	viewer := topUpExportRouter(store, auth.RoleViewer)
	viewerRecorder := httptest.NewRecorder()
	viewerRequest := httptest.NewRequest(http.MethodGet, "/api/top-ups/export", nil)
	viewer.ServeHTTP(viewerRecorder, viewerRequest)
	if viewerRecorder.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403; body=%s", viewerRecorder.Code, viewerRecorder.Body.String())
	}

	admin := topUpExportRouter(store, auth.RoleAdmin)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/top-ups/export?status=success", nil)
	request.RemoteAddr = "192.0.2.45:12345"
	admin.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if disposition := recorder.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") {
		t.Fatalf("missing CSV attachment header: %q", disposition)
	}

	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list export audits: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("audit count = %d, want 2: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].Action != "top_ups.export.outcome" || page.Items[0].Status != toolstore.OperationSucceeded {
		t.Fatalf("unexpected outcome audit: %+v", page.Items[0])
	}
	if page.Items[1].Action != "top_ups.export.intent" || page.Items[1].TargetType != "financial_export" {
		t.Fatalf("unexpected intent audit: %+v", page.Items[1])
	}
	if page.Items[0].RequestID == "" || page.Items[0].RequestID != page.Items[1].RequestID {
		t.Fatalf("audit request IDs are not correlated: %+v", page.Items)
	}
	runs, err := store.ListReconciliationRuns(context.Background(), toolstore.ReconciliationRunFilter{
		Kind: "top_up_export_audit_outcome", Status: toolstore.ReconciliationSucceeded, Limit: 10,
	})
	if err != nil || len(runs.Items) != 1 {
		t.Fatalf("resolved export audit recovery = %+v, error=%v", runs.Items, err)
	}
	var outcome struct {
		RowCount         int64 `json:"row_count"`
		ExpectedRowCount int64 `json:"expected_row_count"`
		Truncated        bool  `json:"truncated"`
	}
	if err := json.Unmarshal(page.Items[0].AfterJSON, &outcome); err != nil {
		t.Fatalf("decode outcome audit: %v", err)
	}
	if outcome.RowCount != 1 || outcome.ExpectedRowCount != 1 || outcome.Truncated {
		t.Fatalf("outcome audit = %+v, want one complete written row", outcome)
	}
	response := recorder.Result()
	if response.Trailer.Get("X-Export-Row-Count") != "1" || response.Trailer.Get("X-Export-Truncated") != "false" {
		t.Fatalf("export trailers = %#v", response.Trailer)
	}
}

func TestTopUpExportPersistsReconcileableRecordWhenOutcomeAuditFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, storePath := installTopUpExportFixture(t)
	rawStore, err := sqlx.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open raw tool store: %v", err)
	}
	t.Cleanup(func() { _ = rawStore.Close() })
	if _, err := rawStore.Exec(`CREATE TRIGGER fail_top_up_export_outcome_audit
		BEFORE INSERT ON operation_audit
		WHEN NEW.action = 'top_ups.export.outcome'
		BEGIN SELECT RAISE(ABORT, 'injected outcome audit failure'); END`); err != nil {
		t.Fatalf("install outcome audit failure: %v", err)
	}

	admin := topUpExportRouter(store, auth.RoleAdmin)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/top-ups/export?status=success", nil)
	request.RemoteAddr = "192.0.2.46:12345"
	admin.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	audits, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list export audits: %v", err)
	}
	if len(audits.Items) != 1 || audits.Items[0].Action != "top_ups.export.intent" {
		t.Fatalf("failed outcome append did not leave exactly the durable intent: %+v", audits.Items)
	}
	runs, err := store.ListReconciliationRuns(context.Background(), toolstore.ReconciliationRunFilter{
		Kind: "top_up_export_audit_outcome", Status: toolstore.ReconciliationRunning, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list audit recovery records: %v", err)
	}
	if len(runs.Items) != 1 || runs.Items[0].ErrorCode != "OPERATION_AUDIT_APPEND_PENDING" {
		t.Fatalf("outcome audit recovery record = %+v, want one pending reconciliation", runs.Items)
	}
	var summary struct {
		RequestID        string                    `json:"request_id"`
		Status           toolstore.OperationStatus `json:"status"`
		ErrorCode        string                    `json:"error_code"`
		IdempotencyKey   string                    `json:"idempotency_key"`
		Phase            string                    `json:"phase"`
		OutcomePersisted bool                      `json:"outcome_persisted"`
		Filters          map[string]any            `json:"filters"`
		Result           struct {
			RowCount         int64 `json:"row_count"`
			ExpectedRowCount int64 `json:"expected_row_count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(runs.Items[0].SummaryJSON, &summary); err != nil {
		t.Fatalf("decode audit recovery summary: %v", err)
	}
	if summary.RequestID == "" || summary.Status != toolstore.OperationSucceeded || summary.ErrorCode != "" ||
		summary.Phase != "pending" || summary.OutcomePersisted ||
		summary.IdempotencyKey != "export:"+summary.RequestID+":outcome" || summary.Filters["status"] != "success" ||
		summary.Result.RowCount != 1 || summary.Result.ExpectedRowCount != 1 {
		t.Fatalf("audit recovery summary is incomplete: %+v", summary)
	}
}

func TestTopUpExportKeepsPrebuiltPendingAuditWhenOutcomeAndRecoveryUpdatesFail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, storePath := installTopUpExportFixture(t)
	rawStore, err := sqlx.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open raw tool store: %v", err)
	}
	t.Cleanup(func() { _ = rawStore.Close() })
	for _, statement := range []string{
		`CREATE TRIGGER fail_top_up_export_outcome_audit_hard
			BEFORE INSERT ON operation_audit
			WHEN NEW.action = 'top_ups.export.outcome'
			BEGIN SELECT RAISE(ABORT, 'injected outcome audit failure'); END`,
		`CREATE TRIGGER fail_top_up_export_recovery_update
			BEFORE UPDATE ON reconciliation_runs
			BEGIN SELECT RAISE(ABORT, 'injected recovery update failure'); END`,
	} {
		if _, err := rawStore.Exec(statement); err != nil {
			t.Fatalf("install tool-store failure trigger: %v", err)
		}
	}

	admin := topUpExportRouter(store, auth.RoleAdmin)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/top-ups/export?status=success", nil)
	request.RemoteAddr = "192.0.2.47:12345"
	admin.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	runs, err := store.ListReconciliationRuns(context.Background(), toolstore.ReconciliationRunFilter{
		Kind: "top_up_export_audit_outcome", Status: toolstore.ReconciliationRunning, Limit: 10,
	})
	if err != nil || len(runs.Items) != 1 {
		t.Fatalf("prebuilt pending audit recovery = %+v, error=%v", runs.Items, err)
	}
	var summary struct {
		RequestID        string `json:"request_id"`
		Phase            string `json:"phase"`
		OutcomePersisted bool   `json:"outcome_persisted"`
		Result           struct {
			ExpectedRowCount int64 `json:"expected_row_count"`
			SnapshotMaxID    int64 `json:"snapshot_max_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(runs.Items[0].SummaryJSON, &summary); err != nil {
		t.Fatalf("decode prebuilt recovery summary: %v", err)
	}
	if summary.RequestID == "" || summary.Phase != "pending" || summary.OutcomePersisted ||
		summary.Result.ExpectedRowCount != 1 || summary.Result.SnapshotMaxID != 10 {
		t.Fatalf("prebuilt recovery did not retain request/filter/snapshot basis: %+v", summary)
	}
}

func installTopUpExportFixture(t *testing.T) (*toolstore.Store, string) {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open top-up export database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`
		ATTACH DATABASE ':memory:' AS information_schema;
		CREATE TABLE information_schema.columns (table_name TEXT, column_name TEXT);
		INSERT INTO information_schema.columns (table_name, column_name)
		VALUES ('top_ups', 'payment_provider');
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, inviter_id INTEGER);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER, money REAL,
			trade_no TEXT, payment_method TEXT, payment_provider TEXT,
			create_time INTEGER, complete_time INTEGER, status TEXT
		);
		INSERT INTO users(id, username, inviter_id) VALUES (1, 'alice', 2);
		INSERT INTO top_ups(id, user_id, amount, money, trade_no, payment_method,
			payment_provider, create_time, complete_time, status)
		VALUES (10, 1, 1000, 10.0, 'trade-10', 'card', 'provider', 1700000000, 1700000030, 'success');
	`)
	database.SetForTesting(&database.Manager{DB: db, IsPG: true})

	storePath := filepath.Join(t.TempDir(), "control-plane.db")
	store, err := toolstore.Init(storePath)
	if err != nil {
		database.SetForTesting(nil)
		_ = db.Close()
		t.Fatalf("initialize tool store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return store, storePath
}

func topUpExportRouter(store *toolstore.Store, role auth.Role) *gin.Engine {
	router := gin.New()
	router.Use(middleware.RequestIDMiddleware())
	api := router.Group("/api")
	api.Use(func(c *gin.Context) {
		c.Set("auth_method", "jwt")
		c.Set("user_sub", "admin")
		auth.SetRole(c, role)
		c.Next()
	})
	RegisterTopUpRoutes(api, store)
	return router
}
