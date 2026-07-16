package handler

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/buildinfo"
	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/models"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
)

const dependencyCheckTimeout = 5 * time.Second

type newAPIStatusClient interface {
	Status(context.Context) (*newapi.Status, error)
}

// HealthHandler owns liveness, readiness, dependency diagnostics, and the
// version-gated NewAPI capability view for this control-plane process.
type HealthHandler struct {
	cfg       *config.Config
	newAPI    newAPIStatusClient
	toolStore *toolstore.Store
	metrics   *observability.Registry
}

type DependencyCheck struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Required   bool           `json:"required"`
	Diagnostic bool           `json:"diagnostic,omitempty"`
	LatencyMS  int64          `json:"latency_ms"`
	Details    map[string]any `json:"details,omitempty"`
	OK         bool           `json:"-"`
}

func NewHealthHandler(cfg *config.Config, client newAPIStatusClient, store *toolstore.Store, metrics *observability.Registry) *HealthHandler {
	if metrics == nil {
		metrics = observability.Default
	}
	return &HealthHandler{cfg: cfg, newAPI: client, toolStore: store, metrics: metrics}
}

// RegisterHealthRoutes is kept for compatibility with tests and embedders. The
// production server uses a configured HealthHandler so Tool Store and NewAPI
// state are included in readiness and dependency diagnostics.
func RegisterHealthRoutes(r *gin.Engine) {
	h := NewHealthHandler(config.GetOptional(), nil, nil, observability.Default)
	r.GET("/livez", h.Liveness)
	r.GET("/api/health", h.HealthCheck)
	r.GET("/api/health/db", h.DatabaseHealthCheck)
}

func (h *HealthHandler) RegisterPublicRoutes(r *gin.Engine) {
	r.GET("/livez", h.Liveness)
	r.GET("/readyz", h.Readiness)
	r.GET("/api/health", h.HealthCheck)
	r.GET("/api/health/db", h.DatabaseHealthCheck)
}

func (h *HealthHandler) RegisterProtectedRoutes(api *gin.RouterGroup) {
	api.GET("/health/dependencies", h.DependencyHealth)
	api.GET("/control-plane/newapi/capabilities", h.NewAPICapabilities)
}

func (h *HealthHandler) Liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "alive",
		"version": buildinfo.Version,
	})
}

func (h *HealthHandler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, models.HealthResponse{
		Status:  "healthy",
		Version: buildinfo.Version,
	})
}

// Readiness covers only dependencies required to serve the control plane. An
// optional NewAPI/Redis/log-source outage must not hide the recovery console.
func (h *HealthHandler) Readiness(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	ready := true
	if err := database.Get().DB.PingContext(ctx); err != nil {
		checks["main_database"] = "unavailable"
		ready = false
		h.metrics.SetDependency("main_database", false)
	} else {
		checks["main_database"] = "ok"
		h.metrics.SetDependency("main_database", true)
	}

	if h.toolStore == nil {
		checks["tool_store"] = "unavailable"
		ready = false
		h.metrics.SetDependency("tool_store", false)
	} else if _, err := h.toolStore.Health(ctx); err != nil {
		checks["tool_store"] = "unavailable"
		ready = false
		h.metrics.SetDependency("tool_store", false)
	} else {
		checks["tool_store"] = "ok"
		h.metrics.SetDependency("tool_store", true)
	}

	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	c.JSON(status, gin.H{"status": state, "checks": checks})
}

// DatabaseHealthCheck is a compatibility endpoint. It never returns DSNs,
// hosts, driver errors, or other connection details.
func (h *HealthHandler) DatabaseHealthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	db := database.Get()

	if err := db.DB.PingContext(ctx); err != nil {
		logger.L.DBError("Database health check failed")
		h.metrics.SetDependency("main_database", false)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"status":  "disconnected",
			"error": gin.H{
				"code":    "DB_CONNECTION_FAILED",
				"message": "Database connection unavailable",
			},
		})
		return
	}

	h.metrics.SetDependency("main_database", true)
	engine := "mysql"
	if db.IsPG {
		engine = "postgresql"
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "status": "connected", "engine": engine})
}

