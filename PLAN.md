# Nina — OpenAPI-native API Gateway

A KrakenD-class API gateway in Go where the OpenAPI document is a first-class citizen in
both directions: upstream specs drive the route table, and the route table renders the
published gateway spec.

## 1. Decisions

Confirmed with the user:

| Area | Decision |
|---|---|
| Scope | Full KrakenD replacement (proxy, auth, rate limiting, circuit breaking, caching) |
| Spec flow | Bidirectional — ingest upstream specs *and* emit a merged gateway spec |
| Aggregation | 1:1 proxy only. Consolidation = namespacing many APIs under one host/spec, **not** response merging |
| Config | File-based (YAML, JSON accepted), hot reload on change. No control plane, no DB |
| Spec sources | Local files and remote URLs, with periodic re-fetch and live route reload |
| Protocols | HTTP/JSON only |
| Extensibility | Compiled-in middleware registry + embedded Lua scripting |
| Auth | JWT (JWKS) and API keys |
| Rate limiting | Pluggable store: in-memory default, Redis for cluster-wide limits |
| Validation | Request validation only. Response bodies are never parsed |
| Observability | Prometheus metrics + health/readiness endpoints |

Assumed (open questions the user chose not to answer — revisit before M2):

- **Merge strategy:** prefix namespacing (`/users/...`, schemas to `UsersUser`), with
  structurally-identical component dedupe, and a hard build failure on any collision that
  namespacing cannot resolve.
- **OpenAPI versions:** accept 3.0.x and 3.1.x on input (3.0 upconverted internally),
  emit 3.1. Swagger 2.0 ingest and 3.0 downconvert output are post-v1.
- **Resilience:** all four — timeouts/retries, circuit breaker, response caching,
  load balancing with health checks.
- **Admin surface:** separate admin listener (metrics, health, spec, docs) plus
  `nina check` and `nina spec build` CLI subcommands for CI.

## 2. The core idea

The thing that makes this not-KrakenD is that **the route table is the single intermediate
representation**, and both directions pass through it:

```
upstream specs --ingest--+
                         +--> RouteTable --render--> merged gateway OpenAPI 3.1
gateway config ----------+        |
                                  +--> HTTP router + per-route middleware chain
```

Because the served router and the published document are built from the same structure,
the spec cannot drift from reality. A route that is not served cannot appear in the
document, and a route that is served always does. This is the property to protect in
design reviews and to assert in tests.

Consequence: request validation is nearly free. The schemas used to validate an inbound
request are the same objects rendered into the published spec, compiled once at load.

## 3. Package layout

```
cmd/nina/                  CLI entrypoint (run | check | spec build | version)
internal/config/           load, env interp, includes, JSON Schema validation
internal/specsrc/          fetch upstream specs (file/http), cache, ETag, poll for changes
internal/oas/              parse, 3.0->3.1 upconvert, namespace, dedupe, merge, render
internal/routetable/       the IR: Route, Backend, chain spec; built from config+specs
internal/router/           radix matcher, atomically swappable
internal/runtime/          Runtime object, build + atomic swap + drain
internal/proxy/            upstream transport, LB, health checks, retry, breaker
internal/chain/            middleware registry, chain assembly, ordering rules
internal/mw/               one package per middleware (jwt, apikey, ratelimit,
                           validate, cache, cors, headers, lua, ...)
internal/store/            KV abstraction: memory + redis (rate limits, cache)
internal/observ/           prometheus metrics, slog setup, health tracking
internal/admin/            admin listener: /metrics /healthz /readyz /openapi.json /docs
```

Rule: `internal/mw/*` may depend on `chain` and `store` but never on `config` or `oas`.
Middleware receives already-resolved, already-validated typed settings. This keeps
config-schema churn out of the request path.

## 4. Config shape

Sketch of the intended surface. The JSON Schema in `internal/config/schema.json` is the
normative version, and is also what `nina check` enforces:

