package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/google/uuid"
)

// Inlined from FlowU's flowu-backend/internal/shared/httpserver to avoid pulling the package.
const (
	headerAuthorization = "Authorization"
	headerAccept        = "Accept"
	headerRequestID     = "RqUID"
	prefixBearer        = "Bearer "
	contentTypeJSON     = "application/json"
)

type adapterConfig struct {
	OAuthURL        string        `env:"GIGA_OAUTH_URL" envDefault:"https://ngw.devices.sberbank.ru:9443/api/v2/oauth"`
	APIURL          string        `env:"GIGA_API_URL"   envDefault:"https://gigachat.devices.sberbank.ru"`
	OAuthToken      string        `env:"GIGA_AUTH_TOKEN,required"`
	Scope           string        `env:"GIGA_SCOPE"     envDefault:"GIGACHAT_API_PERS"`
	ConcurrentLimit int           `env:"GIGA_CONCURRENT_LIMIT" envDefault:"16"`
	Timeout         time.Duration `env:"GIGA_TIMEOUT"          envDefault:"5m"`
	SkipTLSVerify   bool          `env:"GIGA_SKIP_TLS_VERIFY"  envDefault:"true"`
}

type gigaChatAdapter struct {
	logger          *slog.Logger
	cancelRefresher context.CancelFunc

	config      *adapterConfig
	proxy       *httputil.ReverseProxy
	restyClient *resty.Client

	concurrentLimit chan struct{}

	tokenMu        sync.RWMutex
	accessToken    string
	tokenExpiresAt time.Time
}

func newGigaChatAdapter(ctx context.Context, cfg *adapterConfig, logger *slog.Logger) (*gigaChatAdapter, error) {
	apiURL, err := url.Parse(cfg.APIURL)
	if err != nil {
		return nil, fmt.Errorf("parse GIGA_API_URL: %w", err)
	}

	// Tuned transport — defaults of net/http (MaxIdleConnsPerHost=2) would force
	// new TCP+TLS handshakes for every concurrent request, killing latency.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.SkipTLSVerify, //nolint:gosec // controlled via env
			MinVersion:         tls.VersionTLS12,
		},
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     64,
		IdleConnTimeout:     90 * time.Second,
		WriteBufferSize:     64 * 1024,
		ReadBufferSize:      64 * 1024,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // SSE: flush on every write so streaming chunks reach the client immediately
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(apiURL) // sets Scheme/Host on Out URL and Out.Host header
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.ErrorContext(r.Context(), "upstream proxy error",
				slog.String("path", r.URL.Path),
				slog.String("err", err.Error()))
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	if cfg.SkipTLSVerify {
		logger.Warn("GigaChat TLS verification is DISABLED (GIGA_SKIP_TLS_VERIFY=true). Add Sber root CA to truststore and set false for production.")
	}

	a := &gigaChatAdapter{
		logger: logger.With(slog.String("component", "gigachat-adapter")),
		config: cfg,
		proxy:  proxy,
		restyClient: resty.New().
			SetTLSClientConfig(&tls.Config{
				InsecureSkipVerify: cfg.SkipTLSVerify, //nolint:gosec // controlled via env
				MinVersion:         tls.VersionTLS12,
			}).
			SetTimeout(15 * time.Second).
			SetRetryCount(2).
			SetRetryWaitTime(1 * time.Second).
			SetRetryMaxWaitTime(5 * time.Second),
		concurrentLimit: make(chan struct{}, cfg.ConcurrentLimit),
	}

	// Synchronous first refresh — fail fast on bad credentials, gives healthcheck a true signal.
	if err := a.refreshNow(ctx); err != nil {
		return nil, fmt.Errorf("initial token refresh: %w", err)
	}

	ctxRefresher, cancelRefresher := context.WithCancel(context.Background())
	a.cancelRefresher = cancelRefresher
	go a.runRefresher(ctxRefresher)

	return a, nil
}

