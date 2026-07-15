package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
