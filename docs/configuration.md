# Configuration reference

Nina reads one YAML (or JSON) document. This page lists every setting the
gateway accepts, its default, and what it does.

Validate a file before shipping it:

```
nina check -c nina.yaml      # exits non-zero on any problem
```

## Document rules

These apply everywhere in the file.

**Unknown keys are rejected.** A typo is a startup error, not a silently
ignored line. This holds for middleware bodies too, which are decoded by each
middleware with the same strictness.

**All problems are reported at once.** `nina check` lists every validation
failure it finds rather than making you fix one per restart.

**YAML or JSON.** The document is converted to JSON and decoded from there, so
a `.json` file works with no separate code path.

**Environment variables** expand as `${NAME}` or `${NAME:-default}`. A variable
that is neither set nor defaulted is an error — blanking an upstream host or a
JWKS URL fails in far more confusing ways later:

```yaml
hosts: ["${USERS_HOST:-http://127.0.0.1:9001}"]
```

**Durations** are strings such as `5s`, `250ms`, `1m30s`, or a plain number of
nanoseconds.

**Relative paths** in `backends[].spec.file` resolve against the directory
holding the config file, not the process working directory. (`keys_file` in the
apikey middleware is the exception — see the note there.)

**Settings layer** route override → backend → `defaults`. The most specific
value that is set wins.

---

## `version`

| Key | Type | Default | Description |
|---|---|---|---|
| `version` | int | `1` | Document schema version. Must be `1`. |

## `server`

The listeners and connection-level limits.

| Key | Type | Default | Description |
|---|---|---|---|
| `listen` | string | `:8080` | Address for the proxy listener. Use `0.0.0.0:8080` in a container; `127.0.0.1` is unreachable from outside it. |
| `read_header_timeout` | duration | `5s` | How long a client may take to send request headers. Guards against slow-header attacks. |
| `write_timeout` | duration | none | Total time allowed to write the response. Leave unset for streaming or long-polling endpoints. |
| `idle_timeout` | duration | `60s` | How long a keep-alive connection may sit idle. |
| `max_request_body` | int (bytes) | `8388608` (8 MiB) | Cap on a buffered request body. Also bounds how much is retained for retries — a body larger than this cannot be replayed. |
| `trusted_proxy_cidrs` | []string | `[]` | Networks whose `X-Forwarded-For` is believed when deriving the client IP. **Empty means trust nothing** and use the peer address. Set this if you rate limit by IP behind a load balancer, or every caller shares the balancer's address. |

### `server.admin`

A second listener for operational endpoints. Keep it off the public interface.

| Key | Type | Default | Description |
|---|---|---|---|
| `listen` | string | none | Address for the admin listener. Empty disables it entirely. |
| `docs` | bool | `false` | Serve the merged document at `/openapi.json` and `/openapi.yaml`, plus rendered docs at `/docs`. |

Endpoints: `/healthz` (process is up), `/readyz` (a route table is loaded),
`/status`, `/routes`, `/metrics` (Prometheus). With `docs: true`, also
`/openapi.json`, `/openapi.yaml` and `/docs`.

## `spec`

Controls the single merged OpenAPI document the gateway publishes.

| Key | Type | Default | Description |
|---|---|---|---|
| `title` | string | `API Gateway` | `info.title` of the published document. |
| `version` | string | `1.0.0` | `info.version`. |
| `description` | string | none | `info.description`. |
| `servers` | []string | none | `servers` entries — the public URLs clients should call. |
| `dedupe` | bool | `true` | Collapse structurally identical components that share a base name, so two services' `Error` schemas become one instead of `UsersError` and `OrdersError`. |

Upstream authentication is carried into the merged document. An operation's
`security` requirement is republished against the namespaced copy of the scheme
it names (`jwtAuth` from the `portal` backend becomes `PortalJwtAuth`, subject to
the same `dedupe` collapsing as any other component), and an operation that
declares no requirement of its own inherits the one from its source document.
This is what lets a documentation viewer offer the credential box for an
authenticated endpoint. A requirement naming a scheme its own document never
declared is dropped rather than published broken — the gateway cannot describe a
credential nobody defined.

