package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseRoleRejectsUnknownAndEmptyValues(t *testing.T) {
	for _, value := range []string{"", "   ", "superuser", "unknown"} {
		if role := ParseRole(value); role != RoleInvalid {
			t.Fatalf("ParseRole(%q) = %q, want invalid role", value, role)
		}
	}
	for value, want := range map[string]Role{
		"viewer":     RoleViewer,
		" OPERATOR ": RoleOperator,
		"Admin":      RoleAdmin,
	} {
		if role := ParseRole(value); role != want {
			t.Fatalf("ParseRole(%q) = %q, want %q", value, role, want)
		}
	}
}

func TestRBACViewerCanReadButCannotMutate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth_method", "api_key")
		SetRole(c, RoleViewer)
		c.Next()
	}, RBACMiddleware())
	router.GET("/api/items", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/api/items", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/api/tokens/search", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/api/model-status/status/batch", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/api/models/status/batch", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/items", http.StatusNoContent},
		{http.MethodPost, "/api/tokens/search", http.StatusNoContent},
		{http.MethodPost, "/api/model-status/status/batch", http.StatusNoContent},
		{http.MethodPost, "/api/models/status/batch", http.StatusForbidden},
		{http.MethodPost, "/api/items", http.StatusForbidden},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != test.want {
			t.Fatalf("%s %s = %d, want %d", test.method, test.path, recorder.Code, test.want)
		}
	}
}

func TestRequireAdminRejectsOperator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		SetRole(c, RoleOperator)
		c.Next()
	})
	router.POST("/admin", RequireRole(RoleAdmin), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("operator reached admin route: %d", recorder.Code)
	}
}
