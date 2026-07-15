package handler

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
)

const genericUnavailableMessage = "Requested data is temporarily unavailable"

var (
	logURLCredentialPattern    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/\s:@]+):([^@\s/]+)@`)
	logMySQLCredentialPattern  = regexp.MustCompile(`\b([^:\s/]+):([^@\s]+)@tcp\(`)
	logSecretAssignmentPattern = regexp.MustCompile(`(?i)\b(password|passwd|pwd|api[_-]?key|jwt[_-]?secret|token|secret)\s*=\s*("[^"]*"|'[^']*'|[^\s,;&]+)`)
	logBearerPattern           = regexp.MustCompile(`(?i)\b(Bearer)\s+[A-Za-z0-9._~+/=-]+`)
)

func sanitizeHandlerErrorForLog(err error) string {
	if err == nil {
		return "operation failed without an underlying error"
	}
	message := err.Error()
	message = logURLCredentialPattern.ReplaceAllString(message, `${1}${2}:[REDACTED]@`)
	message = logMySQLCredentialPattern.ReplaceAllString(message, `${1}:[REDACTED]@tcp(`)
	message = logSecretAssignmentPattern.ReplaceAllString(message, `${1}=[REDACTED]`)
	message = logBearerPattern.ReplaceAllString(message, `${1} [REDACTED]`)
	return message
}

// respondHandlerError keeps implementation details in server logs while
// returning only a stable, operator-controlled message to API clients.
func respondHandlerError(c *gin.Context, status int, code, clientMessage, operation string, err error) {
	logger.L.Error(fmt.Sprintf("%s failed: %s", operation, sanitizeHandlerErrorForLog(err)), logger.CatAPI)
	c.JSON(status, models.ErrorResp(code, clientMessage, ""))
}

func respondInternalError(c *gin.Context, code, clientMessage, operation string, err error) {
	respondHandlerError(c, http.StatusInternalServerError, code, clientMessage, operation, err)
}
