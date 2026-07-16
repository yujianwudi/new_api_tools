package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

func newControlPlaneSearchTestService(
	t *testing.T,
	store *toolstore.Store,
	logStatus database.LogSourceStatus,
) (*sqlx.DB, *sqlx.DB, *ControlPlaneSearchService) {
	t.Helper()
	mainDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open main test database: %v", err)
	}
	logDB, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		_ = mainDB.Close()
		t.Fatalf("open log test database: %v", err)
	}
	mainDB.SetMaxOpenConns(1)
	logDB.SetMaxOpenConns(1)
	mainManager := &database.Manager{DB: mainDB, IsPG: false}
	logManager := &database.Manager{DB: logDB, IsPG: false}
	database.SetForTesting(mainManager)
	database.SetLogForTesting(logManager, logStatus)
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = logDB.Close()
		_ = mainDB.Close()
	})
	service := NewControlPlaneSearchService(store)
	return mainDB, logDB, service
}

func createControlPlaneSearchMainTables(t *testing.T, db *sqlx.DB) {
	t.Helper()
	db.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`)
	db.MustExec(`CREATE TABLE top_ups (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		trade_no TEXT,
		amount INTEGER,
		status TEXT,
		create_time INTEGER
	)`)
}

func createControlPlaneSearchLogsTable(t *testing.T, db *sqlx.DB, requestID bool) {
	t.Helper()
	requestColumn := ""
	if requestID {
		requestColumn = ", request_id TEXT"
	}
	db.MustExec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		username TEXT,
		model_name TEXT,
		created_at INTEGER,
		type INTEGER,
		quota INTEGER` + requestColumn + `
	)`)
}

