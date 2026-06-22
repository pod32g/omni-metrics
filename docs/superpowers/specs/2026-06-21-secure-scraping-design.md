# Secure metric scraping — design

Date: 2026-06-21
Status: approved (brainstorming) → ready for plan

## Problem

omni-identity (`github.com/pod32g/omni-identity`) gates its `/metrics` endpoint
behind a bearer token: the endpoint is disabled unless `metrics.bearer_token` /
`OMNI_METRICS_TOKEN` is set, after which it must be scraped with
`Authorization: Bearer <token>`. It recommends HTTPS in production but does **not**
require mTLS.

omni-metrics' scraper currently uses a single bare `&http.Client{}`
(`internal/scrape/scrape.go`) with no authentication header and no TLS
configuration. The scrape job (`config.ScrapeConfig` and the scrape-internal
`ScrapeConfig`) carries no credentials. The live deploy (`omni.yml`) scrapes
omni-identity at `192.168.68.34:8081` over plain HTTP with no auth. Once
omni-identity turns on its token, that scrape begins failing with HTTP 401.

This is a **coordinated cross-repo change**: the scraper gains the capability to
authenticate, and omni-identity on chizu is configured to require the token at
the same time.

## Goal

Give scrape jobs a Prometheus-shaped authentication and TLS surface, supply the
secrets without committing them, and wire the live omni-identity scrape
end-to-end.

Decisions locked during brainstorming:
- **Scope:** full Prometheus auth surface — `authorization` (bearer),
  `basic_auth`, and `tls_config` (including mTLS).
- **Secret delivery:** inline values, `<field>_file` path references, **and**
  `${ENV}` expansion — all three.
- **Deploy:** wire the live omni-identity scrape, which entails configuring the
  omni-identity deployment on chizu to require the token.
- **Schema:** mirror Prometheus' own `scrape_config` schema (industry standard;
  matches the project's Grafana-compat ethos), not a flatter custom shape.

## Config schema (Prometheus-shaped)

Added to each `scrape_configs` entry. All fields optional; omitting the auth/TLS
blocks preserves today's behaviour (plain HTTP, no auth).

```yaml
scrape_configs:
  - job_name: omni-identity
    scheme: https                      # http | https; default http
    authorization:                     # modern Prometheus bearer form
      type: Bearer                     # default Bearer
      credentials: ${OMNI_IDENTITY_TOKEN}
      # credentials_file: /run/secrets/omni_identity_token   # mutually exclusive with credentials
    basic_auth:                        # mutually exclusive with authorization
      username: scraper
      password: ${SCRAPE_PW}
      # username_file / password_file also accepted
    tls_config:
      ca_file: /etc/ssl/ca.pem         # custom CA to verify the server
      cert_file: /etc/ssl/client.pem   # mTLS: cert + key both-or-neither
      key_file:  /etc/ssl/client.key
      server_name: omni-identity.internal
      insecure_skip_verify: false
    static_configs:
      - targets: [192.168.68.34:8081]
```

### Resolution & validation rules

- **`${VAR}` expansion** applies to credential string fields and file-path
  fields at config load. Forms: `${VAR}` and `${VAR:-default}`. A `${VAR}`
  referenced but **unset (and no default) is a load error** — fail loud rather
  than silently scrape with an empty token.
- **`_file` resolution:** credential `_file` fields (bearer/basic_auth) are read
  once at load into concrete values. TLS `ca_file`/`cert_file`/`key_file` are
  read when the per-job HTTP client is built at startup.
- A field and its `_file` twin set together → load error.
- `authorization` and `basic_auth` are mutually exclusive (Prometheus rule).
- `cert_file` and `key_file` are both-or-neither.
- `scheme` must be `http` or `https` (default `http`).
- **Deferred (documented):** live secret/cert *rotation* without a restart.
  Secrets are resolved once at startup; rotating a token or cert requires a
  process restart. Acceptable for this milestone.

## Components

### 1. Config (`internal/config`)

New structs `Authorization`, `BasicAuth`, `TLSConfig`; `Scheme` and these blocks
added to `ScrapeConfig`. Env expansion + `_file` reading + validation happen in
the existing `LoadBytes` → `applyDefaults`/`validate` flow. The resolved,
secret-bearing values are passed to the scrape layer; raw config never holds a
token that wasn't already concrete.

