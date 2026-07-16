package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/requestmeta"
)

const (
	RequestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

var requestIDFallbackCounter atomic.Uint64

// RequestIDMiddleware establishes one bounded correlation identifier for each
// request. Caller-provided IDs are accepted only when they are safe to log and
// forward; arbitrary header content is never reflected to logs or NewAPI.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(RequestIDHeader)
		if !validRequestID(requestID) {
			requestID = newRequestID()
		}

		c.Set(requestIDKey, requestID)
		c.Header(RequestIDHeader, requestID)
		c.Request = c.Request.WithContext(requestmeta.WithRequestID(c.Request.Context(), requestID))
		c.Next()
	}
}

// RequestID returns the correlation identifier attached to a Gin request.
func RequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, _ := c.Get(requestIDKey)
	requestID, _ := value.(string)
	return requestID
}

func validRequestID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}

	// The fallback remains unique enough for observability if the OS random
	// source is temporarily unavailable. Request IDs are correlation values,
	// never credentials or authorization tokens.
	sequence := requestIDFallbackCounter.Add(1)
	return "fallback-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(sequence, 36)
}
