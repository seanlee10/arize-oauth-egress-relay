// Command auth-proxy is an in-cluster egress relay for Arize on-prem.
//
// Arize's outbound calls to Custom Model Endpoints (including the OAuth 2.0
// client-credentials token request) connect directly to the destination and
// cannot use an authenticated corporate proxy. This relay runs inside the
// cluster: Arize is pointed at the relay's Service URL, and the relay forwards
// each request to the real upstream through HTTPS_PROXY (with Basic auth),
// honouring NO_PROXY for destinations that can be reached directly.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	routes, err := parseRoutes(os.Getenv("ROUTES"))
	if err != nil {
		return err
	}
	proxy, err := proxyConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	transport := &http.Transport{
		Proxy:                 proxy.ProxyFunc(),
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newRelay(routes, transport, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	for _, rt := range routes {
		logger.Info("route", "prefix", rt.prefix, "upstream", rt.target.String())
	}
	desc := proxy.Describe()
	logger.Info("starting", "addr", addr, "https_proxy", desc["https_proxy"], "http_proxy", desc["http_proxy"], "no_proxy", desc["no_proxy"])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
