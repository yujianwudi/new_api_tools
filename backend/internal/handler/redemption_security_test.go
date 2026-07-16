package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/database"
	_ "modernc.org/sqlite"
)

func TestViewerRedemptionListDoesNotExposeStoredKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT)`)
	db.MustExec(`CREATE TABLE redemptions (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		key TEXT,
		status INTEGER DEFAULT 1,
		name TEXT,
		quota INTEGER,
		created_time INTEGER,
		redeemed_time INTEGER,
		used_user_id INTEGER,
		deleted_at TEXT,
		expired_time INTEGER
	)`)
	const rawKey = "viewer-must-not-receive-this-redemption-key"
	db.MustExec(`INSERT INTO redemptions
		(id, user_id, key, status, name, quota, created_time, redeemed_time, used_user_id, expired_time)
		VALUES (1, 1, ?, 1, 'sensitive', 500000, 1, 0, 0, 0)`, rawKey)
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	router := gin.New()
	api := router.Group("/api")
	api.Use(func(c *gin.Context) {
		c.Set("auth_method", "api_key")
		auth.SetRole(c, auth.RoleViewer)
		c.Next()
	})
	api.Use(auth.RBACMiddleware())
	RegisterRedemptionRoutes(api, &MutationHandler{})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/redemptions", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("viewer list status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, rawKey) {
		t.Fatalf("viewer response leaked stored redemption key: %s", body)
	}
	if !strings.Contains(body, `"key":"redacted:`) || !strings.Contains(body, `"key_fingerprint":"`) {
		t.Fatalf("viewer response omitted redacted key identity: %s", body)
	}
}
