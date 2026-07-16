package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/controlplane"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/service"
	"github.com/new-api-tools/backend/internal/toolstore"
)

// 每个调用方（按 user_sub / api_key / IP 退化）同一时刻只允许一个 CSV 导出在跑。
// 60s 长查询连点 5 次会同时打 5 条 / top_ups —— 这把它压成串行。
var exportInFlight sync.Map // map[string]struct{}

func exportLockKey(c *gin.Context) string {
	if sub, ok := c.Get("user_sub"); ok {
		if s, ok := sub.(string); ok && s != "" {
			return "jwt:" + s
		}
	}
	if m, ok := c.Get("auth_method"); ok && m == "api_key" {
		// api_key 请求没有 subject，按 IP + 鉴权方式区分；同一 key + 同一 IP 也算一个使用者。
		return "key:" + c.ClientIP()
	}
	return "ip:" + c.ClientIP()
}

// RegisterTopUpRoutes registers /api/top-ups endpoints
func RegisterTopUpRoutes(r *gin.RouterGroup, store *toolstore.Store) {
	g := r.Group("/top-ups")
	{
		g.GET("", ListTopUps)
		g.GET("/statistics", GetTopUpStatistics)
		g.GET("/payment-methods", GetPaymentMethods)
		g.GET("/payment-providers", GetPaymentProviders)
		g.GET("/export", auth.RequireRole(auth.RoleAdmin), func(c *gin.Context) {
			exportTopUps(c, store)
		})
		g.GET("/:id", GetTopUpRecord)
	}
}

func topUpQueryError(c *gin.Context, operation string, err error) {
	respondInternalError(c, "QUERY_ERROR", "Top-up data is temporarily unavailable", "top-up "+operation, err)
}

// GET /api/top-ups
func ListTopUps(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	params, err := parseTopUpFilters(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid top-up filters", ""))
		return
	}
	params.Page = page
	params.PageSize = pageSize

	result, err := service.ListTopUpRecords(params)
	if err != nil {
		topUpQueryError(c, "list query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    result,
	})
}

func parseTopUpFilters(c *gin.Context) (service.ListTopUpParams, error) {
	params := service.ListTopUpParams{
		Status:          c.Query("status"),
		PaymentMethod:   c.Query("payment_method"),
		PaymentProvider: c.Query("payment_provider"),
		TradeNo:         c.Query("trade_no"),
		Username:        c.Query("username"),
		StartDate:       c.Query("start_date"),
		EndDate:         c.Query("end_date"),
	}

	// Parse optional user_id
	if userIDStr := c.Query("user_id"); userIDStr != "" {
		uid, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil || uid <= 0 {
			return params, errors.New("invalid user_id")
		}
		params.UserID = &uid
	}

	// Parse optional inviter_id (用于邀请返利统计行展开)
	if inviterIDStr := c.Query("inviter_id"); inviterIDStr != "" {
		iid, err := strconv.ParseInt(inviterIDStr, 10, 64)
		if err != nil || iid <= 0 {
			return params, errors.New("invalid inviter_id")
		}
		params.InviterID = &iid
	}

	return params, nil
}

func optionalTopUpFilterID(id *int64) interface{} {
	if id == nil {
		return nil
	}
	return *id
}

// GET /api/top-ups/statistics
func GetTopUpStatistics(c *gin.Context) {
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

	stats, err := service.GetTopUpStatistics(startDate, endDate)
	if err != nil {
		topUpQueryError(c, "statistics query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    stats,
	})
}

// GET /api/top-ups/payment-methods
func GetPaymentMethods(c *gin.Context) {
	methods, err := service.GetPaymentMethods()
	if err != nil {
		topUpQueryError(c, "payment methods query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    methods,
	})
}

// GET /api/top-ups/payment-providers
func GetPaymentProviders(c *gin.Context) {
	providers, err := service.GetPaymentProviders()
	if err != nil {
		topUpQueryError(c, "payment providers query", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    providers,
	})
}

// GET /api/top-ups/:id
func GetTopUpRecord(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid ID", ""))
		return
	}

	record, err := service.GetTopUpByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResp("NOT_FOUND", "Top up record not found", ""))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    record,
	})
}

// GET /api/top-ups/export — streams matching records as CSV. Filters mirror /api/top-ups.
func ExportTopUps(c *gin.Context) {
	exportTopUps(c, nil)
}