## `defaults`

Applied to every backend that does not set its own value.

| Key | Type | Default | Description |
|---|---|---|---|
| `timeout` | duration | `10s` | Per-request upstream timeout. |
| `retry` | object | none | See below. Absent means no retries. |
| `circuit_breaker` | object | none | See below. Absent means no breaker. |

### `retry`

Valid under `defaults` and `backends[]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `attempts` | int | `0` | Retries **after** the first try. Must be 0–10; the upper bound is enforced because retries amplify load on an upstream that is already struggling. |
| `backoff` | duration | `50ms` | Base delay between attempts. |
| `jitter` | bool | `false` | Randomise the backoff, so a fleet of clients does not retry in lockstep. |
| `idempotent_only` | bool | `true` | Retry only GET/HEAD/PUT/DELETE/OPTIONS/TRACE. Setting this to `false` can duplicate POSTs. |

### `circuit_breaker`

Valid under `defaults` and `backends[]`.

| Key | Type | Default | Description |
|---|---|---|---|
| `failure_ratio` | float | required | Trip when this fraction of recent requests fail. Must be >0 and ≤1. |
| `min_requests` | int | required | Minimum observed requests before the ratio is consulted. Must be ≥1. Stops one failure in a sample of two from opening the circuit. |
| `open_for` | duration | required | How long to stay open before probing the upstream again. Must be positive. |

## `backends`

A list. Each entry is one upstream service and its OpenAPI document.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | Identifier used by `expose[].backend`. Must start with a letter; letters, digits, `-` and `_` thereafter. Must be unique. |
| `spec` | object | required | Where the OpenAPI document comes from. See below. |
| `hosts` | []string | required | Absolute upstream URLs, e.g. `https://users.internal:8443`. Scheme must be `http` or `https`. More than one enables load balancing. |
| `load_balance` | string | `round_robin` | `round_robin`, `least_conn` or `random`. |
| `health_check` | object | none | Active host checking. See below. Absent means hosts are only marked down by observed failures. |
| `timeout` | duration | `defaults.timeout` | Per-request upstream timeout for this backend. |
| `retry` | object | `defaults.retry` | Overrides the global retry policy. |
| `circuit_breaker` | object | `defaults.circuit_breaker` | Overrides the global breaker. |
| `namespace` | string | capitalised `name` | Prefix for this backend's component names in the merged document. `user-profiles` becomes `UserProfiles`. |
| `security_scheme_names` | map | none | Upstream security scheme name → the name to publish it under. See below. |
| `strip_prefix` | string | none | Removed from the front of each upstream path before the gateway prefix is applied, so a service publishing `/v1/users` can be mounted anywhere without doubling the segment. Must start with `/`. |
| `forward_headers` | map | none | Headers added to every upstream request from this backend. |
| `tls` | object | none | Certificate verification. See below. |

### `backends[].spec`

Exactly one of `file` or `url`.

| Key | Type | Default | Description |
|---|---|---|---|
| `file` | string | — | Path to a local document, resolved against the config file's directory. Changes to it trigger a hot reload. |
| `url` | string | — | `http(s)` URL to fetch the document from. |
| `refresh` | duration | `0` | Re-fetch a `url` on this interval. `0` disables polling. Minimum `1s`. |
| `on_error` | string | `fail` | `fail` refuses to build a runtime when the document cannot be fetched. `stale` keeps serving the cached copy and marks the backend degraded — visible in `/status` and in the `check` output. With no cached copy yet, `stale` still fails: there is nothing to fall back to. |

### `backends[].health_check`

Every field defaults only once the block is present.

| Key | Type | Default | Description |
|---|---|---|---|
| `path` | string | `/healthz` | Path probed on each host. |
| `interval` | duration | `10s` | Time between probes. |
| `timeout` | duration | `2s` | Per-probe timeout. |
| `unhealthy_after` | int | `3` | Consecutive failures before a host is taken out of rotation. |
| `healthy_after` | int | `2` | Consecutive successes before it returns. |

### `backends[].tls`

| Key | Type | Default | Description |
|---|---|---|---|
| `insecure_skip_verify` | bool | `false` | Disable certificate chain **and** hostname verification for every connection to this backend, including the fetch of its OpenAPI document. |

