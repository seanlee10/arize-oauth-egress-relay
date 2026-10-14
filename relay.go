package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// newRelay returns a handler that forwards requests matching a route prefix
// to that route's upstream, using transport (which carries the corporate
// proxy settings). Arize calls the relay over plain HTTP inside the cluster;
// the relay speaks HTTPS to the upstream through the proxy tunnel.
func newRelay(routes []route, transport http.RoundTripper, logger *slog.Logger) http.Handler {
	type entry struct {
		route
		proxy *httputil.ReverseProxy
	}
	entries := make([]entry, 0, len(routes))
	for _, rt := range routes {
		rt := rt
		entries = append(entries, entry{route: rt, proxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Rewrite (unlike Director) already drops X-Forwarded-* and
				// Forwarded, so in-cluster addresses are not leaked upstream.
				// Proxy-Authorization is hop-by-hop and is stripped too.
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, rt.prefix)
				pr.Out.URL.RawPath = ""
				pr.SetURL(rt.target) // also sets Host to the upstream host
			},
			Transport: transport,
			// Flush immediately so streamed model responses (SSE) are not buffered.
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				logger.Warn("upstream request failed", "route", rt.prefix, "upstream", rt.target.Host, "error", err.Error())
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"error":             "upstream_unreachable",
					"error_description": "relay could not reach " + rt.target.Host + " via the configured proxy",
				})
			},
		}})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		for _, e := range entries {
			if r.URL.Path == e.prefix || strings.HasPrefix(r.URL.Path, e.prefix+"/") {
				start := time.Now()
				rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
				e.proxy.ServeHTTP(rec, r)
				// Never log headers or bodies: they carry client secrets and tokens.
				logger.Info("relayed",
					"method", r.Method,
					"route", e.prefix,
					"upstream", e.target.Host,
					"status", rec.status,
					"duration_ms", time.Since(start).Milliseconds(),
				)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_route", "error_description": "no route for " + r.URL.Path})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying Flusher.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
