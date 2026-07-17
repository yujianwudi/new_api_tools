package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestLocalCacheRemovesExpiredEntriesWithoutRedis(t *testing.T) {
	m := &Manager{}
	if err := m.Set("expired", map[string]string{"value": "secret"}, time.Nanosecond); err != nil {
		t.Fatalf("set expired entry: %v", err)
	}
	time.Sleep(time.Millisecond)

	m.removeExpiredLocalEntries()
	if got := m.localCount.Load(); got != 0 {
		t.Fatalf("expired local entry count = %d, want 0", got)
	}
	var value map[string]string
	if found, err := m.GetJSON("expired", &value); err != nil || found {
		t.Fatalf("expired entry found=%v err=%v, want cache miss", found, err)
	}
}

func TestLocalCacheEnforcesHardEntryLimitWithoutRedis(t *testing.T) {
	m := &Manager{}
	if err := m.Set("permanent-settings", map[string]bool{"enabled": true}, 0); err != nil {
		t.Fatalf("set permanent entry: %v", err)
	}
	for i := 0; i < maxLocalCacheEntries+256; i++ {
		key := fmt.Sprintf("attacker-controlled:%d", i)
		if err := m.Set(key, map[string]int{"slot": i}, time.Hour); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	if got := m.localCount.Load(); got > maxLocalCacheEntries {
		t.Fatalf("local entry count = %d, exceeds hard limit %d", got, maxLocalCacheEntries)
	}
	actual := int64(0)
	m.localCache.Range(func(_, _ interface{}) bool {
		actual++
		return true
	})
	if actual > maxLocalCacheEntries {
		t.Fatalf("actual local entries = %d, exceeds hard limit %d", actual, maxLocalCacheEntries)
	}
	if counted := m.localCount.Load(); counted != actual {
		t.Fatalf("tracked local count = %d, actual entries = %d", counted, actual)
	}
	if got := m.Stats()["local_count"]; got != int64(maxLocalCacheEntries) {
		t.Fatalf("reported local_count = %v, want %d", got, maxLocalCacheEntries)
	}
	var permanent map[string]bool
	if found, err := m.GetJSON("permanent-settings", &permanent); err != nil || !found || !permanent["enabled"] {
		t.Fatalf("permanent settings were evicted: found=%v value=%v err=%v", found, permanent, err)
	}
}

func TestClearLocalMaintainsEntryCount(t *testing.T) {
	m := &Manager{}
	for i := 0; i < 10; i++ {
		if err := m.Set(fmt.Sprintf("key:%d", i), i, time.Minute); err != nil {
			t.Fatalf("set entry %d: %v", i, err)
		}
	}

	m.ClearLocal()
	if got := m.localCount.Load(); got != 0 {
		t.Fatalf("local entry count after clear = %d, want 0", got)
	}
}
