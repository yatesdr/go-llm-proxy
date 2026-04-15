package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/handler"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/mcp"
	"go-llm-proxy/internal/pipeline"
	"go-llm-proxy/internal/ratelimit"
	"go-llm-proxy/internal/usage"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	addUser := flag.Bool("adduser", false, "interactively add a new API key to the config")
	serveConfigPage := flag.Bool("serve-config-generator", false, "serve the config generator UI at GET /")
	serveDashboard := flag.Bool("serve-dashboard", false, "serve the usage dashboard at /usage")
	logMetrics := flag.Bool("log-metrics", false, "enable per-request usage logging to SQLite")
	usageReport := flag.Bool("usage-report", false, "print usage summary report and exit")
	modelReport := flag.Bool("model-report", false, "print per-model usage report and exit")
	reportDays := flag.Int("report-days", 30, "number of days to include in reports")
	usageDBPath := flag.String("usage-db", "", "path to SQLite usage database (overrides config)")
	logDebug := flag.Bool("log-debug", false, "enable debug-level logging for translation troubleshooting")
	flag.Parse()

	if *addUser {
		auth.RunAddUser(*configPath)
		return
	}

	// Handle report modes: load config to find DB path, then print report and exit.
	if *usageReport || *modelReport {
		dbPath := *usageDBPath
		if dbPath == "" {
			cs, err := config.NewConfigStore(*configPath)
			if err == nil {
				if cfg := cs.Get(); cfg.UsageDB != "" {
					dbPath = cfg.UsageDB
				}
			}
		}
		if dbPath == "" {
			dbPath = "usage.db"
		}
		if *usageReport {
			usage.RunUsageReport(dbPath, *reportDays)
		}
		if *modelReport {
			usage.RunModelReport(dbPath, *reportDays)
		}
		return
	}

	logLevel := slog.LevelInfo
	if *logDebug {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	cs, err := config.NewConfigStore(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	cfg := cs.Get()

	// Initialize usage logger if enabled via CLI flag or config.
	var ul *usage.UsageLogger
	if *logMetrics || cfg.LogMetrics {
		dbPath := *usageDBPath
		if dbPath == "" && cfg.UsageDB != "" {
			dbPath = cfg.UsageDB
		}
		if dbPath == "" {
			dbPath = "usage.db"
		}
		var err error
		ul, err = usage.NewUsageLogger(dbPath)
		if err != nil {
			slog.Error("failed to open usage database", "error", err, "path", dbPath)
			os.Exit(1)
		}
		slog.Info("usage logging enabled", "db", dbPath)
	}

	// Auto-detect context window sizes from backends (async, non-blocking).
	config.DetectContextWindows(cs)

	// Start health checker for model availability tracking.
	healthStore := config.NewHealthStore(cs, 30*time.Second, 5*time.Second)

	// Create the processing pipeline (shared by all handlers).
	pl := pipeline.NewPipeline(cs, httputil.NewHTTPClient())

	proxy := handler.NewProxyHandler(cs, ul, pl)
	responses := handler.NewResponsesHandler(cs, ul, pl)
	messages := handler.NewMessagesHandler(cs, ul, pl)
	models := handler.NewModelsHandler(cs, healthStore)
	rl := ratelimit.NewRateLimiter(cfg.TrustedProxies)

	var dashRl *ratelimit.RateLimiter
	if (*serveDashboard || cfg.UsageDashboard) && ul != nil {
		dashRl = ratelimit.NewRateLimiter(cfg.TrustedProxies)
	}

	cs.SetOnReload(func(newCfg *config.Config) {
		rl.SetTrustedProxies(newCfg.TrustedProxies)
		healthStore.RefreshFromConfig()
		if dashRl != nil {
			dashRl.SetTrustedProxies(newCfg.TrustedProxies)
		}
		config.DetectContextWindows(cs)
	})

	// Watch config file for changes (auto-reload on save).
	stopWatch, err := cs.Watch()
	if err != nil {
		slog.Error("failed to watch config file", "error", err)
		// Non-fatal: SIGHUP still works as a fallback.
	}

	// Reload config on SIGHUP (manual trigger, systemd ExecReload).
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP)
		for range sig {
			slog.Info("received SIGHUP, reloading config")
			if err := cs.Load(); err != nil {
				slog.Error("failed to reload config", "error", err)
			}
		}
	}()

	mux := http.NewServeMux()
	if *serveConfigPage || cfg.ServeConfigGenerator {
		configPage := handler.NewConfigPageHandler(cs, healthStore)
		mux.Handle("GET /{$}", configPage)
		slog.Info("config generator page enabled at GET /")
	}
	if dashRl != nil {
		dash := handler.NewUsageDashboardHandler(cs, ul, dashRl)
		mux.Handle("GET /usage", http.HandlerFunc(dash.LoginPage))
		mux.Handle("POST /usage", http.HandlerFunc(dash.HandleLogin))
		mux.Handle("POST /usage/logout", http.HandlerFunc(dash.HandleLogout))
		mux.Handle("GET /usage/data", http.HandlerFunc(dash.ServeData))
		slog.Info("usage dashboard enabled at /usage")
	}
	mux.Handle("GET /v1/models", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, models)))
	mux.Handle("GET /v1/models/status", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, http.HandlerFunc(models.ServeStatus))))
	mux.Handle("POST /v1/responses", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, responses)))
	mux.Handle("POST /v1/responses/compact", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs,
		http.HandlerFunc(responses.HandleCompact),
	)))
	countTokens := handler.NewCountTokensHandler(cs, ul)
	mux.Handle("POST /v1/messages/count_tokens", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, countTokens)))
	mux.Handle("POST /anthropic/v1/messages/count_tokens", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, countTokens)))
	mux.Handle("POST /v1/messages", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, messages)))
	mux.Handle("POST /anthropic/v1/messages", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, messages)))
	mux.Handle("/v1/", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, proxy)))
	mux.Handle("/anthropic/", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, proxy)))

	// MCP endpoint for web search (OpenCode, Qwen Code, any MCP client).
	mcpHandler := mcp.NewHandler(cs, pl)
	mux.Handle("GET /mcp/sse", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, http.HandlerFunc(mcpHandler.ServeSSE))))
	mux.Handle("POST /mcp/messages", ratelimit.RateLimitMiddleware(rl, auth.AuthMiddleware(cs, http.HandlerFunc(mcpHandler.ServeMessages))))
	if cfg.Processors.WebSearchKey != "" {
		slog.Info("MCP endpoint enabled at /mcp/sse (web_search tool available)")
	}

	// Qdrant vector database proxy with app isolation.
	if cfg.Services.Qdrant != nil {
		qdrant := handler.NewQdrantHandler(cs, ul)
		mux.Handle("/qdrant/", ratelimit.RateLimitMiddleware(rl, auth.AppKeyAuthMiddleware(cs, qdrant)))
		slog.Info("qdrant proxy enabled at /qdrant/", "backend", cfg.Services.Qdrant.Backend, "app_keys", len(cfg.Services.Qdrant.AppKeys))
	}

	// Start health checker background goroutine.
	healthStore.Start(context.Background())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httputil.RecoveryMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64 KB
		// Note: no WriteTimeout — streaming SSE responses can run for minutes.
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
		sig := <-quit
		slog.Info("shutting down", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
		rl.Close()
		if dashRl != nil {
			dashRl.Close()
		}
		if ul != nil {
			ul.Close()
		}
		healthStore.Stop()

		if stopWatch != nil {
			stopWatch()
		}
	}()

	slog.Info("starting server", "listen", cfg.Listen)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