`insecure_skip_verify` removes the only defence against an attacker interposed
on the connection to the upstream: any certificate is accepted, so traffic stays
encrypted but is no longer authenticated. It exists for internal services
presenting self-signed certificates. Where you can trust the issuing CA instead,
do that.

### `backends[].security_scheme_names`

The published name of a security scheme is what a documentation viewer labels
its credential box with, so it is worth being able to choose. Keys are the names
the backend's own document declares; values are what the merged document
publishes.

```yaml
backends:
  - name: portal
    security_scheme_names:
      jwtAuth: Bearer
      cookieAuth: Session
```

A chosen name is published verbatim — no `namespace` prefix, and the `dedupe`
pass leaves it alone. Schemes you do not name still derive theirs as usual, so
naming `jwtAuth` on a backend that also declares `cookieAuth` leaves the latter
as `PortalCookieAuth`.

Two backends may choose the same name for the same credential, which is how you
say "these really are one token": the scheme is published once and both
backends' operations point at it. If the two schemes are not identical this is
an error rather than a silent pick, because publishing one of two different
credentials under a single name would misdescribe how to call half the gateway.

A name that matches no scheme the backend declares is also an error — an
override that quietly did nothing would leave the derived name in the document
with no hint as to why.

## `middleware`

A map of name → definition. Names follow the same rule as backend names. `type`
selects the implementation; the remaining keys are decoded by that middleware.
Defining a middleware does nothing until an `expose` entry names it.

```yaml
middleware:
  per-ip:
    type: ratelimit
    key: ip
    limit: 100
    window: 1m
```

Available types: `validate`, `jwt`, `apikey`, `ratelimit`, `cors`, `headers`.

**Execution order is fixed by category, not by the order you list them.** The
chain always runs CORS → auth (`jwt`, `apikey`) → rate limit → validate →
transform (`headers`). Listing `strict` before `public-cors` does not move it;
a preflight is answered before authentication either way.

### `type: validate`

Enforces the published OpenAPI contract on inbound requests. Only requests are
validated — response bodies stream straight through, which keeps large
downloads working and keeps upstream latency off the critical path.

| Key | Type | Default | Description |
|---|---|---|---|
| `request` | string | `enforce` | `off`, `warn` (record the violation and forward anyway) or `enforce` (reject with 400). |
| `max_body` | int (bytes) | `1048576` (1 MiB) | Largest body that will be parsed for validation. |
| `unknown_query` | bool | `false` | Reject query parameters the operation does not declare. Off by default because clients append tracking parameters constantly and breaking them is rarely intended. |

### `type: jwt`

Validates bearer tokens against a remote JWKS. The JWKS is fetched once
synchronously at startup and reload — a URL that cannot be reached is a startup
error, not a surprise on the first request.

| Key | Type | Default | Description |
|---|---|---|---|
| `jwks_url` | string | required | Where to fetch the key set. |
| `issuer` | string | none | Required `iss`. |
| `audience` | []string | none | Every listed audience must be present in `aud`. |
| `required_scopes` | []string | none | All must be present in the scope claim. |
| `scope_claim` | string | `scope` | Claim holding scopes. `scope` (space separated) and `scp` (array) are both handled. |
| `required_claims` | map | none | Claim name → exact required value. |
| `forward_claims` | map | none | Claim name → upstream header name. `sub: X-User-Id` sends the `sub` claim as `X-User-Id`. |
| `header` | string | `Authorization` | Header carrying the token. |
| `prefix` | string | `Bearer ` | Stripped from the header value. Note the trailing space. |
| `refresh_interval` | duration | `15m` | JWKS polling interval. A failed refresh logs a warning and keeps the existing keys. |
| `clock_skew` | duration | `0` | Tolerance on `exp` and `nbf`. |

The credential is consumed by the gateway and not forwarded upstream unless
`forward_claims` explicitly copies parts of it.

This middleware publishes its own security scheme into the merged document, so
the document describes the gateway's authentication rather than the upstream's.

### `type: apikey`

Authenticates callers by a shared key mapped to a consumer name. Keys are
indexed by SHA-256 and compared in constant time.