func TestControlPlaneSearchQueriesAllSourcesWithExplicitMetadata(t *testing.T) {
	mainDB, logDB, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	createControlPlaneSearchMainTables(t, mainDB)
	createControlPlaneSearchLogsTable(t, logDB, true)
	now := time.Now().Unix()
	mainDB.MustExec(`INSERT INTO users(id, username) VALUES (1, 'alice'), (2, 'bob')`)
	mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time)
		VALUES (11, 1, 'trade-alice-001', 100, 'success', ?)`, now-10)
	logDB.MustExec(`INSERT INTO logs(id, user_id, username, model_name, created_at, type, quota, request_id)
		VALUES (21, 1, 'alice', 'gpt-search', ?, 2, 9, 'req-alice-001')`, now-5)

	report, err := service.Search(context.Background(), "alice", 10)
	if err != nil {
		t.Fatalf("search control plane: %v", err)
	}
	if report.Query != "alice" || report.Limit != 10 || report.GeneratedAt <= 0 {
		t.Fatalf("missing report metadata: %#v", report)
	}
	if len(report.Sources) != 3 || len(report.Results) != 3 {
		t.Fatalf("search source/result counts = %d/%d: %#v", len(report.Sources), len(report.Results), report)
	}
	for _, source := range report.Sources {
		if !source.Available || source.Source == "" || source.Grain == "" || source.Freshness == "" || source.DataSource == "" {
			t.Fatalf("source metadata incomplete: %#v", source)
		}
	}
	if report.Sources[2].Capability != "request_id" || report.Sources[2].Mode != database.LogSourceModeDedicated {
		t.Fatalf("log request_id capability/source mode missing: %#v", report.Sources[2])
	}
	for _, item := range report.Results {
		if item.Source == "" || item.Grain == "" || item.DataSource == "" || item.Freshness.Basis == "" || item.Freshness.Unit != "unix_ms" {
			t.Fatalf("result metadata incomplete: %#v", item)
		}
	}

	requestReport, err := service.Search(context.Background(), "req-alice-001", 10)
	if err != nil {
		t.Fatalf("search by request ID: %v", err)
	}
	if len(requestReport.Results) != 1 || requestReport.Results[0].Source != "newapi.logs" ||
		requestReport.Results[0].Attributes["request_id"] != "req-alice-001" {
		t.Fatalf("request_id was not prioritized: %#v", requestReport.Results)
	}
}

func TestControlPlaneSearchFallsBackWhenLogsLackRequestID(t *testing.T) {
	mainDB, logDB, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{
		Mode: database.LogSourceModeFallback, Configured: true, Healthy: false, UsingFallback: true,
	})
	createControlPlaneSearchMainTables(t, mainDB)
	createControlPlaneSearchLogsTable(t, logDB, false)
	logDB.MustExec(`INSERT INTO logs(id, user_id, username, model_name, created_at, type, quota)
		VALUES (31, 7, 'legacy-user', 'legacy-model', ?, 2, 1)`, time.Now().Unix())

	report, err := service.Search(context.Background(), "legacy-model", 5)
	if err != nil {
		t.Fatalf("legacy log search: %v", err)
	}
	logSource := report.Sources[2]
	if !logSource.Available || logSource.Capability != "legacy_fields" || !logSource.Fallback {
		t.Fatalf("legacy/fallback log source was not explicit: %#v", logSource)
	}
	if len(report.Results) != 1 || report.Results[0].Attributes["request_id"] != "" {
		t.Fatalf("legacy log result was fabricated: %#v", report.Results)
	}
}

func TestControlPlaneSearchAppliesGlobalLimitRecountsSourcesAndEscapesLikeWildcards(t *testing.T) {
	mainDB, logDB, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{Mode: database.LogSourceModeMain, Healthy: true})
	createControlPlaneSearchMainTables(t, mainDB)
	createControlPlaneSearchLogsTable(t, logDB, true)
	now := time.Now().Unix()
	for index := 1; index <= 5; index++ {
		mainDB.MustExec(`INSERT INTO users(id, username) VALUES (?, ?)`, index, "match-user-"+strconv.Itoa(index))
		mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time) VALUES (?, ?, ?, 1, 'success', ?)`,
			100+index, index, "match-trade-"+strconv.Itoa(index), now-int64(index))
		logDB.MustExec(`INSERT INTO logs(id, user_id, username, model_name, created_at, type, quota, request_id)
			VALUES (?, ?, ?, 'match-model', ?, 2, 1, ?)`, 200+index, index, "match-user", now-int64(index), "match-request-"+strconv.Itoa(index))
	}
	report, err := service.Search(context.Background(), "match", 2)
	if err != nil {
		t.Fatalf("limited search: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("global result count = %d, want 2", len(report.Results))
	}
	counted := 0
	for _, source := range report.Sources {
		counted += source.ResultCount
		actual := 0
		for _, item := range report.Results {
			if item.Source == source.Source {
				actual++
			}
		}
		if source.ResultCount != actual {
			t.Fatalf("source %s result_count=%d, returned=%d: %#v", source.Source, source.ResultCount, actual, report.Results)
		}
	}
	if counted != len(report.Results) {
		t.Fatalf("source counts sum=%d, returned=%d", counted, len(report.Results))
	}

	wildcardReport, err := service.Search(context.Background(), "%_", 10)
	if err != nil {
		t.Fatalf("literal wildcard search: %v", err)
	}
	if len(wildcardReport.Results) != 0 {
		t.Fatalf("LIKE wildcards were not escaped: %#v", wildcardReport.Results)
	}
}

