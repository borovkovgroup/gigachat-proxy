package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
)

type appConfig struct {
	ListenAddr string `env:"LISTEN_ADDR" envDefault:":8080"`
	LogLevel   string `env:"LOG_LEVEL"   envDefault:"info"`

	Adapter adapterConfig
}

func main() {
	// Healthcheck subcommand: exits 0 if /healthz returns 200, 1 otherwise.
	// Used by Docker HEALTHCHECK since distroless has no curl/wget.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg appConfig
	if err := env.Parse(&cfg); err != nil {
		return fmt.Errorf("parse env: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("starting gigachat-proxy",
		slog.String("listen", cfg.ListenAddr),
		slog.String("api_url", cfg.Adapter.APIURL),
		slog.Int("concurrent_limit", cfg.Adapter.ConcurrentLimit),
		slog.Duration("timeout", cfg.Adapter.Timeout),
		slog.String("scope", cfg.Adapter.Scope),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	adapter, err := newGigaChatAdapter(ctx, &cfg.Adapter, logger)
	if err != nil {
		return fmt.Errorf("init adapter: %w", err)
	}
	defer adapter.Close()

	srv := newServer(cfg.ListenAddr, adapter, logger)

	if err := runHTTPServer(ctx, srv, logger); err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	logger.Info("stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// runHealthcheck calls /healthz on our own listen address and returns shell exit code.
// Reads LISTEN_ADDR from env so it stays in sync with the server.
func runHealthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// ":8080" → "127.0.0.1:8080"
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(os.Stderr, "healthcheck: status %d body %s\n", resp.StatusCode, string(body))
		return 1
	}
	return 0
}
