# arize-oauth-egress-relay

This is a sample in-cluster egress relay. With it, Arize on-prem can use OAuth 2.0 on Custom Model Endpoints in a network where outbound traffic must go through a corporate HTTP proxy that requires a username and password. It needs no changes to Arize.

> [!WARNING]
> **Unofficial sample, not supported.** This is not an Arize product and is not supported by Arize AI. It is a reference implementation provided as-is, without warranty of any kind (see [LICENSE](LICENSE)). Review, test and harden it against your own security and compliance requirements before running it in production. You are responsible for operating it. Arize may add native proxy support later, which would make this relay unnecessary.

## The problem

Arize sends every outbound call to a customer endpoint through its `externalendpoints` gateway, and that gateway always connects directly to the destination. It ignores `HTTP(S)_PROXY` and has no setting for an upstream proxy. Before calling out, app-server also does a DNS lookup on the Token URL. In a network where the only way out is the proxy, `login.microsoftonline.com` doesn't resolve, so the integration fails with `DNS resolution failed for both IPv4 and IPv6`.

## How the relay works

```
 Arize (generative / app-server)
   │  Token URL = http://auth-proxy.arize.svc.cluster.local:8080/entra/<tenant>/oauth2/v2.0/token
   ▼
 externalendpoints ──(direct, in-cluster)──▶ auth-proxy ──CONNECT + Proxy-Authorization: Basic──▶ corporate proxy ──TLS──▶ login.microsoftonline.com
```

- Arize is configured with the relay's **in-cluster** URL. That name resolves through cluster DNS and the relay is reachable directly, so both the DNS pre-check and `externalendpoints` work unchanged.
- The relay removes the route prefix and forwards the request as-is to the real upstream over HTTPS. The method, body, `Authorization: Basic <client_id:secret>` header and the response all pass through unchanged. Redirects are passed through, not followed.
- Upstream traffic goes through the corporate proxy using the standard `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` variables. The proxy username and password are sent as Basic auth on `CONNECT`. Nothing in the relay is specific to Azure: adding another IdP is one more entry in `ROUTES`.
- Headers and bodies are never logged, because they carry client secrets and access tokens. Credentials are redacted in the startup log.

It is built around three requirements:

| Requirement | How it is met |
|---|---|
| Only `login.microsoftonline.com` goes through the proxy | The relay only handles hosts listed in `ROUTES`. All other Arize egress is untouched. `NO_PROXY` is honoured for anything else. |
| Standard `HTTP(S)_PROXY` / `NO_PROXY`, not Azure-specific | Uses the same semantics as Go and curl (`golang.org/x/net/http/httpproxy`). Routes are generic `prefix=https://host`. |
| Basic-auth proxy | `PROXY_USERNAME` / `PROXY_PASSWORD` come from a Secret, or credentials can be embedded in the proxy URL. |

## Configuration

| Env | Example | Notes |
|---|---|---|
| `ROUTES` | `/entra=https://login.microsoftonline.com` | Comma-separated `prefix=upstream` pairs. Longest prefix wins. |
| `HTTPS_PROXY` | `http://proxy.corp.example:8080` | Proxy used for `https://` upstreams. Lower-case names also work. |
| `HTTP_PROXY` | | Proxy used for `http://` upstreams (rarely needed). |
| `NO_PROXY` | `.svc.cluster.local,10.0.0.0/8` | Hosts, domains and CIDRs to reach directly. |
| `PROXY_USERNAME` / `PROXY_PASSWORD` | | Override any userinfo in the proxy URL. Need no URL-escaping. |
| `LISTEN_ADDR` | `:8080` | |
| `SSL_CERT_FILE` | `/etc/corp-ca/ca.crt` | Only needed if the corporate proxy does TLS inspection: the CA bundle to trust. |

## Deploy (on-prem)

1. **Build and push the image** to the internal registry:
   ```sh
   docker build -t REGISTRY/auth-proxy:0.1.0 . && docker push REGISTRY/auth-proxy:0.1.0
   ```
2. **Edit and apply** [`deploy/kubernetes.yaml`](deploy/kubernetes.yaml). Set the proxy URL and credentials in the Secret and the image name. Change the namespace if Arize is not in `arize`.
   ```sh
   kubectl apply -f deploy/kubernetes.yaml
   kubectl -n arize get svc auth-proxy -o jsonpath='{.spec.clusterIP}'
   ```
