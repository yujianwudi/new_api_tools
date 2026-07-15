package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLoginClientKeyTrustedProxyBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		trustedProxies string
		remoteAddr     string
		forwarded      []string
		realIP         string
		want           string
	}{
		{
			name:           "empty configuration ignores loopback proxy headers",
			trustedProxies: "",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9"},
			realIP:         "203.0.113.9",
			want:           "127.0.0.1",
		},
		{
			name:           "explicit loopback proxy forwards client",
			trustedProxies: "127.0.0.1/32",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9"},
			want:           "203.0.113.9",
		},
		{
			name:           "bare trusted proxy IP is exact",
			trustedProxies: "127.0.0.1",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9"},
			want:           "203.0.113.9",
		},
		{
			name:           "public direct client cannot spoof forwarding headers",
			trustedProxies: "127.0.0.1/32",
			remoteAddr:     "198.51.100.8:43210",
			forwarded:      []string{"203.0.113.9"},
			realIP:         "203.0.113.10",
			want:           "198.51.100.8",
		},
		{
			name:           "walks trusted chain from right and ignores spoofed left prefix",
			trustedProxies: "127.0.0.1/32, 172.20.0.1/32; 10.0.0.10/32",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"198.51.100.99, 203.0.113.9", "10.0.0.10, 172.20.0.1"},
			want:           "203.0.113.9",
		},
		{
			name:           "untrusted intermediary is the client boundary",
			trustedProxies: "127.0.0.1/32",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9, 172.20.0.1"},
			want:           "172.20.0.1",
		},
		{
			name:           "malformed forwarded chain fails closed to peer",
			trustedProxies: "127.0.0.1/32,172.20.0.1/32",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9, unknown, 172.20.0.1"},
			want:           "127.0.0.1",
		},
		{
			name:           "invalid trusted proxy configuration fails closed",
			trustedProxies: "127.0.0.1/32,not-a-network",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9"},
			want:           "127.0.0.1",
		},
		{
			name:           "fully trusted forwarded chain falls back to peer",
			trustedProxies: "127.0.0.1/32,203.0.113.9/32",
			remoteAddr:     "127.0.0.1:43210",
			forwarded:      []string{"203.0.113.9"},
			want:           "127.0.0.1",
		},
		{
			name:           "x real ip alone is not a trusted client source",
			trustedProxies: "127.0.0.1/32",
			remoteAddr:     "127.0.0.1:43210",
			realIP:         "203.0.113.9",
			want:           "127.0.0.1",
		},
		{
			name:           "ipv6 trusted proxy chain",
			trustedProxies: "::1/128,2001:db8::10/128",
			remoteAddr:     "[::1]:43210",
			forwarded:      []string{"2001:db8::20, 2001:db8::10"},
			want:           "2001:db8::20",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(trustedProxyCIDRsEnv, tt.trustedProxies)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("POST", "/api/auth/login", nil)
			ctx.Request.RemoteAddr = tt.remoteAddr
			ctx.Request.Header.Set("X-Real-IP", tt.realIP)
			for _, value := range tt.forwarded {
				ctx.Request.Header.Add("X-Forwarded-For", value)
			}
			if got := loginClientKey(ctx); got != tt.want {
				t.Fatalf("loginClientKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGinClientIPUsesFailClosedTrustedProxySet(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		trustedRaw string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			name:       "direct public peer cannot spoof XFF",
			trustedRaw: "127.0.0.1/32,::1/128",
			remoteAddr: "198.51.100.8:43210",
			forwarded:  "203.0.113.9",
			want:       "198.51.100.8",
		},
		{
			name:       "loopback reverse proxy supplies client IP",
			trustedRaw: "127.0.0.1/32,::1/128",
			remoteAddr: "127.0.0.1:43210",
			forwarded:  "203.0.113.9",
			want:       "203.0.113.9",
		},
		{
			name:       "invalid config trusts no proxy",
			trustedRaw: "127.0.0.1/32,invalid",
			remoteAddr: "127.0.0.1:43210",
			forwarded:  "203.0.113.9",
			want:       "127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trusted, valid := TrustedProxyCIDRsForGin(tt.trustedRaw)
			if !valid {
				trusted = nil
			}
			router := gin.New()
			if err := router.SetTrustedProxies(trusted); err != nil {
				t.Fatalf("SetTrustedProxies returned error: %v", err)
			}
			router.GET("/", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = tt.remoteAddr
			request.Header.Set("X-Forwarded-For", tt.forwarded)
			router.ServeHTTP(recorder, request)
			if recorder.Body.String() != tt.want {
				t.Fatalf("ClientIP() = %q, want %q", recorder.Body.String(), tt.want)
			}
		})
	}
}

func TestRemoteAddressHost(t *testing.T) {
	tests := map[string]string{
		"198.51.100.8:43210":  "198.51.100.8",
		"[2001:db8::1]:43210": "2001:db8::1",
		"2001:db8::1":         "2001:db8::1",
		"[2001:db8::1]":       "2001:db8::1",
		"":                    "",
	}
	for input, want := range tests {
		if got := remoteAddressHost(input); got != want {
			t.Errorf("remoteAddressHost(%q) = %q, want %q", input, got, want)
		}
	}
}
