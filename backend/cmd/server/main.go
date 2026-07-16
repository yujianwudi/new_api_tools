package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/auth"
	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/controlplane"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/handler"
	"github.com/new-api-tools/backend/internal/logger"
	"github.com/new-api-tools/backend/internal/middleware"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
)

func main() {
	// ========== 1. Load configuration ==========
	cfg := config.Load()

	// ========== 2. Initialize logger ==========
	logger.Init(cfg.LogLevel, cfg.LogFile)
	logger.L.Banner("🚀 NewAPI Middleware Tool - Go Backend")
	logger.L.System(fmt.Sprintf("服务器地址: %s", cfg.ServerAddr()))
	logger.L.System(fmt.Sprintf("数据库引擎: %s", cfg.DatabaseEngine))
	logger.L.System(fmt.Sprintf("时区: %s", cfg.TimeZone))

	// ========== 3. Initialize database ==========
	_, err := database.Init(cfg)
	if err != nil {
		logger.L.Fatal("数据库初始化失败: " + err.Error())
	}
	defer database.Close()

	// ========== 4. Initialize the independent control-plane store ==========
	toolStore, err := toolstore.Init(cfg.ToolStorePath)
	if err != nil {
		logger.L.Fatal("Tool Store initialization failed: " + err.Error())
	}
	defer toolStore.Close()

	// NewAPI is an adapter dependency, not this process's persistence layer. A
	// bad endpoint disables upstream operations but keeps the recovery console
	// available for database and audit diagnostics.
	var newAPIClient *newapi.Client
	newAPIClient, err = newapi.NewClient(
		cfg.NewAPIBaseURL,
		cfg.NewAPIAdminAccessToken,
		cfg.NewAPIAdminUserID,
		nil,
	)
	if err != nil {
		newAPIClient = nil
		logger.L.Warn("NewAPI adapter disabled: NEWAPI_BASEURL is invalid")
	}

	// ========== 5. Initialize Redis cache ==========
	if cfg.RedisConnString != "" {
		_, err := cache.Init(cfg.RedisConnString)
		if err != nil {
			logger.L.Warn("Redis 连接失败，将使用无缓存模式: " + err.Error())
		}
	} else {
		logger.L.Warn("REDIS_CONN_STRING 未配置，缓存功能不可用")
	}
	defer cache.Close()

	// ========== 6. Setup Gin router ==========
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	trustedProxyCIDRs, trustedProxyConfigValid := handler.TrustedProxyCIDRsForGin(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if !trustedProxyConfigValid {
		logger.L.Warn("TRUSTED_PROXY_CIDRS 配置无效，Gin 客户端 IP 解析已禁用代理信任")
		trustedProxyCIDRs = nil
	}
	if err := r.SetTrustedProxies(trustedProxyCIDRs); err != nil {
		logger.L.Warn("Gin 可信代理配置失败，已禁用代理信任: " + err.Error())
		_ = r.SetTrustedProxies(nil)
	}

	// Global middleware
	r.Use(middleware.RequestIDMiddleware())       // Correlation and upstream propagation
	r.Use(observability.Default.HTTPMiddleware()) // Prometheus-compatible SLI metrics
	r.Use(middleware.RequestLoggerMiddleware())   // Request logging
	r.Use(middleware.ErrorHandlerMiddleware())    // Panic recovery
	r.Use(middleware.CORSMiddleware())            // CORS

	// ========== 7. Register routes ==========

	// Health check (no auth required)
	healthHandler := handler.NewHealthHandler(cfg, newAPIClient, toolStore, observability.Default)
	mutationService := controlplane.NewService(
		newAPIClient,
		toolStore,
		database.Get(),
		observability.Default,
		controlplane.RedemptionLimits{
			MaxQuotaPerCode: cfg.RedemptionMaxQuotaPerCode,
			MaxTotalQuota:   cfg.RedemptionMaxTotalQuota,
		},
	)
	mutationHandler := handler.NewMutationHandler(mutationService)
	storeHandler := handler.NewStoreHandler(toolStore)
	searchHandler := handler.NewSearchHandler(toolStore)
	healthHandler.RegisterPublicRoutes(r)
	r.GET("/metrics", observability.Default.Handler(cfg.ObservabilityToken))

	// API group with authentication
	api := r.Group("/api")
	api.Use(auth.AuthMiddleware())
	api.Use(auth.RBACMiddleware())
	{
		healthHandler.RegisterProtectedRoutes(api)
		mutationHandler.RegisterControlPlaneMutationRoutes(api)
		storeHandler.RegisterRoutes(api)
		searchHandler.RegisterRoutes(api)
		api.GET("/control-plane/channel-quality", handler.GetChannelQuality)

		// Auth routes (login/logout are whitelisted in middleware)
		handler.RegisterAuthRoutes(api)

		// Phase 2.1: Basic modules
		handler.RegisterRedemptionRoutes(api, mutationHandler)
		handler.RegisterTopUpRoutes(api, toolStore)
		handler.RegisterTopUpAnalyticsRoutes(api)
		handler.RegisterStorageRoutes(api)
		handler.RegisterSystemRoutes(api)

		// Phase 2.2: Dashboard, UserManagement, LogAnalytics
		handler.RegisterDashboardRoutes(api)
		handler.RegisterUserManagementRoutes(api, mutationHandler)
		handler.RegisterAffiliateStatsRoutes(api)
		handler.RegisterLogAnalyticsRoutes(api)

		// Phase 2.3: IP Monitoring, Risk Monitoring, Model Status
		handler.RegisterIPMonitoringRoutes(api)
		handler.RegisterRiskMonitoringRoutes(api)
		handler.RegisterModelStatusRoutes(api)
		// The legacy abuse-broadcast implementation creates sidecar tables inside
		// NewAPI's database. v0.5 replaces that boundary with Tool Store risk cases,
		// so the legacy routes and background writer are intentionally not mounted.

		// Phase 2.4: Token Management
		handler.RegisterTokenRoutes(api)

		// Phase 3: AI AutoBan, AutoGroup, LinuxDo Lookup
		handler.RegisterAIAutoBanRoutes(api)
		handler.RegisterAutoGroupRoutes(api)
		handler.RegisterLinuxDoRoutes(api)
	}

	// Public embed routes (no auth)
	handler.RegisterModelStatusEmbedRoutes(r)

	// ========== 8. Start server with graceful shutdown ==========
	srv := &http.Server{
		Addr:         cfg.ServerAddr(),
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in goroutine
	go func() {
		logger.L.Success(fmt.Sprintf("服务已启动: http://%s", cfg.ServerAddr()))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.L.Fatal("服务启动失败: " + err.Error())
		}
	}()

	// ========== 9. Wait for interrupt signal ==========
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.L.System("正在优雅关闭服务...")

	// Give the server 10 seconds to finish processing requests
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.L.Error("服务关闭异常: " + err.Error())
	}

	logger.L.Success("服务已关闭")
}
