package service

import (
	"context"
	"errors"
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
		downloadFn: func(context.Context, string) error {
			return nil
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
	reader := &fakeGeoIPCityReader{}
	var attempts atomic.Int32
	svc := &IPGeoService{
		dbPath:         filepath.Join(t.TempDir(), "GeoLite2-City.mmdb"),
		retryInterval:  5 * time.Millisecond,
		updateInterval: time.Hour,
		downloadFn: func(context.Context, string) error {
			if attempts.Add(1) < 3 {
				return errors.New("temporary mirror failure")
			}
			return nil
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
