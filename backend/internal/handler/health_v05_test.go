package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

type stubNewAPIStatusClient struct {
	status *newapi.Status
	err    error
}

func (s stubNewAPIStatusClient) Status(context.Context) (*newapi.Status, error) {
	return s.status, s.err
}

func setupHealthTestDatabase(t *testing.T, latestLog int64) {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, created_at INTEGER)`)
	if latestLog > 0 {
		db.MustExec(`INSERT INTO logs (created_at) VALUES (?)`, latestLog)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() {
		_ = db.Close()
		database.SetForTesting(nil)
	})
}

func TestDependencyHealthTreatsStalenessAsDiagnostic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupHealthTestDatabase(t, time.Now().Add(-time.Hour).Unix())
	store, err := toolstore.Init(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := NewHealthHandler(&config.Config{
		LogFreshnessMaxAge:     time.Minute,
		NewAPIAdminAccessToken: "admin-token",
		NewAPIAdminUserID:      1,
	}, stubNewAPIStatusClient{status: &newapi.Status{Version: "v1.0.0-rc.21"}}, store, observability.NewRegistry())

	router := gin.New()
	router.GET("/dependencies", h.DependencyHealth)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dependencies", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("dependency health = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status string            `json:"status"`
		Checks []DependencyCheck `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode dependency health response: %v", err)
	}
	if response.Status != "healthy" {
		t.Fatalf("dependency health status = %q, want healthy: %s", response.Status, recorder.Body.String())
	}
	for _, check := range response.Checks {
		if check.Name != "log_freshness" {
			continue
		}
		if check.Status != "stale" || !check.Diagnostic || check.Required {
			t.Fatalf("log freshness check = %#v, want optional stale diagnostic", check)
		}
		return
	}
	t.Fatalf("dependency health omitted log_freshness check: %s", recorder.Body.String())
}

func TestCollectDependencyChecksEnforcesDeadlineWhenProbeIgnoresCancellation(t *testing.T) {
	coordinator := &dependencyProbeCoordinator{probeTimeout: 25 * time.Millisecond}
	release := make(chan struct{})
	releaseProbe := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(releaseProbe)
	finished := make(chan struct{})
	var starts atomic.Int32
	stalled := dependencyProbe{name: "stalled", required: true, check: func(context.Context) DependencyCheck {
		call := starts.Add(1)
		<-release
		if call == 1 {
			close(finished)
		}
		return DependencyCheck{Status: "healthy", OK: true}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	checks := collectDependencyCheckResults(ctx, observability.NewRegistry(), coordinator, []dependencyProbe{
		{name: "fast", check: func(context.Context) DependencyCheck {
			return DependencyCheck{Status: "healthy", OK: true}
		}},
		stalled,
	})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("dependency checks blocked for %s after deadline", elapsed)
	}
	if len(checks) != 2 || checks[0].Name != "fast" || checks[1].Name != "stalled" {
		t.Fatalf("dependency checks were not complete and sorted: %#v", checks)
	}
	if checks[1].Status != "timeout" || checks[1].OK || !checks[1].Required {
		t.Fatalf("stalled dependency was not synthesized as a required timeout: %#v", checks[1])
	}
	if starts.Load() != 1 {
		t.Fatalf("stalled dependency starts = %d, want 1", starts.Load())
	}

	reusedStarted := time.Now()
	reused := collectDependencyCheckResults(context.Background(), observability.NewRegistry(), coordinator, []dependencyProbe{stalled})
	if elapsed := time.Since(reusedStarted); elapsed > 100*time.Millisecond {
		t.Fatalf("cached stalled dependency timeout took %s", elapsed)
	}
	if len(reused) != 1 || reused[0].Status != "timeout" || reused[0].Details["probe_state"] != "still_running" {
		t.Fatalf("stalled dependency timeout was not reused: %#v", reused)
	}
	if starts.Load() != 1 {
		t.Fatalf("timed-out dependency started another goroutine: starts=%d", starts.Load())
	}

	releaseProbe()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stalled dependency did not finish after release")
	}
	waitForDependencyProbeIdle(t, coordinator, "stalled")

	refreshed := collectDependencyCheckResults(context.Background(), observability.NewRegistry(), coordinator, []dependencyProbe{stalled})
	if len(refreshed) != 1 || refreshed[0].Status != "healthy" || !refreshed[0].OK {
		t.Fatalf("completed dependency could not be probed again: %#v", refreshed)
	}
	if starts.Load() != 2 {
		t.Fatalf("dependency starts after completion = %d, want 2", starts.Load())
	}
}

