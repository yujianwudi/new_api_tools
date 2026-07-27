package modelprobe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const (
	CapabilityChatCompletions = "chat_completions"
	CapabilityResponses       = "responses"
	CapabilityEmbeddings      = "embeddings"
	CapabilityRerank          = "rerank"
	CapabilityUnsupported     = "unsupported"

	maxProbeResponseBytes int64 = 64 * 1024
)

type Runner struct {
	baseURL         *url.URL
	apiKey          string
	client          *http.Client
	maxOutputTokens int
	capabilityMap   map[string]string
}

func NewRunner(baseURL, apiKey string, timeout time.Duration, maxOutputTokens int, capabilityMap map[string]string) (*Runner, error) {
	parsed, err := newapi.ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("MODEL_PROBE_API_KEY is required")
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if maxOutputTokens < 1 || maxOutputTokens > 32 {
		maxOutputTokens = 4
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	mapping := make(map[string]string, len(capabilityMap))
	for model, capability := range capabilityMap {
		mapping[strings.TrimSpace(model)] = normalizeCapability(capability)
	}
	return &Runner{
		baseURL: parsed,
		apiKey:  apiKey,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("model probe redirects are not allowed")
			},
		},
		maxOutputTokens: maxOutputTokens,
		capabilityMap:   mapping,
	}, nil
}

func (r *Runner) CapabilityFor(model string) string {
	model = strings.TrimSpace(model)
	if capability := normalizeCapability(r.capabilityMap[model]); capability != CapabilityUnsupported || r.capabilityMap[model] != "" {
		return capability
	}
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "embedding") || strings.Contains(lower, "embed-"):
		return CapabilityEmbeddings
	case strings.Contains(lower, "rerank") || strings.Contains(lower, "reranker"):
		return CapabilityRerank
	case containsAny(lower, "dall-e", "image", "flux", "stable-diffusion", "midjourney", "sdxl"):
		return CapabilityUnsupported
	case containsAny(lower, "whisper", "audio", "speech", "tts", "transcri"):
		return CapabilityUnsupported
	default:
		return CapabilityChatCompletions
	}
}

func (r *Runner) Probe(ctx context.Context, runID int64, model string) toolstore.ModelProbeAttemptInput {
	started := time.Now().UTC()
	capability := r.CapabilityFor(model)
	if capability == CapabilityUnsupported {
		return toolstore.ModelProbeAttemptInput{
			RunID: runID, ModelName: model, Capability: capability, Endpoint: "",
			Outcome: "skipped", ProtocolSuccess: false, TotalLatencyMS: 0,
			ErrorCode:    "unsupported_capability",
			ErrorMessage: "Model capability is intentionally not probed; configure an explicit supported adapter if safe",
			StartedAt:    started, FinishedAt: started,
		}
	}

	endpoint, payload := r.requestFor(model, capability)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return failedAttempt(runID, model, capability, endpoint, started, 0, "encode_request", "Probe request could not be encoded")
	}
	target := r.baseURL.ResolveReference(&url.URL{Path: endpoint})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(encoded))
	if err != nil {
		return failedAttempt(runID, model, capability, endpoint, started, 0, "build_request", "Probe request could not be constructed")
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "new-api-tools-model-probe/0.6")

	var firstResponseByte time.Time
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstResponseByte = time.Now().UTC() },
	}))
	resp, err := r.client.Do(req)
	if err != nil {
		code := "network_error"
		message := "Probe request failed before a valid response was received"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, message = "timeout", "Probe request exceeded its configured timeout"
		} else if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			code, message = "cancelled", "Probe request was cancelled"
		}
		return failedAttempt(runID, model, capability, endpoint, started, 0, code, message)
	}
	defer resp.Body.Close()
	headerLatency := elapsedMillis(started, firstResponseByte)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := readBounded(resp.Body, 8*1024)
		finished := time.Now().UTC()
		semantic := false
		return toolstore.ModelProbeAttemptInput{
			RunID: runID, ModelName: model, Capability: capability, Endpoint: endpoint,
			Outcome: "failure", ProtocolSuccess: false, SemanticSuccess: &semantic,
			HTTPStatus: resp.StatusCode, HeaderLatencyMS: headerLatency,
			TotalLatencyMS: durationMillis(started, finished), ErrorCode: "upstream_status",
			ErrorMessage: fmt.Sprintf("NewAPI returned HTTP %d", resp.StatusCode), ResponseSHA256: digest(data),
			StartedAt: started, FinishedAt: finished,
		}
	}

	parsed, raw, firstTokenAt, parseErr := parseProbeResponse(resp.Body, capability)
	finished := time.Now().UTC()
	protocolSuccess := parseErr == nil
	semanticSuccess := protocolSuccess && parsed
	outcome := "success"
	errorCode, errorMessage := "", ""
	if parseErr != nil {
		outcome, errorCode, errorMessage = "failure", "invalid_response", "Probe response did not match the expected bounded protocol"
		if errors.Is(parseErr, errProbeResponseTooLarge) {
			errorCode, errorMessage = "response_too_large", "Probe response exceeded the 64 KiB safety limit"
		}
	} else if !semanticSuccess {
		outcome, errorCode, errorMessage = "failure", "semantic_mismatch", "Probe response was valid but did not satisfy the health-check assertion"
	}
	var firstTokenLatency *int64
	if !firstTokenAt.IsZero() {
		firstTokenLatency = elapsedMillis(started, firstTokenAt)
	}
	return toolstore.ModelProbeAttemptInput{
		RunID: runID, ModelName: model, Capability: capability, Endpoint: endpoint,
		Outcome: outcome, ProtocolSuccess: protocolSuccess, SemanticSuccess: &semanticSuccess,
		HTTPStatus: resp.StatusCode, HeaderLatencyMS: headerLatency, FirstTokenLatencyMS: firstTokenLatency,
		TotalLatencyMS: durationMillis(started, finished), ErrorCode: errorCode, ErrorMessage: errorMessage,
		ResponseSHA256: digest(raw), StartedAt: started, FinishedAt: finished,
	}
}

