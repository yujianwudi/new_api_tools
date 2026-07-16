package observability

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/buildinfo"
)

var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type httpMetricKey struct {
	Method      string
	Route       string
	StatusClass string
}

type httpMetric struct {
	Count   uint64
	Sum     float64
	Buckets [len(durationBuckets)]uint64
}

type operationKey struct {
	Action string
	Result string
}

// Registry is a deliberately small, dependency-free Prometheus registry for
// the control plane's core SLI and audit counters. Label values are bounded by
// code-defined route templates and operation names to avoid cardinality leaks.
type Registry struct {
	startedAt time.Time
	inflight  atomic.Int64

	mu           sync.RWMutex
	http         map[httpMetricKey]httpMetric
	dependencies map[string]float64
	operations   map[operationKey]uint64
	freshnessSet bool
	logLag       float64
	latestLog    float64
}

func NewRegistry() *Registry {
	return &Registry{
		startedAt:    time.Now(),
		http:         make(map[httpMetricKey]httpMetric),
		dependencies: make(map[string]float64),
		operations:   make(map[operationKey]uint64),
	}
}

var Default = NewRegistry()

// HTTPMiddleware records bounded route-template latency and status metrics.
func (r *Registry) HTTPMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		r.inflight.Add(1)
		started := time.Now()
		defer func() {
			r.inflight.Add(-1)
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			r.observeHTTP(c.Request.Method, route, c.Writer.Status(), time.Since(started))
		}()
		c.Next()
	}
}

func (r *Registry) observeHTTP(method, route string, status int, duration time.Duration) {
	key := httpMetricKey{
		Method:      boundedLabel(method, 16),
		Route:       boundedLabel(route, 160),
		StatusClass: strconv.Itoa(status/100) + "xx",
	}
	seconds := duration.Seconds()

	r.mu.Lock()
	metric := r.http[key]
	metric.Count++
	metric.Sum += seconds
	for index, bucket := range durationBuckets {
		if seconds <= bucket {
			metric.Buckets[index]++
		}
	}
	r.http[key] = metric
	r.mu.Unlock()
}

// SetDependency records the most recent health result for a bounded dependency
// name such as main_database, tool_store, or newapi.
func (r *Registry) SetDependency(name string, healthy bool) {
	name = boundedLabel(name, 64)
	value := 0.0
	if healthy {
		value = 1
	}
	r.mu.Lock()
	r.dependencies[name] = value
	r.mu.Unlock()
}

// SetLogFreshness publishes the newest observed NewAPI log timestamp and lag.
func (r *Registry) SetLogFreshness(latestUnix int64, lag time.Duration) {
	r.mu.Lock()
	r.freshnessSet = latestUnix > 0
	if r.freshnessSet {
		r.latestLog = float64(latestUnix)
		r.logLag = lag.Seconds()
		if r.logLag < 0 {
			r.logLag = 0
		}
	}
	r.mu.Unlock()
}

// IncOperation tracks bounded control-plane mutation outcomes. Callers should
// use a stable action name and a low-cardinality result such as success/error.
func (r *Registry) IncOperation(action, result string) {
	key := operationKey{Action: boundedLabel(action, 64), Result: boundedLabel(result, 32)}
	r.mu.Lock()
	r.operations[key]++
	r.mu.Unlock()
}

// Handler returns a token-protected Prometheus text endpoint. When no token is
// configured the endpoint behaves as not found instead of becoming public.
func (r *Registry) Handler(token string) gin.HandlerFunc {
	token = strings.TrimSpace(token)
	return func(c *gin.Context) {
		if token == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(r.snapshot()))
	}
}

