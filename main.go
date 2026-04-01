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
	flag.Parse()

	if *addUser {
		runAddUser(*configPath)
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cs, err := NewConfigStore(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	cfg := cs.Get()

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

	proxy := NewProxyHandler(cs)
	models := NewModelsHandler(cs)
	rl := NewRateLimiter(cfg.TrustedProxies)

	configPage := NewConfigPageHandler(cs)

	mux := http.NewServeMux()
	mux.Handle("GET /{$}", configPage)
	mux.Handle("GET /v1/models", RateLimitMiddleware(rl, AuthMiddleware(cs, models)))
	mux.Handle("/v1/", RateLimitMiddleware(rl, AuthMiddleware(cs, proxy)))
	mux.Handle("/anthropic/", RateLimitMiddleware(rl, AuthMiddleware(cs, proxy)))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
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