```yaml
version: 1

server:
  listen: ":8080"
  read_header_timeout: 5s
  admin:
    listen: "127.0.0.1:9090"
    docs: true          # serve /openapi.json and /docs on the admin listener

defaults:               # inherited by every backend/route unless overridden
  timeout: 3s
  retry: { attempts: 2, backoff: 50ms, jitter: true, idempotent_only: true }
  circuit_breaker: { failure_ratio: 0.5, min_requests: 20, open_for: 10s }

backends:
  - name: users
    spec:
      url: https://users.internal/openapi.json
      refresh: 5m       # re-fetch; reload routes only if content hash changes
      on_error: stale   # stale | fail
    hosts: ["https://users-1.internal", "https://users-2.internal"]
    load_balance: round_robin
    health_check: { path: /healthz, interval: 10s, unhealthy_after: 3 }

  - name: orders
    spec: { file: ./specs/orders.yaml }
    hosts: ["https://orders.internal"]

expose:
  - backend: users
    prefix: /users                    # gateway path prefix
    include: ["get*", "listUsers"]    # operationId globs; omit = all operations
    exclude: ["internal*"]
    middleware: [jwt, ratelimit-user]

  - backend: orders
    prefix: /orders
    overrides:
      createOrder:
        path: /orders/new             # explicit gateway path for one operation
        middleware: [jwt, validate-strict]

middleware:
  jwt:
    type: jwt
    jwks_url: https://idp.example.com/.well-known/jwks.json
    issuer: https://idp.example.com/
    audience: [nina]
    required_scopes: [api.read]
    forward_claims: { sub: X-User-Id }

  ratelimit-user:
    type: ratelimit
    store: redis
    key: "jwt.sub"
    limit: 100
    window: 1m

  validate-strict:
    type: validate
    request: enforce      # off | warn | enforce

stores:
  redis: { addr: "redis:6379", db: 0 }
```

Notes on the format:

- YAML is parsed by converting to JSON (`sigs.k8s.io/yaml`) and unmarshalling that, so a
  `.json` config works with zero extra code and one JSON Schema validates both.
- `${ENV_VAR}` interpolation and `!include` of sub-files, resolved before schema validation
  so error positions still point at the authored file.
- Config validation errors are reported with file, line, and JSON pointer. This is a
  quality bar, not a nice-to-have — bad gateway config is the most common failure mode.

## 5. Hot reload

`runtime.Runtime` is an immutable value holding: the router, backend pools, compiled
schemas, compiled Lua chunks, and middleware instances. The server holds
`atomic.Pointer[Runtime]`.

Reload sequence:

1. Trigger: fsnotify on config files, spec refresh poller, or SIGHUP.
2. Build a **complete** new `Runtime` off to the side — parse config, fetch/read specs,
   merge, build route table, compile schemas and Lua. Dial nothing yet.
3. If any step fails: keep the old runtime, log the error, increment
   `nina_reload_failures_total`, leave `/readyz` green. **A bad reload must never take
   down a healthy gateway.**
4. On success: `atomic.Store` the pointer. New requests get the new runtime.
5. Drain the old one: in-flight requests captured the old pointer at entry and finish
   against it. The old runtime's transports close after a grace period (default 30s),
   gated by a refcount held for each request's duration.

Debounce file events (~200ms) so an editor's write-then-rename doesn't trigger three
rebuilds. Spec re-fetch compares a content hash and skips the rebuild entirely when
unchanged — this matters, because a 5-minute poll across 20 backends must not mean a
rebuild every 5 minutes.

## 6. Spec ingest and merge

**Ingest.** `specsrc` fetches from file or URL, honors ETag/Last-Modified, and caches the
last good copy on disk. Failure policy is explicit per backend: `fail` (refuse to build the
runtime) or `stale` (use the cached copy, mark the backend degraded). On first boot with no
cache, an unreachable spec is always fatal — better to fail loudly at startup than to serve
an empty route table.

**Merge.** For each exposed operation:

- Gateway path = backend prefix + upstream path, path-template parameters preserved.
- `operationId` is prefixed to stay unique (`users_listUsers`); the original is kept in
  `x-nina-upstream-operation-id`.
- Every `#/components/*` reference reachable from the operation is copied into the merged
  document under a prefixed name (`UsersUser`), rewriting `$ref`s as it goes.
- Dedupe pass: components that are structurally identical after canonicalization collapse
  to one shared name. This is what keeps a merged 8-backend document readable instead of
  producing eight near-identical `Error` schemas.
- Anything namespacing cannot resolve — two backends claiming the same gateway path — is a
  hard build error naming both sources. Never last-wins silently.

