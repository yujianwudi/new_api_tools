package newapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/new-api-tools/backend/internal/requestmeta"
)

func TestStatusAndAdminHeaders(t *testing.T) {
	var mu sync.Mutex
	var seenAuth, seenUser, seenRequestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			mu.Lock()
			seenRequestID = r.Header.Get("X-Request-ID")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"version": "v1.0.0-rc.21"}})
		case "/api/user/manage":
			mu.Lock()
			seenAuth = r.Header.Get("Authorization")
			seenUser = r.Header.Get("New-Api-User")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "access-token", 7, server.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := requestmeta.WithRequestID(context.Background(), "relay-request-1234")
	status, err := client.Status(ctx)
	if err != nil || status.Version != "v1.0.0-rc.21" {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	if err := client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"}); err != nil {
		t.Fatalf("ManageUser: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenAuth != "access-token" || seenUser != "7" {
		t.Fatalf("admin headers = %q/%q", seenAuth, seenUser)
	}
	if seenRequestID != "relay-request-1234" {
		t.Fatalf("request id header = %q", seenRequestID)
	}
}

func TestHTTP200SuccessFalseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "denied"})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "", 0, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background()); err == nil {
		t.Fatal("success:false was accepted")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("success:false error = %T %v, want authoritative APIError", err, err)
		}
	}
}

func TestHTTP502IsNotAuthoritativeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"success":false,"message":"gateway failure"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"})
	if err == nil {
		t.Fatal("502 response was accepted")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("502 error = %#v, must remain non-authoritative", apiErr)
	}
}

func TestHTTP400IsAuthoritativeAPIErrorWithInvalidBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("400 error = %T %v, want authoritative APIError", err, err)
	}
}

func TestHTTP200InvalidJSONIsNotAuthoritativeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"})
	if err == nil {
		t.Fatal("invalid JSON response was accepted")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("invalid 2xx JSON error = %#v, must remain non-authoritative", apiErr)
	}
}

func TestHTTP200MissingSuccessIsNotAuthoritativeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"ambiguous response"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"})
	if err == nil {
		t.Fatal("response without success field was accepted")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("missing success field error = %#v, must remain non-authoritative", apiErr)
	}
}

func TestResponseReadFailureIsNotAuthoritativeAPIError(t *testing.T) {
	customClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       failingReadCloser{},
		}, nil
	})}
	client, err := NewClient("http://new-api:3000", "token", 1, customClient)
	if err != nil {
		t.Fatal(err)
	}
	err = client.ManageUser(context.Background(), ManageUserRequest{ID: 42, Action: "disable"})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read failure error = %v, want unexpected EOF", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("read failure error = %#v, must remain non-authoritative", apiErr)
	}
}

func TestCreateRedemptionsUsesOneBoundedRequest(t *testing.T) {
	var counts []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("New-Api-User") == "" {
			t.Fatal("missing admin headers")
		}
		var request RedemptionCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, request.Count)
		keys := make([]string, request.Count)
		for i := range keys {
			keys[i] = "key-" + strconv.Itoa(len(counts)) + "-" + strconv.Itoa(i)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": keys})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := client.CreateRedemptions(context.Background(), RedemptionCreateRequest{Name: "batch", Count: 100, Quota: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 100 || len(counts) != 1 || counts[0] != 100 {
		t.Fatalf("keys=%d counts=%v", len(keys), counts)
	}
	if _, err := client.CreateRedemptions(context.Background(), RedemptionCreateRequest{Name: "too-large", Count: 101, Quota: 10}); err == nil {
		t.Fatal("count above 100 was accepted")
	}
	if len(counts) != 1 {
		t.Fatalf("oversized request reached NewAPI: counts=%v", counts)
	}
}

