package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/security"
	"github.com/oschwald/geoip2-golang"
)

// IPGeoInfo represents IP geolocation information
type IPGeoInfo struct {
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	ASN         string `json:"asn"`
	Success     bool   `json:"success"`
}

const (
	geoipDatabaseCommit = "a83d44508ee6831c2770b2c4be91f9850ec429d7"
	geoipDatabaseSHA256 = "168b01d10d0742129be1bee92bba85affaaefcf2e86b4187bcf1924ea50068bf"
)

// GeoIP data is pinned to one immutable upstream commit. Mirrors may improve
// availability, but every payload must match the release checksum before it is
// opened or installed.
var geoipDownloadURLs = []string{
	"https://raw.githubusercontent.com/adysec/IP_database/" + geoipDatabaseCommit + "/geolite/GeoLite2-City.mmdb",
	"https://cdn.jsdelivr.net/gh/adysec/IP_database@" + geoipDatabaseCommit + "/geolite/GeoLite2-City.mmdb",
}

// geoipUpdateInterval is the interval between automatic database updates (24 hours)
const geoipUpdateInterval = 24 * time.Hour

// geoipRetryInterval keeps unavailable services retrying until a reader is
// actually installed. A fresh file alone is not proof that the service works.
const geoipRetryInterval = 5 * time.Minute

// geoipMinFileSize is the minimum valid database file size (1 MB)
const geoipMinFileSize = 1024 * 1024

// geoipMaxFileSize bounds both network reads and temporary disk usage. The
// GeoLite2 City database is normally well below this limit.
const geoipMaxFileSize = 128 * 1024 * 1024

type geoIPCityReader interface {
	City(net.IP) (*geoip2.City, error)
	Close() error
}

// IPGeoService provides IP geolocation queries using MaxMind GeoLite2
type IPGeoService struct {
	cityReader geoIPCityReader
	dbPath     string
	mu         sync.RWMutex
	available  bool
	stopped    bool

	lifecycleOnce sync.Once
	updaterOnce   sync.Once
	stopOnce      sync.Once
	ctx           context.Context
	cancel        context.CancelFunc
	updaterWG     sync.WaitGroup
	updateMu      sync.Mutex

	// Test seams are configured before the service starts and remain immutable.
	downloadFn        func(context.Context, string) error
	openReaderFn      func(string) (geoIPCityReader, error)
	openReaderBytesFn func([]byte) (geoIPCityReader, error)
	downloadURLs      []string
	downloadClient    *http.Client
	retryInterval     time.Duration
	updateInterval    time.Duration
	minFileSize       int64
	maxFileSize       int64
	expectedSHA256    string
}

var (
	geoService     *IPGeoService
	geoServiceOnce sync.Once
)

var ipGeoServiceProvider = func() *IPGeoService {
	return GetIPGeoService()
}

// domesticCountryCodes defines Chinese domestic country codes
var domesticCountryCodes = map[string]bool{
	"CN": true,
	"HK": true,
	"MO": true,
	"TW": true,
}

// GetIPGeoService returns the singleton IPGeoService
func GetIPGeoService() *IPGeoService {
	geoServiceOnce.Do(func() {
		geoService = &IPGeoService{}
		geoService.init()
	})
	return geoService
}

func (s *IPGeoService) ensureLifecycle() {
	s.lifecycleOnce.Do(func() {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	})
}

func (s *IPGeoService) retryEvery() time.Duration {
	if s.retryInterval > 0 {
		return s.retryInterval
	}
	return geoipRetryInterval
}

func (s *IPGeoService) updateEvery() time.Duration {
	if s.updateInterval > 0 {
		return s.updateInterval
	}
	return geoipUpdateInterval
}

func (s *IPGeoService) minimumFileSize() int64 {
	if s.minFileSize > 0 {
		return s.minFileSize
	}
	return geoipMinFileSize
}

