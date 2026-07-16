package service

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/util"
)

// TopUpRecord represents a top-up record
type TopUpRecord struct {
	ID                int64    `json:"id" db:"id"`
	UserID            int64    `json:"user_id" db:"user_id"`
	Username          *string  `json:"username" db:"username"`
	Amount            int64    `json:"amount" db:"amount"`
	Money             float64  `json:"money" db:"money"`
	TradeNo           string   `json:"trade_no" db:"trade_no"`
	PaymentMethod     string   `json:"payment_method" db:"payment_method"`
	PaymentProvider   string   `json:"payment_provider" db:"payment_provider"`
	CreateTime        int64    `json:"create_time" db:"create_time"`
	CompleteTime      int64    `json:"complete_time" db:"complete_time"`
	Status            string   `json:"status" db:"status"`
	StatusBucket      string   `json:"status_bucket" db:"status_bucket"`
	CompletionSeconds int64    `json:"completion_seconds" db:"completion_seconds"`
	AnomalyReasons    []string `json:"anomaly_reasons,omitempty"`
}

// TopUpStatistics holds aggregate top-up statistics
type TopUpStatistics struct {
	TotalCount    int64   `json:"total_count"`
	TotalAmount   int64   `json:"total_amount"`
	TotalMoney    float64 `json:"total_money"`
	SuccessCount  int64   `json:"success_count"`
	SuccessAmount int64   `json:"success_amount"`
	SuccessMoney  float64 `json:"success_money"`
	PendingCount  int64   `json:"pending_count"`
	PendingAmount int64   `json:"pending_amount"`
	PendingMoney  float64 `json:"pending_money"`
	FailedCount   int64   `json:"failed_count"`
	FailedAmount  int64   `json:"failed_amount"`
	FailedMoney   float64 `json:"failed_money"`
	ExpiredCount  int64   `json:"expired_count"`
	ExpiredAmount int64   `json:"expired_amount"`
	ExpiredMoney  float64 `json:"expired_money"`
	UnknownCount  int64   `json:"unknown_count"`
	UnknownAmount int64   `json:"unknown_amount"`
	UnknownMoney  float64 `json:"unknown_money"`
}

// ListTopUpParams holds list query parameters
type ListTopUpParams struct {
	Page            int    `json:"page"`
	PageSize        int    `json:"page_size"`
	UserID          *int64 `json:"user_id"`
	Username        string `json:"username"`
	InviterID       *int64 `json:"inviter_id"`
	Status          string `json:"status"`
	PaymentMethod   string `json:"payment_method"`
	PaymentProvider string `json:"payment_provider"`
	TradeNo         string `json:"trade_no"`
	StartDate       string `json:"start_date"`
	EndDate         string `json:"end_date"`
}

