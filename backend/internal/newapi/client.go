package newapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/new-api-tools/backend/internal/requestmeta"
)

const (
	defaultMaxResponseBytes  int64 = 2 << 20
	defaultHTTPTimeout             = 20 * time.Second
	MaxRedemptionCreateCount       = 100
)

var (
	ErrAdminCredentialsMissing = errors.New("NewAPI admin credentials are not configured")
	ErrUnsupportedCapability   = errors.New("NewAPI capability is not supported by the detected version")
	ErrAmbiguousRedemptionKeys = errors.New("NewAPI redemption response does not authoritatively contain the requested keys")
)

// APIError represents an authoritative rejection from NewAPI: either an
// explicit 4xx response or a valid API envelope with success=false. It
// intentionally keeps only the bounded upstream message rather than the raw
// response body. Errors that leave a mutation's commit state uncertain (for
// example 5xx or a malformed 2xx response) must not use this type.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e == nil {
		return "NewAPI request failed"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("NewAPI request failed with status %d: %s", e.StatusCode, e.Message)
	}
	return "NewAPI request failed: " + e.Message
}

type Client struct {
	baseURL      *url.URL
	accessToken  string
	adminUserID  int
	httpClient   *http.Client
	maxBodyBytes int64
}

type apiEnvelope struct {
	Success *bool           `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type Status struct {
	Version          string `json:"version"`
	StartTime        int64  `json:"start_time"`
	SystemName       string `json:"system_name"`
	Setup            bool   `json:"setup"`
	EnableDataExport bool   `json:"enable_data_export"`
}

type ManageUserRequest struct {
	ID     int    `json:"id"`
	Action string `json:"action"`
	Value  int    `json:"value,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type RedemptionCreateRequest struct {
	Name        string `json:"name"`
	Count       int    `json:"count"`
	Quota       int64  `json:"quota"`
	ExpiredTime int64  `json:"expired_time"`
}

func NewClient(baseURL, accessToken string, adminUserID int, customClient *http.Client) (*Client, error) {
	parsed, err := validateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if customClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		customClient = &http.Client{
			Transport: transport,
		}
	} else {
		copiedClient := *customClient
		customClient = &copiedClient
	}
	if customClient.Timeout <= 0 {
		customClient.Timeout = defaultHTTPTimeout
	}
	customClient.CheckRedirect = rejectRedirect
	return &Client{
		baseURL:      parsed,
		accessToken:  strings.TrimSpace(accessToken),
		adminUserID:  adminUserID,
		httpClient:   customClient,
		maxBodyBytes: defaultMaxResponseBytes,
	}, nil
}

func rejectRedirect(*http.Request, []*http.Request) error {
	return errors.New("NewAPI redirects are not allowed")
}

func validateBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("NEWAPI_BASEURL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid NEWAPI_BASEURL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("NEWAPI_BASEURL must be absolute and include a host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("NEWAPI_BASEURL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("NEWAPI_BASEURL must not contain credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("NEWAPI_BASEURL must not contain an application path")
	}
	if parsed.Scheme == "http" && !isPrivateHTTPHost(parsed.Hostname()) {
		return nil, errors.New("plain HTTP is only allowed for loopback, private IP, or single-label internal hosts")
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	return parsed, nil
}

func isPrivateHTTPHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	// Docker Compose and Kubernetes service names are normally single-label.
	return host != "" && !strings.Contains(host, ".")
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var status Status
	if err := c.doJSON(ctx, http.MethodGet, "/api/status", nil, false, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *Client) ManageUser(ctx context.Context, req ManageUserRequest) error {
	if req.ID <= 0 {
		return errors.New("user id must be positive")
	}
	switch req.Action {
	case "enable", "disable", "delete", "add_quota", "promote", "demote":
	default:
		return fmt.Errorf("unsupported user management action %q", req.Action)
	}
	return c.doJSON(ctx, http.MethodPost, "/api/user/manage", req, true, nil)
}

func (c *Client) HardDeleteUser(ctx context.Context, userID int, capabilities Capabilities) error {
	if userID <= 0 {
		return errors.New("user id must be positive")
	}
	if !capabilities.HardDeleteSafe {
		return fmt.Errorf("%w: hard delete is disabled for NewAPI %s", ErrUnsupportedCapability, capabilities.Version)
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/user/"+strconv.Itoa(userID), nil, true, nil)
}

// CreateRedemptions intentionally maps one control-plane operation to exactly
// one NewAPI request. v0.5 caps the request at NewAPI's 100-code boundary so a
// later batch can never fail after earlier non-idempotent batches succeeded.
func (c *Client) CreateRedemptions(ctx context.Context, req RedemptionCreateRequest) ([]string, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("redemption name is required")
	}
	if req.Count <= 0 || req.Count > MaxRedemptionCreateCount {
		return nil, fmt.Errorf("redemption count must be between 1 and %d", MaxRedemptionCreateCount)
	}
	if req.Quota <= 0 {
		return nil, errors.New("redemption quota must be positive")
	}

	var keys []string
	if err := c.doJSON(ctx, http.MethodPost, "/api/redemption", req, true, &keys); err != nil {
		return nil, err
	}
	if err := ValidateRedemptionKeys(keys, req.Count); err != nil {
		return nil, err
	}
	return keys, nil
}

// ValidateRedemptionKeys treats missing, truncated, duplicate or malformed key
// data as non-authoritative. NewAPI may already have committed this
// non-idempotent operation, so callers must reconcile rather than retry.
func ValidateRedemptionKeys(keys []string, expected int) error {
	if expected <= 0 || len(keys) != expected {
		return fmt.Errorf("%w: received %d keys for requested count %d", ErrAmbiguousRedemptionKeys, len(keys), expected)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key != strings.TrimSpace(key) || len(key) == 0 || len(key) > 512 {
			return fmt.Errorf("%w: response contains a malformed key", ErrAmbiguousRedemptionKeys)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: response contains duplicate keys", ErrAmbiguousRedemptionKeys)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (c *Client) DeleteRedemption(ctx context.Context, redemptionID int) error {
	if redemptionID <= 0 {
		return errors.New("redemption id must be positive")
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/redemption/"+strconv.Itoa(redemptionID), nil, true, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, admin bool, out any) error {
	if c == nil || c.baseURL == nil || c.httpClient == nil {
		return errors.New("NewAPI client is not initialized")
	}
	if admin && (c.accessToken == "" || c.adminUserID <= 0) {
		return ErrAdminCredentialsMissing
	}

	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode NewAPI request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("build NewAPI request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if requestID := requestmeta.RequestID(ctx); requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if admin {
		req.Header.Set("Authorization", c.accessToken)
		req.Header.Set("New-Api-User", strconv.Itoa(c.adminUserID))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call NewAPI: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read NewAPI response: %w", err)
	}
	if int64(len(data)) > c.maxBodyBytes {
		if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError {
			return newAPIRejection(resp.StatusCode, "")
		}
		return errors.New("NewAPI response exceeds configured limit")
	}

	var envelope apiEnvelope
	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError {
		// A 4xx response is an explicit rejection even when an intermediary or
		// older NewAPI version returns a non-envelope body.
		if len(data) > 0 {
			_ = json.Unmarshal(data, &envelope)
		}
		return newAPIRejection(resp.StatusCode, envelope.Message)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// A 5xx (or any other unexpected non-2xx response) can arrive after an
		// upstream mutation committed. Keep it outside APIError so callers mark
		// the operation uncertain and prohibit automatic retry.
		return fmt.Errorf("NewAPI returned non-authoritative status %d", resp.StatusCode)
	}
	if len(data) == 0 {
		return errors.New("NewAPI returned an empty success response")
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode NewAPI response envelope: %w", err)
	}
	if envelope.Success == nil {
		return errors.New("NewAPI success response is missing the success field")
	}
	message := strings.TrimSpace(envelope.Message)
	if len(message) > 1024 {
		message = message[:1024]
	}
	if !*envelope.Success {
		return newAPIRejection(resp.StatusCode, message)
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode NewAPI response data: %w", err)
		}
	}
	return nil
}

func newAPIRejection(statusCode int, message string) *APIError {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		message = message[:1024]
	}
	if message == "" {
		message = http.StatusText(statusCode)
		if message == "" || statusCode < http.StatusBadRequest {
			message = "operation rejected"
		}
	}
	return &APIError{StatusCode: statusCode, Message: message}
}
