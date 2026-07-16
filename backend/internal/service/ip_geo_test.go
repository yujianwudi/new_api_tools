package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oschwald/geoip2-golang"
)

type fakeGeoIPCityReader struct {
	closeCount atomic.Int32
}

func (r *fakeGeoIPCityReader) City(net.IP) (*geoip2.City, error) {
	return &geoip2.City{}, nil
}

func (r *fakeGeoIPCityReader) Close() error {
	r.closeCount.Add(1)
	return nil
}

func waitForGeoIPCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for GeoIP condition")
}

func TestIPGeoCloseWaitsForUpdaterAndPreventsReaderRevival(t *testing.T) {
	payload := []byte("valid-test-mmdb")
	digest := sha256.Sum256(payload)
	oldReader := &fakeGeoIPCityReader{}
	newReader := &fakeGeoIPCityReader{}
	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	var openCalls atomic.Int32

	svc := &IPGeoService{
		cityReader:     oldReader,
		available:      true,
		dbPath:         filepath.Join(t.TempDir(), "GeoLite2-City.mmdb"),
		retryInterval:  5 * time.Millisecond,
		updateInterval: 5 * time.Millisecond,
		minFileSize:    1,
		maxFileSize:    1024,
		expectedSHA256: fmt.Sprintf("%x", digest[:]),
		downloadFn: func(_ context.Context, path string) error {
			return os.WriteFile(path, payload, 0644)
		},
		openReaderFn: func(string) (geoIPCityReader, error) {
			if openCalls.Add(1) == 1 {
				close(openStarted)
			}
			<-releaseOpen
			return newReader, nil
		},
	}
	svc.startBackgroundUpdater()

	select {
	case <-openStarted:
	case <-time.After(time.Second):
		t.Fatal("background updater did not reach reader installation")
	}

	closeDone1 := make(chan struct{})
	closeDone2 := make(chan struct{})
	go func() {
		svc.Close()
		close(closeDone1)
	}()
	go func() {
		svc.Close()
		close(closeDone2)
	}()

	select {
	case <-closeDone1:
		t.Fatal("Close returned while the updater was still opening a reader")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseOpen)
	for _, done := range []chan struct{}{closeDone1, closeDone2} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Close did not finish after updater release")
		}
	}

	if svc.IsAvailable() {
		t.Fatal("closed GeoIP service became available again")
	}
	svc.mu.RLock()
	installedReader := svc.cityReader
	stopped := svc.stopped
	svc.mu.RUnlock()
	if installedReader != nil || !stopped {
		t.Fatalf("closed service state is invalid: reader=%v stopped=%v", installedReader, stopped)
	}
	if got := oldReader.closeCount.Load(); got != 1 {
		t.Fatalf("old reader close count = %d, want 1", got)
	}
	if got := newReader.closeCount.Load(); got != 1 {
		t.Fatalf("late reader close count = %d, want 1", got)
	}
	if got := openCalls.Load(); got != 1 {
		t.Fatalf("updater reopened the database after Close: calls=%d", got)
	}
}

func TestIPGeoBackgroundRetriesUntilReaderIsAvailable(t *testing.T) {
	payload := []byte("valid-retry-mmdb")
	digest := sha256.Sum256(payload)
	reader := &fakeGeoIPCityReader{}
	var attempts atomic.Int32
	svc := &IPGeoService{
		dbPath:         filepath.Join(t.TempDir(), "GeoLite2-City.mmdb"),
		retryInterval:  5 * time.Millisecond,
		updateInterval: time.Hour,
		minFileSize:    1,
		maxFileSize:    1024,
		expectedSHA256: fmt.Sprintf("%x", digest[:]),
		downloadFn: func(_ context.Context, path string) error {
			if attempts.Add(1) < 3 {
				return errors.New("temporary mirror failure")
			}
			return os.WriteFile(path, payload, 0644)
		},
		openReaderFn: func(string) (geoIPCityReader, error) {
			return reader, nil
		},
	}
	svc.startBackgroundUpdater()
	t.Cleanup(svc.Close)

	waitForGeoIPCondition(t, time.Second, func() bool {
		return attempts.Load() >= 3 && svc.IsAvailable()
	})
	stableAttempts := attempts.Load()
	time.Sleep(4 * svc.retryEvery())
	if got := attempts.Load(); got != stableAttempts {
		t.Fatalf("available service kept using retry interval: attempts %d -> %d", stableAttempts, got)
	}
}

func TestIPGeoParserUsesTheVerifiedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "GeoLite2-City.mmdb")
	verifiedPayload := []byte("verified-mmdb-snapshot")
	replacementPayload := []byte("path-was-replaced-after-verification")
	digest := sha256.Sum256(verifiedPayload)
	if err := os.WriteFile(path, verifiedPayload, 0644); err != nil {
		t.Fatal(err)
	}

	reader := &fakeGeoIPCityReader{}
	svc := &IPGeoService{
		minFileSize:    1,
		maxFileSize:    1024,
		expectedSHA256: fmt.Sprintf("%x", digest[:]),
		openReaderBytesFn: func(payload []byte) (geoIPCityReader, error) {
			if err := os.WriteFile(path, replacementPayload, 0644); err != nil {
				t.Fatal(err)
			}
			if string(payload) != string(verifiedPayload) {
				t.Fatalf("parser payload = %q, want verified snapshot %q", payload, verifiedPayload)
			}
			return reader, nil
		},
	}

	opened, err := svc.openVerifiedDatabase(path)
	if err != nil {
		t.Fatalf("openVerifiedDatabase() error = %v", err)
	}
	if opened != reader {
		t.Fatalf("opened reader = %T, want test reader", opened)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(replacementPayload) {
		t.Fatalf("replacement file = %q, %v", got, err)
	}
}

func TestIPGeoFreshFileOnlySkipsWhenReaderIsAvailable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "GeoLite2-City.mmdb")
	if err := os.WriteFile(dbPath, []byte("fresh but not open"), 0644); err != nil {
		t.Fatalf("write fresh database placeholder: %v", err)
	}

	t.Run("unavailable retries despite fresh file", func(t *testing.T) {
		var downloads atomic.Int32
		svc := &IPGeoService{
			dbPath:         dbPath,
			updateInterval: time.Hour,
			downloadFn: func(context.Context, string) error {
				downloads.Add(1)
				return errors.New("expected test failure")
			},
		}
		svc.tryUpdateDatabase()
		svc.Close()
		if got := downloads.Load(); got != 1 {
			t.Fatalf("fresh but unavailable database skipped retry: downloads=%d", got)
		}
	})

	t.Run("available reader may use freshness", func(t *testing.T) {
		reader := &fakeGeoIPCityReader{}
		var downloads atomic.Int32
		svc := &IPGeoService{
			cityReader:     reader,
			available:      true,
			dbPath:         dbPath,
			updateInterval: time.Hour,
			downloadFn: func(context.Context, string) error {
				downloads.Add(1)
				return nil
			},
		}
		svc.tryUpdateDatabase()
		svc.Close()
		if got := downloads.Load(); got != 0 {
			t.Fatalf("healthy fresh database downloaded unexpectedly: downloads=%d", got)
		}
		if got := reader.closeCount.Load(); got != 1 {
			t.Fatalf("installed reader close count = %d, want 1", got)
		}
	})
}

func TestIPGeoInitDoesNotDownloadWithoutExplicitOptIn(t *testing.T) {
	t.Setenv("GEOIP_DATA_DIR", t.TempDir())
	t.Setenv("GEOIP_AUTO_DOWNLOAD", "false")
	t.Setenv("GEOIP_AUTO_UPDATE", "false")
	var downloads atomic.Int32
	svc := &IPGeoService{
		downloadFn: func(context.Context, string) error {
			downloads.Add(1)
			return nil
		},
	}
	svc.init()
	svc.Close()
	if got := downloads.Load(); got != 0 {
		t.Fatalf("GeoIP GET initialization triggered an implicit download: %d", got)
	}
}