func (h *HealthHandler) DependencyHealth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), dependencyCheckTimeout)
	defer cancel()

	checks := h.collectDependencyChecks(ctx)
	overall := "healthy"
	statusCode := http.StatusOK
	for _, check := range checks {
		if check.OK || check.Diagnostic {
			continue
		}
		if check.Required {
			overall = "unhealthy"
			statusCode = http.StatusServiceUnavailable
			break
		}
		overall = "degraded"
	}
	if overall == "healthy" {
		for _, check := range checks {
			if !check.OK && !check.Diagnostic {
				overall = "degraded"
				break
			}
		}
	}

	c.JSON(statusCode, gin.H{
		"success":    statusCode == http.StatusOK,
		"status":     overall,
		"version":    buildinfo.Version,
		"checked_at": time.Now().UTC().Format(time.RFC3339Nano),
		"checks":     checks,
	})
}

func (h *HealthHandler) collectDependencyChecks(ctx context.Context) []DependencyCheck {
	checks := make([]DependencyCheck, 0, 6)
	var mu sync.Mutex
	var wg sync.WaitGroup
	add := func(check DependencyCheck) {
		mu.Lock()
		checks = append(checks, check)
		mu.Unlock()
		h.metrics.SetDependency(check.Name, check.OK)
	}
	run := func(name string, required, diagnostic bool, fn func(context.Context) DependencyCheck) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			check := fn(ctx)
			check.Name = name
			check.Required = required
			check.Diagnostic = diagnostic
			check.LatencyMS = time.Since(started).Milliseconds()
			add(check)
		}()
	}

	run("main_database", true, false, h.checkMainDatabase)
	run("tool_store", true, false, h.checkToolStore)
	run("log_database", false, false, h.checkLogDatabase)
	run("log_freshness", false, true, h.checkLogFreshness)
	run("newapi", false, false, h.checkNewAPI)
	run("redis", false, false, h.checkRedis)
	wg.Wait()
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	return checks
}

func (h *HealthHandler) checkMainDatabase(ctx context.Context) DependencyCheck {
	db := database.Get()
	if err := db.DB.PingContext(ctx); err != nil {
		return DependencyCheck{Status: "unhealthy", OK: false}
	}
	engine := "mysql"
	if db.IsPG {
		engine = "postgresql"
	}
	return DependencyCheck{Status: "healthy", OK: true, Details: map[string]any{"engine": engine}}
}

func (h *HealthHandler) checkToolStore(ctx context.Context) DependencyCheck {
	if h.toolStore == nil {
		return DependencyCheck{Status: "unhealthy", OK: false}
	}
	status, err := h.toolStore.Health(ctx)
	if err != nil {
		return DependencyCheck{Status: "unhealthy", OK: false}
	}
	return DependencyCheck{Status: "healthy", OK: true, Details: map[string]any{
		"schema_version":        status.SchemaVersion,
		"latest_schema_version": status.LatestSchemaVersion,
		"journal_mode":          status.JournalMode,
		"synchronous":           status.Synchronous,
		"foreign_keys":          status.ForeignKeys,
	}}
}

func (h *HealthHandler) checkLogDatabase(ctx context.Context) DependencyCheck {
	status := database.GetLogSourceStatus()
	if err := database.GetLog().DB.PingContext(ctx); err != nil {
		return DependencyCheck{Status: "unhealthy", OK: false, Details: map[string]any{
			"mode": status.Mode,
		}}
	}
	if status.UsingFallback || !status.Healthy {
		return DependencyCheck{Status: "degraded", OK: false, Details: map[string]any{
			"mode":           status.Mode,
			"using_fallback": status.UsingFallback,
		}}
	}
	return DependencyCheck{Status: "healthy", OK: true, Details: map[string]any{
		"mode":           status.Mode,
		"using_fallback": false,
	}}
}

