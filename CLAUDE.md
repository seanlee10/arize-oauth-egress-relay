# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A sample in-cluster egress relay. Arize on-prem can't send OAuth 2.0 token requests for Custom Model Endpoints through an authenticated corporate HTTP proxy. Arize is pointed at this relay's in-cluster URL instead. The relay forwards each request to the real IdP (e.g. `login.microsoftonline.com`) through `HTTPS_PROXY`, using Basic auth and honouring `NO_PROXY`. It is a separate Kubernetes workload, not a sidecar, and needs no changes to Arize.

Behaviour of Arize this relay depends on:
- Arize checks custom endpoint URLs with a DNS lookup and an SSRF allowlist before calling them.
- The OAuth token request has a fixed format (see Deployment constraints).
- Arize's egress gateway (`externalendpoints`) connects directly and ignores proxy env vars.

## Commands

```sh
go test -race ./...                                  # all tests
go test -race -run TestTokenRequestTunnels ./...     # single test
go vet ./... && gofmt -l .
docker build -t auth-proxy .
kubectl apply --dry-run=client -f deploy/kubernetes.yaml   # validate manifest

# Local e2e against real Entra through an authenticated Squid (no direct internet)
cd demo && docker-compose up --build -d
docker-compose run --rm client -X POST -u 'id:secret' \
  --data 'grant_type=client_credentials&scope=api://example/.default' \
  http://auth-proxy:8080/entra/organizations/oauth2/v2.0/token   # expect an AADSTS JSON error = success
docker-compose down
```

On this machine, `docker compose` (the plugin) is not installed. Use `docker-compose`.

## Architecture

A single `main` package that uses only the standard library plus `golang.org/x/net/http/httpproxy`:
- `config.go`:
  - `parseRoutes` reads `ROUTES` (`/prefix=https://upstream,...`) and sorts routes longest prefix first.
  - `proxyConfigFromEnv` builds an `httpproxy.Config` from `HTTP(S)_PROXY`/`NO_PROXY`. `PROXY_USERNAME`/`PROXY_PASSWORD` are injected as URL userinfo. Go's `http.Transport` then sends them as `Proxy-Authorization: Basic` on CONNECT.
- `relay.go`:
  - `newRelay` creates one `httputil.ReverseProxy` per route.
  - It uses `Rewrite`, not `Director`, so `X-Forwarded-*` headers are dropped. It strips the prefix and `SetURL` sets the upstream Host.
  - `FlushInterval: -1` lets streamed model responses pass through without buffering.
  - Transport failures become a JSON 502 `upstream_unreachable`. Unknown paths return 404 `no_route`, and `/healthz` returns 200.
- `main.go`: wires up the transport and server, logs JSON with `slog`, and shuts down gracefully on SIGTERM.

Invariants:
- **Never log headers or bodies.** They carry client secrets and access tokens. Proxy credentials are logged only through `redact`/`Describe`.
- Upstream responses, including 4xx and 3xx, pass through unchanged. Arize's fetch uses `redirect: "error"`, so the relay must not follow or invent redirects.
- `httpproxy` always bypasses the proxy for loopback hosts. The tests therefore pin the proxy with `http.ProxyURL` against `httptest` servers, and test `NO_PROXY` matching separately with non-loopback hostnames. Keep that split when adding tests.

## Deployment constraints

These all come from Arize's code. Keep the README and `deploy/kubernetes.yaml` consistent with them:
- **Allowlist the relay's ClusterIP.** Add it as a CIDR to Arize `values.yaml` `allowedIPv4Addresses`. Otherwise Arize rejects it with `IPV4_NOT_ALLOWED_ON_PREM`.
- **Use the FQDN in the Token URL** (`auth-proxy.<ns>.svc.cluster.local`). Arize resolves it with `dns.resolve4`, which does not apply search domains.
- **`http://` and non-443 ports are fine on-prem.** The https and port-443 rules apply only to SaaS.
- **Arize's request shape:** the token request is a form POST with `grant_type=client_credentials` and the client credentials in an `Authorization: Basic` header (`client_secret_basic`). Test Integration always uses this.
- **NetworkPolicy:** ingress to the relay is allowed only from pods labelled `app: externalendpoints`.