func (s *IPGeoService) maximumFileSize() int64 {
	if s.maxFileSize > 0 {
		return s.maxFileSize
	}
	return geoipMaxFileSize
}

func (s *IPGeoService) databaseDownloadURLs() []string {
	if len(s.downloadURLs) > 0 {
		return s.downloadURLs
	}
	return geoipDownloadURLs
}

func (s *IPGeoService) databaseSHA256() string {
	if s != nil && strings.TrimSpace(s.expectedSHA256) != "" {
		return strings.ToLower(strings.TrimSpace(s.expectedSHA256))
	}
	return geoipDatabaseSHA256
}

func geoIPFeatureEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (s *IPGeoService) httpClient() *http.Client {
	if s.downloadClient != nil {
		return s.downloadClient
	}
	return security.NewHTTPSClient(120 * time.Second)
}

// openVerifiedDatabase applies the same bounded-size and pinned-checksum policy
// to image-bundled, persistent, downloaded, and manually mounted databases.
// The parser receives the exact byte slice that was hashed; it never reopens
// the path after verification, which prevents a local path-swap TOCTOU.
func (s *IPGeoService) openVerifiedDatabase(path string) (geoIPCityReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open database for verification: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat database for verification: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database is not a regular file")
	}
	minFileSize := s.minimumFileSize()
	maxFileSize := s.maximumFileSize()
	if info.Size() < minFileSize {
		return nil, fmt.Errorf("database is too small: %d bytes", info.Size())
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("database exceeds maximum size: %d bytes", info.Size())
	}

	payload, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read database for verification: %w", err)
	}
	if int64(len(payload)) != info.Size() {
		return nil, fmt.Errorf("database size changed during verification")
	}
	digest := sha256.Sum256(payload)
	actual := fmt.Sprintf("%x", digest[:])
	if actual != s.databaseSHA256() {
		return nil, fmt.Errorf("database checksum mismatch")
	}

	if s.openReaderBytesFn != nil {
		return s.openReaderBytesFn(payload)
	}
	if s.openReaderFn != nil {
		// Path-based opening is retained only as a test seam. Production always
		// parses the verified in-memory snapshot below.
		return s.openReaderFn(path)
	}
	return geoip2.FromBytes(payload)
}

func (s *IPGeoService) downloadTo(ctx context.Context, path string) error {
	if s.downloadFn != nil {
		return s.downloadFn(ctx, path)
	}
	return s.downloadDatabase(ctx, path)
}

func (s *IPGeoService) setDatabasePath(path string) {
	s.mu.Lock()
	s.dbPath = path
	s.mu.Unlock()
}

func (s *IPGeoService) databasePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dbPath
}

func (s *IPGeoService) isStopped() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopped
}

func (s *IPGeoService) installReader(reader geoIPCityReader, path string) (geoIPCityReader, bool) {
	if reader == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, false
	}
	oldReader := s.cityReader
	s.cityReader = reader
	s.dbPath = path
	s.available = true
	return oldReader, true
}

func (s *IPGeoService) startBackgroundUpdater() {
	s.ensureLifecycle()
	s.updaterOnce.Do(func() {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		// Add while holding the same mutex Close uses to enter the stopped state.
		// This guarantees Wait never races a late positive Add.
		s.updaterWG.Add(1)
		s.mu.Unlock()

		go func() {
			defer s.updaterWG.Done()
			s.backgroundUpdater()
		}()
	})
}

