package handler

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
)

const trustedProxyCIDRsEnv = "TRUSTED_PROXY_CIDRS"

// TrustedProxyCIDRsForGin returns the same fail-closed proxy set used by the
// login and public endpoint limiters, normalized for gin.Engine.
func TrustedProxyCIDRsForGin(raw string) ([]string, bool) {
	networks, valid := parseTrustedProxyCIDRs(raw)
	if !valid {
		return nil, false
	}
	result := make([]string, 0, len(networks))
	for _, network := range networks {
		result = append(result, network.String())
	}
	return result, true
}

// RegisterAuthRoutes registers authentication endpoints
func RegisterAuthRoutes(rg *gin.RouterGroup) {
	authGroup := rg.Group("/auth")
	{
		authGroup.POST("/login", Login)
		authGroup.POST("/logout", Logout)
	}
}

// Login handles POST /api/auth/login
// Matches Python's login endpoint in auth_routes.py
//
// 请求体:
//
//	{"password": "admin_password"}
//
// 成功响应 (200):
//
//	{"success": true, "message": "登录成功", "token": "eyJ...", "expires_at": "2024-01-01T00:00:00Z"}
//
// 失败响应 (401):
//
//	{"success": false, "message": "密码错误"}
func Login(c *gin.Context) {
	clientIP := loginClientKey(c)
	if allowed, retryAfter := auth.AllowLoginAttempt(clientIP); !allowed {
		setRetryAfter(c, retryAfter)
		c.JSON(http.StatusTooManyRequests, models.LoginResponse{
			Success: false,
			Message: "登录尝试过于频繁，请稍后再试",
		})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		setRetryAfter(c, auth.RecordLoginFailure(clientIP))
		c.JSON(http.StatusBadRequest, models.LoginResponse{
			Success: false,
			Message: "请求格式错误",
		})
		return
	}

	// Verify password
	if !auth.VerifyPassword(req.Password) {
		setRetryAfter(c, auth.RecordLoginFailure(clientIP))
		logger.L.AuthFail("登录失败 | ip=" + clientIP)
		c.JSON(http.StatusUnauthorized, models.LoginResponse{
			Success: false,
			Message: "密码错误",
		})
		return
	}

	// Generate JWT token
	token, expiresAt, err := auth.GenerateToken("admin")
	if err != nil {
		logger.L.Error("Token 生成失败: "+err.Error(), logger.CatAuth)
		c.JSON(http.StatusInternalServerError, models.LoginResponse{
			Success: false,
			Message: "Token 生成失败",
		})
		return
	}

	auth.ResetLoginFailures(clientIP)
	logger.L.Auth("登录成功 | ip=" + clientIP)

	c.JSON(http.StatusOK, models.LoginResponse{
		Success:   true,
		Message:   "登录成功",
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

func setRetryAfter(c *gin.Context, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	c.Header("Retry-After", strconv.FormatInt(seconds, 10))
}

// Use the direct TCP peer as the limiter key unless that peer is an explicitly
// trusted proxy. An empty or invalid TRUSTED_PROXY_CIDRS configuration trusts
// no proxy, so a public listener cannot be bypassed with a client-supplied XFF.
func loginClientKey(c *gin.Context) string {
	host := remoteAddressHost(c.Request.RemoteAddr)
	peerIP := net.ParseIP(host)
	trustedProxies, valid := parseTrustedProxyCIDRs(os.Getenv(trustedProxyCIDRsEnv))
	if peerIP != nil && valid && isTrustedProxy(peerIP, trustedProxies) {
		forwarded := strings.Join(c.Request.Header.Values("X-Forwarded-For"), ",")
		return forwardedLoginClientIP(peerIP, forwarded, trustedProxies)
	}

	if host != "" {
		return host
	}
	return "unknown"
}

func remoteAddressHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil && host != "" {
		return host
	}
	if len(remoteAddr) > 2 && remoteAddr[0] == '[' && remoteAddr[len(remoteAddr)-1] == ']' {
		return remoteAddr[1 : len(remoteAddr)-1]
	}
	return remoteAddr
}

// parseTrustedProxyCIDRs accepts comma-, semicolon-, or whitespace-separated
// IPs and CIDRs. A bare IP is converted to an exact /32 or /128 network. Any
// malformed entry invalidates the whole list so configuration mistakes fail
// closed instead of unexpectedly trusting part of a proxy chain.
func parseTrustedProxyCIDRs(raw string) ([]*net.IPNet, bool) {
	entries := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\r', '\n':
			return true
		default:
			return false
		}
	})
	if len(entries) == 0 {
		return nil, true
	}

	networks := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		if ip := net.ParseIP(entry); ip != nil {
			bits := 128
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 32
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}

		ip, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, false
		}
		network.IP = ip.Mask(network.Mask)
		networks = append(networks, network)
	}
	return networks, true
}

func isTrustedProxy(ip net.IP, trustedProxies []*net.IPNet) bool {
	for _, network := range trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedLoginClientIP walks X-Forwarded-For from the proxy nearest to this
// service toward the client. Trusted proxy hops are removed from the right;
// the first untrusted address is the client key. Malformed or fully trusted
// chains fall back to the direct peer, which is the safe limiter identity.
func forwardedLoginClientIP(peerIP net.IP, header string, trustedProxies []*net.IPNet) string {
	parts := strings.Split(header, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			return peerIP.String()
		}
		if !isTrustedProxy(candidate, trustedProxies) {
			return candidate.String()
		}
	}
	return peerIP.String()
}

// Logout handles POST /api/auth/logout
// Matches Python's logout endpoint
//
// 响应 (200):
//
//	{"success": true, "message": "已登出"}
func Logout(c *gin.Context) {
	c.JSON(http.StatusOK, models.LogoutResponse{
		Success: true,
		Message: "已登出",
	})
}
