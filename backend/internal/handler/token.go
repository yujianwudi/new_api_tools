package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/service"
)

const maxTokenSearchBodyBytes int64 = 4 * 1024

type tokenSearchRequest struct {
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Status   string `json:"status"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	UserID   int64  `json:"user_id"`
	Group    string `json:"group"`
	Expired  string `json:"expired"`
}

func (r tokenSearchRequest) listParams() (service.TokenListParams, error) {
	key := strings.TrimSpace(r.Key)
	if key == "" {
		return service.TokenListParams{}, errors.New("token key is required")
	}
	if len(key) > 256 {
		return service.TokenListParams{}, errors.New("token key is too long")
	}
	return service.TokenListParams{
		Page:     r.Page,
		PageSize: r.PageSize,
		Status:   r.Status,
		Name:     r.Name,
		Key:      key,
		UserID:   r.UserID,
		Group:    r.Group,
		Expired:  r.Expired,
	}, nil
}

func tokenHandlerError(c *gin.Context, operation, clientMessage string, err error) {
	logger.L.Error(fmt.Sprintf("Token %s failed: %v", operation, err), logger.CatAPI)
	c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": clientMessage})
}

// RegisterTokenRoutes registers /api/tokens endpoints
func RegisterTokenRoutes(r *gin.RouterGroup) {
	g := r.Group("/tokens")
	{
		g.GET("", ListTokens)
		g.POST("/search", SearchTokens)
		g.GET("/statistics", GetTokenStatistics)
		g.GET("/groups", GetTokenGroups)
	}
}

// GET /api/tokens
func ListTokens(c *gin.Context) {
	if _, supplied := c.GetQuery("key"); supplied {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Token key lookup must use POST /api/tokens/search",
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	params := service.TokenListParams{
		Page:     page,
		PageSize: pageSize,
		Status:   c.Query("status"),
		Name:     c.Query("name"),
		UserID:   userID,
		Group:    c.Query("group"),
		Expired:  c.Query("expired"),
	}

	svc := service.NewTokenService()
	result, err := svc.ListTokens(params)
	if err != nil {
		tokenHandlerError(c, "list", "Failed to list tokens", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    result,
	})
}

// POST /api/tokens/search performs an exact token-key lookup without placing
// credentials in the request URL, reverse-proxy access logs, or browser history.
func SearchTokens(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTokenSearchBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()

	var request tokenSearchRequest
	if err := decoder.Decode(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid token search request"})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid token search request"})
		return
	}

	params, err := request.listParams()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "A valid token key is required"})
		return
	}

	result, err := service.NewTokenService().ListTokens(params)
	if err != nil {
		tokenHandlerError(c, "exact search", "Failed to search tokens", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// GET /api/tokens/groups
func GetTokenGroups(c *gin.Context) {
	svc := service.NewTokenService()
	groups, err := svc.GetTokenGroups()
	if err != nil {
		tokenHandlerError(c, "groups", "Failed to get token groups", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    groups,
	})
}

// GET /api/tokens/statistics
func GetTokenStatistics(c *gin.Context) {
	svc := service.NewTokenService()
	stats, err := svc.GetTokenStatistics()
	if err != nil {
		tokenHandlerError(c, "statistics", "Failed to get token statistics", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}