**Render.** The merged document gets gateway-level `servers`, security schemes derived from
the auth middleware actually configured (so the published spec documents the auth the
gateway really enforces, not what the upstream wanted), and `x-nina-*` extensions recording
provenance for every operation.

Library choice: `github.com/pb33f/libopenapi` for parse/mutate/render, since it handles both
3.0 and 3.1 and supports rendering a mutated document. `kin-openapi` is more battle-tested
for request validation but is 3.0-only, which conflicts with the 3.1 output decision.
**Spike this in M0 before committing** — the merge/rewrite work is the part most likely to
hit library limitations, and discovering that in M4 would be expensive.

## 7. Request path

Ordered chain, assembled per route at build time, not per request:

```
recover -> request-id -> access-log -> cors -> auth -> ratelimit -> validate
        -> cache-lookup -> lua-request -> proxy -> cache-store -> lua-response
```

Ordering is fixed by category rather than by config order, so a config author cannot
accidentally put rate limiting before auth. Within a category, config order applies. The
registry maps a `type` string to a constructor
`func(json.RawMessage) (Middleware, error)`.

**Validation** compiles each operation's parameter and requestBody schemas once at runtime
build. Per request: validate path/query/header params and, if the body is JSON and a schema
exists, the decoded body. Bodies over a configured limit are rejected rather than buffered.
Non-JSON bodies pass through untouched. Response bodies are never read — the proxy streams
straight through, which keeps large downloads and chunked responses working.

**Proxy** uses a dedicated `http.Transport` per backend (own connection pool, own tuning),
not a shared default client. Hop-by-hop headers stripped, `X-Forwarded-*` set, upstream host
selected by the LB, breaker consulted before dial. Retries fire only on idempotent methods
and only on connection errors or 502/503/504 — never on a response the upstream produced
deliberately.

**Lua** uses `gopher-lua`. Chunks compile once and `lua.LState`s are pooled — creating a
state per request is the obvious performance trap here. Scripts get a restricted API (read
and modify headers, read and modify a JSON body, set status, abort) with no filesystem, no
network, and an instruction-count budget so a runaway script cannot pin a core.

## 8. Cross-cutting

**Auth.** JWT: JWKS fetched and cached with background refresh and rotation handling
(`lestrrat-go/jwx/v3`), validating signature, `iss`, `aud`, `exp`/`nbf`, plus required
claims and scopes per route. API keys: keys defined in config or a referenced file, mapped
to a named consumer, read from a configurable header or query param, compared in constant
time. The resolved consumer identity lands in the request context so rate limiting and
logging can key on it.

**Rate limiting.** A `store.Limiter` interface with two implementations: in-memory
(`golang.org/x/time/rate` per key, LRU-bounded so unbounded key cardinality cannot leak) and
Redis (atomic Lua script, sliding window). The key is a small expression over request
context — `jwt.sub`, `apikey.consumer`, `ip`, `header.X-Tenant`, or a combination. Redis
unavailability is fail-open by default with a metric and a log, configurable to fail-closed.
A dead Redis must not become a total outage unless the operator asked for that.

**Caching.** GET/HEAD only, keyed by method + gateway path + a configured `vary` set.
In-memory LRU by default, Redis optional. Honors upstream `Cache-Control: no-store`.

**Observability.** Prometheus: request counter and latency histogram by route/method/status,
upstream latency, breaker state gauge, rate limit decisions, reload success/failure, spec
fetch outcomes. `/healthz` is liveness. `/readyz` goes green once a runtime has ever been
built successfully — deliberately not tied to upstream health, so one sick backend does not
get the whole gateway pulled from the load balancer. Structured slog access logs with
configurable field redaction.

## 9. Milestones

Each milestone ends with something runnable.

- **M0 — Spikes (before committing to the design).**
  Prove `libopenapi` can merge two real specs with `$ref` rewriting and render valid 3.1.
  Prove the atomic-swap reload holds under `-race` with load. Benchmark the proxy hot path
  against a stub upstream to set a baseline. *Exit: go/no-go on the library choice.*

- **M1 — Skeleton proxy.** Config load + JSON Schema validation, static route table written
  by hand in config, radix router, single-host proxy, slog, `/healthz`.
  *Exit: proxies a request to one backend.*

