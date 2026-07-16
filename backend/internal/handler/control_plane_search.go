package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/toolstore"
)

type SearchHandler struct {
	service *service.ControlPlaneSearchService
}

func NewSearchHandler(store *toolstore.Store) *SearchHandler {
	return &SearchHandler{service: service.NewControlPlaneSearchService(store)}
}

// RegisterRoutes expects the authenticated /api group and additionally applies
// an explicit viewer role floor to every read-only search endpoint.
func (h *SearchHandler) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/control-plane")
	authenticated := requireControlPlaneAuthentication()
	viewer := auth.RequireRole(auth.RoleViewer)
	group.GET("/search", authenticated, viewer, h.Search)
	group.GET("/users/:id/timeline", authenticated, viewer, h.UserTimeline)
}

func (h *SearchHandler) Search(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	if err := validateControlPlaneQuery(c, querySpec{"q": 128, "limit": 2}); err != nil {
		writeControlPlaneInputError(c, "Invalid search query")
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	if err := service.ValidateControlPlaneSearchQuery(query); err != nil {
		writeControlPlaneInputError(c, "Search query must contain 2 to 128 characters")
		return
	}
	limit, err := parseControlPlaneBoundedLimit(c, service.ControlPlaneSearchDefaultLimit, service.ControlPlaneSearchMaxLimit)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid search limit")
		return
	}

	report, err := h.service.Search(c.Request.Context(), query, limit)
	if err != nil {
		h.writeError(c, "control-plane search", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(report))
}

func (h *SearchHandler) UserTimeline(c *gin.Context) {
	if !h.begin(c) {
		return
	}
	if err := validateControlPlaneQuery(c, querySpec{"before": 2048, "limit": 3}); err != nil {
		writeControlPlaneInputError(c, "Invalid timeline query")
		return
	}
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		writeControlPlaneInputError(c, "Invalid user ID")
		return
	}
	limit, err := parseControlPlaneBoundedLimit(c, service.ControlPlaneTimelineDefaultLimit, service.ControlPlaneTimelineMaxLimit)
	if err != nil {
		writeControlPlaneInputError(c, "Invalid timeline limit")
		return
	}
	report, err := h.service.UserTimeline(c.Request.Context(), userID, c.Query("before"), limit)
	if err != nil {
		if errors.Is(err, service.ErrControlPlaneInvalidCursor) {
			writeControlPlaneInputError(c, "Invalid timeline cursor")
			return
		}
		h.writeError(c, "control-plane user timeline", err)
		return
	}
	c.JSON(http.StatusOK, models.NewSuccessResponse(report))
}

func (h *SearchHandler) begin(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if _, ok := authenticatedControlPlaneActor(c); !ok {
		c.JSON(http.StatusUnauthorized, models.NewErrorResponse(
			"UNAUTHORIZED", "Authenticated control-plane access is required"))
		return false
	}
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"SEARCH_UNAVAILABLE", "Control-plane search is temporarily unavailable"))
		return false
	}
	return true
}

func (h *SearchHandler) writeError(c *gin.Context, operation string, err error) {
	switch {
	case errors.Is(err, service.ErrControlPlaneMainDatabaseUnavailable):
		respondHandlerError(c, http.StatusServiceUnavailable, "MAIN_DATABASE_UNAVAILABLE",
			"Main database is temporarily unavailable", operation, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		respondHandlerError(c, http.StatusGatewayTimeout, "QUERY_TIMEOUT",
			"Control-plane query did not complete", operation, err)
	case errors.Is(err, service.ErrControlPlaneInvalidSearch):
		writeControlPlaneInputError(c, "Invalid control-plane query")
	default:
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, operation, err)
	}
}

func parseControlPlaneBoundedLimit(c *gin.Context, defaultValue, maximum int) (int, error) {
	raw, present := c.GetQuery("limit")
	if !present {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximum {
		return 0, errors.New("limit is outside the allowed range")
	}
	return value, nil
}
