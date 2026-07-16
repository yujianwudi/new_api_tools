package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	ControlPlaneSearchDefaultLimit   = 20
	ControlPlaneSearchMaxLimit       = 50
	ControlPlaneTimelineDefaultLimit = 50
	ControlPlaneTimelineMaxLimit     = 100

	controlPlaneTimelineCursorVersion = 2

	timelineSourceLogs uint32 = 1 << iota
	timelineSourceTopUps
	timelineSourceAudits
	timelineSourceRiskCases
	timelineSourceSupportNotes
)

var (
	ErrControlPlaneMainDatabaseUnavailable = errors.New("control-plane main database unavailable")
	ErrControlPlaneInvalidCursor           = errors.New("control-plane timeline cursor is invalid")
	ErrControlPlaneInvalidSearch           = errors.New("control-plane search parameters are invalid")
)

type ControlPlaneFreshness struct {
	Basis      string `json:"basis"`
	ObservedAt int64  `json:"observed_at"`
	Unit       string `json:"unit"`
}

type ControlPlaneSourceStatus struct {
	Source      string `json:"source"`
	Grain       string `json:"grain"`
	Freshness   string `json:"freshness"`
	DataSource  string `json:"data_source"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	ResultCount int    `json:"result_count"`
	Mode        string `json:"mode,omitempty"`
	Fallback    bool   `json:"fallback,omitempty"`
	Capability  string `json:"capability,omitempty"`
}

type ControlPlaneSearchResult struct {
	Source     string                 `json:"source"`
	Grain      string                 `json:"grain"`
	Freshness  ControlPlaneFreshness  `json:"freshness"`
	DataSource string                 `json:"data_source"`
	ID         string                 `json:"id"`
	UserID     int64                  `json:"user_id,omitempty"`
	Label      string                 `json:"label"`
	Attributes map[string]interface{} `json:"attributes"`
}

type ControlPlaneSearchReport struct {
	Query       string                     `json:"query"`
	Limit       int                        `json:"limit"`
	GeneratedAt int64                      `json:"generated_at"`
	Sources     []ControlPlaneSourceStatus `json:"sources"`
	Results     []ControlPlaneSearchResult `json:"results"`
}

type ControlPlaneTimelineEvent struct {
	Source     string                 `json:"source"`
	Grain      string                 `json:"grain"`
	Freshness  ControlPlaneFreshness  `json:"freshness"`
	DataSource string                 `json:"data_source"`
	EventType  string                 `json:"event_type"`
	EventID    string                 `json:"event_id"`
	UserID     int64                  `json:"user_id"`
	Summary    string                 `json:"summary"`
	Details    map[string]interface{} `json:"details"`

	sourceBit  uint32
	sourceRank int
	sourceID   int64
	sourceTime int64
}

type ControlPlaneTimelineReport struct {
	UserID      int64                       `json:"user_id"`
	Limit       int                         `json:"limit"`
	GeneratedAt int64                       `json:"generated_at"`
	Sources     []ControlPlaneSourceStatus  `json:"sources"`
	Events      []ControlPlaneTimelineEvent `json:"events"`
	NextCursor  *string                     `json:"next_cursor"`
	HasMore     bool                        `json:"has_more"`
}

type ControlPlaneSearchService struct {
	mainDB    *database.Manager
	logDB     *database.Manager
	store     *toolstore.Store
	now       func() time.Time
	logStatus func() database.LogSourceStatus
}

type controlPlaneSearchSourceResult struct {
	status  ControlPlaneSourceStatus
	results []ControlPlaneSearchResult
	err     error
}

type controlPlaneTimelineSourceResult struct {
	status  ControlPlaneSourceStatus
	events  []ControlPlaneTimelineEvent
	hasMore bool
	err     error
	bit     uint32
}

type controlPlaneTimelineCursor struct {
	Version   int    `json:"v"`
	Disabled  uint32 `json:"disabled,omitempty"`
	LogTime   int64  `json:"log_time,omitempty"`
	LogID     int64  `json:"log_id,omitempty"`
	TopUpTime int64  `json:"topup_time,omitempty"`
	TopUpID   int64  `json:"topup_id,omitempty"`
	AuditTime int64  `json:"audit_time,omitempty"`
	AuditID   int64  `json:"audit_id,omitempty"`
	RiskTime  int64  `json:"risk_time,omitempty"`
	RiskID    int64  `json:"risk_id,omitempty"`
	NoteTime  int64  `json:"note_time,omitempty"`
	NoteID    int64  `json:"note_id,omitempty"`
}

func NewControlPlaneSearchService(store *toolstore.Store) *ControlPlaneSearchService {
	return &ControlPlaneSearchService{
		mainDB:    database.Get(),
		logDB:     database.GetLog(),
		store:     store,
		now:       time.Now,
		logStatus: database.GetLogSourceStatus,
	}
}

func ValidateControlPlaneSearchQuery(query string) error {
	query = strings.TrimSpace(query)
	length := utf8.RuneCountInString(query)
	if length < 2 || length > 128 {
		return ErrControlPlaneInvalidSearch
	}
	for _, character := range query {
		if unicode.IsControl(character) {
			return ErrControlPlaneInvalidSearch
		}
	}
	return nil
}

func (s *ControlPlaneSearchService) Search(ctx context.Context, query string, limit int) (*ControlPlaneSearchReport, error) {
	if ctx == nil {
		return nil, errors.New("control-plane search context is required")
	}
	query = strings.TrimSpace(query)
	if err := ValidateControlPlaneSearchQuery(query); err != nil || limit < 1 || limit > ControlPlaneSearchMaxLimit {
		return nil, ErrControlPlaneInvalidSearch
	}
	if err := s.ensureMainDatabase(ctx); err != nil {
		return nil, err
	}

	generatedAt := s.clock().UnixMilli()
	results := make([]controlPlaneSearchSourceResult, 3)
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		results[0] = s.searchUsers(ctx, query, limit, generatedAt)
	}()
	go func() {
		defer wait.Done()
		results[1] = s.searchTopUps(ctx, query, limit)
	}()
	go func() {
		defer wait.Done()
		results[2] = s.searchLogs(ctx, query, limit)
	}()
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sources := make([]ControlPlaneSourceStatus, 0, len(results))
	items := make([]ControlPlaneSearchResult, 0, len(results)*limit)
	for _, result := range results {
		if isContextError(result.err) {
			return nil, result.err
		}
		if result.err != nil {
			result.status.Available = false
			result.status.Reason = controlPlaneUnavailableReason(result.err)
			result.status.ResultCount = 0
			result.results = []ControlPlaneSearchResult{}
		}
		sources = append(sources, result.status)
		items = append(items, result.results...)
	}
	if len(items) > limit {
		items = items[:limit]
	}
	returnedBySource := make(map[string]int, len(sources))
	for _, item := range items {
		returnedBySource[item.Source]++
	}
	for index := range sources {
		sources[index].ResultCount = returnedBySource[sources[index].Source]
	}

	return &ControlPlaneSearchReport{
		Query:       query,
		Limit:       limit,
		GeneratedAt: generatedAt,
		Sources:     sources,
		Results:     items,
	}, nil
}

func (s *ControlPlaneSearchService) UserTimeline(
	ctx context.Context,
	userID int64,
	before string,
	limit int,
) (*ControlPlaneTimelineReport, error) {
	if ctx == nil {
		return nil, errors.New("control-plane timeline context is required")
	}
	if userID <= 0 || limit < 1 || limit > ControlPlaneTimelineMaxLimit {
		return nil, ErrControlPlaneInvalidSearch
	}
	cursor, err := decodeControlPlaneTimelineCursor(before)
	if err != nil {
		return nil, err
	}
	if err := s.ensureMainDatabase(ctx); err != nil {
		return nil, err
	}

	fetchLimit := limit + 1
	results := make([]controlPlaneTimelineSourceResult, 5)
	var wait sync.WaitGroup
	wait.Add(5)
	go func() {
		defer wait.Done()
		results[0] = s.timelineLogs(ctx, userID, cursor, fetchLimit)
	}()
	go func() {
		defer wait.Done()
		results[1] = s.timelineTopUps(ctx, userID, cursor, fetchLimit)
	}()
	go func() {
		defer wait.Done()
		results[2] = s.timelineOperationAudits(ctx, userID, cursor, fetchLimit)
	}()
	go func() {
		defer wait.Done()
		results[3] = s.timelineRiskCases(ctx, userID, cursor, fetchLimit)
	}()
	go func() {
		defer wait.Done()
		results[4] = s.timelineSupportNotes(ctx, userID, cursor, fetchLimit)
	}()
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sources := make([]ControlPlaneSourceStatus, 0, len(results))
	events := make([]ControlPlaneTimelineEvent, 0, len(results)*fetchLimit)
	nextState := cursor
	hasSourceMore := false
	for _, result := range results {
		if isContextError(result.err) {
			return nil, result.err
		}
		if result.err != nil {
			result.status.Available = false
			result.status.Reason = controlPlaneUnavailableReason(result.err)
			result.status.ResultCount = 0
			result.events = []ControlPlaneTimelineEvent{}
			if result.status.Reason == "schema_unavailable" {
				nextState.Disabled |= result.bit
			}
		}
		sources = append(sources, result.status)
		events = append(events, result.events...)
		hasSourceMore = hasSourceMore || result.hasMore
	}

	sort.SliceStable(events, func(i, j int) bool {
		left := events[i]
		right := events[j]
		if left.Freshness.ObservedAt != right.Freshness.ObservedAt {
			return left.Freshness.ObservedAt > right.Freshness.ObservedAt
		}
		if left.sourceRank != right.sourceRank {
			return left.sourceRank < right.sourceRank
		}
		return left.sourceID > right.sourceID
	})
	hasMore := hasSourceMore || len(events) > limit
	if len(events) > limit {
		events = events[:limit]
	}
	for _, event := range events {
		switch event.sourceBit {
		case timelineSourceLogs:
			nextState.LogTime = event.sourceTime
			nextState.LogID = event.sourceID
		case timelineSourceTopUps:
			nextState.TopUpTime = event.sourceTime
			nextState.TopUpID = event.sourceID
		case timelineSourceAudits:
			nextState.AuditTime = event.sourceTime
			nextState.AuditID = event.sourceID
		case timelineSourceRiskCases:
			nextState.RiskTime = event.sourceTime
			nextState.RiskID = event.sourceID
		case timelineSourceSupportNotes:
			nextState.NoteTime = event.sourceTime
			nextState.NoteID = event.sourceID
		}
	}

	var nextCursor *string
	if hasMore {
		encoded, err := encodeControlPlaneTimelineCursor(nextState)
		if err != nil {
			return nil, err
		}
		nextCursor = &encoded
	}
	return &ControlPlaneTimelineReport{
		UserID:      userID,
		Limit:       limit,
		GeneratedAt: s.clock().UnixMilli(),
		Sources:     sources,
		Events:      events,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
	}, nil
}

func (s *ControlPlaneSearchService) ensureMainDatabase(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.mainDB == nil || s.mainDB.DB == nil {
		return ErrControlPlaneMainDatabaseUnavailable
	}
	if err := s.mainDB.DB.PingContext(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %v", ErrControlPlaneMainDatabaseUnavailable, err)
	}
	return nil
}

func (s *ControlPlaneSearchService) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *ControlPlaneSearchService) searchUsers(ctx context.Context, query string, limit int, observedAt int64) controlPlaneSearchSourceResult {
	status := controlPlaneSource("newapi.users", "user", "query_time", "newapi_main_database")
	like := controlPlaneLikePattern(query)
	where := "username LIKE ? ESCAPE '!'"
	args := []interface{}{like}
	if numericID, ok := controlPlanePositiveInt(query); ok {
		where += " OR id = ?"
		args = append(args, numericID)
	}
	statement := s.mainDB.RebindQuery(`SELECT id, COALESCE(username, '') AS username
		FROM users WHERE (` + where + `)
		ORDER BY CASE WHEN username = ? THEN 0 ELSE 1 END, id DESC LIMIT ?`)
	args = append(args, query, limit)
	rows, err := s.mainDB.QueryContext(ctx, statement, args...)
	if err != nil {
		return controlPlaneSearchSourceResult{status: status, err: err}
	}
	items := make([]ControlPlaneSearchResult, 0, len(rows))
	for _, row := range rows {
		id := controlPlaneInt64(row["id"])
		username := controlPlaneString(row["username"])
		items = append(items, ControlPlaneSearchResult{
			Source: "newapi.users", Grain: "user",
			Freshness:  ControlPlaneFreshness{Basis: "query_time", ObservedAt: observedAt, Unit: "unix_ms"},
			DataSource: "newapi_main_database", ID: strconv.FormatInt(id, 10), UserID: id,
			Label: username, Attributes: map[string]interface{}{"username": username},
		})
	}
	status.Available = true
	status.ResultCount = len(items)
	return controlPlaneSearchSourceResult{status: status, results: items}
}

func (s *ControlPlaneSearchService) searchTopUps(ctx context.Context, query string, limit int) controlPlaneSearchSourceResult {
	status := controlPlaneSource("newapi.top_ups", "top_up", "create_time", "newapi_main_database")
	like := controlPlaneLikePattern(query)
	where := "COALESCE(trade_no, '') LIKE ? ESCAPE '!'"
	args := []interface{}{like}
	if numericID, ok := controlPlanePositiveInt(query); ok {
		where += " OR id = ? OR user_id = ?"
		args = append(args, numericID, numericID)
	}
	statement := s.mainDB.RebindQuery(`SELECT id, user_id, COALESCE(trade_no, '') AS trade_no,
		COALESCE(status, '') AS status, COALESCE(create_time, 0) AS create_time
		FROM top_ups WHERE (` + where + `)
		ORDER BY CASE WHEN trade_no = ? THEN 0 ELSE 1 END, create_time DESC, id DESC LIMIT ?`)
	args = append(args, query, limit)
	rows, err := s.mainDB.QueryContext(ctx, statement, args...)
	if err != nil {
		return controlPlaneSearchSourceResult{status: status, err: err}
	}
	items := make([]ControlPlaneSearchResult, 0, len(rows))
	for _, row := range rows {
		id := controlPlaneInt64(row["id"])
		userID := controlPlaneInt64(row["user_id"])
		tradeNo := controlPlaneString(row["trade_no"])
		createdAt := controlPlaneInt64(row["create_time"])
		items = append(items, ControlPlaneSearchResult{
			Source: "newapi.top_ups", Grain: "top_up",
			Freshness:  ControlPlaneFreshness{Basis: "create_time", ObservedAt: unixSecondsToMillis(createdAt), Unit: "unix_ms"},
			DataSource: "newapi_main_database", ID: strconv.FormatInt(id, 10), UserID: userID,
			Label: tradeNo, Attributes: map[string]interface{}{
				"trade_no": tradeNo, "status": controlPlaneString(row["status"]),
			},
		})
	}
	status.Available = true
	status.ResultCount = len(items)
	return controlPlaneSearchSourceResult{status: status, results: items}
}

func (s *ControlPlaneSearchService) searchLogs(ctx context.Context, query string, limit int) controlPlaneSearchSourceResult {
	status := s.logSource("newapi.logs", "request_log", "created_at")
	items, capability, err := s.queryLogSearch(ctx, query, limit, true)
	if err != nil && !isContextError(err) {
		items, capability, err = s.queryLogSearch(ctx, query, limit, false)
	}
	status.Capability = capability
	if err != nil {
		return controlPlaneSearchSourceResult{status: status, err: err}
	}
	status.Available = true
	status.ResultCount = len(items)
	return controlPlaneSearchSourceResult{status: status, results: items}
}

func (s *ControlPlaneSearchService) queryLogSearch(
	ctx context.Context,
	query string,
	limit int,
	withRequestID bool,
) ([]ControlPlaneSearchResult, string, error) {
	like := controlPlaneLikePattern(query)
	args := make([]interface{}, 0, 8)
	requestIDSelect := "'' AS request_id"
	where := "username LIKE ? ESCAPE '!' OR model_name LIKE ? ESCAPE '!'"
	args = append(args, like, like)
	capability := "legacy_fields"
	orderPrefix := ""
	if withRequestID {
		requestIDSelect = "COALESCE(request_id, '') AS request_id"
		where = "request_id LIKE ? ESCAPE '!' OR " + where
		args = append([]interface{}{like}, args...)
		capability = "request_id"
		orderPrefix = "CASE WHEN request_id = ? THEN 0 ELSE 1 END, "
	}
	if numericID, ok := controlPlanePositiveInt(query); ok {
		where += " OR id = ? OR user_id = ?"
		args = append(args, numericID, numericID)
	}
	statement := `SELECT id, user_id, COALESCE(username, '') AS username,
		COALESCE(model_name, '') AS model_name, COALESCE(created_at, 0) AS created_at, ` + requestIDSelect + `
		FROM logs WHERE (` + where + `) ORDER BY ` + orderPrefix + `created_at DESC, id DESC LIMIT ?`
	if withRequestID {
		args = append(args, query)
	}
	args = append(args, limit)
	rows, err := s.logDB.QueryContext(ctx, s.logDB.RebindQuery(statement), args...)
	if err != nil {
		return nil, capability, err
	}
	items := make([]ControlPlaneSearchResult, 0, len(rows))
	for _, row := range rows {
		id := controlPlaneInt64(row["id"])
		userID := controlPlaneInt64(row["user_id"])
		requestID := controlPlaneString(row["request_id"])
		model := controlPlaneString(row["model_name"])
		label := requestID
		if label == "" {
			label = model
		}
		items = append(items, ControlPlaneSearchResult{
			Source: "newapi.logs", Grain: "request_log",
			Freshness:  ControlPlaneFreshness{Basis: "created_at", ObservedAt: unixSecondsToMillis(controlPlaneInt64(row["created_at"])), Unit: "unix_ms"},
			DataSource: "newapi_log_database", ID: strconv.FormatInt(id, 10), UserID: userID,
			Label: label, Attributes: map[string]interface{}{
				"request_id": requestID, "username": controlPlaneString(row["username"]), "model": model,
			},
		})
	}
	return items, capability, nil
}

func (s *ControlPlaneSearchService) timelineLogs(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
) controlPlaneTimelineSourceResult {
	status := s.logSource("newapi.logs", "request_log", "created_at")
	result := controlPlaneTimelineSourceResult{status: status, bit: timelineSourceLogs}
	if cursor.Disabled&timelineSourceLogs != 0 {
		result.status.Reason = "cursor_source_unavailable"
		return result
	}
	events, capability, err := s.queryTimelineLogs(ctx, userID, cursor, limit, true)
	if err != nil && !isContextError(err) {
		events, capability, err = s.queryTimelineLogs(ctx, userID, cursor, limit, false)
	}
	result.status.Capability = capability
	if err != nil {
		result.err = err
		return result
	}
	result.status.Available = true
	result.status.ResultCount = len(events)
	result.events = events
	result.hasMore = len(events) >= limit
	return result
}

func (s *ControlPlaneSearchService) queryTimelineLogs(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
	withRequestID bool,
) ([]ControlPlaneTimelineEvent, string, error) {
	requestIDSelect := "'' AS request_id"
	capability := "legacy_fields"
	if withRequestID {
		requestIDSelect = "COALESCE(request_id, '') AS request_id"
		capability = "request_id"
	}
	where := "user_id = ?"
	args := []interface{}{userID}
	if cursor.LogTime > 0 || cursor.LogID > 0 {
		where += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, cursor.LogTime, cursor.LogTime, cursor.LogID)
	}
	statement := `SELECT id, user_id, COALESCE(username, '') AS username,
		COALESCE(model_name, '') AS model_name, COALESCE(type, 0) AS type,
		COALESCE(quota, 0) AS quota, COALESCE(created_at, 0) AS created_at, ` + requestIDSelect + `
		FROM logs WHERE ` + where + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.logDB.QueryContext(ctx, s.logDB.RebindQuery(statement), args...)
	if err != nil {
		return nil, capability, err
	}
	events := make([]ControlPlaneTimelineEvent, 0, len(rows))
	for _, row := range rows {
		id := controlPlaneInt64(row["id"])
		createdAt := controlPlaneInt64(row["created_at"])
		logType := controlPlaneInt64(row["type"])
		eventType := "request_log"
		if logType == 2 {
			eventType = "request_succeeded"
		} else if logType == 5 {
			eventType = "request_failed"
		}
		model := controlPlaneString(row["model_name"])
		events = append(events, ControlPlaneTimelineEvent{
			Source: "newapi.logs", Grain: "request_log",
			Freshness:  ControlPlaneFreshness{Basis: "created_at", ObservedAt: unixSecondsToMillis(createdAt), Unit: "unix_ms"},
			DataSource: "newapi_log_database", EventType: eventType,
			EventID: "log:" + strconv.FormatInt(id, 10), UserID: userID,
			Summary: controlPlaneSummary("API request", model),
			Details: map[string]interface{}{
				"request_id": controlPlaneString(row["request_id"]), "username": controlPlaneString(row["username"]),
				"model": model, "log_type": logType, "quota": controlPlaneInt64(row["quota"]),
			},
			sourceBit: timelineSourceLogs, sourceRank: 0, sourceID: id, sourceTime: createdAt,
		})
	}
	return events, capability, nil
}

func (s *ControlPlaneSearchService) timelineTopUps(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
) controlPlaneTimelineSourceResult {
	status := controlPlaneSource("newapi.top_ups", "top_up", "create_time", "newapi_main_database")
	result := controlPlaneTimelineSourceResult{status: status, bit: timelineSourceTopUps}
	if cursor.Disabled&timelineSourceTopUps != 0 {
		result.status.Reason = "cursor_source_unavailable"
		return result
	}
	where := "user_id = ?"
	args := []interface{}{userID}
	if cursor.TopUpTime > 0 || cursor.TopUpID > 0 {
		where += " AND (create_time < ? OR (create_time = ? AND id < ?))"
		args = append(args, cursor.TopUpTime, cursor.TopUpTime, cursor.TopUpID)
	}
	statement := s.mainDB.RebindQuery(`SELECT id, user_id, COALESCE(trade_no, '') AS trade_no,
		COALESCE(amount, 0) AS amount, COALESCE(status, '') AS status,
		COALESCE(create_time, 0) AS create_time
		FROM top_ups WHERE ` + where + ` ORDER BY create_time DESC, id DESC LIMIT ?`)
	args = append(args, limit)
	rows, err := s.mainDB.QueryContext(ctx, statement, args...)
	if err != nil {
		result.err = err
		return result
	}
	events := make([]ControlPlaneTimelineEvent, 0, len(rows))
	for _, row := range rows {
		id := controlPlaneInt64(row["id"])
		createdAt := controlPlaneInt64(row["create_time"])
		statusValue := controlPlaneString(row["status"])
		events = append(events, ControlPlaneTimelineEvent{
			Source: "newapi.top_ups", Grain: "top_up",
			Freshness:  ControlPlaneFreshness{Basis: "create_time", ObservedAt: unixSecondsToMillis(createdAt), Unit: "unix_ms"},
			DataSource: "newapi_main_database", EventType: "top_up_created",
			EventID: "top_up:" + strconv.FormatInt(id, 10), UserID: userID,
			Summary: controlPlaneSummary("Top-up", statusValue),
			Details: map[string]interface{}{
				"trade_no": controlPlaneString(row["trade_no"]), "amount": controlPlaneInt64(row["amount"]), "status": statusValue,
			},
			sourceBit: timelineSourceTopUps, sourceRank: 1, sourceID: id, sourceTime: createdAt,
		})
	}
	result.status.Available = true
	result.status.ResultCount = len(events)
	result.events = events
	result.hasMore = len(events) >= limit
	return result
}

func (s *ControlPlaneSearchService) timelineOperationAudits(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
) controlPlaneTimelineSourceResult {
	status := controlPlaneSource("tool_store.operation_audit", "operation_audit", "created_at", "tool_store")
	result := controlPlaneTimelineSourceResult{status: status, bit: timelineSourceAudits}
	if cursor.Disabled&timelineSourceAudits != 0 {
		result.status.Reason = "cursor_source_unavailable"
		return result
	}
	if s.store == nil {
		result.err = toolstore.ErrStoreClosed
		return result
	}
	page, err := s.store.ListOperationAudits(ctx, toolstore.OperationAuditFilter{
		TargetType: "user", TargetID: strconv.FormatInt(userID, 10),
		BeforeCreatedAt: controlPlaneTimelineCursorTime(cursor.AuditTime), BeforeID: cursor.AuditID,
		OrderByCreatedAt: true, Limit: limit,
	})
	if err != nil {
		result.err = err
		return result
	}
	events := make([]ControlPlaneTimelineEvent, 0, len(page.Items))
	for _, item := range page.Items {
		events = append(events, ControlPlaneTimelineEvent{
			Source: "tool_store.operation_audit", Grain: "operation_audit",
			Freshness:  ControlPlaneFreshness{Basis: "created_at", ObservedAt: item.CreatedAt.UnixMilli(), Unit: "unix_ms"},
			DataSource: "tool_store", EventType: string(item.Status),
			EventID: "operation_audit:" + strconv.FormatInt(item.ID, 10), UserID: userID,
			Summary: controlPlaneSummary(item.Action, item.Reason),
			Details: map[string]interface{}{
				"request_id": item.RequestID, "actor": item.Actor, "action": item.Action,
				"target_type": item.TargetType, "target_id": item.TargetID,
				"status": item.Status, "error_code": item.ErrorCode, "occurred_at": item.OccurredAt.UnixMilli(),
			},
			sourceBit: timelineSourceAudits, sourceRank: 2, sourceID: item.ID, sourceTime: item.CreatedAt.UnixMilli(),
		})
	}
	result.status.Available = true
	result.status.ResultCount = len(events)
	result.events = events
	result.hasMore = page.HasMore
	return result
}

func (s *ControlPlaneSearchService) timelineRiskCases(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
) controlPlaneTimelineSourceResult {
	status := controlPlaneSource("tool_store.risk_cases", "risk_case", "created_at", "tool_store")
	result := controlPlaneTimelineSourceResult{status: status, bit: timelineSourceRiskCases}
	if cursor.Disabled&timelineSourceRiskCases != 0 {
		result.status.Reason = "cursor_source_unavailable"
		return result
	}
	if s.store == nil {
		result.err = toolstore.ErrStoreClosed
		return result
	}
	page, err := s.store.ListRiskCases(ctx, toolstore.RiskCaseFilter{
		SubjectType: "user", SubjectID: strconv.FormatInt(userID, 10),
		BeforeCreatedAt: controlPlaneTimelineCursorTime(cursor.RiskTime), BeforeID: cursor.RiskID,
		OrderByCreatedAt: true, Limit: limit,
	})
	if err != nil {
		result.err = err
		return result
	}
	events := make([]ControlPlaneTimelineEvent, 0, len(page.Items))
	for _, item := range page.Items {
		events = append(events, ControlPlaneTimelineEvent{
			Source: "tool_store.risk_cases", Grain: "risk_case",
			Freshness:  ControlPlaneFreshness{Basis: "created_at", ObservedAt: item.CreatedAt.UnixMilli(), Unit: "unix_ms"},
			DataSource: "tool_store", EventType: "risk_case_created",
			EventID: "risk_case:" + strconv.FormatInt(item.ID, 10), UserID: userID,
			Summary: controlPlaneSummary("Risk case created", item.CaseKey),
			Details: map[string]interface{}{
				"case_key": item.CaseKey, "opened_at": item.OpenedAt.UnixMilli(),
			},
			sourceBit: timelineSourceRiskCases, sourceRank: 3, sourceID: item.ID, sourceTime: item.CreatedAt.UnixMilli(),
		})
	}
	result.status.Available = true
	result.status.ResultCount = len(events)
	result.events = events
	result.hasMore = page.HasMore
	return result
}

func (s *ControlPlaneSearchService) timelineSupportNotes(
	ctx context.Context,
	userID int64,
	cursor controlPlaneTimelineCursor,
	limit int,
) controlPlaneTimelineSourceResult {
	status := controlPlaneSource("tool_store.support_notes", "support_note", "created_at", "tool_store")
	result := controlPlaneTimelineSourceResult{status: status, bit: timelineSourceSupportNotes}
	if cursor.Disabled&timelineSourceSupportNotes != 0 {
		result.status.Reason = "cursor_source_unavailable"
		return result
	}
	if s.store == nil {
		result.err = toolstore.ErrStoreClosed
		return result
	}
	page, err := s.store.ListSupportNotes(ctx, toolstore.SupportNoteFilter{
		SubjectType: "user", SubjectID: strconv.FormatInt(userID, 10), IncludeDeleted: true,
		BeforeCreatedAt: controlPlaneTimelineCursorTime(cursor.NoteTime), BeforeID: cursor.NoteID,
		OrderByCreatedAt: true, Limit: limit,
	})
	if err != nil {
		result.err = err
		return result
	}
	events := make([]ControlPlaneTimelineEvent, 0, len(page.Items))
	for _, item := range page.Items {
		events = append(events, ControlPlaneTimelineEvent{
			Source: "tool_store.support_notes", Grain: "support_note",
			Freshness:  ControlPlaneFreshness{Basis: "created_at", ObservedAt: item.CreatedAt.UnixMilli(), Unit: "unix_ms"},
			DataSource: "tool_store", EventType: "support_note_created",
			EventID: "support_note:" + strconv.FormatInt(item.ID, 10), UserID: userID,
			Summary: controlPlaneSummary("Support note created", item.Author),
			Details: map[string]interface{}{
				"author": item.Author,
			},
			sourceBit: timelineSourceSupportNotes, sourceRank: 4, sourceID: item.ID, sourceTime: item.CreatedAt.UnixMilli(),
		})
	}
	result.status.Available = true
	result.status.ResultCount = len(events)
	result.events = events
	result.hasMore = page.HasMore
	return result
}

func (s *ControlPlaneSearchService) logSource(source, grain, freshness string) ControlPlaneSourceStatus {
	status := database.GetLogSourceStatus()
	if s != nil && s.logStatus != nil {
		status = s.logStatus()
	}
	result := controlPlaneSource(source, grain, freshness, "newapi_log_database")
	result.Mode = status.Mode
	result.Fallback = status.UsingFallback
	return result
}

func controlPlaneSource(source, grain, freshness, dataSource string) ControlPlaneSourceStatus {
	return ControlPlaneSourceStatus{
		Source: source, Grain: grain, Freshness: freshness, DataSource: dataSource,
	}
}

func controlPlaneUnavailableReason(err error) string {
	if err == nil {
		return ""
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"no such table", "no such column", "doesn't exist", "does not exist",
		"undefined table", "undefined column", "unknown column",
	} {
		if strings.Contains(lower, marker) {
			return "schema_unavailable"
		}
	}
	return "query_unavailable"
}

func controlPlaneLikePattern(value string) string {
	replacer := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return "%" + replacer.Replace(value) + "%"
}

func controlPlanePositiveInt(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed, err == nil && parsed > 0
}

func controlPlaneInt64(value interface{}) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed
	case []byte:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(string(typed)), 10, 64)
		return parsed
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func controlPlaneString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func unixSecondsToMillis(value int64) int64 {
	if value <= 0 || value > int64(^uint64(0)>>1)/1000 {
		return 0
	}
	return value * 1000
}

func controlPlaneTimelineCursorTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func controlPlaneSummary(primary, secondary string) string {
	primary = strings.TrimSpace(primary)
	secondary = strings.TrimSpace(secondary)
	if primary == "" {
		return truncateControlPlaneText(secondary, 160)
	}
	if secondary == "" {
		return truncateControlPlaneText(primary, 160)
	}
	return truncateControlPlaneText(primary+": "+secondary, 160)
}

func truncateControlPlaneText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= maximum {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximum]) + "…"
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func decodeControlPlaneTimelineCursor(value string) (controlPlaneTimelineCursor, error) {
	if strings.TrimSpace(value) == "" {
		return controlPlaneTimelineCursor{Version: controlPlaneTimelineCursorVersion}, nil
	}
	if len(value) > 2048 {
		return controlPlaneTimelineCursor{}, ErrControlPlaneInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) == 0 || len(raw) > 1024 {
		return controlPlaneTimelineCursor{}, ErrControlPlaneInvalidCursor
	}
	var cursor controlPlaneTimelineCursor
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != controlPlaneTimelineCursorVersion {
		return controlPlaneTimelineCursor{}, ErrControlPlaneInvalidCursor
	}
	if cursor.Disabled & ^uint32(timelineSourceLogs|timelineSourceTopUps|timelineSourceAudits|timelineSourceRiskCases|timelineSourceSupportNotes) != 0 ||
		cursor.LogTime < 0 || cursor.LogID < 0 || cursor.TopUpTime < 0 || cursor.TopUpID < 0 ||
		cursor.AuditTime < 0 || cursor.AuditID < 0 || cursor.RiskTime < 0 || cursor.RiskID < 0 || cursor.NoteTime < 0 || cursor.NoteID < 0 ||
		(cursor.LogTime == 0) != (cursor.LogID == 0) || (cursor.TopUpTime == 0) != (cursor.TopUpID == 0) ||
		(cursor.AuditTime == 0) != (cursor.AuditID == 0) || (cursor.RiskTime == 0) != (cursor.RiskID == 0) ||
		(cursor.NoteTime == 0) != (cursor.NoteID == 0) {
		return controlPlaneTimelineCursor{}, ErrControlPlaneInvalidCursor
	}
	return cursor, nil
}

func encodeControlPlaneTimelineCursor(cursor controlPlaneTimelineCursor) (string, error) {
	cursor.Version = controlPlaneTimelineCursorVersion
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode control-plane timeline cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