3. **Allowlist the relay in Arize `values.yaml`.** Arize's SSRF guard rejects private IPs on-prem unless they are allowlisted (`IPV4_NOT_ALLOWED_ON_PREM`), and the field accepts CIDRs. Then roll out the change:
   ```yaml
   allowedIPv4Addresses: "<clusterIP>/32"     # append to any existing entries
   # allowedIPv6Addresses: "<ipv6 clusterIP>/128"   # dual-stack clusters only
   ```
   To keep the IP stable if the Service is ever recreated, pin `spec.clusterIP` in the Service (there is a comment showing where).
4. **Configure the Custom Model Endpoint in Arize** (OAuth 2.0):
   - **Token URL:** `http://auth-proxy.arize.svc.cluster.local:8080/entra/<tenant-id>/oauth2/v2.0/token`
     Use the **fully-qualified** name. Arize resolves it with `dns.resolve4`, which does not apply the resolv.conf search domains.
   - **Client ID / Secret / Scope:** unchanged, for example `api://<app-id>/.default`.
   - **API Base URL:** unchanged if the model endpoint is reachable directly. If it also sits behind the proxy, add a route such as `/llm=https://llm.example.com` and use `http://auth-proxy.arize.svc.cluster.local:8080/llm/v1`. Responses are streamed without buffering.
5. Click **Test Integration**. It runs the same token flow through the relay.

`http://` and port `8080` are accepted on-prem, since the https and port-443 rules apply only to SaaS. The hop from Arize to the relay is cluster-internal. The client secret goes to the relay in plaintext over the pod network, and from the relay to Entra over TLS. If that internal hop must be encrypted, put the relay behind your service mesh (mTLS) or terminate TLS on the relay.

## Local demo: a real proxy with Basic auth and real Entra

[`demo/docker-compose.yaml`](demo/docker-compose.yaml) reproduces the customer's network. The relay and the "Arize" client sit on an `internal: true` Docker network with no route to the internet. Squid, with Basic auth (`svc-arize` / `Corp-Pa55!`), is the only way out.

```sh
cd demo
docker-compose up --build -d     # or: docker compose up --build -d

# Arize-style token request through the relay. With fake credentials, a JSON
# AADSTS error from Entra proves the full round trip worked.
docker-compose run --rm client -X POST -u 'client-id:secret' \
  --data 'grant_type=client_credentials&scope=api://example/.default' \
  http://auth-proxy:8080/entra/organizations/oauth2/v2.0/token
# -> {"error":"invalid_request","error_description":"AADSTS900023: ..."}  HTTP 400

# Direct egress from the same network fails:
docker-compose run --rm client --max-time 5 https://login.microsoftonline.com   # HTTP 000

# Squid shows the authenticated tunnel. CONNECT lines are written when the
# tunnel closes, so restart the relay to flush its pooled connection first.
docker-compose restart auth-proxy
docker-compose exec corp-proxy cat /var/log/squid/access.log
# ... TCP_TUNNEL/200 ... CONNECT login.microsoftonline.com:443 svc-arize ...

docker-compose down
```

With real tenant, client and scope values, the same request returns `{"token_type":"Bearer","access_token":...}`.

## Tests

```sh
go test -race ./...
```

The tests cover:
- A token request tunnelled through a fake proxy that requires Basic auth on `CONNECT`, to a TLS fake of the Entra v2 token endpoint. The request matches what Arize's `tokenClient` sends (`client_secret_basic`, form `grant_type`/`scope`).
- Wrong proxy credentials, which return 502 `upstream_unreachable`.
- IdP error responses (401 `invalid_client`) passing through unchanged.
- Choosing the proxy with `NO_PROXY`, and credential injection and redaction.
- Route parsing.

## Troubleshooting

| Symptom in Arize | Cause |
|---|---|
| `DNS resolution failed for both IPv4 and IPv6` | Token URL is not the FQDN `*.svc.cluster.local`, or still points at `login.microsoftonline.com`. |
| `IPV4_NOT_ALLOWED_ON_PREM` | The relay's ClusterIP is not in `allowedIPv4Addresses`. |
| Token request fails with 502 `upstream_unreachable` | Relay → proxy failed. Check the relay logs: `Proxy Authentication Required` means wrong credentials; a TLS error usually means TLS inspection, so set `SSL_CERT_FILE`. |
| Timeout | A NetworkPolicy is blocking `externalendpoints` → `auth-proxy:8080`, or the relay is blocked from reaching the proxy. |
| 404 `no_route` | The Token URL path doesn't start with a configured `ROUTES` prefix. |

## Long-term

This is a workaround. The real fix is for `externalendpoints` to support an upstream proxy using `HTTPS_PROXY`/`NO_PROXY` and Basic auth, configured in `values.yaml`. The DNS pre-check would also need to be skipped or relaxed for hosts that go through the proxy. Then this relay can be removed.

## License

[MIT](LICENSE)