func exportTopUps(c *gin.Context, store *toolstore.Store) {
	params, err := parseTopUpFilters(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResp("INVALID_PARAMS", "Invalid top-up filters", ""))
		return
	}
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResp(
			"AUDIT_STORE_UNAVAILABLE",
			"Export was not started because the audit store is unavailable",
			"",
		))
		return
	}

	// 并发互斥：同一个用户已有导出在跑就直接 429，不让长查询叠加。
	lockKey := exportLockKey(c)
	if _, busy := exportInFlight.LoadOrStore(lockKey, struct{}{}); busy {
		c.JSON(http.StatusTooManyRequests, models.ErrorResp(
			"EXPORT_IN_PROGRESS",
			"上一次导出尚未完成，请稍候再试",
			"",
		))
		return
	}
	defer exportInFlight.Delete(lockKey)

	plan, err := service.PrepareTopUpExport(c.Request.Context(), params)
	if err != nil {
		topUpQueryError(c, "export count query", err)
		return
	}
	defer plan.Close()
	total := plan.Snapshot.Total
	if total > service.TopUpExportLimit {
		c.JSON(http.StatusBadRequest, models.ErrorResp(
			"EXPORT_TOO_LARGE",
			fmt.Sprintf("数据量 %d 行超过 %d 行上限，请收窄日期或筛选范围", total, service.TopUpExportLimit),
			"",
		))
		return
	}

	meta := operationMeta(c, "top-up CSV export")
	filters := topUpExportAuditFilters(params)
	recovery, err := createTopUpExportAuditIntentAndRecovery(
		c.Request.Context(), store, meta, filters,
		gin.H{"expected_row_count": total, "snapshot_max_id": plan.Snapshot.MaxID, "format": "csv"},
	)
	if err != nil {
		respondHandlerError(c, http.StatusServiceUnavailable, "AUDIT_STORE_UNAVAILABLE",
			"Export was not started because its audit evidence could not be persisted",
			"top-up export intent and pending audit recovery", errors.New("tool store operation failed"))
		return
	}

	filename := fmt.Sprintf("top_ups_%s.csv", time.Now().Format("20060102_150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Export-Expected-Rows", strconv.FormatInt(total, 10))
	c.Header("Trailer", "X-Export-Row-Count, X-Export-Truncated")

	// 审计日志：合规要求"谁、何时、按什么过滤、导出多少行"，便于事后追踪。
	subject, _ := c.Get("user_sub")
	method, _ := c.Get("auth_method")
	log.Printf(
		"audit top_ups_export user=%v auth=%v rows=%d filters={status:%q payment:%q provider:%q trade_no:%q username:%q user_id:%v inviter_id:%v start:%q end:%q} ip=%s",
		subject, method, total,
		params.Status, params.PaymentMethod, params.PaymentProvider, params.TradeNo, params.Username,
		optionalTopUpFilterID(params.UserID), optionalTopUpFilterID(params.InviterID),
		params.StartDate, params.EndDate, c.ClientIP(),
	)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	exportResult, exportErr := plan.WriteCSV(ctx, c.Writer)
	truncated := exportResult.Truncated || exportErr != nil || exportResult.RowsWritten != total
	c.Header("X-Export-Row-Count", strconv.FormatInt(exportResult.RowsWritten, 10))
	c.Header("X-Export-Truncated", strconv.FormatBool(truncated))
	status := toolstore.OperationSucceeded
	errorCode := ""
	if truncated {
		status = toolstore.OperationFailed
		errorCode = "EXPORT_TRUNCATED"
	}
	if exportErr != nil {
		errorCode = "EXPORT_STREAM_FAILED"
		if errors.Is(exportErr, context.Canceled) || errors.Is(exportErr, context.DeadlineExceeded) {
			status = toolstore.OperationCancelled
			errorCode = "EXPORT_CANCELLED"
		}
	}
	outcomeResult := gin.H{
		"row_count": exportResult.RowsWritten, "expected_row_count": total,
		"truncated": truncated, "snapshot_max_id": plan.Snapshot.MaxID, "format": "csv",
	}
	// Persist the final export evidence in the pre-created recovery record
	// before attempting the append-only outcome. This closes the crash window
	// where the outcome append could fail and the process could stop before the
	// pending record learned the actual row count and terminal status.
	recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
	pendingRecoveryErr := updateTopUpExportAuditRecovery(
		recoveryCtx, store, recovery.ID, meta, status, errorCode, filters, outcomeResult, false,
	)
	recoveryCancel()

	auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
	auditErr := appendTopUpExportAudit(
		auditCtx, store, meta, "outcome", status, errorCode, filters,
		outcomeResult,
	)
	auditCancel()

	recoveryErr := pendingRecoveryErr
	if auditErr == nil {
		resolvedCtx, resolvedCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
		recoveryErr = updateTopUpExportAuditRecovery(
			resolvedCtx, store, recovery.ID, meta, status, errorCode, filters, outcomeResult, true,
		)
		resolvedCancel()
	} else if pendingRecoveryErr != nil {
		// A transient failure while storing the final pending evidence must not
		// be allowed to combine with the failed outcome append without one last
		// detached retry. The original prebuilt snapshot still remains if the
		// Tool Store itself is unavailable.
		retryCtx, retryCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Second)
		recoveryErr = updateTopUpExportAuditRecovery(
			retryCtx, store, recovery.ID, meta, status, errorCode, filters, outcomeResult, false,
		)
		retryCancel()
	}

	if auditErr != nil && recoveryErr != nil {
		log.Printf("top_ups export outcome audit failed; durable pending recovery could not retain final evidence request_id=%s", meta.RequestID)
	} else if auditErr != nil {
		log.Printf("top_ups export outcome audit queued for reconciliation request_id=%s", meta.RequestID)
	} else if recoveryErr != nil {
		log.Printf("top_ups export outcome audit persisted but pending recovery could not be resolved request_id=%s", meta.RequestID)
	}
	if exportErr != nil {
		// 响应头已发出，无法切回 JSON。CSV 末尾追加注释会污染 Excel 解析，
		// 这里仅 server log，前端通过文件最后一行可观察到截断。
		if !errors.Is(exportErr, context.Canceled) {
			log.Printf("top_ups export failed: %v", exportErr)
		}
	}
}