func (s *IPGeoService) init() {
	s.ensureLifecycle()
	allowDownload := geoIPFeatureEnabled("GEOIP_AUTO_DOWNLOAD")
	allowUpdate := allowDownload && geoIPFeatureEnabled("GEOIP_AUTO_UPDATE")
	if allowUpdate {
		defer s.startBackgroundUpdater()
	}

	// Determine the preferred database directory
	geoipDir := os.Getenv("GEOIP_DATA_DIR")
	if geoipDir == "" {
		geoipDir = "/app/data/geoip"
	}

	// Try to find GeoLite2-City.mmdb in common paths
	paths := []string{
		filepath.Join(geoipDir, "GeoLite2-City.mmdb"),
		"/app/data/geoip/GeoLite2-City.mmdb",
		"./data/geoip/GeoLite2-City.mmdb",
		"/usr/share/GeoIP/GeoLite2-City.mmdb",
	}

	for _, path := range paths {
		if path == "/GeoLite2-City.mmdb" || path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			reader, err := s.openVerifiedDatabase(path)
			if err != nil {
				fmt.Printf("[GeoIP] Refusing database %s: %v\n", path, err)
				continue
			}
			oldReader, installed := s.installReader(reader, path)
			if !installed {
				_ = reader.Close()
				return
			}
			if oldReader != nil {
				_ = oldReader.Close()
			}
			fmt.Printf("[GeoIP] Loaded database: %s\n", path)
			return
		}
	}

	if !allowDownload {
		fmt.Println("[GeoIP] No local GeoLite2-City.mmdb found; automatic downloads are disabled")
		return
	}

	// Database not found — try to download it
	fmt.Println("[GeoIP] No GeoLite2-City.mmdb found, attempting auto-download...")
	downloadPath := filepath.Join(geoipDir, "GeoLite2-City.mmdb")
	s.setDatabasePath(downloadPath)
	if err := s.downloadTo(s.ctx, downloadPath); err != nil {
		fmt.Printf("[GeoIP] Auto-download failed: %v\n", err)
		fmt.Println("[GeoIP] IP geolocation disabled. Will retry in background.")
		return
	}

	// Load the downloaded database
	reader, err := s.openVerifiedDatabase(downloadPath)
	if err != nil {
		fmt.Printf("[GeoIP] Failed to open downloaded database: %v\n", err)
		fmt.Println("[GeoIP] IP geolocation disabled. Will retry in background.")
		return
	}
	oldReader, installed := s.installReader(reader, downloadPath)
	if !installed {
		_ = reader.Close()
		return
	}
	if oldReader != nil {
		_ = oldReader.Close()
	}
	fmt.Printf("[GeoIP] Database downloaded and loaded: %s\n", downloadPath)
}

// downloadDatabase downloads the GeoLite2-City.mmdb file from mirror URLs.
// Reads are capped so a compromised or misconfigured mirror cannot consume
// unbounded memory, bandwidth, or temporary disk space.
func (s *IPGeoService) downloadDatabase(ctx context.Context, destPath string) error {
	// Ensure directory exists
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tempPath := destPath + ".tmp"
	defer os.Remove(tempPath) // clean up temp file on any failure

	client := s.httpClient()
	minFileSize := s.minimumFileSize()
	maxFileSize := s.maximumFileSize()

	for _, url := range s.databaseDownloadURLs() {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Printf("[GeoIP] Downloading from %s ...\n", url)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create download request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Printf("[GeoIP] Download failed from %s: %v\n", url, err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			fmt.Printf("[GeoIP] Download failed from %s: HTTP %d\n", url, resp.StatusCode)
			continue
		}
		if resp.ContentLength > maxFileSize {
			_ = resp.Body.Close()
			fmt.Printf("[GeoIP] Downloaded file is too large (%d bytes), skipping\n", resp.ContentLength)
			continue
		}

		out, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("create temp file: %w", err)
		}

		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(resp.Body, maxFileSize+1))
		closeErr := out.Close()
		_ = resp.Body.Close()

		if copyErr != nil {
			fmt.Printf("[GeoIP] Download write failed from %s: %v\n", url, copyErr)
			_ = os.Remove(tempPath)
			continue
		}
		if closeErr != nil {
			fmt.Printf("[GeoIP] Download close failed from %s: %v\n", url, closeErr)
			_ = os.Remove(tempPath)
			continue
		}
		if written > maxFileSize {
			fmt.Printf("[GeoIP] Downloaded file exceeds maximum size (%d bytes), skipping\n", maxFileSize)
			_ = os.Remove(tempPath)
			continue
		}

		// Validate file size
		if written < minFileSize {
			fmt.Printf("[GeoIP] Downloaded file too small (%d bytes), skipping\n", written)
			_ = os.Remove(tempPath)
			continue
		}
		if actual := fmt.Sprintf("%x", hasher.Sum(nil)); actual != s.databaseSHA256() {
			fmt.Printf("[GeoIP] Downloaded file checksum mismatch from %s, skipping\n", url)
			_ = os.Remove(tempPath)
			continue
		}

		// Validate the exact checksum-matched bytes before installing them.
		testReader, err := s.openVerifiedDatabase(tempPath)
		if err != nil {
			fmt.Printf("[GeoIP] Downloaded file is not valid mmdb: %v\n", err)
			_ = os.Remove(tempPath)
			continue
		}
		if testReader == nil {
			fmt.Println("[GeoIP] Downloaded file opener returned no reader")
			_ = os.Remove(tempPath)
			continue
		}
		_ = testReader.Close()

		// Atomically replace the old file
		if err := os.Rename(tempPath, destPath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", tempPath, destPath, err)
		}

		sizeMB := float64(written) / (1024 * 1024)
		fmt.Printf("[GeoIP] Download complete: %.1f MB\n", sizeMB)
		return nil
	}

	return fmt.Errorf("all download mirrors failed")
}

