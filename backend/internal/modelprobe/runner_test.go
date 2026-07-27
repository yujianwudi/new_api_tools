package modelprobe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerUsesCapabilitySpecificAdapters(t *testing.T) {
	var chatCalls, embeddingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer probe-secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
			_, _ = fmt.Fprintln(w, "data: [DONE]")
		case "/v1/embeddings":
			embeddingCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner, err := NewRunner(server.URL, "probe-secret", 2*time.Second, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := runner.Probe(context.Background(), 1, "gpt-test")
	if chat.Outcome != "success" || chat.Capability != CapabilityChatCompletions || chat.FirstTokenLatencyMS == nil {
		t.Fatalf("chat probe = %+v", chat)
	}
	embedding := runner.Probe(context.Background(), 1, "text-embedding-test")
	if embedding.Outcome != "success" || embedding.Capability != CapabilityEmbeddings || embedding.Endpoint != "/v1/embeddings" {
		t.Fatalf("embedding probe = %+v", embedding)
	}
	image := runner.Probe(context.Background(), 1, "flux-image-test")
	if image.Outcome != "skipped" || image.Capability != CapabilityUnsupported || image.Endpoint != "" {
		t.Fatalf("unsupported probe = %+v", image)
	}
	if chatCalls.Load() != 1 || embeddingCalls.Load() != 1 {
		t.Fatalf("adapter calls = chat:%d embedding:%d", chatCalls.Load(), embeddingCalls.Load())
	}
}

func TestRunnerDoesNotPersistRawErrorBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, `{"error":"upstream-secret-marker"}`)
	}))
	defer server.Close()
	runner, err := NewRunner(server.URL, "probe-secret", time.Second, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt := runner.Probe(context.Background(), 1, "gpt-test")
	if attempt.Outcome != "failure" || attempt.ErrorCode != "upstream_status" || attempt.ResponseSHA256 == "" {
		t.Fatalf("failed probe = %+v", attempt)
	}
	if strings.Contains(attempt.ErrorMessage, "upstream-secret-marker") {
		t.Fatalf("raw response leaked into persisted error: %q", attempt.ErrorMessage)
	}
}
