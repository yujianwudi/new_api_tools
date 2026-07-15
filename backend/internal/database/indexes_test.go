package database

import (
	"reflect"
	"strings"
	"testing"
)

func TestPostgresIndexExistsLookupUsesResolvedTableOID(t *testing.T) {
	query, args := indexExistsLookup(true, "idx_logs_created_type_user", "logs")
	lowerQuery := strings.ToLower(query)

	for _, required := range []string{
		"pg_catalog.pg_index",
		"pg_catalog.pg_class",
		"index_meta.indrelid = to_regclass($1)",
		"index_class.relname = $2",
	} {
		if !strings.Contains(lowerQuery, required) {
			t.Fatalf("PostgreSQL index lookup is missing %q: %s", required, query)
		}
	}
	if strings.Contains(lowerQuery, "current_schema") || strings.Contains(lowerQuery, "pg_indexes") {
		t.Fatalf("PostgreSQL index lookup still assumes the first search_path schema: %s", query)
	}
	if want := []interface{}{"logs", "idx_logs_created_type_user"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("PostgreSQL index lookup args = %#v, want %#v", args, want)
	}
}

func TestMySQLIndexExistsLookupKeepsTableAndIndexOrder(t *testing.T) {
	query, args := indexExistsLookup(false, "idx_users_group", "users")
	if !strings.Contains(query, "information_schema.statistics") {
		t.Fatalf("unexpected MySQL index lookup: %s", query)
	}
	if want := []interface{}{"users", "idx_users_group"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("MySQL index lookup args = %#v, want %#v", args, want)
	}
}
