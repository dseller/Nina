# Nina

An OpenAPI-native API gateway in Go.

Nina consolidates several backend services behind one host and publishes a single
merged OpenAPI 3.1 document describing exactly what it serves. Upstream specs
drive the route table; the route table renders the published document. Neither
can drift from the other.

```
upstream specs --ingest--+
                         +--> RouteTable --render--> merged OpenAPI 3.1
gateway config ----------+        |
                                  +--> HTTP router + per-route middleware
```

Because the router and the published document are built from the same structure,
**a route that is not served cannot appear in the document, and a route that is
served always does** — unless you hide it on purpose with
[`spec.hide_tags`](#hiding-endpoints). There is a test that asserts exactly this.

## Quick start

```sh
make build

# Validate the config, fetch every upstream spec, report collisions. Exits
# non-zero on any problem, so it works as a CI gate.
bin/nina check -c examples/nina.yaml

# Render the merged document.
bin/nina spec build -c examples/nina.yaml

# Serve.
bin/nina run -c examples/nina.yaml
```

With the example config, two services that know nothing about each other are
published as one API:

```
GET     /api/orders          -> orders  /orders
POST    /api/orders          -> orders  /orders
GET     /api/users           -> users   /v1/users
POST    /api/users           -> users   /v1/users
GET     /api/users/{id}      -> users   /v1/users/{id}
```

## What it does

| | |
|---|---|
| **Spec ingest** | OpenAPI 3.0 and 3.1, from files or URLs, with ETag caching and periodic re-fetch. External `$ref`s are bundled; 3.0 idioms (`nullable`, boolean `exclusiveMinimum`) are normalised to their 3.1 form |
| **Spec publish** | One merged 3.1 document at `/openapi.json`, `/openapi.yaml` and `/docs`, with `x-nina-*` provenance on every operation. Operations carrying a configured tag can be served without being documented |
| **Merging** | Components namespaced per backend (`UsersUser`); structurally identical components sharing a base name collapse to one (`Error`); route collisions are a hard error naming both claimants |
| **Proxying** | 1:1 operation mapping, per-backend connection pools, streaming responses |
| **Resilience** | Timeouts, bounded retries with jitter (idempotent methods only), circuit breaker, round-robin / least-conn / random load balancing, active health checks |
| **Auth** | JWT with JWKS caching and rotation, API keys mapped to consumers, both with scope enforcement |
| **Rate limiting** | In-memory (LRU-bounded) or Redis-backed sliding window, keyed by IP, consumer, JWT claim, header or query |
| **Validation** | Request parameters and JSON bodies validated against the published schemas. `off` / `warn` / `enforce` |
| **Hot reload** | Config and spec changes rebuild and swap atomically, with no dropped requests. A failed reload never disturbs the running gateway |
| **Observability** | Prometheus metrics, structured `slog` access logs, `/healthz`, `/readyz`, `/status`, `/routes` |

Only requests are validated. Response bodies are streamed through untouched,
which keeps large downloads working and upstream latency off the critical path.

## Configuration

See [`examples/nina.yaml`](examples/nina.yaml) for a commented example. YAML and
JSON are both accepted — YAML is converted to JSON and decoded from there, so one
set of rules covers both. `${VAR}` and `${VAR:-default}` interpolate from the
environment; an undefined variable with no default is an error rather than an
empty string.

**Unknown fields are rejected.** A silently ignored typo in a gateway config is a
production incident waiting to happen.

### Upstream TLS

Certificate verification is on by default. A backend presenting a self-signed or
otherwise unverifiable certificate can opt out:

```yaml
backends:
  - name: legacy
    hosts: ["https://legacy.internal"]
    tls:
      insecure_skip_verify: true
```

This applies to every connection to that backend — proxied traffic, active health
checks, and the fetch of its OpenAPI document — and to that backend only. The
connection stays encrypted but is no longer authenticated: any certificate is
accepted, so an attacker interposed on the path to the upstream would not be
detected. Where the issuing CA can be trusted instead, do that.

Turning it on logs a warning on every runtime build and shows up as
`tls_verification_disabled` on the admin `/status` endpoint, so it can be audited
without reading the config.

### Hiding endpoints

Some operations are served but should not be advertised: an internal maintenance
call, a partner-only endpoint, an operation still being stabilised. List the tags
that mark them and they stay out of the published document:

```yaml
spec:
  hide_tags: ["internal", "partner-only"]
```

An operation is hidden when any of its upstream tags matches. Matching is exact
and case-sensitive, because a tag is an arbitrary string in someone else's
document and guessing at case would hide things nobody asked to hide.

A hidden route is served exactly like any other: same middleware chain, same
request validation against the same upstream schemas, same proxying. Only the
documentation is withheld — along with any component that no visible operation
references, so a hidden endpoint does not leak its schemas into the document.

This is the only sanctioned way to break the document-matches-router invariant,
so hidden routes stay auditable from outside the config: they are listed with
`"hidden": true` on the admin `/routes` endpoint, counted as `hidden_routes` on
`/status`, logged on every runtime build, and marked `[hidden]` by `nina check`.

Two things this is not. It is not access control — the endpoint is still reachable
by anyone who knows the path, so put auth middleware on it. And it is not
`exclude`, which drops the operation from the route table entirely and therefore
stops serving it:

| | Served | Published |
|---|---|---|
| `expose.exclude` / `overrides.<id>.disabled` | no | no |
| `spec.hide_tags` | yes | no |

### Merging strategy

Each backend gets a namespace derived from its name (`user-profiles` becomes
`UserProfiles`). Every component reachable from an exposed operation is copied
into the merged document under that prefix, with `$ref`s rewritten.

A dedupe pass then collapses components that are structurally identical *and*
share a base name. Equality is checked through `$ref`s across the whole reachable
subgraph, so two `User` schemas only merge if everything they point at matches
too. Without this, merging eight services yields eight near-identical `Error`
schemas.

Namespacing makes component collisions impossible. Route collisions — two
backends claiming the same method and path — cannot be resolved that way, so they
fail the build and name both claimants.

### Middleware ordering

Order is fixed by category, not by the order you list things:

```
cors -> auth -> ratelimit -> validate -> cache -> transform -> script -> proxy
```

Within a category, configuration order applies. This means you cannot
accidentally place rate limiting before authentication and meter requests that
were about to be rejected anyway.

An `overrides` block *replaces* the middleware list rather than extending it.
Partial inheritance of a security chain is how endpoints end up unauthenticated
without anyone noticing.

### CORS preflight

`OPTIONS` is a browser protocol detail, not an API operation, so OpenAPI cannot
describe it and the published document does not claim it. Nina synthesises an
`OPTIONS` route in the router for every path whose routes use CORS middleware,
advertising the methods actually served at that path. These routes exist only in
the router; they never reach the document.

## Operations

The admin listener binds separately from the gateway so none of it has to be
public:

| Endpoint | |
|---|---|
| `/metrics` | Prometheus |
| `/healthz` | Liveness |
| `/readyz` | Ready once a runtime has been built. Deliberately **not** tied to upstream health, so one sick backend cannot pull the gateway out of a load balancer |
| `/status` | Generation, route count, hidden-route count, per-backend breaker state and host health |
| `/routes` | What is served, what it maps to, and whether it is hidden from the document |
| `/openapi.json`, `/openapi.yaml`, `/docs` | The merged document and a viewer |

### Reload

Reloads are triggered by a config or local spec file change, by `SIGHUP` (not on
Windows), or by an upstream spec whose content hash changed on a refresh poll.

A new runtime is built completely — specs fetched, merged, schemas and routes
compiled — before anything is swapped. If any step fails the running runtime is
untouched and the error is logged with its stage and reason.
**A typo in a config file must never take down a healthy gateway.**

In-flight requests hold a reference to the runtime they started on and finish
against it. The retired runtime closes once they drain, or after the grace period.

## Development

```sh
make check      # vet + tests
make race       # needs cgo and a C toolchain; CI runs this on Linux
make build
```

The highest-value test surfaces, in order:

- `internal/oas/merge_test.go` — golden tests for the merge. Merge bugs are subtle and silent.
- `internal/runtime/integration_test.go` — `TestPublishedSpecMatchesServedRoutes` is the invariant above; `TestReloadSwapsRoutesWithoutDroppingRequests` drives live traffic through repeated reloads.
- `internal/oas/libopenapi_contract_test.go` — pins the library behaviour the merger depends on.

## Design notes

**Why a hand-rolled router.** OpenAPI path templates need exact `{param}`
semantics, a static-beats-parameter rule with backtracking, and the matched route
object handed straight back. ~300 lines is cheaper than fighting a router's
opinions. It matches in ~180ns with 2 allocations.

**Why the raw YAML tree for merging.** `libopenapi` parses and validates well,
but the merge has to be lossless — vendor extensions, examples, descriptions and
key ordering all survive into the published document — and every `$ref` rewrite
has to be under our control. So the typed model validates, and the merge operates
on `yaml.Node`. `libopenapi_contract_test.go` pins the properties this relies on.

**Why per-backend transports.** One slow upstream must not exhaust the connection
pool another one needs.

**Why fail-open rate limiting by default.** A dead Redis should degrade rate
limiting, not cause a total outage. Operators who need the opposite can set
`fail_open: false`.

### Deviations from PLAN.md

Three, all deliberate:

- **Circuit breaker is hand-written** rather than `sony/gobreaker`. The gateway
  needs to publish breaker state on `/status` and to control exactly when a
  half-open probe is admitted; wrapping a library to get both back was more code
  than the ~150 lines it replaced.
- **JWKS caching is hand-written** on top of `lestrrat-go/jwx` rather than using
  its `jwk.Cache`. Same reason: explicit control over refresh, failure logging,
  and what happens when a refresh fails (keep serving with the keys we have).
- **Config validation uses strict struct decoding plus explicit semantic checks**
  instead of the normative `schema.json` the plan called for. This gives much
  better error messages — file, field path, and what to do about it — but it is
  not a schema anyone can consume. The plan's version is still the right end
  state; this is the cheaper 90%.

## Status

Working and tested end to end: merge, publish, route, proxy, validate, auth, rate
limit, CORS, resilience, hot reload, metrics.

Not built yet: response caching, Lua scripting, and a normative JSON Schema for
the config file (validation is currently done with strict struct decoding plus
explicit semantic checks, which gives better error messages but is not a
publishable schema). Response validation, gRPC and WebSocket are out of scope by
design — see [PLAN.md](PLAN.md).