func topUpExportAuditFilters(params service.ListTopUpParams) gin.H {
	return gin.H{
		"status": params.Status, "payment_method": params.PaymentMethod,
		"payment_provider": params.PaymentProvider, "trade_no": params.TradeNo,
		"username": params.Username, "user_id": optionalTopUpFilterID(params.UserID),
		"inviter_id": optionalTopUpFilterID(params.InviterID),
		"start_date": params.StartDate, "end_date": params.EndDate,
	}
}

func appendTopUpExportAudit(
	ctx context.Context,
	store *toolstore.Store,
	meta controlplane.OperationMeta,
	phase string,
	status toolstore.OperationStatus,
	errorCode string,
	filters any,
	result any,
) error {
	if store == nil {
		return toolstore.ErrStoreClosed
	}
	auditInput, err := topUpExportAuditInput(meta, phase, status, errorCode, filters, result)
	if err != nil {
		return err
	}
	_, err = store.AppendOperationAudit(ctx, auditInput)
	return err
}

func topUpExportAuditInput(
	meta controlplane.OperationMeta,
	phase string,
	status toolstore.OperationStatus,
	errorCode string,
	filters any,
	result any,
) (toolstore.OperationAuditInput, error) {
	beforeJSON, err := json.Marshal(filters)
	if err != nil {
		return toolstore.OperationAuditInput{}, fmt.Errorf("marshal top-up export filters: %w", err)
	}
	afterJSON, err := json.Marshal(result)
	if err != nil {
		return toolstore.OperationAuditInput{}, fmt.Errorf("marshal top-up export result: %w", err)
	}
	return toolstore.OperationAuditInput{
		RequestID: meta.RequestID, Actor: meta.Actor, SourceIP: meta.SourceIP,
		AuthMethod: meta.AuthMethod, Action: "top_ups.export." + phase,
		TargetType: "financial_export", TargetID: meta.RequestID,
		Reason: meta.Reason, BeforeJSON: json.RawMessage(beforeJSON), AfterJSON: json.RawMessage(afterJSON),
		Status: status, ErrorCode: errorCode,
		IdempotencyKey: "export:" + meta.RequestID + ":" + phase,
	}, nil
}