func TestDependencyProbeCoordinatorSingleFlightsConcurrentRequests(t *testing.T) {
	coordinator := &dependencyProbeCoordinator{}
	release := make(chan struct{})
	started := make(chan struct{})
	var starts atomic.Int32
	probe := dependencyProbe{name: "shared", check: func(context.Context) DependencyCheck {
		if starts.Add(1) == 1 {
			close(started)
		}
		<-release
		return DependencyCheck{Status: "healthy", OK: true}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first, cached := coordinator.acquire(ctx, probe)
	if cached != nil || first == nil {
		t.Fatalf("first probe acquire = run:%p cached:%#v", first, cached)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first dependency probe did not start")
	}
	second, cached := coordinator.acquire(ctx, probe)
	if cached != nil || second != first {
		t.Fatalf("concurrent dependency probe was not single-flighted: first=%p second=%p cached=%#v", first, second, cached)
	}
	if starts.Load() != 1 {
		t.Fatalf("concurrent dependency probe starts = %d, want 1", starts.Load())
	}

	close(release)
	if check := coordinator.wait(ctx, first, time.Now()); check.Status != "healthy" || !check.OK {
		t.Fatalf("single-flight dependency result = %#v", check)
	}
	waitForDependencyProbeIdle(t, coordinator, "shared")

	third, cached := coordinator.acquire(ctx, probe)
	if cached != nil || third == nil || third == first {
		t.Fatalf("completed dependency did not start a fresh probe: first=%p third=%p cached=%#v", first, third, cached)
	}
	if check := coordinator.wait(ctx, third, time.Now()); check.Status != "healthy" || !check.OK {
		t.Fatalf("fresh dependency result = %#v", check)
	}
	if starts.Load() != 2 {
		t.Fatalf("dependency starts after fresh probe = %d, want 2", starts.Load())
	}
}

func TestDependencyProbeCoordinatorDoesNotLetFirstWaiterCancelSharedProbe(t *testing.T) {
	coordinator := &dependencyProbeCoordinator{}
	started := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	probe := dependencyProbe{name: "shared-cancellation", check: func(ctx context.Context) DependencyCheck {
		if starts.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return DependencyCheck{Status: "healthy", OK: true}
		case <-ctx.Done():
			return DependencyCheck{Status: "cancelled", Details: map[string]any{"error": ctx.Err().Error()}}
		}
	}}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first, cached := coordinator.acquire(firstCtx, probe)
	if cached != nil || first == nil {
		t.Fatalf("first probe acquire = run:%p cached:%#v", first, cached)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared dependency probe did not start")
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	second, cached := coordinator.acquire(secondCtx, probe)
	if cached != nil || second != first {
		t.Fatalf("second waiter did not join shared probe: first=%p second=%p cached=%#v", first, second, cached)
	}

	cancelFirst()
	if check := coordinator.wait(firstCtx, first, time.Now()); check.Status != "timeout" {
		t.Fatalf("cancelled first waiter = %#v, want request-local timeout", check)
	}
	if starts.Load() != 1 {
		t.Fatalf("shared dependency probe starts = %d, want 1", starts.Load())
	}
	lateCtx, cancelLate := context.WithTimeout(context.Background(), time.Second)
	defer cancelLate()
	late, cached := coordinator.acquire(lateCtx, probe)
	if cached != nil || late != first {
		t.Fatalf("first waiter cancellation poisoned shared probe: first=%p late=%p cached=%#v", first, late, cached)
	}

	close(release)
	if check := coordinator.wait(secondCtx, second, time.Now()); check.Status != "healthy" || !check.OK {
		t.Fatalf("active second waiter inherited first cancellation: %#v", check)
	}
	if check := coordinator.wait(lateCtx, late, time.Now()); check.Status != "healthy" || !check.OK {
		t.Fatalf("late waiter inherited first cancellation: %#v", check)
	}
	waitForDependencyProbeIdle(t, coordinator, probe.name)
}

func TestDependencyProbeCoordinatorDoesNotLetShortWaiterDeadlinePoisonSharedProbe(t *testing.T) {
	coordinator := &dependencyProbeCoordinator{probeTimeout: time.Second}
	started := make(chan struct{})
	release := make(chan struct{})
	probe := dependencyProbe{name: "shared-short-deadline", check: func(ctx context.Context) DependencyCheck {
		close(started)
		select {
		case <-release:
			return DependencyCheck{Status: "healthy", OK: true}
		case <-ctx.Done():
			return DependencyCheck{Status: "cancelled", Details: map[string]any{"error": ctx.Err().Error()}}
		}
	}}

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelShort()
	run, cached := coordinator.acquire(shortCtx, probe)
	if cached != nil || run == nil {
		t.Fatalf("short waiter acquire = run:%p cached:%#v", run, cached)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared dependency probe did not start")
	}
	if check := coordinator.wait(shortCtx, run, time.Now()); check.Status != "timeout" {
		t.Fatalf("short waiter result = %#v, want request-local timeout", check)
	}

	lateCtx, cancelLate := context.WithTimeout(context.Background(), time.Second)
	defer cancelLate()
	late, cached := coordinator.acquire(lateCtx, probe)
	if cached != nil || late != run {
		t.Fatalf("short waiter deadline poisoned shared probe: first=%p late=%p cached=%#v", run, late, cached)
	}
	close(release)
	if check := coordinator.wait(lateCtx, late, time.Now()); check.Status != "healthy" || !check.OK {
		t.Fatalf("late waiter inherited short deadline: %#v", check)
	}
	waitForDependencyProbeIdle(t, coordinator, probe.name)
}

func TestDependencyProbeCoordinatorCachesElapsedDeadlineBeforeTimerCallback(t *testing.T) {
	coordinator := &dependencyProbeCoordinator{probeTimeout: time.Second}
	started := make(chan struct{})
	release := make(chan struct{})
	probe := dependencyProbe{name: "deadline-callback-race", check: func(context.Context) DependencyCheck {
		close(started)
		<-release
		return DependencyCheck{Status: "healthy", OK: true}
	}}

	run, cached := coordinator.acquire(context.Background(), probe)
	if cached != nil || run == nil {
		t.Fatalf("probe acquire = run:%p cached:%#v", run, cached)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dependency probe did not start")
	}

	// Model the narrow state where the coordinator deadline has elapsed but
	// the timer callback has not acquired the mutex yet. The next lock holder
	// must make the timeout durable instead of joining the stale live probe.
	coordinator.mu.Lock()
	run.deadline = time.Now().Add(-time.Millisecond)
	coordinator.mu.Unlock()
	late, cached := coordinator.acquire(context.Background(), probe)
	if late != nil || cached == nil || cached.Status != "timeout" || cached.Details["probe_state"] != "still_running" {
		t.Fatalf("elapsed coordinator deadline = run:%p cached:%#v, want cached timeout", late, cached)
	}
	coordinator.mu.Lock()
	timedOut := run.timedOut
	coordinator.mu.Unlock()
	if !timedOut {
		t.Fatal("elapsed coordinator deadline was returned but not cached on the shared run")
	}

	close(release)
	waitForDependencyProbeIdle(t, coordinator, probe.name)
}

func waitForDependencyProbeIdle(t *testing.T, coordinator *dependencyProbeCoordinator, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		coordinator.mu.Lock()
		_, active := coordinator.active[name]
		coordinator.mu.Unlock()
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dependency probe %q remained active after completion", name)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLogFreshnessRejectsFutureTimestampsAsClockSkew(t *testing.T) {
	setupHealthTestDatabase(t, time.Now().Add(time.Hour).Unix())
	h := NewHealthHandler(&config.Config{LogFreshnessMaxAge: time.Minute}, nil, nil, observability.NewRegistry())

	check := h.checkLogFreshness(context.Background())
	if check.Status != "clock_skew" || check.OK {
		t.Fatalf("future log freshness = %#v, want unhealthy clock_skew", check)
	}
	if check.Details["ahead_seconds"] == nil || check.Details["latest_log_at"] == nil {
		t.Fatalf("clock skew details are incomplete: %#v", check.Details)
	}
}

func TestReadinessRequiresToolStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupHealthTestDatabase(t, 0)
	h := NewHealthHandler(&config.Config{}, nil, nil, observability.NewRegistry())
	router := gin.New()
	router.GET("/readyz", h.Readiness)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"tool_store":"unavailable"`) {
		t.Fatalf("readiness without tool store = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCompatibilityHealthRoutesDoNotRegisterUnconfiguredReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterHealthRoutes(router)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("compatibility /readyz status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestNewAPICapabilitiesFailClosedForUnknownVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHealthHandler(&config.Config{
		NewAPIAdminAccessToken: "admin-token",
		NewAPIAdminUserID:      1,
	}, stubNewAPIStatusClient{status: &newapi.Status{Version: "v2.0.0"}}, nil, observability.NewRegistry())
	router := gin.New()
	router.GET("/capabilities", h.NewAPICapabilities)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("capabilities = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	data, _ := body["data"].(map[string]any)
	if data["write_mode"] != "read_only" {
		t.Fatalf("unknown version write mode = %#v", data["write_mode"])
	}
}
