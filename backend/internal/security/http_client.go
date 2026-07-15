package security

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultDNSLookupTimeout = 5 * time.Second
	defaultMaxRedirects     = 3
)

var ErrResponseTooLarge = errors.New("response body exceeds configured limit")

// IPResolver is the subset of net.Resolver used by the outbound URL policy.
// It is intentionally small so the policy can be tested without live DNS.
type IPResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// NewHTTPSClient returns an HTTP client for user-configurable outbound URLs.
// It requires HTTPS, rejects special-use/private destinations both before the
// request and at dial time, ignores ambient proxy settings, and only follows
// redirects that stay on the original authority.
func NewHTTPSClient(timeout time.Duration) *http.Client {
	return newHTTPSClient(timeout, net.DefaultResolver, (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)
}

func newHTTPSClient(timeout time.Duration, resolver IPResolver, dial dialContextFunc) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = safeDialContext(resolver, dial)
	base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	return &http.Client{
		Timeout:       timeout,
		Transport:     &validatingTransport{base: base, resolver: resolver},
		CheckRedirect: newRedirectPolicy(resolver, defaultMaxRedirects),
	}
}

type validatingTransport struct {
	base     http.RoundTripper
	resolver IPResolver
}

func (t *validatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("outbound request URL is missing")
	}
	if err := validateHTTPSURL(req.Context(), req.URL, t.resolver); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// ValidateHTTPSURL validates a user-configurable URL and resolves its host.
// Callers should still use NewHTTPSClient so the same checks happen at dial and
// redirect time, which closes DNS-rebinding and redirect-based SSRF bypasses.
func ValidateHTTPSURL(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("base URL must not contain a query string or fragment")
	}
	return validateHTTPSURL(ctx, parsed, net.DefaultResolver)
}

func validateHTTPSURL(ctx context.Context, parsed *url.URL, resolver IPResolver) error {
	if parsed == nil || !parsed.IsAbs() || parsed.Host == "" {
		return errors.New("URL must be absolute and include a host")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("URL must use HTTPS")
	}
	if parsed.User != nil {
		return errors.New("URL must not contain user credentials")
	}
	if parsed.Fragment != "" {
		return errors.New("URL must not contain a fragment")
	}
	if parsed.Opaque != "" {
		return errors.New("opaque URLs are not allowed")
	}

	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(parsed.Hostname())), ".")
	if host == "" || strings.Contains(host, "%") {
		return errors.New("URL host is invalid")
	}
	if isMetadataHostname(host) {
		return fmt.Errorf("URL host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("URL resolves to a private or special-use address: %s", ip.String())
		}
		return nil
	}
	if resolver == nil {
		return errors.New("DNS resolver is unavailable")
	}

	lookupCtx, cancel := context.WithTimeout(ctx, defaultDNSLookupTimeout)
	defer cancel()
	addresses, err := resolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("failed to resolve URL host: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("URL host did not resolve to an address")
	}
	for _, address := range addresses {
		if isBlockedIP(address.IP) {
			return fmt.Errorf("URL host resolves to a private or special-use address: %s", address.IP.String())
		}
	}
	return nil
}

func safeDialContext(resolver IPResolver, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid outbound address: %w", err)
		}
		host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if isMetadataHostname(host) {
			return nil, fmt.Errorf("outbound host %q is not allowed", host)
		}

		var addresses []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			addresses = []net.IPAddr{{IP: ip}}
		} else {
			if resolver == nil {
				return nil, errors.New("DNS resolver is unavailable")
			}
			addresses, err = resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve outbound host: %w", err)
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("outbound host did not resolve to an address")
		}
		for _, candidate := range addresses {
			if isBlockedIP(candidate.IP) {
				return nil, fmt.Errorf("outbound host resolves to a private or special-use address: %s", candidate.IP.String())
			}
		}

		var lastErr error
		for _, candidate := range addresses {
			conn, dialErr := dial(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("failed to connect to outbound host: %w", lastErr)
	}
}

func newRedirectPolicy(resolver IPResolver, maxRedirects int) func(req *http.Request, via []*http.Request) error {
	if maxRedirects <= 0 {
		maxRedirects = defaultMaxRedirects
	}
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req == nil || req.URL == nil {
			return errors.New("redirect URL is missing")
		}
		if err := validateHTTPSURL(req.Context(), req.URL, resolver); err != nil {
			return fmt.Errorf("unsafe redirect target: %w", err)
		}
		if len(via) > 0 && !sameAuthority(via[0].URL, req.URL) {
			return errors.New("cross-origin redirects are not allowed for credentialed requests")
		}
		return nil
	}
}

func sameAuthority(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func isMetadataHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	switch host {
	case "metadata", "metadata.google.internal", "metadata.google", "instance-data", "instance-data.ec2.internal":
		return true
	default:
		return false
	}
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedNetworks = mustParseCIDRs([]string{
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"2001:db8::/32",
})

func mustParseCIDRs(values []string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		result = append(result, network)
	}
	return result
}

// ReadLimitedBody reads at most maxBytes and returns a distinct error when the
// peer exceeds the limit instead of silently accepting truncated JSON.
func ReadLimitedBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("response body is missing")
	}
	if maxBytes <= 0 {
		return nil, errors.New("response body limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
