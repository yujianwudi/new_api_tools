package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

const statusClientClosedRequest = 499

// GetChannelQuality serves a read-only channel quality snapshot. Route wiring
// is intentionally left to the control-plane router.
func GetChannelQuality(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	window := c.DefaultQuery("window", "24h")
	if !service.IsValidChannelQualityWindow(window) {
		c.JSON(http.StatusBadRequest, models.NewErrorResponse(
			"INVALID_WINDOW",
			"window must be one of 1h, 24h, or 7d",
		))
		return
	}

	report, err := service.NewChannelQualityService().GetChannelQuality(c.Request.Context(), window)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			respondHandlerError(
				c,
				statusClientClosedRequest,
				"QUERY_CANCELED",
				"Channel quality query was canceled",
				"channel quality query",
				err,
			)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			respondHandlerError(
				c,
				http.StatusGatewayTimeout,
				"QUERY_TIMEOUT",
				"Channel quality query did not complete",
				"channel quality query",
				err,
			)
			return
		}
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "channel quality query", err)
		return
	}

	c.JSON(http.StatusOK, models.NewSuccessResponse(report))
}
