package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const quotaDataTestTimeout = 5 * time.Second

func TestQuotaDataRefreshDoesNotBlockCachedReaders(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	refreshDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() {
		releaseOnce.Do(func() { close(release) })
	}
	var calls atomic.Int32

	quotaDataMu.Lock()
	originalCheck := quotaDataAvailabilityCheck
	quotaDataCheckedAt = time.Now().Add(-quotaDataCheckTTL)
	quotaDataAvailable = true
	quotaDataRefreshing = false
	quotaDataAvailabilityCheck = func() bool {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return false
	}
	quotaDataMu.Unlock()
	t.Cleanup(func() {
		releaseRefresh()
		select {
		case <-refreshDone:
		case <-time.After(quotaDataTestTimeout):
			t.Error("quota-data refresh goroutine did not exit during cleanup")
		}
		quotaDataMu.Lock()
		quotaDataAvailabilityCheck = originalCheck
		quotaDataCheckedAt = time.Time{}
		quotaDataAvailable = false
		quotaDataRefreshing = false
		quotaDataMu.Unlock()
	})

	refreshResult := make(chan bool, 1)
	go func() {
		defer close(refreshDone)
		refreshResult <- IsQuotaDataAvailable()
	}()
	select {
	case <-started:
	case <-time.After(quotaDataTestTimeout):
		t.Fatal("quota-data refresh did not start")
	}

	const readers = 20
	results := make(chan bool, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- IsQuotaDataAvailable()
		}()
	}
	readersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(readersDone)
	}()
	select {
	case <-readersDone:
	case <-time.After(quotaDataTestTimeout):
		t.Fatal("cached quota-data readers blocked behind the refresh")
	}
	close(results)
	for got := range results {
		if !got {
			t.Fatal("reader did not receive the cached availability during refresh")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh checks = %d, want 1", got)
	}

	releaseRefresh()
	select {
	case got := <-refreshResult:
		if got {
			t.Fatal("refresh result = true, want false")
		}
	case <-time.After(quotaDataTestTimeout):
		t.Fatal("quota-data refresh did not finish after release")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh checks after release = %d, want 1", got)
	}
	if IsQuotaDataAvailable() {
		t.Fatal("cached result was not updated after refresh")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TTL cache triggered an extra refresh, checks = %d", got)
	}
	select {
	case <-refreshDone:
	case <-time.After(quotaDataTestTimeout):
		t.Fatal("quota-data refresh goroutine did not exit")
	}
}
