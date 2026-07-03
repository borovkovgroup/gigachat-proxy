package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

func newServer(addr string, adapter *gigaChatAdapter, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		if !adapter.HasToken() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting", "reason": "no_token"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Everything under /v1/ → adapter (chat/completions, embeddings, models, …)
	mux.Handle("/v1/", logAccess(logger, adapter))

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard
		WriteTimeout:      0,                // streaming: no per-write deadline (per-request timeout enforced inside adapter)
		IdleTimeout:       120 * time.Second,
		// ReadTimeout left at 0: embeddings batches with many long inputs can take a while to upload.
	}
}

// logAccess wraps a handler with structured access logging.
// Returned ResponseWriter wrapper preserves http.Flusher (critical for SSE).
func logAccess(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := newStatusWriter(w)
		h.ServeHTTP(sw, r)
		logger.Info("access",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("dur", time.Since(start)),
		)
	})
}

// statusWriter records the response status while delegating Flush() so that
// httputil.ReverseProxy's streaming detection still works through the wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher — required for SSE/streaming through ReverseProxy.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func runHTTPServer(ctx context.Context, srv *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("err", err.Error()))
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}
