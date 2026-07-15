package config

import (
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestNormalizeMySQLURLDSNPreservesCredentialsAndOptions(t *testing.T) {
	raw := "mysql://user:p%40ss@[2001:db8::1]:3307/prod%2Ddb?parseTime=true&tls=preferred&timeout=5s&loc=Asia%2FShanghai"
	normalized := normalizeMySQLURLDSN(raw)
	parsed, err := mysqlDriver.ParseDSN(normalized)
	if err != nil {
		t.Fatalf("normalized DSN is not driver-compatible: %v (%s)", err, normalized)
	}
	if parsed.User != "user" || parsed.Passwd != "p@ss" {
		t.Fatalf("credentials changed: user=%q password=%q", parsed.User, parsed.Passwd)
	}
	if parsed.Net != "tcp" || parsed.Addr != "[2001:db8::1]:3307" || parsed.DBName != "prod-db" {
		t.Fatalf("address or database changed: net=%q addr=%q db=%q", parsed.Net, parsed.Addr, parsed.DBName)
	}
	if !parsed.ParseTime || parsed.TLSConfig != "preferred" || parsed.Timeout != 5*time.Second {
		t.Fatalf("query options changed: parseTime=%t tls=%q timeout=%s", parsed.ParseTime, parsed.TLSConfig, parsed.Timeout)
	}
	if parsed.Loc == nil || parsed.Loc.String() != "Asia/Shanghai" {
		t.Fatalf("location option changed: %v", parsed.Loc)
	}
}
