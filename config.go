package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// route maps an inbound path prefix on the relay to an upstream origin.
// A request to <prefix>/rest/of/path is forwarded to <target>/rest/of/path.
type route struct {
	prefix string
	target *url.URL
}

// parseRoutes parses ROUTES, a comma-separated list of "prefix=upstream" pairs:
//
//	/entra=https://login.microsoftonline.com,/llm=https://llm.example.com/v1
//
// Routes are returned longest-prefix first so the most specific one wins.
func parseRoutes(spec string) ([]route, error) {
	var routes []route
	seen := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, rawTarget, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("route %q: expected prefix=upstream", entry)
		}
		prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
		if !strings.HasPrefix(prefix, "/") || prefix == "" {
			return nil, fmt.Errorf("route %q: prefix must start with / and not be just /", entry)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("route %q: duplicate prefix %s", entry, prefix)
		}
		target, err := url.Parse(strings.TrimSpace(rawTarget))
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", entry, err)
		}
		if target.Scheme != "https" && target.Scheme != "http" {
			return nil, fmt.Errorf("route %q: upstream scheme must be http or https", entry)
		}
		if target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
			return nil, fmt.Errorf("route %q: upstream must be a bare origin with optional path", entry)
		}
		target.Path = strings.TrimRight(target.Path, "/")
		seen[prefix] = true
		routes = append(routes, route{prefix: prefix, target: target})
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("ROUTES is empty; set e.g. ROUTES=/entra=https://login.microsoftonline.com")
	}
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].prefix) > len(routes[j].prefix) })
	return routes, nil
}

// proxyConfig is the upstream (corporate) proxy configuration, read from the
// standard HTTP_PROXY / HTTPS_PROXY / NO_PROXY variables (upper or lower case).
//
// Credentials can be embedded in the proxy URL (http://user:pass@proxy:8080),
// or supplied separately via PROXY_USERNAME / PROXY_PASSWORD so they can come
// from a Kubernetes Secret without URL-escaping. Separate values win.
type proxyConfig struct {
	cfg httpproxy.Config
}

func proxyConfigFromEnv(getenv func(string) string) (*proxyConfig, error) {
	env := func(name string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return getenv(strings.ToLower(name))
	}
	cfg := httpproxy.Config{
		HTTPProxy:  env("HTTP_PROXY"),
		HTTPSProxy: env("HTTPS_PROXY"),
		NoProxy:    env("NO_PROXY"),
	}
	user, pass := getenv("PROXY_USERNAME"), getenv("PROXY_PASSWORD")
	for _, p := range []*string{&cfg.HTTPProxy, &cfg.HTTPSProxy} {
		if *p == "" {
			continue
		}
		u, err := parseProxyURL(*p)
		if err != nil {
			return nil, err
		}
		if user != "" {
			u.User = url.UserPassword(user, pass)
		}
		*p = u.String()
	}
	return &proxyConfig{cfg: cfg}, nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", redact(raw))
	}
	return u, nil
}

// ProxyFunc returns an http.Transport Proxy function. Go's transport sends
// "Proxy-Authorization: Basic ..." on the CONNECT request when the proxy URL
// carries userinfo, which is what an authenticated corporate proxy expects.
func (p *proxyConfig) ProxyFunc() func(*http.Request) (*url.URL, error) {
	f := p.cfg.ProxyFunc()
	return func(r *http.Request) (*url.URL, error) { return f(r.URL) }
}

// Describe returns the configuration with credentials redacted, for logging.
func (p *proxyConfig) Describe() map[string]string {
	return map[string]string{
		"http_proxy":  redact(p.cfg.HTTPProxy),
		"https_proxy": redact(p.cfg.HTTPSProxy),
		"no_proxy":    p.cfg.NoProxy,
	}
}

func redact(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}
