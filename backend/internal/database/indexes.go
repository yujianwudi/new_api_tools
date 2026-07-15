package database

import "time"

// IndexDef describes an index that operators may evaluate for the upstream
// NewAPI database. These definitions are diagnostic-only: this service never
// creates or drops upstream indexes.
type IndexDef struct {
	Name    string
	Table   string
	Columns []string
}

var RecommendedIndexes = []IndexDef{
	{Name: "idx_logs_created_type_user", Table: "logs", Columns: []string{"created_at", "type", "user_id"}},
	{Name: "idx_logs_type_created_user", Table: "logs", Columns: []string{"type", "created_at", "user_id"}},
	{Name: "idx_logs_type_created_token", Table: "logs", Columns: []string{"type", "created_at", "token_id"}},
	{Name: "idx_logs_type_created_model", Table: "logs", Columns: []string{"type", "created_at", "model_name"}},
	{Name: "idx_logs_user_type_created", Table: "logs", Columns: []string{"user_id", "type", "created_at"}},
	{Name: "idx_logs_user_created_ip", Table: "logs", Columns: []string{"user_id", "created_at", "ip"}},
	{Name: "idx_logs_created_token_ip", Table: "logs", Columns: []string{"created_at", "token_id", "ip"}},
	{Name: "idx_logs_created_ip_token", Table: "logs", Columns: []string{"created_at", "ip", "token_id"}},
	{Name: "idx_users_deleted_status", Table: "users", Columns: []string{"deleted_at", "status"}},
	{Name: "idx_tokens_user_deleted", Table: "tokens", Columns: []string{"user_id", "deleted_at"}},
	{Name: "idx_users_group", Table: "users", Columns: []string{"group"}},
}

// IndexExists checks metadata without mutating the connected database.
func (m *Manager) IndexExists(indexName, tableName string) (bool, error) {
	if m.IsPG {
		row, err := m.QueryOneWithTimeout(5*time.Second, `
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = current_schema()
				AND tablename = $1
				AND indexname = $2
			LIMIT 1`, tableName, indexName)
		return row != nil, err
	}

	row, err := m.QueryOneWithTimeout(5*time.Second, `
		SELECT 1
		FROM information_schema.statistics
		WHERE table_schema = DATABASE()
			AND table_name = ?
			AND index_name = ?
		LIMIT 1`, tableName, indexName)
	return row != nil, err
}
