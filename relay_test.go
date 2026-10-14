package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	proxyUser = "svc-arize"
	proxyPass = "p@ss:w0rd/with#specials"
)

// fakeCorporateProxy is an HTTP CONNECT proxy that requires Basic auth,
// like the customer's egress proxy.
type fakeCorporateProxy struct {
	*httptest.Server
	mu        sync.Mutex
	connected []string // CONNECT targets that were authorised
	rejected  int
}

func newFakeCorporateProxy(t *testing.T) *fakeCorporateProxy {
	p := &fakeCorporateProxy{}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+proxyPass))
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Proxy-Authorization") != want {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			w.Header().Set("Proxy-Authenticate", `Basic realm="corp"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		p.mu.Lock()
		p.connected = append(p.connected, r.Host)
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		client, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		go func() {
			defer upstream.Close()
			defer client.Close()
			go io.Copy(upstream, buf)
			io.Copy(client, upstream)
		}()
	}))
	t.Cleanup(p.Close)
	return p
}

// fakeEntra mimics login.microsoftonline.com's v2 token endpoint, checking the
// request exactly as Arize's tokenClient sends it (client_secret_basic).
func fakeEntra(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/my-tenant/oauth2/v2.0/token" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != "client-id" || secret != "client-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		if r.Header.Get("X-Forwarded-For") != "" {
			http.Error(w, "X-Forwarded-For leaked", http.StatusBadRequest)
			return
		}
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") != "api://llm/.default" {
			http.Error(w, "bad form: "+r.PostForm.Encode(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":   "Bearer",
			"expires_in":   3599,
			"access_token": "eyJ0eXAi.fake.token",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestRelay builds a relay whose transport sends everything through the
// given proxy URL. (httpproxy always bypasses loopback addresses, so tests
// pin the proxy explicitly; NO_PROXY matching is covered separately below.)
func newTestRelay(t *testing.T, idp *httptest.Server, proxyURL *url.URL) *httptest.Server {
	routes, err := parseRoutes("/entra=" + idp.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(idp.Certificate())
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots},
	}
	relay := httptest.NewServer(newRelay(routes, transport, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(relay.Close)
	return relay
}

// tokenRequest sends the request the way Arize's OAuth token client does.
func tokenRequest(t *testing.T, relayURL string) *http.Response {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"api://llm/.default"}}
	req, _ := http.NewRequest(http.MethodPost, relayURL+"/entra/my-tenant/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("client-id", "client-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTokenRequestTunnelsThroughAuthenticatedProxy(t *testing.T) {
	idp := fakeEntra(t)
	proxy := newFakeCorporateProxy(t)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword(proxyUser, proxyPass)
	relay := newTestRelay(t, idp, proxyURL)

	resp := tokenRequest(t, relay.URL)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var tok map[string]any
	if err := json.Unmarshal(body, &tok); err != nil || tok["access_token"] != "eyJ0eXAi.fake.token" {
		t.Fatalf("unexpected token response: %s", body)
	}
	idpHost := strings.TrimPrefix(idp.URL, "https://")
	if len(proxy.connected) != 1 || proxy.connected[0] != idpHost {
		t.Fatalf("expected one CONNECT to %s, got %v", idpHost, proxy.connected)
	}
}

func TestWrongProxyCredentialsReturn502(t *testing.T) {
	idp := fakeEntra(t)
	proxy := newFakeCorporateProxy(t)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword(proxyUser, "wrong")
	relay := newTestRelay(t, idp, proxyURL)

	resp := tokenRequest(t, relay.URL)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	if proxy.rejected != 1 || len(proxy.connected) != 0 {
		t.Fatalf("expected proxy to reject once, rejected=%d connected=%v", proxy.rejected, proxy.connected)
	}
}

func TestUpstreamErrorsPassThroughUnchanged(t *testing.T) {
	idp := fakeEntra(t)
	proxy := newFakeCorporateProxy(t)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword(proxyUser, proxyPass)
	relay := newTestRelay(t, idp, proxyURL)

	req, _ := http.NewRequest(http.MethodPost, relay.URL+"/entra/my-tenant/oauth2/v2.0/token", strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("client-id", "bad-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "invalid_client") {
		t.Fatalf("expected upstream 401 invalid_client, got %d %s", resp.StatusCode, body)
	}
}

func TestHealthAndUnknownRoute(t *testing.T) {
	relay := newTestRelay(t, fakeEntra(t), &url.URL{Scheme: "http", Host: "127.0.0.1:1"})
	for path, want := range map[string]int{"/healthz": 200, "/other/x": 404, "/entraX/y": 404} {
		resp, err := http.Get(relay.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d want %d", path, resp.StatusCode, want)
		}
	}
}

func TestProxyEnvSelectsProxyPerHost(t *testing.T) {
	env := map[string]string{
		"HTTPS_PROXY":    "proxy.corp.example:3128",
		"NO_PROXY":       ".svc.cluster.local,llm.internal.example",
		"PROXY_USERNAME": proxyUser,
		"PROXY_PASSWORD": proxyPass,
	}
	cfg, err := proxyConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	pf := cfg.ProxyFunc()

	req, _ := http.NewRequest("POST", "https://login.microsoftonline.com/t/oauth2/v2.0/token", nil)
	got, err := pf(req)
	if err != nil || got == nil {
		t.Fatalf("expected proxy for login.microsoftonline.com, got %v %v", got, err)
	}
	if got.Host != "proxy.corp.example:3128" {
		t.Errorf("proxy host = %s", got.Host)
	}
	if pw, _ := got.User.Password(); got.User.Username() != proxyUser || pw != proxyPass {
		t.Errorf("proxy credentials not applied: %v", got.User)
	}

	for _, direct := range []string{"https://llm.internal.example/v1/chat/completions", "http://foo.ns.svc.cluster.local/"} {
		req, _ := http.NewRequest("POST", direct, nil)
		if got, _ := pf(req); got != nil {
			t.Errorf("%s should bypass the proxy via NO_PROXY, got %v", direct, got)
		}
	}

	if d := cfg.Describe()["https_proxy"]; strings.Contains(d, proxyPass) || !strings.Contains(d, "xxxxx") {
		t.Errorf("password not redacted in %q", d)
	}
}

func TestParseRoutes(t *testing.T) {
	routes, err := parseRoutes(" /a=https://a.example , /a/b=https://b.example/base/ ")
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].prefix != "/a/b" || routes[0].target.Path != "/base" {
		t.Errorf("expected longest prefix first with trimmed path, got %+v", routes[0])
	}
	for _, bad := range []string{"", "/=https://x", "a=https://x", "/a=ftp://x", "/a", "/a=https://x,/a=https://y", "/a=https://u:p@x"} {
		if _, err := parseRoutes(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
