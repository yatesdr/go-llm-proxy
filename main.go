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
		runAddUser(*configPath)
		return
	}

	// Handle report modes: load config to find DB path, then print report and exit.
	if *usageReport || *modelReport {
		dbPath := *usageDBPath
		if dbPath == "" {
			cs, err := NewConfigStore(*configPath)
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
			RunUsageReport(dbPath, *reportDays)
		}
		if *modelReport {
			RunModelReport(dbPath, *reportDays)
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

	cs, err := NewConfigStore(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	cfg := cs.Get()

	// Initialize usage logger if enabled via CLI flag or config.
	var usage *UsageLogger
	if *logMetrics || cfg.LogMetrics {
		dbPath := *usageDBPath
		if dbPath == "" && cfg.UsageDB != "" {
			dbPath = cfg.UsageDB
		}
		if dbPath == "" {
			dbPath = "usage.db"
		}
		var err error
		usage, err = NewUsageLogger(dbPath)
		if err != nil {
			slog.Error("failed to open usage database", "error", err, "path", dbPath)
			os.Exit(1)
		}
		slog.Info("usage logging enabled", "db", dbPath)
	}

	// Auto-detect context window sizes from backends (async, non-blocking).
	DetectContextWindows(cs)

	proxy := NewProxyHandler(cs, usage)
	responses := NewResponsesHandler(cs, usage)
	messages := NewMessagesHandler(cs, usage)
	models := NewModelsHandler(cs)
	rl := NewRateLimiter(cfg.TrustedProxies)

	var dashRl *RateLimiter
	if (*serveDashboard || cfg.UsageDashboard) && usage != nil {
		dashRl = NewRateLimiter(cfg.TrustedProxies)
	}

	cs.SetOnReload(func(newCfg *Config) {
		rl.SetTrustedProxies(newCfg.TrustedProxies)
		if dashRl != nil {
			dashRl.SetTrustedProxies(newCfg.TrustedProxies)
		}
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
		configPage := NewConfigPageHandler(cs)
		mux.Handle("GET /{$}", configPage)
		slog.Info("config generator page enabled at GET /")
	}
	if dashRl != nil {
		dash := NewUsageDashboardHandler(cs, usage, dashRl)
		mux.Handle("GET /usage", http.HandlerFunc(dash.LoginPage))
		mux.Handle("POST /usage", http.HandlerFunc(dash.HandleLogin))
		mux.Handle("GET /usage/data", http.HandlerFunc(dash.ServeData))
		slog.Info("usage dashboard enabled at /usage")
	}
	mux.Handle("GET /v1/models", RateLimitMiddleware(rl, AuthMiddleware(cs, models)))
	mux.Handle("POST /v1/responses", RateLimitMiddleware(rl, AuthMiddleware(cs, responses)))
	mux.Handle("POST /v1/responses/compact", RateLimitMiddleware(rl, AuthMiddleware(cs,
		http.HandlerFunc(responses.HandleCompact),
	)))
	mux.Handle("POST /v1/messages", RateLimitMiddleware(rl, AuthMiddleware(cs, messages)))
	mux.Handle("POST /anthropic/v1/messages", RateLimitMiddleware(rl, AuthMiddleware(cs, messages)))
	mux.Handle("/v1/", RateLimitMiddleware(rl, AuthMiddleware(cs, proxy)))
	mux.Handle("/anthropic/", RateLimitMiddleware(rl, AuthMiddleware(cs, proxy)))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           RecoveryMiddleware(mux),
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
		if usage != nil {
			usage.Close()
		}
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