func (h *HealthHandler) checkLogFreshness(ctx context.Context) DependencyCheck {
	var latest int64
	query := "SELECT COALESCE(MAX(created_at), 0) FROM logs"
	if err := database.GetLog().DB.QueryRowContext(ctx, query).Scan(&latest); err != nil {
		return DependencyCheck{Status: "unknown", OK: false}
	}
	if latest <= 0 {
		h.metrics.SetLogFreshness(0, 0)
		return DependencyCheck{Status: "empty", OK: true, Details: map[string]any{"latest_log_at": nil}}
	}

	latestAt := time.Unix(latest, 0)
	lag := time.Since(latestAt)
	if lag < 0 {
		ahead := -lag
		h.metrics.SetLogFreshness(latest, 0)
		return DependencyCheck{Status: "clock_skew", OK: false, Details: map[string]any{
			"latest_log_at": time.Unix(latest, 0).UTC().Format(time.RFC3339),
			"ahead_seconds": int64(ahead.Seconds()),
		}}
	}
	h.metrics.SetLogFreshness(latest, lag)
	maxAge := 15 * time.Minute
	if h.cfg != nil && h.cfg.LogFreshnessMaxAge > 0 {
		maxAge = h.cfg.LogFreshnessMaxAge
	}
	status := "fresh"
	ok := true
	if lag > maxAge {
		status = "stale"
		ok = false
	}
	return DependencyCheck{Status: status, OK: ok, Details: map[string]any{
		"latest_log_at":   latestAt.UTC().Format(time.RFC3339),
		"lag_seconds":     int64(lag.Seconds()),
		"max_age_seconds": int64(maxAge.Seconds()),
	}}
}

func (h *HealthHandler) checkNewAPI(ctx context.Context) DependencyCheck {
	configured := h.cfg != nil && h.cfg.NewAPIAdminAccessToken != "" && h.cfg.NewAPIAdminUserID > 0
	if h.newAPI == nil {
		return DependencyCheck{Status: "unhealthy", OK: false, Details: map[string]any{
			"admin_credentials_configured": configured,
		}}
	}
	status, err := h.newAPI.Status(ctx)
	if err != nil {
		return DependencyCheck{Status: "unhealthy", OK: false, Details: map[string]any{
			"admin_credentials_configured": configured,
		}}
	}
	capabilities := newapi.DetectCapabilities(status.Version)
	return DependencyCheck{Status: "healthy", OK: true, Details: map[string]any{
		"version":                      status.Version,
		"known_version":                capabilities.Known,
		"admin_credentials_configured": configured,
		"write_mode":                   writeMode(capabilities, configured),
	}}
}

func (h *HealthHandler) checkRedis(ctx context.Context) DependencyCheck {
	configured := h.cfg != nil && h.cfg.RedisConnString != ""
	if !configured {
		return DependencyCheck{Status: "disabled", OK: true, Details: map[string]any{"configured": false}}
	}
	if !cache.Available() || cache.Get().RedisClient() == nil {
		return DependencyCheck{Status: "unhealthy", OK: false, Details: map[string]any{"configured": true}}
	}
	if err := cache.Get().RedisClient().Ping(ctx).Err(); err != nil {
		return DependencyCheck{Status: "unhealthy", OK: false, Details: map[string]any{"configured": true}}
	}
	return DependencyCheck{Status: "healthy", OK: true, Details: map[string]any{"configured": true}}
}

func (h *HealthHandler) NewAPICapabilities(c *gin.Context) {
	if h.newAPI == nil {
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"NEWAPI_UNAVAILABLE",
			"NewAPI adapter is not available",
		))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), dependencyCheckTimeout)
	defer cancel()
	status, err := h.newAPI.Status(ctx)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, models.NewErrorResponse(
			"NEWAPI_UNAVAILABLE",
			"NewAPI status probe failed",
		))
		return
	}

	capabilities := newapi.DetectCapabilities(status.Version)
	credentialsConfigured := h.cfg != nil && h.cfg.NewAPIAdminAccessToken != "" && h.cfg.NewAPIAdminUserID > 0
	c.JSON(http.StatusOK, models.NewSuccessResponse(gin.H{
		"status":                       status,
		"capabilities":                 capabilities,
		"admin_credentials_configured": credentialsConfigured,
		"write_mode":                   writeMode(capabilities, credentialsConfigured),
		"checked_at":                   time.Now().UTC().Format(time.RFC3339Nano),
	}))
}

func writeMode(capabilities newapi.Capabilities, credentialsConfigured bool) string {
	if !capabilities.Known || capabilities.UnknownVersionReadOnly || !credentialsConfigured {
		return "read_only"
	}
	return "admin_api"
}