func TestIPGeoInitRequiresPinnedChecksumForLocalDatabase(t *testing.T) {
	payload := []byte("local-test-mmdb")
	sum := sha256.Sum256(payload)

	t.Run("matching local file is opened", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "GeoLite2-City.mmdb")
		if err := os.WriteFile(path, payload, 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GEOIP_DATA_DIR", dir)
		t.Setenv("GEOIP_AUTO_DOWNLOAD", "false")
		reader := &fakeGeoIPCityReader{}
		var openCalls atomic.Int32
		svc := &IPGeoService{
			minFileSize:    1,
			maxFileSize:    1024,
			expectedSHA256: fmt.Sprintf("%x", sum[:]),
			openReaderFn: func(got string) (geoIPCityReader, error) {
				openCalls.Add(1)
				if got != path {
					t.Fatalf("opened path = %q, want %q", got, path)
				}
				return reader, nil
			},
		}
		svc.init()
		if !svc.IsAvailable() || openCalls.Load() != 1 {
			t.Fatalf("verified local database was not installed: available=%v calls=%d", svc.IsAvailable(), openCalls.Load())
		}
		svc.Close()
	})

	t.Run("mismatched local file never reaches parser", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "GeoLite2-City.mmdb"), payload, 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GEOIP_DATA_DIR", dir)
		t.Setenv("GEOIP_AUTO_DOWNLOAD", "false")
		var openCalls atomic.Int32
		svc := &IPGeoService{
			minFileSize:    1,
			maxFileSize:    1024,
			expectedSHA256: strings.Repeat("0", 64),
			openReaderFn: func(string) (geoIPCityReader, error) {
				openCalls.Add(1)
				return &fakeGeoIPCityReader{}, nil
			},
		}
		svc.init()
		if svc.IsAvailable() || openCalls.Load() != 0 {
			t.Fatalf("unverified local database reached parser: available=%v calls=%d", svc.IsAvailable(), openCalls.Load())
		}
		svc.Close()
	})
}

func TestIPGeoDownloadRejectsChunkedBodyOverMaximumSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush() // force an unknown-length/chunked response
		}
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "GeoLite2-City.mmdb")
	svc := &IPGeoService{
		downloadURLs:   []string{server.URL},
		downloadClient: server.Client(),
		minFileSize:    1,
		maxFileSize:    1024,
	}
	err := svc.downloadDatabase(context.Background(), destPath)
	if err == nil {
		t.Fatal("oversized GeoIP download unexpectedly succeeded")
	}
	for _, path := range []string{destPath, destPath + ".tmp"} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("oversized download left file %s: %v", path, statErr)
		}
	}
}

func TestIPGeoDownloadRequiresPinnedChecksumBeforeInstall(t *testing.T) {
	payload := []byte("test-mmdb-payload")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	t.Run("matching checksum installs atomically", func(t *testing.T) {
		sum := sha256.Sum256(payload)
		reader := &fakeGeoIPCityReader{}
		destPath := filepath.Join(t.TempDir(), "GeoLite2-City.mmdb")
		svc := &IPGeoService{
			downloadURLs:   []string{server.URL},
			downloadClient: server.Client(),
			minFileSize:    1,
			maxFileSize:    1024,
			expectedSHA256: fmt.Sprintf("%x", sum[:]),
			openReaderFn: func(string) (geoIPCityReader, error) {
				return reader, nil
			},
		}
		if err := svc.downloadDatabase(context.Background(), destPath); err != nil {
			t.Fatalf("downloadDatabase returned error: %v", err)
		}
		installed, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatalf("read installed database: %v", err)
		}
		if string(installed) != string(payload) {
			t.Fatalf("installed payload = %q, want %q", installed, payload)
		}
		if reader.closeCount.Load() != 1 {
			t.Fatalf("validation reader close count = %d, want 1", reader.closeCount.Load())
		}
	})

	t.Run("mismatch never reaches parser or destination", func(t *testing.T) {
		var openCalls atomic.Int32
		destPath := filepath.Join(t.TempDir(), "GeoLite2-City.mmdb")
		svc := &IPGeoService{
			downloadURLs:   []string{server.URL},
			downloadClient: server.Client(),
			minFileSize:    1,
			maxFileSize:    1024,
			expectedSHA256: strings.Repeat("0", 64),
			openReaderFn: func(string) (geoIPCityReader, error) {
				openCalls.Add(1)
				return &fakeGeoIPCityReader{}, nil
			},
		}
		if err := svc.downloadDatabase(context.Background(), destPath); err == nil {
			t.Fatal("checksum mismatch unexpectedly succeeded")
		}
		if openCalls.Load() != 0 {
			t.Fatalf("checksum mismatch reached mmdb parser: calls=%d", openCalls.Load())
		}
		if _, err := os.Stat(destPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("checksum mismatch left destination file: %v", err)
		}
	})
}

func TestIPGeoDefaultDownloadClientRejectsPlainHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := (&IPGeoService{}).httpClient()
	resp, err := client.Get(server.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("default GeoIP download client accepted plain HTTP")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("unsafe request reached the HTTP server: requests=%d", got)
	}
}
