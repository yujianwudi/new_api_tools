package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/new-api-tools/backend/internal/cache"
)

func TestAIBanConfigNeverReturnsRawAPIKey(t *testing.T) {
	cache.Get().ClearLocal()
	svc := &AIAutoBanService{}
	secret := "sk-unit-test-super-secret-value"
	if err := svc.SaveConfig(context.Background(), map[string]interface{}{
		"api_key": secret,
		"model":   "test-model",
	}); err != nil {
		t.Fatal(err)
	}

	view := svc.GetConfig()
	if _, exists := view["api_key"]; exists {
		t.Fatal("GetConfig exposed the api_key field")
	}
	if view["has_api_key"] != true {
		t.Fatalf("has_api_key = %#v", view["has_api_key"])
	}
	masked, _ := view["masked_api_key"].(string)
	if masked == "" || masked == secret {
		t.Fatalf("invalid masked key %q", masked)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("serialized config response contains the raw API key")
	}
	if stored, _ := svc.getStoredConfig()["api_key"].(string); stored != secret {
		t.Fatal("write-only API key was not retained internally")
	}
}

func TestMaskAPIKeyNeverReturnsCredentialFragments(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{name: "empty", apiKey: "", want: ""},
		{name: "one character", apiKey: "a", want: "********"},
		{name: "eight characters", apiKey: "12345678", want: "********"},
		{name: "nine characters", apiKey: "123456789", want: "********"},
		{name: "long credential", apiKey: "sk-unit-test-super-secret-value", want: "********"},
		{name: "unicode credential", apiKey: "密钥-测试", want: "********"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskAPIKey(tt.apiKey); got != tt.want {
				t.Fatalf("maskAPIKey(%q) = %q, want %q", tt.apiKey, got, tt.want)
			}
		})
	}
}

func TestAIBanConfigRejectsUnsafeBaseURL(t *testing.T) {
	cache.Get().ClearLocal()
	svc := &AIAutoBanService{}
	unsafeURLs := []string{
		"http://api.example.com",
		"https://127.0.0.1:3000",
		"https://169.254.169.254/latest/meta-data",
		"https://user:pass@api.example.com",
	}
	for _, baseURL := range unsafeURLs {
		if err := svc.SaveConfig(context.Background(), map[string]interface{}{"base_url": baseURL}); err == nil {
			t.Fatalf("unsafe Base URL %q was accepted", baseURL)
		}
	}
}
