package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
)

// RegisterRedemptionRoutes registers /api/redemptions endpoints
func RegisterRedemptionRoutes(r *gin.RouterGroup, mutationHandler *MutationHandler) {
	if mutationHandler == nil {
		panic("redemption routes require the audited NewAPI mutation handler")
	}
	g := r.Group("/redemptions")
	{
		g.POST("/generate", auth.RequireRole(auth.RoleAdmin), mutationHandler.GenerateRedemptionCodes)
		g.GET("", ListRedemptionCodes)
		g.GET("/statistics", GetRedemptionStatistics)
		g.POST("/batch-delete", mutationHandler.BatchDeleteRedemptionCodes)
		g.DELETE("/batch", mutationHandler.BatchDeleteRedemptionCodes)
		g.POST("/batch", mutationHandler.BatchDeleteRedemptionCodes)
		g.DELETE("/:id", mutationHandler.DeleteRedemptionCode)
	}
}

// POST /api/redemption/generate
func GenerateRedemptionCodes(c *gin.Context) {
	var req service.GenerateParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	result, err := service.GenerateCodes(req)
	if err != nil {
		respondHandlerError(c, http.StatusBadRequest, "GENERATION_ERROR", "Invalid redemption generation parameters", "redemption code generation validation", err)
		return
	}

	if !result.Success {
		respondInternalError(c, "GENERATION_FAILED", "Unable to generate redemption codes", "redemption code generation", errors.New(result.Message))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Successfully generated %d redemption codes", result.Count),
		"data": gin.H{
			"keys":  result.Keys,
			"count": result.Count,
		},
	})
}

// GET /api/redemption
func ListRedemptionCodes(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	params := service.ListRedemptionParams{
		Page:      page,
		PageSize:  pageSize,
		Name:      c.Query("name"),
		Status:    c.Query("status"),
		StartDate: c.Query("start_date"),
		EndDate:   c.Query("end_date"),
	}

	result, err := service.ListCodes(params)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "redemption code list query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    result,
	})
}

// GET /api/redemption/statistics
func GetRedemptionStatistics(c *gin.Context) {
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

	stats, err := service.GetRedemptionStatistics(startDate, endDate)
	if err != nil {
		respondInternalError(c, "QUERY_ERROR", genericUnavailableMessage, "redemption statistics query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

// POST /api/redemption/batch-delete
func BatchDeleteRedemptionCodes(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid request body", ""))
		return
	}

	affected, err := service.DeleteCodes(req.IDs)
	if err != nil {
		respondInternalError(c, "DELETE_ERROR", "Unable to delete redemption codes", "redemption batch delete", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Redemption codes deleted successfully",
		"data":    gin.H{"deleted": affected},
	})
}

// DELETE /api/redemption/:id
func DeleteRedemptionCode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid ID", ""))
		return
	}

	affected, err := service.DeleteCodes([]int64{id})
	if err != nil {
		respondInternalError(c, "DELETE_ERROR", "Unable to delete redemption code", "redemption delete", err)
		return
	}

	if affected == 0 {
		c.JSON(http.StatusNotFound, models.ErrorResp("NOT_FOUND", "Redemption code not found", ""))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Redemption code deleted successfully",
	})
}
