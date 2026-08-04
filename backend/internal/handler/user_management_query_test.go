package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/service"
	_ "modernc.org/sqlite"
)

func installHandlerUserQueryDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '', display_name TEXT, email TEXT,
			role INTEGER NOT NULL DEFAULT 1, status INTEGER NOT NULL DEFAULT 1,
			quota INTEGER NOT NULL DEFAULT 0, used_quota INTEGER NOT NULL DEFAULT 0,
			request_count INTEGER NOT NULL DEFAULT 0, "group" TEXT DEFAULT 'default',
			aff_code TEXT, aff_count INTEGER NOT NULL DEFAULT 0,
			aff_quota INTEGER NOT NULL DEFAULT 0, aff_history INTEGER NOT NULL DEFAULT 0,
			inviter_id INTEGER, remark TEXT, github_id TEXT, wechat_id TEXT,
			telegram_id TEXT, discord_id TEXT, oidc_id TEXT, linux_do_id TEXT,
			deleted_at INTEGER
		);
		CREATE TABLE logs (id INTEGER PRIMARY KEY, user_id INTEGER, type INTEGER, created_at INTEGER);
	`)
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})
	return db
}

func TestGetUsersRejectsInvalidActivityAtHandler(t *testing.T) {
	installHandlerUserQueryDB(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users", GetUsers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/users?activity=bogus", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGetUsersReturns422ForSchemaUnsupportedSource(t *testing.T) {
	db := installHandlerUserQueryDB(t)
	db.MustExec(`ALTER TABLE users DROP COLUMN linux_do_id`)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users", GetUsers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/users?source=linux_do", nil))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestActivityCandidateScaleErrorFailsClosedAtHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	respondUserReadError(context, "activity-filtered user list", service.ErrActivityFilterScaleExceeded)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode handler response: %v", err)
	}
	if payload.Success || payload.Error.Code != "USER_QUERY_UNAVAILABLE" {
		t.Fatalf("handler did not fail closed: %+v", payload)
	}
}

func TestGetInvitedUsersReturns404ForMissingInviter(t *testing.T) {
	installHandlerUserQueryDB(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users/:user_id/invited", GetInvitedUsers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/users/404/invited", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGetInvitedUsersViewerResponseRedactsPIIAndUsesUserID(t *testing.T) {
	db := installHandlerUserQueryDB(t)
	db.MustExec(`INSERT INTO users
		(id, username, aff_code, aff_quota, aff_history) VALUES (1, 'inviter', 'SECRET', 10, 20)`)
	db.MustExec(`INSERT INTO users
		(id, username, email, role, quota, used_quota, request_count, inviter_id)
		VALUES (2, 'invitee', 'private@example.com', 10, 100, 25, 3, 1)`)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users/:user_id/invited", func(c *gin.Context) {
		auth.SetRole(c, auth.RoleViewer)
		GetInvitedUsers(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/users/1/invited", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data := payload["data"].(map[string]interface{})
	items := data["items"].([]interface{})
	item := items[0].(map[string]interface{})
	if _, ok := item["user_id"]; !ok {
		t.Fatalf("item has no user_id: %v", item)
	}
	for _, key := range []string{"id", "email", "role", "quota", "used_quota"} {
		if _, ok := item[key]; ok {
			t.Fatalf("viewer item contains %s: %v", key, item)
		}
	}
	inviter := data["inviter"].(map[string]interface{})
	for _, key := range []string{"aff_code", "aff_quota", "aff_history"} {
		if _, ok := inviter[key]; ok {
			t.Fatalf("viewer inviter contains %s: %v", key, inviter)
		}
	}
}

func TestGetInvitedUsersEnforcesCrossPageSnapshotIdentity(t *testing.T) {
	db := installHandlerUserQueryDB(t)
	db.MustExec(`INSERT INTO users (id, username, display_name) VALUES (1, 'inviter', 'Inviter')`)
	for id := 2; id <= 12; id++ {
		db.MustExec(`INSERT INTO users
			(id, username, display_name, used_quota, request_count, inviter_id)
			VALUES (?, ?, ?, ?, ?, 1)`, id, fmt.Sprintf("invitee-%02d", id), fmt.Sprintf("User %02d", id), id, id)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users/:user_id/invited", GetInvitedUsers)

	request := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		return recorder
	}
	first := request("/api/users/1/invited?page=1&page_size=10")
	if first.Code != http.StatusOK {
		t.Fatalf("first-page status = %d body=%s", first.Code, first.Body.String())
	}
	var payload struct {
		Data struct {
			AsOf             int64  `json:"as_of"`
			QueryFingerprint string `json:"query_fingerprint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode first-page identity: %v", err)
	}
	if payload.Data.AsOf <= 0 || len(payload.Data.QueryFingerprint) != 64 {
		t.Fatalf("invalid first-page identity: %+v", payload.Data)
	}

	asOfOnly := request(fmt.Sprintf("/api/users/1/invited?page=2&page_size=10&as_of=%d", payload.Data.AsOf))
	if asOfOnly.Code != http.StatusConflict {
		t.Fatalf("as_of-only status = %d, want 409; body=%s", asOfOnly.Code, asOfOnly.Body.String())
	}
	wrongFingerprint := fmt.Sprintf("%064x", 1)
	if wrongFingerprint == payload.Data.QueryFingerprint {
		wrongFingerprint = fmt.Sprintf("%064x", 2)
	}
	wrong := request(fmt.Sprintf("/api/users/1/invited?page=2&page_size=10&as_of=%d&query_fingerprint=%s", payload.Data.AsOf, wrongFingerprint))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong-fingerprint status = %d, want 409; body=%s", wrong.Code, wrong.Body.String())
	}

	target := fmt.Sprintf("/api/users/1/invited?page=2&page_size=10&as_of=%d&query_fingerprint=%s", payload.Data.AsOf, payload.Data.QueryFingerprint)
	second := request(target)
	if second.Code != http.StatusOK {
		t.Fatalf("second-page status = %d body=%s", second.Code, second.Body.String())
	}

	db.MustExec(`UPDATE users SET display_name = 'changed after page one' WHERE id = 2`)
	drifted := request(target)
	if drifted.Code != http.StatusConflict {
		t.Fatalf("content-drift status = %d, want 409; body=%s", drifted.Code, drifted.Body.String())
	}
}

func TestGetInvitedUsersDatabaseFailureIsNotAnEmptySuccess(t *testing.T) {
	db := installHandlerUserQueryDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/users/:user_id/invited", GetInvitedUsers)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/users/1/invited", nil))
	if recorder.Code < 500 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestViewerCannotReadPIIRichUserCollections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api")
	api.Use(func(c *gin.Context) {
		auth.SetRole(c, auth.RoleViewer)
		c.Next()
	})
	RegisterUserManagementRoutes(api, &MutationHandler{})

	for _, target := range []string{
		"/api/users",
		"/api/users/banned",
		"/api/users/soft-deleted/count",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("viewer GET %s status = %d, want 403; body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
}
