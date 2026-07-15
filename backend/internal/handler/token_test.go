package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestListTokensRejectsKeyInQueryStringWithoutEchoingIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-super-secret-token"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/tokens?key="+secret, nil)

	ListTokens(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response echoed token secret: %s", w.Body.String())
	}
}

func TestTokenSearchRequestValidation(t *testing.T) {
	params, err := (tokenSearchRequest{
		Page:     2,
		PageSize: 20,
		Status:   "active",
		Name:     "ops",
		Key:      "  sk-secret  ",
		Group:    "vip",
	}).listParams()
	if err != nil {
		t.Fatalf("listParams returned error: %v", err)
	}
	if params.Key != "sk-secret" || params.Page != 2 || params.PageSize != 20 || params.Status != "active" || params.Name != "ops" || params.Group != "vip" {
		t.Fatalf("unexpected params: %+v", params)
	}

	for _, request := range []tokenSearchRequest{{}, {Key: strings.Repeat("x", 257)}} {
		if _, err := request.listParams(); err == nil {
			t.Fatalf("invalid request was accepted: %+v", request)
		}
	}
}

func TestSearchTokensRejectsInvalidOrOversizedBodiesWithoutEchoingSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"key":"sk-secret"`},
		{name: "unknown field", body: `{"key":"sk-secret","unexpected":true}`},
		{name: "multiple values", body: `{"key":"sk-secret"}{"key":"sk-other"}`},
		{name: "oversized", body: `{"key":"` + strings.Repeat("s", int(maxTokenSearchBodyBytes)) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/tokens/search", strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			SearchTokens(c)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "sk-secret") || strings.Contains(w.Body.String(), "sk-other") {
				t.Fatalf("response echoed token secret: %s", w.Body.String())
			}
		})
	}
}