func TestControlPlaneTimelineDoesNotBackdateMutableRiskAndSupportState(t *testing.T) {
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, _, service := newControlPlaneSearchTestService(t, store, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	risk, err := store.CreateRiskCase(context.Background(), toolstore.RiskCaseInput{
		CaseKey: "history-risk-42", Title: "Original risk title", SubjectType: "user", SubjectID: "42",
		Severity: toolstore.RiskSeverityLow, Status: toolstore.RiskCaseOpen, Assignee: "first-owner",
		Summary: "original risk summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRiskCase(context.Background(), toolstore.RiskCaseUpdate{
		ID: risk.ID, Title: "MUTATED RISK TITLE", Severity: toolstore.RiskSeverityCritical,
		Status: toolstore.RiskCaseInvestigating, Assignee: "later-owner", Summary: "MUTATED RISK SUMMARY",
	}); err != nil {
		t.Fatal(err)
	}
	note, err := store.CreateSupportNote(context.Background(), toolstore.SupportNoteInput{
		SubjectType: "user", SubjectID: "42", Author: "support-agent", Body: "original support body",
		Visibility: toolstore.NoteInternal, IdempotencyKey: "history-support-note-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSupportNote(context.Background(), toolstore.SupportNoteUpdate{
		ID: note.ID, Body: "MUTATED SUPPORT BODY", Visibility: toolstore.NoteCustomer,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSupportNote(context.Background(), note.ID); err != nil {
		t.Fatal(err)
	}

	report, err := service.UserTimeline(context.Background(), 42, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	var riskEvent, noteEvent *ControlPlaneTimelineEvent
	for index := range report.Events {
		switch report.Events[index].Source {
		case "tool_store.risk_cases":
			riskEvent = &report.Events[index]
		case "tool_store.support_notes":
			noteEvent = &report.Events[index]
		}
	}
	if riskEvent == nil || noteEvent == nil {
		t.Fatalf("missing mutable-record timeline events: %#v", report.Events)
	}
	if riskEvent.EventType != "risk_case_created" || riskEvent.Freshness.Basis != "created_at" {
		t.Fatalf("risk creation semantics = %#v", riskEvent)
	}
	if noteEvent.EventType != "support_note_created" || noteEvent.Freshness.Basis != "created_at" {
		t.Fatalf("support creation semantics = %#v", noteEvent)
	}
	encoded, err := json.Marshal([]*ControlPlaneTimelineEvent{riskEvent, noteEvent})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutableValue := range []string{
		"MUTATED RISK TITLE", "MUTATED RISK SUMMARY", "later-owner", "critical", "investigating",
		"MUTATED SUPPORT BODY", "customer", "support_note_deleted",
	} {
		if strings.Contains(string(encoded), mutableValue) {
			t.Fatalf("current mutable state %q was backdated to creation: %s", mutableValue, encoded)
		}
	}
}

func TestControlPlaneSearchMarksMissingTableUnavailable(t *testing.T) {
	mainDB, logDB, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{Mode: database.LogSourceModeDedicated, Healthy: true})
	mainDB.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`)
	createControlPlaneSearchLogsTable(t, logDB, true)
	mainDB.MustExec(`INSERT INTO users(id, username) VALUES (1, 'alice')`)

	report, err := service.Search(context.Background(), "alice", 10)
	if err != nil {
		t.Fatalf("partial search: %v", err)
	}
	if !report.Sources[0].Available || report.Sources[1].Available || report.Sources[1].Reason != "schema_unavailable" || !report.Sources[2].Available {
		t.Fatalf("missing table was not isolated: %#v", report.Sources)
	}
	for _, result := range report.Results {
		if result.Source == "newapi.top_ups" {
			t.Fatalf("missing top_ups source fabricated data: %#v", result)
		}
	}
}

func TestControlPlaneSearchFailsOnlyWhenMainDatabaseUnavailable(t *testing.T) {
	mainDB, _, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{Mode: database.LogSourceModeDedicated, Healthy: true})
	if err := mainDB.Close(); err != nil {
		t.Fatalf("close main database: %v", err)
	}
	if _, err := service.Search(context.Background(), "alice", 10); !errors.Is(err, ErrControlPlaneMainDatabaseUnavailable) {
		t.Fatalf("closed main database error = %v", err)
	}
}

func TestControlPlaneUserTimelineMergesSourcesWithStableCursor(t *testing.T) {
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatalf("open timeline tool store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mainDB, logDB, service := newControlPlaneSearchTestService(t, store, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	createControlPlaneSearchMainTables(t, mainDB)
	createControlPlaneSearchLogsTable(t, logDB, true)
	base := time.Now().UTC().Truncate(time.Second)
	mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time) VALUES
		(101, 42, 'timeline-topup-new', 100, 'success', ?),
		(102, 42, 'timeline-topup-old', 50, 'pending', ?)`, base.Add(50*time.Second).Unix(), base.Add(-20*time.Second).Unix())
	logDB.MustExec(`INSERT INTO logs(id, user_id, username, model_name, created_at, type, quota, request_id) VALUES
		(201, 42, 'alice', 'timeline-new', ?, 2, 10, 'timeline-req-new'),
		(202, 42, 'alice', 'timeline-old', ?, 5, 0, 'timeline-req-old')`, base.Add(60*time.Second).Unix(), base.Add(-10*time.Second).Unix())
	if _, err := store.AppendOperationAudit(context.Background(), toolstore.OperationAuditInput{
		RequestID: "timeline-audit-request", Actor: "admin", SourceIP: "127.0.0.1", AuthMethod: "jwt",
		Action: "user.review", TargetType: "user", TargetID: "42", Reason: "timeline review",
		Status: toolstore.OperationSucceeded,
	}); err != nil {
		t.Fatalf("append timeline audit: %v", err)
	}
	if _, err := store.CreateRiskCase(context.Background(), toolstore.RiskCaseInput{
		CaseKey: "timeline-risk-42", Title: "Timeline risk", SubjectType: "user", SubjectID: "42",
		Severity: toolstore.RiskSeverityHigh, Status: toolstore.RiskCaseOpen, Summary: "timeline evidence",
	}); err != nil {
		t.Fatalf("create timeline risk case: %v", err)
	}
	if _, err := store.CreateSupportNote(context.Background(), toolstore.SupportNoteInput{
		SubjectType: "user", SubjectID: "42", Author: "support", Body: "Timeline support note",
		Visibility: toolstore.NoteInternal,
	}); err != nil {
		t.Fatalf("create timeline support note: %v", err)
	}

	seen := map[string]bool{}
	allEvents := make([]ControlPlaneTimelineEvent, 0, 7)
	before := ""
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := service.UserTimeline(context.Background(), 42, before, 2)
		if err != nil {
			t.Fatalf("timeline page %d: %v", pageNumber, err)
		}
		if len(page.Sources) != 5 {
			t.Fatalf("timeline source count = %d, want 5", len(page.Sources))
		}
		for _, event := range page.Events {
			if seen[event.EventID] {
				t.Fatalf("timeline cursor duplicated %s", event.EventID)
			}
			seen[event.EventID] = true
			if event.Source == "" || event.Grain == "" || event.DataSource == "" || event.Freshness.Basis == "" {
				t.Fatalf("timeline event metadata incomplete: %#v", event)
			}
			allEvents = append(allEvents, event)
		}
		if !page.HasMore {
			if page.NextCursor != nil {
				t.Fatalf("terminal timeline page returned cursor: %#v", page)
			}
			break
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			t.Fatalf("timeline page %d omitted cursor", pageNumber)
		}
		if pageNumber == 0 {
			repeated, err := service.UserTimeline(context.Background(), 42, *page.NextCursor, 2)
			if err != nil {
				t.Fatalf("repeat stable cursor: %v", err)
			}
			again, err := service.UserTimeline(context.Background(), 42, *page.NextCursor, 2)
			if err != nil {
				t.Fatalf("repeat stable cursor again: %v", err)
			}
			if !reflect.DeepEqual(timelineEventIDs(repeated.Events), timelineEventIDs(again.Events)) {
				t.Fatalf("same cursor returned different events: %v vs %v", timelineEventIDs(repeated.Events), timelineEventIDs(again.Events))
			}
		}
		before = *page.NextCursor
	}
	if len(allEvents) != 7 {
		t.Fatalf("timeline event count = %d, want 7: %v", len(allEvents), timelineEventIDs(allEvents))
	}
	for index := 1; index < len(allEvents); index++ {
		if allEvents[index].Freshness.ObservedAt > allEvents[index-1].Freshness.ObservedAt {
			t.Fatalf("timeline order increased at %d: %d > %d", index, allEvents[index].Freshness.ObservedAt, allEvents[index-1].Freshness.ObservedAt)
		}
	}
}

func TestControlPlaneTimelineMarksEveryMissingSourceUnavailable(t *testing.T) {
	_, _, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{
		Mode: database.LogSourceModeFallback, Configured: true, Healthy: false, UsingFallback: true,
	})
	report, err := service.UserTimeline(context.Background(), 42, "", 10)
	if err != nil {
		t.Fatalf("partial timeline: %v", err)
	}
	if len(report.Events) != 0 || len(report.Sources) != 5 {
		t.Fatalf("missing sources fabricated timeline data: %#v", report)
	}
	for _, source := range report.Sources {
		if source.Available || source.Reason == "" {
			t.Fatalf("missing source not marked unavailable: %#v", source)
		}
	}
}