func createTopUpExportAuditIntentAndRecovery(
	ctx context.Context,
	store *toolstore.Store,
	meta controlplane.OperationMeta,
	filters any,
	result any,
) (toolstore.ReconciliationRun, error) {
	if store == nil {
		return toolstore.ReconciliationRun{}, toolstore.ErrStoreClosed
	}
	auditInput, err := topUpExportAuditInput(
		meta, "intent", toolstore.OperationSucceeded, "", filters, result,
	)
	if err != nil {
		return toolstore.ReconciliationRun{}, err
	}
	summaryJSON, err := topUpExportAuditRecoverySummary(meta, "pending", "", "", filters, result, false)
	if err != nil {
		return toolstore.ReconciliationRun{}, err
	}
	now := time.Now().UTC()
	_, run, err := store.AppendOperationAuditWithReconciliationRun(ctx, auditInput, toolstore.ReconciliationRunInput{
		RunKey: "top-up-export-audit:" + meta.RequestID,
		Kind:   "top_up_export_audit_outcome", Status: toolstore.ReconciliationRunning,
		WindowStart: now.Add(-time.Second), WindowEnd: now, StartedAt: now,
		ScannedCount: 1, MatchedCount: 0, DiscrepancyCount: 1,
		Currency: "XXX", SummaryJSON: json.RawMessage(summaryJSON),
		ErrorCode:    "OPERATION_AUDIT_APPEND_PENDING",
		ErrorMessage: "Top-up export outcome audit has not been finalized",
	})
	if err != nil {
		return toolstore.ReconciliationRun{}, fmt.Errorf("persist top-up export intent and pending audit recovery: %w", err)
	}
	return run, nil
}

func updateTopUpExportAuditRecovery(
	ctx context.Context,
	store *toolstore.Store,
	runID int64,
	meta controlplane.OperationMeta,
	status toolstore.OperationStatus,
	errorCode string,
	filters any,
	result any,
	outcomePersisted bool,
) error {
	phase := "pending"
	reconciliationStatus := toolstore.ReconciliationRunning
	reconciliationErrorCode := "OPERATION_AUDIT_APPEND_PENDING"
	reconciliationErrorMessage := "Top-up export outcome audit must be replayed from summary_json"
	matchedCount := int64(0)
	discrepancyCount := int64(1)
	var finishedAt *time.Time
	if outcomePersisted {
		phase = "resolved"
		reconciliationStatus = toolstore.ReconciliationSucceeded
		reconciliationErrorCode = ""
		reconciliationErrorMessage = ""
		matchedCount = 1
		discrepancyCount = 0
		finished := time.Now().UTC()
		finishedAt = &finished
	}
	summaryJSON, err := topUpExportAuditRecoverySummary(meta, phase, status, errorCode, filters, result, outcomePersisted)
	if err != nil {
		return err
	}
	_, err = store.UpdateReconciliationRun(ctx, toolstore.ReconciliationRunUpdate{
		ID: runID, Status: reconciliationStatus, FinishedAt: finishedAt,
		ScannedCount: 1, MatchedCount: matchedCount, DiscrepancyCount: discrepancyCount,
		SummaryJSON: json.RawMessage(summaryJSON), ErrorCode: reconciliationErrorCode,
		ErrorMessage: reconciliationErrorMessage,
	})
	if err != nil {
		return fmt.Errorf("update top-up export pending audit recovery: %w", err)
	}
	return nil
}

func topUpExportAuditRecoverySummary(
	meta controlplane.OperationMeta,
	phase string,
	status toolstore.OperationStatus,
	errorCode string,
	filters any,
	result any,
	outcomePersisted bool,
) ([]byte, error) {
	summaryJSON, err := json.Marshal(gin.H{
		"request_id": meta.RequestID,
		"actor":      meta.Actor, "source_ip": meta.SourceIP, "auth_method": meta.AuthMethod,
		"action": "top_ups.export.outcome", "target_type": "financial_export", "target_id": meta.RequestID,
		"reason": meta.Reason, "status": status, "error_code": errorCode,
		"idempotency_key": "export:" + meta.RequestID + ":outcome",
		"phase":           phase, "outcome_persisted": outcomePersisted,
		"filters": filters, "result": result,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal top-up export audit recovery: %w", err)
	}
	return summaryJSON, nil
}