- **M2 — Spec ingest to routes.** `specsrc` (file + URL), 3.0 to 3.1 upconvert, route table
  derived from upstream operations with include/exclude/overrides.
  *Exit: routes appear from a spec with no hand-written paths.*

- **M3 — Merge and publish.** Namespacing, component rewriting, dedupe, collision errors,
  merged 3.1 render, admin listener serving `/openapi.json` and `/docs`, `nina spec build`.
  *Exit: one document describing all backends, plus a golden test asserting served routes
  and documented routes are the same set.*

- **M4 — Hot reload.** fsnotify + SIGHUP + spec poll, full runtime rebuild, atomic swap,
  drain, failed-reload safety.
  *Exit: edit config or upstream spec, watch routes change with zero dropped requests under
  load.*

- **M5 — Resilience.** Timeouts, retries, circuit breaker, multi-host LB, active health
  checks, response cache.
  *Exit: kill a backend host mid-load; traffic shifts, breaker trips and recovers.*

- **M6 — Security and limits.** JWT/JWKS, API keys, rate limiting on both stores, request
  validation with off/warn/enforce.
  *Exit: unauthenticated and malformed requests rejected at the edge.*

- **M7 — Extensibility and polish.** Middleware registry hardening, Lua with state pooling
  and budgets, CORS, header manipulation, Prometheus metric completeness, `nina check`,
  container image, docs.
  *Exit: v1.*

## 10. Testing

- **Golden files** for spec merge — input specs in, expected merged document out. Highest-
  value test surface in the project; merge bugs are subtle and silent.
- **The invariant test:** for a given config, assert the set of routes the router matches
  equals the set of operations in the published document. Run it against every fixture.
- **Table tests** for routing, key expressions, chain ordering.
- **`httptest` integration** with stub upstreams for proxy, retry, breaker, cache, auth.
- **Race detector on everything**, especially reload — that is where the concurrency bugs
  will be.
- **Load test during reload** (M4 exit criterion): sustained traffic, repeated reloads,
  assert zero 5xx caused by the gateway itself.
- **Fuzz** the config loader and the path matcher.

## 11. Risks

| Risk | Mitigation |
|---|---|
| `libopenapi` cannot cleanly rewrite `$ref`s at the scale needed | M0 spike is a gate, not a formality. Fallback: operate on a decoded generic JSON tree and hand-roll the rewrite |
| 3.1 output breaks consumers' SDK generators | Add a 3.0 downconvert output if it bites; keep the renderer behind an interface so a second target is cheap |
| Request validation costs more latency than expected | Compile schemas once, cache by operation, benchmark in M0, allow `off` per route |
| Reload leaks connections or memory over many cycles | Explicit refcount + drain, plus a soak test doing thousands of reloads |
| Lua becomes a support burden (runaway scripts, blocked cores) | Instruction budget, state pooling, no I/O in the sandbox, per-script timeout |
| Unbounded rate-limit key cardinality exhausts memory | LRU-bounded in-memory store with an explicit max-keys setting |
| Scope creep back toward response aggregation | 1:1 is a decision, not a limitation. Revisit only after v1 ships |

## 12. Dependencies

Deliberately small. Each of these earns its place:

- `github.com/pb33f/libopenapi` — OpenAPI 3.0/3.1 parse, mutate, render *(pending M0)*
- `github.com/santhosh-tekuri/jsonschema/v6` — JSON Schema validation (config + requests)
- `sigs.k8s.io/yaml` — YAML to JSON so one code path handles both formats
- `github.com/fsnotify/fsnotify` — config watching
- `github.com/lestrrat-go/jwx/v3` — JWT + JWKS with caching and rotation
- `github.com/prometheus/client_golang` — metrics
- `github.com/redis/go-redis/v9` — Redis store
- `github.com/yuin/gopher-lua` — embedded Lua
- `github.com/sony/gobreaker/v2` — circuit breaker
- `github.com/hashicorp/golang-lru/v2` — bounded caches
- `github.com/spf13/cobra` — CLI
- `golang.org/x/time/rate` — in-memory limiter

Routing uses a hand-rolled radix matcher rather than a third-party router: OpenAPI path
templates need exact `{param}` semantics, explicit priority rules, and the ability to hand
back the matched `Route` object. Owning roughly 300 lines is cheaper than fighting a
router's opinions.
