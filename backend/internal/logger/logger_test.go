package logger

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestAPILogIncludesStructuredIP(t *testing.T) {
	var output bytes.Buffer
	testLogger := &AppLogger{zl: zerolog.New(&output)}
	testLogger.API("GET", "/api/control-plane/channel-quality", 200, time.Second, "203.0.113.7", "request-123")

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatalf("decode API log: %v (%s)", err, output.String())
	}
	if entry["ip"] != "203.0.113.7" {
		t.Fatalf("structured ip = %#v, want 203.0.113.7", entry["ip"])
	}
	if entry["request_id"] != "request-123" {
		t.Fatalf("structured request_id = %#v, want request-123", entry["request_id"])
	}
}