### 2. Scrape transport (`internal/scrape`)

- Build **one `*http.Client` per job** (today: a single shared bare client),
  whose `Transport` carries the job's `tls.Config` (custom CA pool, optional
  client cert for mTLS, `ServerName`, `InsecureSkipVerify`). Thread the client +
  resolved auth to each target's loop.
- `fetchAndParse` sets `Authorization: Bearer <creds>` or HTTP basic-auth on the
  outgoing request before `client.Do`.
- `scheme` is applied in `normalizeTarget` when the target string carries no
  explicit scheme (today it always prepends `http://`).
- **No new failure path:** wrong/missing credentials → omni-identity returns
  HTTP 401 → existing code already fails the scrape (`up=0`) and records the
  status in target health.

### 3. Deploy wiring — omni-metrics side

- `omni.yml`: add `authorization.credentials: ${OMNI_IDENTITY_TOKEN}` to the
  `omni-identity` job. Token never committed.
- `docker-compose.yml`: add `OMNI_IDENTITY_TOKEN: ${OMNI_IDENTITY_TOKEN:-}` to
  `environment` (mirrors `LOGSHIP_API_KEY`).
- `.github/workflows/cicd.yml`: pass
  `OMNI_IDENTITY_TOKEN: ${{ secrets.OMNI_IDENTITY_TOKEN }}` into the deploy step
  env so it reaches the container via compose.
- A repo secret `OMNI_IDENTITY_TOKEN` must be created (value set by the user; the
  implementation cannot set it).

### 4. Deploy wiring — omni-identity side (chizu, cross-repo)

- Generate one shared token: `openssl rand -hex 32`.
- Configure the omni-identity deployment on chizu to require it by setting
  `OMNI_METRICS_TOKEN` in its deployment env (host `.env` / secret, gitignored —
  the same convention omni-metrics uses for `LOGSHIP_API_KEY`).
- The **same** token value goes into omni-metrics' `OMNI_IDENTITY_TOKEN` GH
  secret.
- Deploy ordering is harmless either way: the scraper merely sends a header
  omni-identity ignores until its gate is enabled; enabling the gate before the
  scraper has the token causes 401s (visible as `up=0`) until the scraper
  redeploys.

## Tests (TDD)

- **Config (table-driven):** `${VAR}` expansion, `${VAR:-default}`, unset-var
  error, `_file` read, field+`_file` conflict, authorization/basic_auth mutual
  exclusion, cert/key both-or-neither, scheme validation.
- **Scrape (`httptest`):** bearer header sent and accepted; basic-auth sent;
  TLS server verified via configured CA (`httptest.NewTLSServer`); mTLS
  handshake with client cert; `insecure_skip_verify` path; 401 → `up=0` with
  status recorded in health.
- `gofmt -w .` → `go vet ./...` → `go test ./... -race` green before commit.

## Verification (evidence, not assertions)

- Run the binary against a local TLS+bearer `httptest`-style target and show a
  successful scrape plus a 401→`up=0` failure.
- SSH to chizu to confirm (a) omni-identity `/metrics` is token-gated after
  configuration (401 without token, 200 with) and (b) whether it serves http or
  https on `:8081`, then flip the live scrape accordingly.

## Open items (confirm during implementation, not blocking)

1. **http vs https for the host-local omni-identity hop.** Leaning **http +
   bearer**: `:8081` is a same-host published port and omni-identity likely does
   not terminate TLS there. Verify on chizu; the `tls_config`/`scheme` capability
   stands regardless for other targets.
2. **Exact GH secret name** — proposed `OMNI_IDENTITY_TOKEN`.
3. **Where omni-identity's deploy config lives on chizu** (compose project dir /
   `.env`) — locate during implementation.

## Out of scope (YAGNI)

- Live secret/cert rotation without restart (deferred, see above).
- OAuth2 / proxy auth / other Prometheus `http_config` knobs beyond
  bearer/basic/TLS.
- Applying the same env/`_file` mechanism to the push `auth_token` (separate
  follow-up; noted for consistency but not built here).