// ServeHTTP implements http.Handler. Mounted at /v1/, proxies to {APIURL}/api/v1/.
func (a *gigaChatAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := a.logger.With(
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)

	ctx, cancel := context.WithTimeout(r.Context(), a.config.Timeout)
	defer cancel()
	r = r.WithContext(ctx)

	ok, release := a.acquire(ctx)
	if !ok {
		http.Error(w, "timeout waiting for upstream slot", http.StatusGatewayTimeout)
		logger.WarnContext(ctx, "failed to acquire concurrency slot")
		return
	}
	defer release()

	a.tokenMu.RLock()
	token := a.accessToken
	a.tokenMu.RUnlock()
	if token == "" {
		http.Error(w, "auth token not ready", http.StatusServiceUnavailable)
		logger.WarnContext(ctx, "request before token ready")
		return
	}

	// Path rewrite: /v1/chat/completions  →  /api/v1/chat/completions
	r.URL.Path = "/api" + r.URL.Path

	// Replace whatever the client sent with our refreshed Bearer.
	r.Header.Set(headerAuthorization, prefixBearer+token)

	a.proxy.ServeHTTP(w, r)
}

// HasToken — for /healthz.
func (a *gigaChatAdapter) HasToken() bool {
	a.tokenMu.RLock()
	defer a.tokenMu.RUnlock()
	return a.accessToken != ""
}

// Close stops the background refresher; safe to call multiple times via main's defer.
func (a *gigaChatAdapter) Close() {
	if a.cancelRefresher != nil {
		a.cancelRefresher()
	}
	a.logger.Info("token refresher stopped")
}

func (a *gigaChatAdapter) acquire(ctx context.Context) (bool, func()) {
	select {
	case a.concurrentLimit <- struct{}{}:
		return true, func() { <-a.concurrentLimit }
	case <-ctx.Done():
		return false, nil
	}
}

func (a *gigaChatAdapter) runRefresher(ctx context.Context) {
	const (
		minDelay     = 15 * time.Second
		refreshRatio = 0.8
	)
	a.logger.InfoContext(ctx, "token refresher loop started")

	for {
		a.tokenMu.RLock()
		expiresAt := a.tokenExpiresAt
		a.tokenMu.RUnlock()

		delay := time.Duration(float64(time.Until(expiresAt)) * refreshRatio)
		if delay < minDelay {
			delay = minDelay
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := a.refreshNow(ctx); err != nil {
			a.logger.ErrorContext(ctx, "refresh failed, retrying after minDelay",
				slog.String("err", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(minDelay):
			}
		}
	}
}

func (a *gigaChatAdapter) refreshNow(ctx context.Context) error {
	tok, err := a.fetchToken(ctx)
	if err != nil {
		return err
	}
	a.tokenMu.Lock()
	a.accessToken = tok.AccessToken
	a.tokenExpiresAt = tok.ExpiresAt
	a.tokenMu.Unlock()
	a.logger.InfoContext(ctx, "token refreshed",
		slog.Time("expires_at", tok.ExpiresAt),
		slog.Duration("ttl", time.Until(tok.ExpiresAt)),
	)
	return nil
}

type gigaChatToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

func (a *gigaChatAdapter) fetchToken(ctx context.Context) (*gigaChatToken, error) {
	var resp struct {
		AccessToken    string `json:"access_token"`
		ExpiresAtMilli int64  `json:"expires_at"`
	}

	httpResp, err := a.restyClient.R().
		SetContext(ctx).
		SetFormData(map[string]string{"scope": a.config.Scope}).
		SetHeader(headerRequestID, uuid.NewString()).
		SetHeader(headerAccept, contentTypeJSON).
		SetHeader(headerAuthorization, "Basic "+a.config.OAuthToken).
		SetResult(&resp).
		Post(a.config.OAuthURL)
	if err != nil {
		return nil, fmt.Errorf("oauth request: %w", err)
	}
	if httpResp.IsError() {
		return nil, fmt.Errorf("oauth status %d: %s", httpResp.StatusCode(), httpResp.String())
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("oauth response missing access_token")
	}

	return &gigaChatToken{
		AccessToken: resp.AccessToken,
		ExpiresAt:   time.UnixMilli(resp.ExpiresAtMilli),
	}, nil
}
