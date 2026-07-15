package database

import "time"

const postgresIndexExistsQuery = `
	SELECT 1
	FROM pg_catalog.pg_index AS index_meta
	JOIN pg_catalog.pg_class AS index_class
		ON index_class.oid = index_meta.indexrelid
	WHERE index_meta.indrelid = to_regclass($1)
		AND index_class.relname = $2
	LIMIT 1`

const mysqlIndexExistsQuery = `
	SELECT 1
	FROM information_schema.statistics
	WHERE table_schema = DATABASE()
		AND table_name = ?
		AND index_name = ?
	LIMIT 1`

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
	query, args := indexExistsLookup(m.IsPG, indexName, tableName)
	row, err := m.QueryOneWithTimeout(5*time.Second, query, args...)
	return row != nil, err
}

func indexExistsLookup(isPG bool, indexName, tableName string) (string, []interface{}) {
	if isPG {
		// Resolve the table exactly as PostgreSQL resolves an unqualified query
		// target through search_path. current_schema() only returns the first
		// valid schema and can miss a table selected from a later schema.
		return postgresIndexExistsQuery, []interface{}{tableName, indexName}
	}
	return mysqlIndexExistsQuery, []interface{}{tableName, indexName}
}