| Key | Type | Default | Description |
|---|---|---|---|
| `in` | string | `header` | `header` or `query`. |
| `name` | string | `X-API-Key` | Header or query parameter holding the key. |
| `keys` | map | — | Consumer name → key. |
| `keys_file` | string | — | Path to a YAML/JSON file of the same mapping, so keys need not share a file (or a secret) with the routing rules. Merged over `keys`. |
| `scopes` | map | none | Consumer name → granted scopes. |
| `required_scopes` | []string | none | All must be granted to the calling consumer. |
| `forward_consumer_header` | string | none | Header carrying the resolved consumer name upstream. |
| `strip_credential` | bool | `true` | Remove the key from the upstream request, so an upstream cannot replay a client's gateway credential. |

At least one key is required. An empty key, or two consumers sharing a key, is
a startup error.

> `keys_file` is resolved against the **process working directory**, unlike
> `backends[].spec.file` which resolves against the config file's directory. Use
> an absolute path unless you are certain where the gateway is started from.

### `type: ratelimit`

| Key | Type | Default | Description |
|---|---|---|---|
| `limit` | int | required | Requests allowed per window. Must be positive. |
| `window` | duration | `1m` | Length of the window. |
| `key` | string | `ip` | What to meter on. See below. |
| `store` | string | `memory` | `memory` (per process) or `redis` (shared across instances — requires `stores.redis`). |
| `max_keys` | int | `100000` | Bound on the in-memory key set, evicted LRU. Ignored for Redis. |
| `fail_open` | bool | `true` | Allow requests when the store is unreachable. `false` fails closed and rejects them. |
| `headers` | bool | `true` | Emit `X-RateLimit-*` response headers. |

The `key` expression is compiled once at startup. Components: `ip`, `consumer`
(from apikey), `route`, `jwt.<claim>`, `header.<Name>`, `query.<name>`. Combine
with `+`:

```yaml
key: consumer + route
```

Counters are shared by middleware name, so two routes naming the same instance
meter against one budget.

> With `key: ip` behind a load balancer, set `server.trusted_proxy_cidrs` — or
> every caller is metered as one client.

### `type: cors`

| Key | Type | Default | Description |
|---|---|---|---|
| `allow_origins` | []string | `["*"]` | Allowed origins, or `*`. An origin that does not match is not an error: the headers are simply omitted and the browser enforces its own policy. |
| `allow_methods` | []string | derived | Left unset, the allowed methods come from the route table, so a preflight can never advertise a method the gateway does not serve. |
| `allow_headers` | []string | reflected | Left unset, the request's `Access-Control-Request-Headers` is reflected. |
| `expose_headers` | []string | none | `Access-Control-Expose-Headers`. |
| `allow_credentials` | bool | `false` | Cannot be combined with the `*` origin — the browser rejects that pairing, so it is a startup error instead of a silently broken request. |
| `max_age` | duration | none | How long a browser may cache the preflight. |

### `type: headers`

| Key | Type | Default | Description |
|---|---|---|---|
| `request.set` | map | none | Set on the upstream request, replacing any existing value. |
| `request.add` | map | none | Appended to the upstream request. |
| `request.remove` | []string | none | Deleted from the upstream request. |
| `response.set` / `.add` / `.remove` | — | none | The same, applied to the response. |

Response rules are applied at `WriteHeader` time, so they land before the status
line and streaming responses keep flushing.

## `expose`

A list, and the part that actually creates routes. At least one entry is
required — without one the gateway serves nothing. A backend may be exposed at
several prefixes, but the same backend and prefix twice is an error.

| Key | Type | Default | Description |
|---|---|---|---|
| `backend` | string | required | Name of a defined backend. |
| `prefix` | string | none | Mount point, e.g. `/v1`. Must start with `/` and must not end with one. |
| `include` | []string | all | Globs matched against upstream `operationId`. |
| `exclude` | []string | none | Globs removed after `include` is applied. |
| `middleware` | []string | none | Named middleware applied to these routes. Every name must exist. |
| `overrides` | map | none | Per-operation adjustments, keyed by upstream `operationId`. |

