package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaDataRefreshDoesNotBlockCachedReaders(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
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
		quotaDataMu.Lock()
		quotaDataAvailabilityCheck = originalCheck
		quotaDataCheckedAt = time.Time{}
		quotaDataAvailable = false
		quotaDataRefreshing = false
		quotaDataMu.Unlock()
	})

	refreshResult := make(chan bool, 1)
	go func() {
		refreshResult <- IsQuotaDataAvailable()
	}()
	<-started

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
	wg.Wait()
	close(results)
	for got := range results {
		if !got {
			t.Fatal("reader did not receive the cached availability during refresh")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh checks = %d, want 1", got)
	}

	close(release)
	if got := <-refreshResult; got {
		t.Fatal("refresh result = true, want false")
	}
	if IsQuotaDataAvailable() {
		t.Fatal("cached result was not updated after refresh")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TTL cache triggered an extra refresh, checks = %d", got)
	}
}