func (r *Registry) snapshot() string {
	r.mu.RLock()
	httpMetrics := make(map[httpMetricKey]httpMetric, len(r.http))
	for key, value := range r.http {
		httpMetrics[key] = value
	}
	dependencies := make(map[string]float64, len(r.dependencies))
	for key, value := range r.dependencies {
		dependencies[key] = value
	}
	operations := make(map[operationKey]uint64, len(r.operations))
	for key, value := range r.operations {
		operations[key] = value
	}
	freshnessSet, logLag, latestLog := r.freshnessSet, r.logLag, r.latestLog
	r.mu.RUnlock()

	var output strings.Builder
	fmt.Fprintf(&output, "# HELP new_api_tools_build_info Build and release identity.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_build_info gauge\n")
	fmt.Fprintf(&output, "new_api_tools_build_info{version=%q,commit=%q,build_date=%q} 1\n", escapeLabel(buildinfo.Version), escapeLabel(buildinfo.Commit), escapeLabel(buildinfo.BuildDate))
	fmt.Fprintf(&output, "# HELP new_api_tools_process_uptime_seconds Process uptime in seconds.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_process_uptime_seconds gauge\n")
	fmt.Fprintf(&output, "new_api_tools_process_uptime_seconds %.3f\n", time.Since(r.startedAt).Seconds())
	fmt.Fprintf(&output, "# HELP new_api_tools_http_inflight_requests Current in-flight HTTP requests.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_http_inflight_requests gauge\n")
	fmt.Fprintf(&output, "new_api_tools_http_inflight_requests %d\n", r.inflight.Load())

	httpKeys := make([]httpMetricKey, 0, len(httpMetrics))
	for key := range httpMetrics {
		httpKeys = append(httpKeys, key)
	}
	sort.Slice(httpKeys, func(i, j int) bool {
		if httpKeys[i].Route != httpKeys[j].Route {
			return httpKeys[i].Route < httpKeys[j].Route
		}
		if httpKeys[i].Method != httpKeys[j].Method {
			return httpKeys[i].Method < httpKeys[j].Method
		}
		return httpKeys[i].StatusClass < httpKeys[j].StatusClass
	})
	fmt.Fprintf(&output, "# HELP new_api_tools_http_requests_total HTTP requests by method, route template, and status class.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_http_requests_total counter\n")
	fmt.Fprintf(&output, "# HELP new_api_tools_http_request_duration_seconds HTTP request duration by route template.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_http_request_duration_seconds histogram\n")
	for _, key := range httpKeys {
		metric := httpMetrics[key]
		labels := fmt.Sprintf("method=%q,route=%q,status_class=%q", escapeLabel(key.Method), escapeLabel(key.Route), escapeLabel(key.StatusClass))
		fmt.Fprintf(&output, "new_api_tools_http_requests_total{%s} %d\n", labels, metric.Count)
		for index, bucket := range durationBuckets {
			fmt.Fprintf(&output, "new_api_tools_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(bucket, 'f', -1, 64), metric.Buckets[index])
		}
		fmt.Fprintf(&output, "new_api_tools_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, metric.Count)
		fmt.Fprintf(&output, "new_api_tools_http_request_duration_seconds_sum{%s} %.9f\n", labels, metric.Sum)
		fmt.Fprintf(&output, "new_api_tools_http_request_duration_seconds_count{%s} %d\n", labels, metric.Count)
	}

	dependencyNames := make([]string, 0, len(dependencies))
	for name := range dependencies {
		dependencyNames = append(dependencyNames, name)
	}
	sort.Strings(dependencyNames)
	fmt.Fprintf(&output, "# HELP new_api_tools_dependency_up Last dependency health result (1 healthy, 0 unhealthy).\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_dependency_up gauge\n")
	for _, name := range dependencyNames {
		fmt.Fprintf(&output, "new_api_tools_dependency_up{dependency=%q} %.0f\n", escapeLabel(name), dependencies[name])
	}

	if freshnessSet {
		fmt.Fprintf(&output, "# HELP new_api_tools_log_freshness_lag_seconds Seconds since the newest NewAPI log.\n")
		fmt.Fprintf(&output, "# TYPE new_api_tools_log_freshness_lag_seconds gauge\n")
		fmt.Fprintf(&output, "new_api_tools_log_freshness_lag_seconds %.3f\n", logLag)
		fmt.Fprintf(&output, "# HELP new_api_tools_latest_log_timestamp_seconds Unix timestamp of the newest NewAPI log.\n")
		fmt.Fprintf(&output, "# TYPE new_api_tools_latest_log_timestamp_seconds gauge\n")
		fmt.Fprintf(&output, "new_api_tools_latest_log_timestamp_seconds %.0f\n", latestLog)
	}

	operationKeys := make([]operationKey, 0, len(operations))
	for key := range operations {
		operationKeys = append(operationKeys, key)
	}
	sort.Slice(operationKeys, func(i, j int) bool {
		if operationKeys[i].Action != operationKeys[j].Action {
			return operationKeys[i].Action < operationKeys[j].Action
		}
		return operationKeys[i].Result < operationKeys[j].Result
	})
	fmt.Fprintf(&output, "# HELP new_api_tools_control_operations_total Audited control-plane operation outcomes.\n")
	fmt.Fprintf(&output, "# TYPE new_api_tools_control_operations_total counter\n")
	for _, key := range operationKeys {
		fmt.Fprintf(&output, "new_api_tools_control_operations_total{action=%q,result=%q} %d\n", escapeLabel(key.Action), escapeLabel(key.Result), operations[key])
	}

	return output.String()
}

func boundedLabel(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if len(value) > max {
		return value[:max]
	}
	return value
}

func escapeLabel(value string) string {
	// Every caller renders this value with %q, which applies the escaping
	// required by the Prometheus text format for quotes, backslashes, and lines.
	return value
}
