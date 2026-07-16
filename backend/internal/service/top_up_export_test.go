package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
)

var errCSVFlush = errors.New("csv flush failed")

type failCSVFlushWriter struct {
	writes int
}

func (w *failCSVFlushWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errCSVFlush
	}
	return len(p), nil
}

// seedTopUps creates the top_ups table on the in-memory SQLite and inserts n rows.
// SQLite 接受 ? 占位符并兼容 LOWER/COALESCE，足够覆盖 ExportTopUpsToCSV 的查询面。
func seedTopUps(t *testing.T, n int) {
	t.Helper()
	db := installSQLiteForTests(t)

	schema := `
	CREATE TABLE top_ups (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		amount INTEGER NOT NULL DEFAULT 0,
		money REAL NOT NULL DEFAULT 0,
		trade_no TEXT,
		payment_method TEXT,
		payment_provider TEXT,
		create_time INTEGER NOT NULL DEFAULT 0,
		complete_time INTEGER NOT NULL DEFAULT 0,
		status TEXT
	);
	CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 1; i <= n; i++ {
		_, err := tx.Exec(
			`INSERT INTO top_ups (id, user_id, amount, money, trade_no, payment_method, payment_provider, create_time, complete_time, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i, i%5+1, 100, 1.5, fmt.Sprintf("T%06d", i), "alipay", "epay", 1700000000+int64(i), 1700000010+int64(i), "success",
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func parseCSVRows(t *testing.T, buf []byte) [][]string {
	t.Helper()
	// 跳过 UTF-8 BOM
	if len(buf) >= 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		buf = buf[3:]
	}
	r := csv.NewReader(strings.NewReader(string(buf)))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return rows
}

func countCSVRows(t *testing.T, buf []byte) int {
	t.Helper()
	return len(parseCSVRows(t, buf))
}

func TestSpreadsheetSafeCSVCell(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "equals", value: "=HYPERLINK(\"https://example.invalid\")", want: "'=HYPERLINK(\"https://example.invalid\")"},
		{name: "plus", value: "+cmd|' /C calc'!A0", want: "'+cmd|' /C calc'!A0"},
		{name: "minus", value: "-2+3", want: "'-2+3"},
		{name: "at", value: "@SUM(1,2)", want: "'@SUM(1,2)"},
		{name: "leading spaces", value: "  =1+1", want: "'  =1+1"},
		{name: "leading tab", value: "\tplain", want: "'\tplain"},
		{name: "leading carriage return", value: "\rplain", want: "'\rplain"},
		{name: "leading newline before formula", value: "\n=1+1", want: "'\n=1+1"},
		{name: "ordinary text", value: "alice", want: "alice"},
		{name: "ordinary leading spaces", value: "  alice", want: "  alice"},
		{name: "existing apostrophe", value: "'=1+1", want: "'=1+1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spreadsheetSafeCSVCell(tt.value); got != tt.want {
				t.Fatalf("spreadsheetSafeCSVCell(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestExportTopUpsToCSV_NeutralizesSpreadsheetFormulas(t *testing.T) {
	db := installSQLiteForTests(t)
	schema := `
	CREATE TABLE top_ups (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		amount INTEGER NOT NULL DEFAULT 0,
		money REAL NOT NULL DEFAULT 0,
		trade_no TEXT,
		payment_method TEXT,
		payment_provider TEXT,
		create_time INTEGER NOT NULL DEFAULT 0,
		complete_time INTEGER NOT NULL DEFAULT 0,
		status TEXT
	);
	CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username) VALUES (1, '  =1+1')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO top_ups (id, user_id, amount, money, trade_no, payment_method, payment_provider, create_time, complete_time, status)
		 VALUES (1, 1, 100, 1.5, '+trade', '-method', '@provider', 1700000000, 1700000010, ?)`,
		"\t=status",
	); err != nil {
		t.Fatalf("insert top-up: %v", err)
	}

	var buf bytes.Buffer
	if _, err := ExportTopUpsToCSV(context.Background(), &buf, ListTopUpParams{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	rows := parseCSVRows(t, buf.Bytes())
	if len(rows) != 2 {
		t.Fatalf("expected header plus one row, got %d rows", len(rows))
	}

	row := rows[1]
	wants := map[int]string{
		2: "'  =1+1",
		5: "'+trade",
		6: "'-method",
		8: "'\t=status",
	}
	for column, want := range wants {
		if got := row[column]; got != want {
			t.Errorf("column %d = %q, want %q", column, got, want)
		}
	}
}

func TestExportTopUpsToCSV_BOMAndHeader(t *testing.T) {
	seedTopUps(t, 3)

	var buf bytes.Buffer
	result, err := ExportTopUpsToCSV(context.Background(), &buf, ListTopUpParams{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.RowsWritten != 3 || result.Truncated {
		t.Fatalf("export result = %+v, want 3 complete rows", result)
	}
	out := buf.Bytes()

	if len(out) < 3 || out[0] != 0xEF || out[1] != 0xBB || out[2] != 0xBF {
		t.Errorf("missing UTF-8 BOM at start of CSV")
	}
	rows := countCSVRows(t, out)
	// 1 header + 3 data
	if rows != 4 {
		t.Errorf("expected 4 csv rows (1 header + 3 data), got %d", rows)
	}
	if !bytes.Contains(out, []byte("ID")) {
		t.Errorf("CSV should contain header column 'ID'")
	}
}

func TestExportTopUpsToCSV_ReturnsFinalFlushError(t *testing.T) {
	seedTopUps(t, 1)

	writer := &failCSVFlushWriter{}
	result, err := ExportTopUpsToCSV(context.Background(), writer, ListTopUpParams{})
	if !errors.Is(err, errCSVFlush) {
		t.Fatalf("export error = %v, want %v", err, errCSVFlush)
	}
	if !result.Truncated {
		t.Fatalf("flush failure result = %+v, want truncated", result)
	}
}

// TestExportTopUpsToCSV_HardLimitBreaks 验证当 SELECT 实际结果超过 TopUpExportLimit 时
// 流式写入精确停在上限行，不会写出第 limit+1 行。模拟 count 与 select 之间有新行插入的 race。
// 临时把全局上限调小，运行结束恢复。
func TestExportTopUpsToCSV_HardLimitBreaks(t *testing.T) {
	original := TopUpExportLimit
	TopUpExportLimit = 10
	t.Cleanup(func() { TopUpExportLimit = original })

	// 模拟 race：插入 limit + 5 行（count 时只看到 10 行，select 时已涌入 5 行）。
	seedTopUps(t, 15)

	var buf bytes.Buffer
	result, err := ExportTopUpsToCSV(context.Background(), &buf, ListTopUpParams{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.RowsWritten != TopUpExportLimit || !result.Truncated {
		t.Fatalf("export result = %+v, want %d rows and truncated", result, TopUpExportLimit)
	}
	rows := countCSVRows(t, buf.Bytes())
	// header(1) + 至多 limit(10) 数据行
	wantMax := 1 + int(TopUpExportLimit)
	if rows > wantMax {
		t.Errorf("rows=%d exceeded header(1)+limit(%d), break didn't fire",
			rows, TopUpExportLimit)
	}
	// 也要确保确实写到了上限（否则不算覆盖 break 路径）。
	if rows < wantMax {
		t.Errorf("rows=%d less than expected %d, break may have fired too early",
			rows, wantMax)
	}
}

// TestExportTopUpsToCSV_ContextCancel 验证 ctx 取消后立即停止流，不再继续写。
func TestExportTopUpsToCSV_ContextCancel(t *testing.T) {
	seedTopUps(t, 200)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 提前取消

	var buf bytes.Buffer
	result, err := ExportTopUpsToCSV(ctx, &buf, ListTopUpParams{})
	// 取消可能在 query 阶段（返回 err）或 next 阶段（返回 ctx.Err()），两种都接受
	if err == nil {
		// 也允许 query 已经完成但 rows.Next 检查 ctx 时返回。检查写入量很小。
		rows := countCSVRows(t, buf.Bytes())
		if rows > 1 { // header 之外不应有数据
			t.Errorf("expected ctx cancel to abort early, but got %d rows", rows)
		}
		return
	}
	if ctx.Err() == nil {
		t.Errorf("expected ctx error, got: %v", err)
	}
	if !result.Truncated {
		t.Errorf("cancelled export result = %+v, want truncated", result)
	}
}

func TestExportTopUpsToCSV_StatusFilter(t *testing.T) {
	db := installSQLiteForTests(t)
	schema := `
	CREATE TABLE top_ups (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		amount INTEGER NOT NULL DEFAULT 0,
		money REAL NOT NULL DEFAULT 0,
		trade_no TEXT,
		payment_method TEXT,
		payment_provider TEXT,
		create_time INTEGER NOT NULL DEFAULT 0,
		complete_time INTEGER NOT NULL DEFAULT 0,
		status TEXT
	);
	CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	seedRows := []struct {
		id     int
		status interface{} // 用 interface 允许塞 nil
	}{
		{1, "success"},
		{2, "completed"},
		{3, "failed"},
		{4, "pending"},
		{5, nil}, // NULL
		{6, "1"},
	}
	for _, r := range seedRows {
		_, err := db.Exec(
			`INSERT INTO top_ups (id, user_id, amount, money, create_time, status) VALUES (?, 1, 0, 0, ?, ?)`,
			r.id, 1700000000+r.id, r.status,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// pending 必须捞到 id=4 (pending) 和 id=5 (NULL)
	var buf bytes.Buffer
	if _, err := ExportTopUpsToCSV(context.Background(), &buf, ListTopUpParams{Status: "pending"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	out := string(buf.Bytes())
	if !strings.Contains(out, ",4,") && !strings.Contains(out, "\n4,") {
		t.Errorf("pending export should include id=4, got:\n%s", out)
	}
	if !strings.Contains(out, ",5,") && !strings.Contains(out, "\n5,") {
		t.Errorf("pending export should include id=5 (NULL status), got:\n%s", out)
	}
	if strings.Contains(out, ",1,success") || strings.Contains(out, ",3,failed") {
		t.Errorf("pending export must NOT include success/failed rows, got:\n%s", out)
	}
}

func TestPreparedTopUpExportKeepsCountAndStreamOnOneSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "top-ups.db")
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA busy_timeout = 1000;`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		database.SetForTesting(nil)
		_ = db.Close()
	})

	db.MustExec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY, user_id INTEGER, amount INTEGER, money REAL,
			trade_no TEXT, payment_method TEXT, payment_provider TEXT,
			create_time INTEGER, complete_time INTEGER, status TEXT
		);
		INSERT INTO users(id, username) VALUES (1, 'alice');
		INSERT INTO top_ups(id, user_id, amount, money, trade_no, create_time, status)
		VALUES (1, 1, 100, 1.0, 'before-snapshot', 100, 'success');
	`)

	plan, err := PrepareTopUpExport(context.Background(), ListTopUpParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if plan.Snapshot.Total != 1 || plan.Snapshot.MaxID != 1 {
		t.Fatalf("snapshot = %+v, want total=1 max_id=1", plan.Snapshot)
	}
	if _, err := db.Exec(`INSERT INTO top_ups(id, user_id, amount, money, trade_no, create_time, status)
		VALUES (2, 1, 200, 2.0, 'after-snapshot', 200, 'success')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE top_ups
		SET amount = 999, trade_no = 'updated-after-snapshot'
		WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	result, err := plan.WriteCSV(context.Background(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsWritten != 1 || result.Truncated {
		t.Fatalf("result = %+v, want one complete snapshot row", result)
	}
	if strings.Contains(buf.String(), ",after-snapshot,") {
		t.Fatalf("row inserted after snapshot leaked into export: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "before-snapshot") || strings.Contains(buf.String(), "updated-after-snapshot") {
		t.Fatalf("row updated after snapshot changed export contents: %s", buf.String())
	}
}

func TestExportTopUpsToCSVReturnsStructScanError(t *testing.T) {
	db := installSQLiteForTests(t)
	db.MustExec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT);
		CREATE TABLE top_ups (
			id INTEGER PRIMARY KEY, user_id INTEGER, amount TEXT, money REAL,
			trade_no TEXT, payment_method TEXT, payment_provider TEXT,
			create_time INTEGER, complete_time INTEGER, status TEXT
		);
		INSERT INTO top_ups(id, user_id, amount, money, create_time, status)
		VALUES (1, 1, 'not-an-integer', 1.0, 100, 'success');
	`)

	var buf bytes.Buffer
	result, err := ExportTopUpsToCSV(context.Background(), &buf, ListTopUpParams{})
	if err == nil || !strings.Contains(err.Error(), "scan export row") {
		t.Fatalf("error = %v, want immediate StructScan failure", err)
	}
	if result.RowsWritten != 0 || !result.Truncated {
		t.Fatalf("result = %+v, want zero rows and truncated", result)
	}
}