// PaginatedTopUps holds paginated top-up results
type PaginatedTopUps struct {
	Items      []TopUpRecord `json:"items"`
	Total      int64         `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	TotalPages int           `json:"total_pages"`
}

const defaultPendingAnomalyHours = 2

// spreadsheetSafeCSVCell prevents user-controlled text from being interpreted
// as a formula when an exported CSV is opened in Excel or LibreOffice. CSV
// quoting only protects the file structure; spreadsheet applications may still
// execute cells whose first non-whitespace character is =, +, -, or @.
func spreadsheetSafeCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed == "" {
		return value
	}

	leadingWhitespace := value[:len(value)-len(trimmed)]
	if strings.ContainsAny(leadingWhitespace, "\t\r\n") {
		return "'" + value
	}

	switch trimmed[0] {
	case '=', '+', '-', '@':
		return "'" + value
	default:
		return value
	}
}

func topUpStatusBucketSQL(column string) string {
	trimmed := fmt.Sprintf("TRIM(COALESCE(%s, ''))", column)
	lower := fmt.Sprintf("LOWER(%s)", trimmed)
	return fmt.Sprintf(`CASE
		WHEN %s = '' THEN 'pending'
		WHEN %s IN ('success', 'completed') OR %s = '1' THEN 'success'
		WHEN %s IN ('failed', 'error') OR %s = '-1' THEN 'failed'
		WHEN %s = 'expired' THEN 'expired'
		WHEN %s IN ('pending', 'processing', 'created', 'waiting', 'unpaid') OR %s = '0' THEN 'pending'
		ELSE 'unknown'
	END`, trimmed, lower, trimmed, lower, trimmed, lower, lower, trimmed)
}

func topUpStatusBucket(status string) string {
	trimmed := strings.TrimSpace(status)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return "pending"
	case lower == "success" || lower == "completed" || trimmed == "1":
		return "success"
	case lower == "failed" || lower == "error" || trimmed == "-1":
		return "failed"
	case lower == "expired":
		return "expired"
	case lower == "pending" || lower == "processing" || lower == "created" || lower == "waiting" || lower == "unpaid" || trimmed == "0":
		return "pending"
	default:
		return "unknown"
	}
}

func topUpCompletionSeconds(createTime, completeTime int64) int64 {
	if createTime <= 0 || completeTime <= 0 || completeTime < createTime {
		return 0
	}
	return completeTime - createTime
}

// isCompleteTradeNo reports whether the input looks like a full trade number
// rather than a search fragment. A complete trade_no has no LIKE wildcards
// (% / _) and no internal whitespace — in that case we match it with an exact
// equality against the unique top_ups_trade_no_key index. Anything else falls
// back to a substring LIKE.
func isCompleteTradeNo(s string) bool {
	return !strings.ContainsAny(s, "%_ \t")
}

func enrichTopUpRecord(rec *TopUpRecord, now int64, pendingHours int) {
	if rec.StatusBucket == "" {
		rec.StatusBucket = topUpStatusBucket(rec.Status)
	}
	if rec.CompletionSeconds == 0 {
		rec.CompletionSeconds = topUpCompletionSeconds(rec.CreateTime, rec.CompleteTime)
	}
	rec.AnomalyReasons = topUpAnomalyReasons(*rec, now, pendingHours)
}

func topUpAnomalyReasons(rec TopUpRecord, now int64, pendingHours int) []string {
	if pendingHours < 1 {
		pendingHours = defaultPendingAnomalyHours
	}

	bucket := rec.StatusBucket
	if bucket == "" {
		bucket = topUpStatusBucket(rec.Status)
	}

	reasons := make([]string, 0, 4)
	if strings.TrimSpace(rec.TradeNo) == "" {
		reasons = append(reasons, "空交易号")
	}
	if rec.Money <= 0 {
		reasons = append(reasons, "金额异常")
	}
	if rec.Amount <= 0 {
		reasons = append(reasons, "额度异常")
	}
	if rec.CreateTime > 0 && rec.CompleteTime > 0 && rec.CompleteTime < rec.CreateTime {
		reasons = append(reasons, "完成早于创建")
	}
	if bucket == "pending" && rec.CreateTime > 0 && now-rec.CreateTime >= int64(pendingHours)*3600 {
		reasons = append(reasons, "超时待支付")
	}
	if bucket == "unknown" {
		reasons = append(reasons, "未知状态")
	}
	return reasons
}

func topUpPaymentProviderExpr(alias string) string {
	db := database.Get()
	if db.ColumnExists("top_ups", "payment_provider") {
		if alias != "" {
			return alias + ".payment_provider"
		}
		return "payment_provider"
	}
	return "''"
}

func topUpSelectColumns() string {
	return fmt.Sprintf(`t.id, t.user_id, u.username, t.amount, t.money,
		COALESCE(t.trade_no,'') as trade_no,
		COALESCE(t.payment_method,'') as payment_method,
		COALESCE(%s,'') as payment_provider,
		COALESCE(t.create_time,0) as create_time,
		COALESCE(t.complete_time,0) as complete_time,
		COALESCE(t.status,'') as status,
		%s as status_bucket,
		CASE
			WHEN t.create_time > 0 AND t.complete_time > 0 AND t.complete_time >= t.create_time THEN t.complete_time - t.create_time
			ELSE 0
		END as completion_seconds`, topUpPaymentProviderExpr("t"), topUpStatusBucketSQL("t.status"))
}

// buildTopUpWhere translates filter params into a parameterised WHERE clause.
// Returns the WHERE body (without the leading "WHERE"), the corresponding args,
// and the next placeholder index that the caller should use for additional args
// (e.g. LIMIT/OFFSET when paginating).
func buildTopUpWhere(params ListTopUpParams) (string, []interface{}, int) {
	db := database.Get()

	where := []string{}
	args := []interface{}{}
	argIdx := 1

	if params.UserID != nil {
		where = append(where, fmt.Sprintf("t.user_id = %s", db.Placeholder(argIdx)))
		args = append(args, *params.UserID)
		argIdx++
	}

	if params.InviterID != nil {
		where = append(where, fmt.Sprintf("u.inviter_id = %s", db.Placeholder(argIdx)))
		args = append(args, *params.InviterID)
		argIdx++
	}

	if uname := strings.TrimSpace(params.Username); uname != "" {
		where = append(where, fmt.Sprintf("u.username LIKE %s", db.Placeholder(argIdx)))
		args = append(args, "%"+uname+"%")
		argIdx++
	}

	if params.Status != "" {
		switch params.Status {
		case "success", "failed", "pending", "expired", "unknown":
			where = append(where, fmt.Sprintf("(%s) = %s", topUpStatusBucketSQL("t.status"), db.Placeholder(argIdx)))
			args = append(args, params.Status)
			argIdx++
		}
	}

	if params.PaymentMethod != "" {
		where = append(where, fmt.Sprintf("t.payment_method = %s", db.Placeholder(argIdx)))
		args = append(args, params.PaymentMethod)
		argIdx++
	}

	if params.PaymentProvider != "" {
		if db.ColumnExists("top_ups", "payment_provider") {
			where = append(where, fmt.Sprintf("t.payment_provider = %s", db.Placeholder(argIdx)))
			args = append(args, params.PaymentProvider)
			argIdx++
		} else {
			where = append(where, "1=0")
		}
	}

	if tradeNo := strings.TrimSpace(params.TradeNo); tradeNo != "" {
		// 账单号智能匹配：粘贴完整交易号（无空格、无 LIKE 通配符）时走精确等值，
		// 命中唯一索引 top_ups_trade_no_key 做秒查；否则按片段 LIKE 模糊匹配。
		if isCompleteTradeNo(tradeNo) {
			where = append(where, fmt.Sprintf("t.trade_no = %s", db.Placeholder(argIdx)))
			args = append(args, tradeNo)
		} else {
			where = append(where, fmt.Sprintf("t.trade_no LIKE %s", db.Placeholder(argIdx)))
			args = append(args, "%"+tradeNo+"%")
		}
		argIdx++
	}

	if params.StartDate != "" {
		ts, err := util.ParseDateToTimestampPublic(params.StartDate, false)
		if err == nil {
			where = append(where, fmt.Sprintf("t.create_time >= %s", db.Placeholder(argIdx)))
			args = append(args, ts)
			argIdx++
		}
	}

	if params.EndDate != "" {
		ts, err := util.ParseDateToTimestampPublic(params.EndDate, true)
		if err == nil {
			where = append(where, fmt.Sprintf("t.create_time <= %s", db.Placeholder(argIdx)))
			args = append(args, ts)
			argIdx++
		}
	}

	whereSQL := "1=1"
	if len(where) > 0 {
		whereSQL = strings.Join(where, " AND ")
	}
	return whereSQL, args, argIdx
}

// ListTopUpRecords lists top-up records with pagination and filtering
func ListTopUpRecords(params ListTopUpParams) (*PaginatedTopUps, error) {
	if params.Page < 1 {
		params.Page = 1
	}
	if params.PageSize < 1 || params.PageSize > 100 {
		params.PageSize = 20
	}

	db := database.Get()

	whereSQL, args, argIdx := buildTopUpWhere(params)

	// Count
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM top_ups t LEFT JOIN users u ON t.user_id = u.id WHERE %s", whereSQL)
	var total int64
	if err := db.DB.Get(&total, countSQL, args...); err != nil {
		return nil, fmt.Errorf("count query failed: %w", err)
	}

	totalPages := int((total + int64(params.PageSize) - 1) / int64(params.PageSize))
	if totalPages < 1 {
		totalPages = 1
	}
	offset := (params.Page - 1) * params.PageSize

	// Select with user join
	selectSQL := fmt.Sprintf(`SELECT %s FROM top_ups t LEFT JOIN users u ON t.user_id = u.id WHERE %s ORDER BY t.create_time DESC LIMIT %s OFFSET %s`,
		topUpSelectColumns(), whereSQL, db.Placeholder(argIdx), db.Placeholder(argIdx+1))
	args = append(args, params.PageSize, offset)

	rows, err := db.DB.Queryx(selectSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("select query failed: %w", err)
	}
	defer rows.Close()

	var items []TopUpRecord
	now := time.Now().Unix()
	for rows.Next() {
		var rec TopUpRecord
		if err := rows.StructScan(&rec); err != nil {
			continue
		}
		enrichTopUpRecord(&rec, now, defaultPendingAnomalyHours)
		items = append(items, rec)
	}

	if items == nil {
		items = []TopUpRecord{}
	}

	return &PaginatedTopUps{
		Items:      items,
		Total:      total,
		Page:       params.Page,
		PageSize:   params.PageSize,
		TotalPages: totalPages,
	}, nil
}

// TopUpExportSnapshot is the immutable database view used by one export. The
// count and stream share the same read transaction, while MaxID is also pinned
// into the SELECT as an explicit monotonic boundary.
type TopUpExportSnapshot struct {
	Total int64
	MaxID int64
}

// TopUpExportResult reports what was actually accepted by the CSV writer. A
// truncated result must never be presented or audited as a complete export.
type TopUpExportResult struct {
	RowsWritten int64
	Truncated   bool
}

// TopUpExportPlan keeps the count and stream on one repeatable-read snapshot.
// Call Close on every path, including size-limit and audit-precondition exits.
type TopUpExportPlan struct {
	Snapshot      TopUpExportSnapshot
	tx            *sqlx.Tx
	whereSQL      string
	whereArgs     []interface{}
	selectColumns string
	maxIDArg      string
}

// PrepareTopUpExport establishes the fixed export snapshot before response
// headers are sent, allowing the handler to reject oversized requests without
// racing a later streaming query.
func PrepareTopUpExport(ctx context.Context, params ListTopUpParams) (*TopUpExportPlan, error) {
	db := database.Get()
	whereSQL, args, nextArg := buildTopUpWhere(params)
	selectColumns := topUpSelectColumns()

	options := &sql.TxOptions{ReadOnly: true}
	// Production uses MySQL or PostgreSQL. SQLite-backed unit tests leave Config
	// nil and use the driver's default serializable read transaction.
	if db.Config != nil {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := db.DB.BeginTxx(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("begin export snapshot: %w", err)
	}

	plan := &TopUpExportPlan{
		tx:            tx,
		whereSQL:      whereSQL,
		whereArgs:     append([]interface{}(nil), args...),
		selectColumns: selectColumns,
		maxIDArg:      db.Placeholder(nextArg),
	}
	countSQL := fmt.Sprintf("SELECT COUNT(*), COALESCE(MAX(t.id), 0) FROM top_ups t LEFT JOIN users u ON t.user_id = u.id WHERE %s", whereSQL)
	if err := tx.QueryRowxContext(ctx, countSQL, args...).Scan(&plan.Snapshot.Total, &plan.Snapshot.MaxID); err != nil {
		_ = plan.Close()
		return nil, fmt.Errorf("count export snapshot: %w", err)
	}
	return plan, nil
}

// Close releases the read snapshot. The transaction is read-only, so rollback
// is the correct cheap close operation even after a successful stream.
func (p *TopUpExportPlan) Close() error {
	if p == nil || p.tx == nil {
		return nil
	}
	tx := p.tx
	p.tx = nil
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}

// CountTopUps returns a count from a fixed snapshot. Export handlers should use
// PrepareTopUpExport directly so the same snapshot remains open for streaming.
func CountTopUps(ctx context.Context, params ListTopUpParams) (int64, error) {
	plan, err := PrepareTopUpExport(ctx, params)
	if err != nil {
		return 0, err
	}
	defer plan.Close()
	return plan.Snapshot.Total, nil
}

// ErrExportTooLarge is returned when an export request exceeds the row cap.
var ErrExportTooLarge = errors.New("export exceeds row limit")

// TopUpExportLimit caps how many rows a single CSV export may contain.
// Streaming the table is fine, but the user-side cost (download size, Excel
// load time) makes a hard ceiling kinder than letting them request millions.
// Declared as var (not const) so tests can shrink it temporarily and verify
// the streaming break — production code should treat it as immutable.
var TopUpExportLimit int64 = 100000

// ExportTopUpsToCSV streams top-up records as CSV to the writer. The caller is
// responsible for setting response headers and (recommended) running CountTopUps
// first to short-circuit oversized exports — this function only flips on the
// limit if the count exceeds it mid-stream.
func ExportTopUpsToCSV(ctx context.Context, w io.Writer, params ListTopUpParams) (TopUpExportResult, error) {
	plan, err := PrepareTopUpExport(ctx, params)
	if err != nil {
		return TopUpExportResult{Truncated: true}, err
	}
	defer plan.Close()
	return plan.WriteCSV(ctx, w)
}

// WriteCSV streams the prepared snapshot and returns the actual written-row
// count. Scan, iterator, context, and writer errors fail immediately.
func (p *TopUpExportPlan) WriteCSV(ctx context.Context, w io.Writer) (TopUpExportResult, error) {
	result := TopUpExportResult{}
	if p == nil || p.tx == nil {
		return TopUpExportResult{Truncated: true}, errors.New("top-up export plan is closed")
	}

	// UTF-8 BOM so Excel (especially zh-CN locale) auto-detects encoding.
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return TopUpExportResult{Truncated: true}, err
	}

	csvW := csv.NewWriter(w)

	header := []string{
		"ID", "用户ID", "用户名", "额度(USD)", "金额(CNY)",
		"交易号", "支付方式", "支付渠道", "状态", "归一状态", "完成耗时(秒)", "异常标记", "创建时间", "完成时间",
	}
	if err := csvW.Write(header); err != nil {
		return TopUpExportResult{Truncated: true}, err
	}

	boundedWhereSQL := fmt.Sprintf("(%s) AND t.id <= %s", p.whereSQL, p.maxIDArg)
	args := append(append([]interface{}(nil), p.whereArgs...), p.Snapshot.MaxID)
	selectSQL := fmt.Sprintf(`SELECT %s FROM top_ups t LEFT JOIN users u ON t.user_id = u.id WHERE %s ORDER BY t.create_time DESC, t.id DESC`, p.selectColumns, boundedWhereSQL)

	rows, err := p.tx.QueryxContext(ctx, selectSQL, args...)
	if err != nil {
		return TopUpExportResult{Truncated: true}, fmt.Errorf("export query failed: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	for rows.Next() {
		if result.RowsWritten >= TopUpExportLimit {
			result.Truncated = true
			break
		}
		// Surface ctx cancellation (timeout / client disconnect) without finishing the loop.
		if err := ctx.Err(); err != nil {
			result.Truncated = true
			return result, err
		}

		var rec TopUpRecord
		if err := rows.StructScan(&rec); err != nil {
			result.Truncated = true
			return result, fmt.Errorf("scan export row: %w", err)
		}
		enrichTopUpRecord(&rec, now, defaultPendingAnomalyHours)

		username := ""
		if rec.Username != nil {
			username = *rec.Username
		}
		createTimeStr := ""
		if rec.CreateTime > 0 {
			createTimeStr = time.Unix(rec.CreateTime, 0).Format(time.RFC3339)
		}
		completeTimeStr := ""
		if rec.CompleteTime > 0 {
			completeTimeStr = time.Unix(rec.CompleteTime, 0).Format(time.RFC3339)
		}

		if err := csvW.Write([]string{
			strconv.FormatInt(rec.ID, 10),
			strconv.FormatInt(rec.UserID, 10),
			spreadsheetSafeCSVCell(username),
			strconv.FormatInt(rec.Amount, 10),
			strconv.FormatFloat(rec.Money, 'f', 2, 64),
			spreadsheetSafeCSVCell(rec.TradeNo),
			spreadsheetSafeCSVCell(rec.PaymentMethod),
			spreadsheetSafeCSVCell(rec.PaymentProvider),
			spreadsheetSafeCSVCell(rec.Status),
			spreadsheetSafeCSVCell(rec.StatusBucket),
			strconv.FormatInt(rec.CompletionSeconds, 10),
			spreadsheetSafeCSVCell(strings.Join(rec.AnomalyReasons, "; ")),
			createTimeStr,
			completeTimeStr,
		}); err != nil {
			result.Truncated = true
			return result, err
		}

		result.RowsWritten++
		if result.RowsWritten >= TopUpExportLimit {
			// 写满上限就停手，不再吐第 100001 行 —— handler 的 CountTopUps 预检通常已经
			// 把超限请求挡在 400 上，这里只是兜底 race（count 之后又有新插入）。
			break
		}
		// Periodic flush so the browser begins receiving bytes promptly.
		if result.RowsWritten%500 == 0 {
			csvW.Flush()
			if err := csvW.Error(); err != nil {
				result.Truncated = true
				return result, err
			}
		}
	}

	if err := rows.Err(); err != nil {
		result.Truncated = true
		return result, fmt.Errorf("iterate export rows: %w", err)
	}
	csvW.Flush()
	if err := csvW.Error(); err != nil {
		result.Truncated = true
		return result, err
	}
	result.Truncated = result.Truncated || result.RowsWritten != p.Snapshot.Total
	return result, nil
}

// GetTopUpStatistics returns aggregate top-up statistics
func GetTopUpStatistics(startDate, endDate string) (*TopUpStatistics, error) {
	db := database.Get()

	where := []string{}
	args := []interface{}{}
	argIdx := 1

	if startDate != "" {
		ts, err := util.ParseDateToTimestampPublic(startDate, false)
		if err == nil {
			where = append(where, fmt.Sprintf("create_time >= %s", db.Placeholder(argIdx)))
			args = append(args, ts)
			argIdx++
		}
	}
	if endDate != "" {
		ts, err := util.ParseDateToTimestampPublic(endDate, true)
		if err == nil {
			where = append(where, fmt.Sprintf("create_time <= %s", db.Placeholder(argIdx)))
			args = append(args, ts)
			argIdx++
		}
	}

	whereSQL := "1=1"
	if len(where) > 0 {
		whereSQL = strings.Join(where, " AND ")
	}

	bucketSQL := topUpStatusBucketSQL("status")
	sql := fmt.Sprintf(`SELECT
		COUNT(*) as total_count,
		COALESCE(SUM(amount), 0) as total_amount,
		COALESCE(SUM(money), 0) as total_money,
		COALESCE(SUM(CASE WHEN (%s) = 'success' THEN 1 ELSE 0 END), 0) as success_count,
		COALESCE(SUM(CASE WHEN (%s) = 'success' THEN amount ELSE 0 END), 0) as success_amount,
		COALESCE(SUM(CASE WHEN (%s) = 'success' THEN money ELSE 0 END), 0) as success_money,
		COALESCE(SUM(CASE WHEN (%s) = 'pending' THEN 1 ELSE 0 END), 0) as pending_count,
		COALESCE(SUM(CASE WHEN (%s) = 'pending' THEN amount ELSE 0 END), 0) as pending_amount,
		COALESCE(SUM(CASE WHEN (%s) = 'pending' THEN money ELSE 0 END), 0) as pending_money,
		COALESCE(SUM(CASE WHEN (%s) = 'failed' THEN 1 ELSE 0 END), 0) as failed_count,
		COALESCE(SUM(CASE WHEN (%s) = 'failed' THEN amount ELSE 0 END), 0) as failed_amount,
		COALESCE(SUM(CASE WHEN (%s) = 'failed' THEN money ELSE 0 END), 0) as failed_money,
		COALESCE(SUM(CASE WHEN (%s) = 'expired' THEN 1 ELSE 0 END), 0) as expired_count,
		COALESCE(SUM(CASE WHEN (%s) = 'expired' THEN amount ELSE 0 END), 0) as expired_amount,
		COALESCE(SUM(CASE WHEN (%s) = 'expired' THEN money ELSE 0 END), 0) as expired_money,
		COALESCE(SUM(CASE WHEN (%s) = 'unknown' THEN 1 ELSE 0 END), 0) as unknown_count,
		COALESCE(SUM(CASE WHEN (%s) = 'unknown' THEN amount ELSE 0 END), 0) as unknown_amount,
		COALESCE(SUM(CASE WHEN (%s) = 'unknown' THEN money ELSE 0 END), 0) as unknown_money
		FROM top_ups WHERE %s`,
		bucketSQL, bucketSQL, bucketSQL,
		bucketSQL, bucketSQL, bucketSQL,
		bucketSQL, bucketSQL, bucketSQL,
		bucketSQL, bucketSQL, bucketSQL,
		bucketSQL, bucketSQL, bucketSQL,
		whereSQL)

	type rawStats struct {
		TotalCount    int64   `db:"total_count"`
		TotalAmount   int64   `db:"total_amount"`
		TotalMoney    float64 `db:"total_money"`
		SuccessCount  int64   `db:"success_count"`
		SuccessAmount int64   `db:"success_amount"`
		SuccessMoney  float64 `db:"success_money"`
		PendingCount  int64   `db:"pending_count"`
		PendingAmount int64   `db:"pending_amount"`
		PendingMoney  float64 `db:"pending_money"`
		FailedCount   int64   `db:"failed_count"`
		FailedAmount  int64   `db:"failed_amount"`
		FailedMoney   float64 `db:"failed_money"`
		ExpiredCount  int64   `db:"expired_count"`
		ExpiredAmount int64   `db:"expired_amount"`
		ExpiredMoney  float64 `db:"expired_money"`
		UnknownCount  int64   `db:"unknown_count"`
		UnknownAmount int64   `db:"unknown_amount"`
		UnknownMoney  float64 `db:"unknown_money"`
	}

	var raw rawStats
	if err := db.DB.Get(&raw, sql, args...); err != nil {
		return nil, fmt.Errorf("statistics query failed: %w", err)
	}

	return &TopUpStatistics{
		TotalCount:    raw.TotalCount,
		TotalAmount:   raw.TotalAmount,
		TotalMoney:    raw.TotalMoney,
		SuccessCount:  raw.SuccessCount,
		SuccessAmount: raw.SuccessAmount,
		SuccessMoney:  raw.SuccessMoney,
		PendingCount:  raw.PendingCount,
		PendingAmount: raw.PendingAmount,
		PendingMoney:  raw.PendingMoney,
		FailedCount:   raw.FailedCount,
		FailedAmount:  raw.FailedAmount,
		FailedMoney:   raw.FailedMoney,
		ExpiredCount:  raw.ExpiredCount,
		ExpiredAmount: raw.ExpiredAmount,
		ExpiredMoney:  raw.ExpiredMoney,
		UnknownCount:  raw.UnknownCount,
		UnknownAmount: raw.UnknownAmount,
		UnknownMoney:  raw.UnknownMoney,
	}, nil
}

// GetPaymentMethods returns distinct payment methods
func GetPaymentMethods() ([]string, error) {
	db := database.Get()
	var methods []string
	err := db.DB.Select(&methods, "SELECT DISTINCT payment_method FROM top_ups WHERE payment_method IS NOT NULL AND payment_method != '' ORDER BY payment_method")
	if err != nil {
		return nil, err
	}
	if methods == nil {
		methods = []string{}
	}
	return methods, nil
}

// GetPaymentProviders returns distinct payment providers.
func GetPaymentProviders() ([]string, error) {
	db := database.Get()
	if !db.ColumnExists("top_ups", "payment_provider") {
		return []string{}, nil
	}
	var providers []string
	err := db.DB.Select(&providers, "SELECT DISTINCT payment_provider FROM top_ups WHERE payment_provider IS NOT NULL AND payment_provider != '' ORDER BY payment_provider")
	if err != nil {
		return nil, err
	}
	if providers == nil {
		providers = []string{}
	}
	return providers, nil
}

// GetTopUpByID returns a single top-up record
func GetTopUpByID(id int64) (*TopUpRecord, error) {
	db := database.Get()
	sql := fmt.Sprintf(`SELECT %s FROM top_ups t LEFT JOIN users u ON t.user_id = u.id WHERE t.id = %s`, topUpSelectColumns(), db.Placeholder(1))

	var rec TopUpRecord
	if err := db.DB.Get(&rec, sql, id); err != nil {
		return nil, err
	}
	enrichTopUpRecord(&rec, time.Now().Unix(), defaultPendingAnomalyHours)
	return &rec, nil
}