func TestCreateRedemptionsRejectsAmbiguousKeyResponses(t *testing.T) {
	tests := []struct {
		name        string
		includeData bool
		data        any
	}{
		{name: "missing data"},
		{name: "null data", includeData: true},
		{name: "empty data", includeData: true, data: []string{}},
		{name: "truncated data", includeData: true, data: []string{"key-valid-0001"}},
		{name: "empty key", includeData: true, data: []string{"key-valid-0001", ""}},
		{name: "duplicate key", includeData: true, data: []string{"key-valid-0001", "key-valid-0001"}},
		{name: "whitespace key", includeData: true, data: []string{"key-valid-0001", " key-valid-0002"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := map[string]any{"success": true}
				if test.includeData {
					response["data"] = test.data
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "token", 1, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateRedemptions(context.Background(), RedemptionCreateRequest{Name: "batch", Count: 2, Quota: 10})
			if !errors.Is(err, ErrAmbiguousRedemptionKeys) {
				t.Fatalf("error = %v, want ErrAmbiguousRedemptionKeys", err)
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				t.Fatalf("ambiguous key response became authoritative APIError: %#v", apiErr)
			}
		})
	}
}

func TestDetectCapabilitiesVersionGate(t *testing.T) {
	rc21 := DetectCapabilities("v1.0.0-rc.21")
	if !rc21.Known || rc21.HardDeleteSafe || !rc21.AdminUserManage {
		t.Fatalf("rc21 capabilities = %#v", rc21)
	}
	rc22 := DetectCapabilities("v1.0.0-rc.22")
	if !rc22.HardDeleteSafe {
		t.Fatalf("rc22 hard delete should be enabled: %#v", rc22)
	}
	unknown := DetectCapabilities("v2.0.0")
	if unknown.Known || !unknown.UnknownVersionReadOnly {
		t.Fatalf("unknown major must be read-only: %#v", unknown)
	}

	for _, version := range []string{
		"v1.0.0-beta.1",
		"v1.0.0-alpha",
		"v1.0.0-rc.22+build.1",
		"v1.0.0-rc.22-dirty",
		"v1.0.0-rc.22garbage",
		"v1.0.0+build.1",
		"v1.0.0garbage",
	} {
		t.Run(version, func(t *testing.T) {
			capabilities := DetectCapabilities(version)
			if capabilities.Known || !capabilities.UnknownVersionReadOnly || capabilities.HardDeleteSafe {
				t.Fatalf("unknown prerelease/suffix must be read-only: %#v", capabilities)
			}
		})
	}

	stable := DetectCapabilities("  1.0.0  ")
	if !stable.Known || !stable.HardDeleteSafe || stable.UnknownVersionReadOnly {
		t.Fatalf("exact stable release was not recognized: %#v", stable)
	}
}

func TestValidateBaseURLRejectsPublicPlainHTTP(t *testing.T) {
	if _, err := NewClient("http://example.com", "", 0, nil); err == nil {
		t.Fatal("public plain HTTP was accepted")
	}
	if _, err := NewClient("http://new-api:3000", "", 0, nil); err != nil {
		t.Fatalf("single-label internal HTTP rejected: %v", err)
	}
}

func TestCustomHTTPClientReceivesMandatoryTimeoutAndRedirectPolicy(t *testing.T) {
	allowRedirect := func(*http.Request, []*http.Request) error { return nil }
	custom := &http.Client{CheckRedirect: allowRedirect}
	client, err := NewClient("http://new-api:3000", "", 0, custom)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient == custom {
		t.Fatal("NewClient mutated and retained the caller-owned HTTP client")
	}
	if client.httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("custom client timeout = %s, want %s", client.httpClient.Timeout, defaultHTTPTimeout)
	}
	if client.httpClient.CheckRedirect == nil || client.httpClient.CheckRedirect(nil, nil) == nil {
		t.Fatal("custom client was allowed to follow NewAPI redirects")
	}
	if custom.Timeout != 0 || custom.CheckRedirect == nil || custom.CheckRedirect(nil, nil) != nil {
		t.Fatal("caller-owned HTTP client was modified")
	}

	bounded := &http.Client{Timeout: 3 * time.Second}
	client, err = NewClient("http://new-api:3000", "", 0, bounded)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != 3*time.Second {
		t.Fatalf("positive custom timeout was replaced: %s", client.httpClient.Timeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (failingReadCloser) Close() error {
	return nil
}