func TestControlPlaneTimelineCursorDisablesOnlyPermanentlyMissingSchema(t *testing.T) {
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mainDB, _, service := newControlPlaneSearchTestService(t, store, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: true,
	})
	createControlPlaneSearchMainTables(t, mainDB)
	now := time.Now().Unix()
	mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time) VALUES
		(301, 42, 'permanent-new', 100, 'success', ?),
		(302, 42, 'permanent-old', 50, 'pending', ?)`, now, now-1)

	report, err := service.UserTimeline(context.Background(), 42, "", 1)
	if err != nil {
		t.Fatalf("timeline with missing logs schema: %v", err)
	}
	if report.NextCursor == nil {
		t.Fatalf("timeline did not return a cursor: %#v", report)
	}
	cursor, err := decodeControlPlaneTimelineCursor(*report.NextCursor)
	if err != nil {
		t.Fatalf("decode timeline cursor: %v", err)
	}
	if cursor.Disabled != timelineSourceLogs {
		t.Fatalf("disabled sources = %b, want only logs (%b)", cursor.Disabled, timelineSourceLogs)
	}
}

func TestControlPlaneTimelineCursorDoesNotPersistTransientSourceErrors(t *testing.T) {
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mainDB, logDB, service := newControlPlaneSearchTestService(t, store, database.LogSourceStatus{
		Mode: database.LogSourceModeDedicated, Configured: true, Healthy: false,
	})
	createControlPlaneSearchMainTables(t, mainDB)
	now := time.Now().Unix()
	mainDB.MustExec(`INSERT INTO top_ups(id, user_id, trade_no, amount, status, create_time) VALUES
		(401, 42, 'transient-new', 100, 'success', ?),
		(402, 42, 'transient-old', 50, 'pending', ?)`, now, now-1)
	if err := logDB.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := service.UserTimeline(context.Background(), 42, "", 1)
	if err != nil {
		t.Fatalf("timeline with transient log error: %v", err)
	}
	if report.NextCursor == nil {
		t.Fatalf("timeline did not return a cursor: %#v", report)
	}
	cursor, err := decodeControlPlaneTimelineCursor(*report.NextCursor)
	if err != nil {
		t.Fatalf("decode timeline cursor: %v", err)
	}
	if cursor.Disabled != 0 {
		t.Fatalf("transient source error was persisted in cursor: disabled=%b", cursor.Disabled)
	}
	if report.Sources[0].Reason != "query_unavailable" {
		t.Fatalf("transient source reason = %q, want query_unavailable", report.Sources[0].Reason)
	}
}

func TestControlPlaneTimelineRejectsInvalidCursorAndHonorsCancellation(t *testing.T) {
	mainDB, logDB, service := newControlPlaneSearchTestService(t, nil, database.LogSourceStatus{Mode: database.LogSourceModeDedicated, Healthy: true})
	createControlPlaneSearchMainTables(t, mainDB)
	createControlPlaneSearchLogsTable(t, logDB, true)
	if _, err := service.UserTimeline(context.Background(), 1, "not-a-cursor", 10); !errors.Is(err, ErrControlPlaneInvalidCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.UserTimeline(ctx, 1, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled timeline error = %v", err)
	}
}

func timelineEventIDs(events []ControlPlaneTimelineEvent) []string {
	ids := make([]string, len(events))
	for index, event := range events {
		ids[index] = event.EventID
	}
	return ids
}
