package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addresses, ok := r[host]
	if !ok {
		return nil, errors.New("not found")
	}
	return addresses, nil
}

func TestValidateHTTPSURLRejectsUnsafeDestinations(t *testing.T) {
	resolver := staticResolver{
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"private.example": {{IP: net.ParseIP("10.0.0.5")}},
	}
	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "public HTTPS", rawURL: "https://public.example/v1", wantErr: false},
		{name: "plain HTTP", rawURL: "http://public.example/v1", wantErr: true},
		{name: "loopback literal", rawURL: "https://127.0.0.1/v1", wantErr: true},
		{name: "link local metadata", rawURL: "https://169.254.169.254/latest", wantErr: true},
		{name: "metadata hostname", rawURL: "https://metadata.google.internal/", wantErr: true},
		{name: "private DNS result", rawURL: "https://private.example/v1", wantErr: true},
		{name: "embedded credentials", rawURL: "https://user:pass@public.example/v1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsedReq, err := http.NewRequest(http.MethodGet, test.rawURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = validateHTTPSURL(context.Background(), parsedReq.URL, resolver)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateHTTPSURL() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
	if err := ValidateHTTPSURL(context.Background(), "https://8.8.8.8/v1?key=secret"); err == nil {
		t.Fatal("base URL containing a query string was accepted")
	}
}

func TestRedirectPolicyRejectsCredentialCrossOrigin(t *testing.T) {
	resolver := staticResolver{
		"api.example":   {{IP: net.ParseIP("93.184.216.34")}},
		"other.example": {{IP: net.ParseIP("93.184.216.35")}},
	}
	policy := newRedirectPolicy(resolver, 3)
	original, _ := http.NewRequest(http.MethodGet, "https://api.example/v1/models", nil)

	sameOrigin, _ := http.NewRequest(http.MethodGet, "https://api.example/v1/models/", nil)
	if err := policy(sameOrigin, []*http.Request{original}); err != nil {
		t.Fatalf("same-origin redirect unexpectedly rejected: %v", err)
	}

	crossOrigin, _ := http.NewRequest(http.MethodGet, "https://other.example/steal", nil)
	crossOrigin.Header.Set("Authorization", "Bearer secret")
	if err := policy(crossOrigin, []*http.Request{original}); err == nil {
		t.Fatal("expected cross-origin redirect to be rejected")
	}

	privateTarget, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1/latest", nil)
	if err := policy(privateTarget, []*http.Request{original}); err == nil {
		t.Fatal("expected private redirect target to be rejected")
	}
}

func TestSafeDialContextRevalidatesDNSAtConnectTime(t *testing.T) {
	resolver := staticResolver{
		"rebound.example": {{IP: net.ParseIP("10.0.0.8")}},
		"public.example":  {{IP: net.ParseIP("93.184.216.34")}},
	}
	dialCalled := false
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialCalled = true
		if address != "93.184.216.34:443" {
			return nil, errors.New("unexpected dial target: " + address)
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
	safeDial := safeDialContext(resolver, dial)
	if _, err := safeDial(context.Background(), "tcp", "rebound.example:443"); err == nil {
		t.Fatal("DNS rebound to a private address was accepted")
	}
	if dialCalled {
		t.Fatal("underlying dialer was called for a blocked address")
	}
	conn, err := safeDial(context.Background(), "tcp", "public.example:443")
	if err != nil {
		t.Fatalf("public address was rejected: %v", err)
	}
	_ = conn.Close()
	if !dialCalled {
		t.Fatal("underlying dialer was not called for a public address")
	}
}

func TestReadLimitedBodyDetectsOverflow(t *testing.T) {
	data, err := ReadLimitedBody(strings.NewReader("1234"), 4)
	if err != nil || string(data) != "1234" {
		t.Fatalf("unexpected bounded read: data=%q err=%v", data, err)
	}
	if _, err := ReadLimitedBody(strings.NewReader("12345"), 4); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}