func (r *Runner) requestFor(model, capability string) (string, any) {
	switch capability {
	case CapabilityResponses:
		return "/v1/responses", map[string]any{
			"model": model, "input": "Respond with exactly: ok", "max_output_tokens": r.maxOutputTokens,
		}
	case CapabilityEmbeddings:
		return "/v1/embeddings", map[string]any{"model": model, "input": "healthcheck"}
	case CapabilityRerank:
		return "/v1/rerank", map[string]any{
			"model": model, "query": "healthcheck", "documents": []string{"healthcheck", "unrelated"}, "top_n": 1,
		}
	default:
		return "/v1/chat/completions", map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "Respond with exactly: ok"}},
			"stream":   true, "temperature": 0, "max_tokens": r.maxOutputTokens,
		}
	}
}

var errProbeResponseTooLarge = errors.New("probe response too large")

func parseProbeResponse(body io.Reader, capability string) (bool, []byte, time.Time, error) {
	if capability == CapabilityChatCompletions {
		return parseChatResponse(body)
	}
	data, err := readBounded(body, maxProbeResponseBytes)
	first := time.Now().UTC()
	if err != nil {
		return false, data, first, err
	}
	switch capability {
	case CapabilityResponses:
		var response struct {
			OutputText string `json:"output_text"`
			Output     []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return false, data, first, err
		}
		text := response.OutputText
		if text == "" {
			for _, output := range response.Output {
				for _, content := range output.Content {
					text += content.Text
				}
			}
		}
		return isOK(text), data, first, nil
	case CapabilityEmbeddings:
		var response struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return false, data, first, err
		}
		return len(response.Data) > 0 && len(response.Data[0].Embedding) > 0, data, first, nil
	case CapabilityRerank:
		var response struct {
			Results []json.RawMessage `json:"results"`
			Data    []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return false, data, first, err
		}
		return len(response.Results) > 0 || len(response.Data) > 0, data, first, nil
	default:
		return false, data, first, errors.New("unsupported response adapter")
	}
}

func parseChatResponse(body io.Reader) (bool, []byte, time.Time, error) {
	limited := io.LimitReader(body, maxProbeResponseBytes+1)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), int(maxProbeResponseBytes)+1)
	var raw bytes.Buffer
	var content strings.Builder
	var firstToken time.Time
	sawEvent := false
	for scanner.Scan() {
		line := scanner.Bytes()
		raw.Write(line)
		raw.WriteByte('\n')
		if int64(raw.Len()) > maxProbeResponseBytes {
			return false, raw.Bytes(), firstToken, errProbeResponseTooLarge
		}
		trimmed := strings.TrimSpace(string(line))
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		sawEvent = true
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return false, raw.Bytes(), firstToken, err
		}
		for _, choice := range chunk.Choices {
			piece := choice.Delta.Content
			if piece == "" {
				piece = choice.Message.Content
			}
			if piece != "" {
				if firstToken.IsZero() {
					firstToken = time.Now().UTC()
				}
				content.WriteString(piece)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if int64(raw.Len()) > maxProbeResponseBytes {
			return false, raw.Bytes(), firstToken, errProbeResponseTooLarge
		}
		return false, raw.Bytes(), firstToken, err
	}
	if sawEvent {
		return isOK(content.String()), raw.Bytes(), firstToken, nil
	}

	data := raw.Bytes()
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false, data, firstToken, err
	}
	if firstToken.IsZero() {
		firstToken = time.Now().UTC()
	}
	if len(response.Choices) == 0 {
		return false, data, firstToken, nil
	}
	return isOK(response.Choices[0].Message.Content), data, firstToken, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > limit {
		return data[:limit], errProbeResponseTooLarge
	}
	return data, nil
}

func failedAttempt(runID int64, model, capability, endpoint string, started time.Time, status int, code, message string) toolstore.ModelProbeAttemptInput {
	finished := time.Now().UTC()
	semantic := false
	return toolstore.ModelProbeAttemptInput{
		RunID: runID, ModelName: model, Capability: capability, Endpoint: endpoint,
		Outcome: "failure", ProtocolSuccess: false, SemanticSuccess: &semantic, HTTPStatus: status,
		TotalLatencyMS: durationMillis(started, finished), ErrorCode: code, ErrorMessage: message,
		StartedAt: started, FinishedAt: finished,
	}
}

func normalizeCapability(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CapabilityChatCompletions, "chat", "chat/completions":
		return CapabilityChatCompletions
	case CapabilityResponses, "response":
		return CapabilityResponses
	case CapabilityEmbeddings, "embedding":
		return CapabilityEmbeddings
	case CapabilityRerank, "reranker":
		return CapabilityRerank
	default:
		return CapabilityUnsupported
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func isOK(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, "\"'`。.!！"))
	return strings.EqualFold(value, "ok")
}

func digest(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func elapsedMillis(start, finish time.Time) *int64 {
	if finish.IsZero() || finish.Before(start) {
		return nil
	}
	value := durationMillis(start, finish)
	return &value
}

func durationMillis(start, finish time.Time) int64 {
	if finish.Before(start) {
		return 0
	}
	return finish.Sub(start).Milliseconds()
}