// backgroundUpdater periodically checks and updates the GeoIP database
func (s *IPGeoService) backgroundUpdater() {
	for {
		delay := s.updateEvery()
		if !s.IsAvailable() {
			delay = s.retryEvery()
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			s.tryUpdateDatabase()
		case <-s.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

// tryUpdateDatabase attempts to download and reload the GeoIP database
func (s *IPGeoService) tryUpdateDatabase() {
	s.ensureLifecycle()
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	if s.isStopped() {
		return
	}

	dbPath := s.databasePath()
	if dbPath == "" {
		return
	}

	// A fresh file may still be corrupt or impossible to open. Only a currently
	// installed reader is allowed to use freshness as a reason to skip retries.
	if s.IsAvailable() {
		info, err := os.Stat(dbPath)
		if err == nil {
			age := time.Since(info.ModTime())
			if age < s.updateEvery() {
				return // database is fresh and the active reader is healthy
			}
		}
	}

	fmt.Println("[GeoIP] Checking for database update...")

	if err := s.downloadTo(s.ctx, dbPath); err != nil {
		if s.isStopped() || s.ctx.Err() != nil {
			return
		}
		fmt.Printf("[GeoIP] Update failed: %v\n", err)
		return
	}
	if s.isStopped() {
		return
	}

	// Reload the database
	newReader, err := s.openVerifiedDatabase(dbPath)
	if err != nil {
		fmt.Printf("[GeoIP] Failed to reload updated database: %v\n", err)
		return
	}
	if newReader == nil {
		fmt.Println("[GeoIP] Failed to reload updated database: opener returned no reader")
		return
	}

	oldReader, installed := s.installReader(newReader, dbPath)
	if !installed {
		_ = newReader.Close()
		return
	}

	if oldReader != nil {
		_ = oldReader.Close()
	}

	fmt.Println("[GeoIP] Database updated and reloaded successfully")
}

// IsAvailable returns whether the GeoIP service is available
func (s *IPGeoService) IsAvailable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.available && s.cityReader != nil && !s.stopped
}

// QuerySingle looks up a single IP address
func (s *IPGeoService) QuerySingle(ip string) IPGeoInfo {
	result := IPGeoInfo{IP: ip}

	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return result
	}

	// Skip private IPs
	if parsedIP.IsPrivate() || parsedIP.IsLoopback() {
		result.Country = "本地网络"
		result.CountryCode = "LO"
		result.Success = true
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.available || s.cityReader == nil {
		return result
	}

	record, err := s.cityReader.City(parsedIP)
	if err != nil {
		return result
	}

	result.Success = true

	// Country
	if name, ok := record.Country.Names["zh-CN"]; ok {
		result.Country = name
	} else if name, ok := record.Country.Names["en"]; ok {
		result.Country = name
	}
	result.CountryCode = record.Country.IsoCode

	// Region/Province
	if len(record.Subdivisions) > 0 {
		if name, ok := record.Subdivisions[0].Names["zh-CN"]; ok {
			result.Region = name
		} else if name, ok := record.Subdivisions[0].Names["en"]; ok {
			result.Region = name
		}
	}

	// City
	if name, ok := record.City.Names["zh-CN"]; ok {
		result.City = name
	} else if name, ok := record.City.Names["en"]; ok {
		result.City = name
	}

	return result
}

// QueryBatch looks up multiple IPs and returns a map of IP -> IPGeoInfo
func (s *IPGeoService) QueryBatch(ips []string) map[string]IPGeoInfo {
	results := make(map[string]IPGeoInfo, len(ips))
	for _, ip := range ips {
		results[ip] = s.QuerySingle(ip)
	}
	return results
}

// LookupIPGeo looks up one IP through the configured GeoIP service provider.
func LookupIPGeo(ip string) IPGeoInfo {
	svc := ipGeoServiceProvider()
	if svc == nil {
		return IPGeoInfo{IP: ip}
	}
	return svc.QuerySingle(ip)
}

// LookupIPGeoBatch looks up multiple IPs through the configured GeoIP service provider.
func LookupIPGeoBatch(ips []string) map[string]IPGeoInfo {
	svc := ipGeoServiceProvider()
	if svc == nil {
		results := make(map[string]IPGeoInfo, len(ips))
		for _, ip := range ips {
			results[ip] = IPGeoInfo{IP: ip}
		}
		return results
	}
	return svc.QueryBatch(ips)
}

// IsIPGeoAvailable reports whether the configured GeoIP service is ready.
func IsIPGeoAvailable() bool {
	svc := ipGeoServiceProvider()
	return svc != nil && svc.IsAvailable()
}

// FormatIPGeoInfo returns the stable snake_case response shape used by IP APIs.
func FormatIPGeoInfo(info IPGeoInfo) map[string]interface{} {
	return map[string]interface{}{
		"ip":           info.IP,
		"country":      info.Country,
		"country_code": info.CountryCode,
		"region":       info.Region,
		"city":         info.City,
		"isp":          info.ISP,
		"org":          info.Org,
		"asn":          info.ASN,
		"success":      info.Success,
	}
}

// SetIPGeoServiceProviderForTesting replaces the GeoIP provider and returns a restore function.
func SetIPGeoServiceProviderForTesting(provider func() *IPGeoService) func() {
	old := ipGeoServiceProvider
	ipGeoServiceProvider = provider
	return func() {
		ipGeoServiceProvider = old
	}
}

// Close releases the GeoIP database resources and stops the background updater
func (s *IPGeoService) Close() {
	if s == nil {
		return
	}
	s.ensureLifecycle()
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.available = false
		cancel := s.cancel
		s.mu.Unlock()
		cancel()
	})

	// Wait for the updater to observe cancellation, then serialize with any
	// direct in-flight update before closing the installed reader. Repeated and
	// concurrent Close calls are safe and cannot race a late reader install.
	s.updaterWG.Wait()
	s.updateMu.Lock()
	s.mu.Lock()
	reader := s.cityReader
	s.cityReader = nil
	s.available = false
	s.mu.Unlock()
	s.updateMu.Unlock()

	if reader != nil {
		_ = reader.Close()
	}
}
