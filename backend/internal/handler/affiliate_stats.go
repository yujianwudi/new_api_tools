package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// maxAffiliatePage bounds deep pagination and keeps the largest possible
// offset safely within the int range on every supported architecture.
const maxAffiliatePage = 100000

// RegisterAffiliateStatsRoutes mounts the invite top-up analysis APIs. These
// routes report current-state attribution and never claim to calculate referral
// rewards or settlement amounts.
func RegisterAffiliateStatsRoutes(r *gin.RouterGroup) {
	operator := auth.RequireRole(auth.RoleOperator)
	g := r.Group("/users/invite-topup-analysis")
	{
		g.GET("", operator, ListAffiliateStats)
		g.GET("/summary", operator, GetAffiliateStatsSummary)
		g.GET("/:inviter_id/details", operator, ListAffiliateTopUpDetails)
	}

	// Keep the v0.6.0 read routes compatible while clients migrate to the
	// truthful invite-topup-analysis name. Detail data only exists on the new,
	// explicitly role-gated route.
	legacy := r.Group("/users/affiliate-stats")
	{
		legacy.GET("", operator, ListAffiliateStats)
		legacy.GET("/summary", operator, GetAffiliateStatsSummary)
	}
}

func strictAffiliateIntQuery(c *gin.Context, name string, defaultValue, maxValue int) (int, error) {
	raw, supplied := c.GetQuery(name)
	if !supplied || strings.TrimSpace(raw) == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || (maxValue > 0 && value > maxValue) {
		if maxValue > 0 {
			return 0, fmt.Errorf("%s must be between 1 and %d", name, maxValue)
		}
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}

func parseAffiliateParams(c *gin.Context) (service.AffiliateStatsParams, error) {
	page, err := strictAffiliateIntQuery(c, "page", 1, maxAffiliatePage)
	if err != nil {
		return service.AffiliateStatsParams{}, err
	}
	pageSize, err := strictAffiliateIntQuery(c, "page_size", 20, 100)
	if err != nil {
		return service.AffiliateStatsParams{}, err
	}
	var asOf int64
	if raw, supplied := c.GetQuery("as_of"); supplied {
		asOf, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || asOf < 1 {
			return service.AffiliateStatsParams{}, errors.New("as_of must be a positive Unix timestamp")
		}
	}
	var expectedDetailTotal *int64
	if raw, supplied := c.GetQuery("expected_total"); supplied {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || value < 0 {
			return service.AffiliateStatsParams{}, errors.New("expected_total must be a non-negative integer")
		}
		expectedDetailTotal = &value
	}
	return service.AffiliateStatsParams{
		Page: page, PageSize: pageSize,
		Search: c.Query("search"), StartDate: c.Query("start_date"), EndDate: c.Query("end_date"),
		SortBy: c.Query("sort_by"), SortDir: c.Query("sort_dir"), AsOf: asOf,
		ExpectedFingerprint:        c.Query("query_fingerprint"),
		ExpectedDetailTotal:        expectedDetailTotal,
		ExpectedDetailEvidenceHash: c.Query("expected_evidence_hash"),
	}, nil
}

func respondAffiliateInputError(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", err.Error(), ""))
}

func respondAffiliateServiceError(c *gin.Context, operation string, err error) {
	if errors.Is(err, service.ErrInvalidAffiliateStatsParams) {
		respondAffiliateInputError(c, err)
		return
	}
	if errors.Is(err, service.ErrAffiliateInviterNotFound) {
		c.JSON(http.StatusNotFound, models.ErrorResp("INVITER_NOT_FOUND", "Invite top-up analysis inviter was not found", ""))
		return
	}
	if errors.Is(err, service.ErrAffiliateSnapshotChanged) {
		c.JSON(http.StatusConflict, models.ErrorResp("QUERY_SNAPSHOT_CHANGED", "Invite top-up detail evidence changed; refresh the parent query", ""))
		return
	}
	respondHandlerError(
		c, http.StatusServiceUnavailable, "INVITE_TOPUP_ANALYSIS_UNAVAILABLE",
		"Invite top-up analysis is temporarily unavailable", operation, err,
	)
}

// GET /api/users/invite-topup-analysis
func ListAffiliateStats(c *gin.Context) {
	params, err := parseAffiliateParams(c)
	if err != nil {
		respondAffiliateInputError(c, err)
		return
	}
	result, err := service.ListAffiliateStatsContext(c.Request.Context(), params)
	if err != nil {
		respondAffiliateServiceError(c, "invite top-up analysis list query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// GET /api/users/invite-topup-analysis/summary
func GetAffiliateStatsSummary(c *gin.Context) {
	params, err := parseAffiliateParams(c)
	if err != nil {
		respondAffiliateInputError(c, err)
		return
	}
	summary, err := service.GetAffiliateStatsSummaryContext(c.Request.Context(), params)
	if err != nil {
		respondAffiliateServiceError(c, "invite top-up analysis summary query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": summary})
}

// GET /api/users/invite-topup-analysis/:inviter_id/details
func ListAffiliateTopUpDetails(c *gin.Context) {
	inviterID, err := strconv.ParseInt(c.Param("inviter_id"), 10, 64)
	if err != nil || inviterID < 1 {
		respondAffiliateInputError(c, errors.New("inviter_id must be positive"))
		return
	}
	params, err := parseAffiliateParams(c)
	if err != nil {
		respondAffiliateInputError(c, err)
		return
	}
	result, err := service.ListAffiliateTopUpDetailsContext(c.Request.Context(), inviterID, params)
	if err != nil {
		respondAffiliateServiceError(c, "invite top-up analysis detail query", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}