Globs are shell-style (`internal*`, `get?ser`), matched against the operationId
only. Selecting **zero** operations is an error rather than a quiet no-op —
that is almost always a mistake in the globs.

### `expose[].overrides`

```yaml
overrides:
  getUserById:
    path: /users/{id}
    timeout: 1s
```

| Key | Type | Default | Description |
|---|---|---|---|
| `path` | string | derived | Replace the gateway path entirely. Must start with `/`. Parameter agreement with the operation is verified. |
| `middleware` | []string | inherited | **Replaces** the expose entry's list rather than extending it. Partial inheritance of a security chain is exactly how an endpoint ends up unauthenticated without anyone noticing — to add one middleware, list them all. |
| `timeout` | duration | backend's | Per-request timeout for this operation. |
| `disabled` | bool | `false` | Drop the operation from the gateway and from the published document. |

## `stores`

Optional backing stores shared by middleware.

### `stores.redis`

| Key | Type | Default | Description |
|---|---|---|---|
| `addr` | string | required | `host:port`. |
| `password` | string | none | Use `${REDIS_PASSWORD}` rather than committing it. |
| `db` | int | `0` | Database number. |
| `timeout` | duration | `200ms` | Applies to dial, read and write alike. Deliberately short: a rate limiter must not become the slowest thing in the request path. |

Redis being unreachable at startup is logged but is not fatal — the gateway
starts with limiters degraded rather than refusing to serve. Whether requests
are then allowed or rejected is the middleware's `fail_open`.

---

## Reloading

The gateway watches the config file and any local `spec.file`, and reloads on
change. `SIGHUP` forces one. A reload that fails validation is rejected and the
running route table is kept, so a bad edit does not take the gateway down.

## A complete example

```yaml
version: 1

server:
  listen: "0.0.0.0:8080"
  read_header_timeout: 5s
  idle_timeout: 60s
  max_request_body: 8388608
  trusted_proxy_cidrs: ["10.0.0.0/8"]
  admin:
    listen: "127.0.0.1:9090"
    docs: true

spec:
  title: Acme Public API
  version: "1.0.0"
  description: Users and Orders, consolidated by the gateway.
  servers:
    - https://api.acme.example
  dedupe: true

defaults:
  timeout: 5s
  retry:
    attempts: 2
    backoff: 50ms
    jitter: true
    idempotent_only: true
  circuit_breaker:
    failure_ratio: 0.5
    min_requests: 20
    open_for: 10s

backends:
  - name: users
    spec:
      file: ./specs/users.yaml
    hosts:
      - ${USERS_HOST:-http://127.0.0.1:9001}
    strip_prefix: /v1
    forward_headers:
      X-Gateway: nina
    health_check:
      path: /healthz
      interval: 10s

  - name: orders
    spec:
      url: https://orders.internal/openapi.json
      refresh: 5m
      on_error: stale
    hosts:
      - https://orders-a.internal
      - https://orders-b.internal
    load_balance: least_conn
    timeout: 3s

stores:
  redis:
    addr: ${REDIS_ADDR:-127.0.0.1:6379}
    password: ${REDIS_PASSWORD:-}

middleware:
  strict:
    type: validate
    request: enforce

  public-cors:
    type: cors
    allow_origins: ["https://app.acme.example"]
    allow_headers: ["Content-Type", "Authorization"]
    allow_credentials: true
    max_age: 10m

  per-consumer:
    type: ratelimit
    store: redis
    key: consumer + route
    limit: 1000
    window: 1m

  auth:
    type: jwt
    jwks_url: https://id.acme.example/.well-known/jwks.json
    issuer: https://id.acme.example
    audience: ["api.acme.example"]
    forward_claims:
      sub: X-User-Id

  trace:
    type: headers
    response:
      set:
        X-Served-By: nina

expose:
  - backend: users
    prefix: /v1
    exclude: ["internal*"]
    middleware: [public-cors, auth, per-consumer, strict, trace]
    overrides:
      getUserById:
        timeout: 1s
      deleteUser:
        disabled: true

  - backend: orders
    prefix: /api
    middleware: [public-cors, auth, per-consumer, strict, trace]
```
