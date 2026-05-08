# External Secret Resolution (Sketch)

**Status:** sketch
**Branch:** `jcasimir/op-secrets-sketch`
**Author:** jcasimir

## Goal

Let `app.toml` reference secrets by URI (initially `op://vault/item/field`)
without writing the resolved value to the manifest, etcd, or any control-plane
storage. Resolution happens on the runner at sandbox boot. The 1Password
integration itself should live outside the core runtime — minimally invasive,
plugin-shaped.

Resolved values *do* persist to the runner host's local disk in an
availability cache (see [Persistent cache](#persistent-cache) below). That's
an explicit tradeoff: the runner host already holds the OP service-account
token, so the trust boundary is the same. The win is graceful degradation
when the secrets backend is briefly unreachable.

```toml
[[services.web.env]]
key = "DATABASE_URL"
value_from = "op://Production/litellm/db_url"
```

## Non-goals

- Replacing the existing literal `value = "..."` form (still works for non-secret config).
- Secret rotation while a sandbox is running. Rotation happens on restart only.
- A secret-management UI. The manifest is the source of truth for *what* to fetch;
  the secrets store owns the values.

## What we'd add to the core runtime

The minimum core change is a **single resolver dispatch point** plus one new
field on env vars. Everything 1Password-specific lives outside core.

### 1. `value_from` field on env vars

`appconfig.AppEnvVar` (`appconfig/appconfig.go:18`) gains:

```go
ValueFrom string `toml:"value_from,omitempty"`
```

Parse-time validation: reject configs that set both `value` and `value_from` on
the same entry. Reject `value_from` strings whose scheme has no registered
resolver (catches typos like `op:/Production/...` at deploy time, not boot
time — same philosophy as `port_timeout` validation).

Schema mirror in `api/core/schema.yml` under `services.env` and the top-level
`variable` component, so the URI persists into `ConfigSpec` (etcd holds the
*pointer*, never the value).

### 2. The resolver dispatch hook

One call site in the runner's container env assembly. Today, env is
materialized into `appCont.Env []string` at
`controllers/deployment/launcher.go:637-659`. We *don't* resolve there — that's
server-side and would land secrets in `SandboxSpec` in etcd. Instead:

- `launcher.go` propagates `value_from` through to `SandboxSpecContainer.Env`
  as a pseudo-entry — e.g., a parallel `EnvFrom []SandboxSpecContainerEnvFrom`
  field on the sandbox spec, *or* a sentinel string format that the runner
  recognizes. (The first is cleaner; second is two fewer schema changes.
  Open question — see below.)
- The runner-side env materialization in
  `controllers/sandbox/sandbox.go::buildSubContainerSpec` (≈line 1851, where
  `co.Env` becomes `oci.WithEnv(envVars)`) gains one block:

```go
resolved, err := secrets.Resolve(ctx, co.EnvFrom)  // ← the only new call
if err != nil {
    return nil, fmt.Errorf("resolving secrets for sandbox %s: %w", sb.ID, err)
}
envVars = append(envVars, resolved...)
```

That's it. Core runtime grows by maybe 50 lines (schema + appconfig field +
propagation + the one call site).

### 3. Resolver registry

A trivial package — `pkg/secrets/` — exposes:

```go
type Resolver interface {
    Scheme() string                                       // "op"
    Resolve(ctx context.Context, uri string) (string, error)
}

func Register(r Resolver)
func Resolve(ctx context.Context, refs []EnvFromRef) ([]string, error)
```

Default registry is empty. The runner registers resolvers at startup based on
configuration — see below.

## What lives outside core

The 1Password resolver is its own package (or eventually its own binary) that
imports `pkg/secrets` and calls `secrets.Register(...)` from an `init()` or an
explicit setup call wired in `cmd/miren-runner/main.go` behind a config flag.

Two viable shapes, in order of invasiveness:

**Shape A — in-tree package, opt-in via runner config.** `pkg/secrets/onepassword/`
implements `Resolver` using the `op` CLI or the 1Password Connect HTTP API.
The runner config (`runner.toml` or env) gates registration:

```toml
[secrets.onepassword]
service_account_token_file = "/etc/miren/op-token"
cache_ttl = "30s"
```

This is the *minimal* shape — one import, no protocol design, no IPC. It still
satisfies "the OP-specific code is its own module."

**Shape B — out-of-process plugin.** Same `Resolver` interface, but `Resolve`
calls a sidecar over a unix socket (gRPC or simple JSON line protocol). The
sidecar is a separate binary (`miren-secrets-onepassword`) that the runner
launches or connects to. Buys real isolation (the OP token only lives in the
sidecar's address space, not the main runner's), at the cost of designing a
stable plugin protocol.

**Recommendation:** ship Shape A first. Get the resolver hook landed and prove
the model with one provider. Promote to Shape B if/when there's a second
provider (Vault, AWS Secrets Manager) or a credible threat model that warrants
process isolation. The interface is the same either way — Shape B is a
non-breaking upgrade.

### Mixed-maturity clusters: complementary low-ceremony resolvers

A single Miren cluster will run apps at very different levels of secrets
maturity — a production app fully wired to 1Password next to a Tuesday-afternoon
spike whose author hasn't created an OP vault yet. The design accommodates
this on two axes without ever needing multiple resolvers per scheme:

1. **Literal `value` keeps working.** Apps that don't want any backend just
   write `value = "..."` as today. No resolver path, no token required.
2. **Other schemes can register alongside `op://`.** Two natural complements
   to file as easy follow-ups, each as their own tiny in-tree package:

   - **`env://VAR_NAME`** — read from the runner host's environment. Useful
     for spike apps where the operator wants to drop a value into the
     runner's systemd unit and not bother with 1Password yet. No upstream
     dependency, no token.
   - **`file:///path/on/runner`** — read from a file the operator placed on
     the runner. Same use case as `env://`, different ergonomics; pairs
     well with config-management tools.

   Both are ~50 lines each and reuse the same registry, cache, and
   stale-fallback machinery as the OP resolver. They're not v1 scope, but
   the design admits them cleanly when someone wants them.

The case that *would* require multi-resolver-per-scheme is per-tenant routing
(App A's `op://` URIs hit account A; App B's hit account B). That's better
solved by encoding the discriminator in the URI itself
(`op://account=team-a@Production/...`) and letting the single OP resolver
route internally.

## Token bootstrap (the unavoidable secret)

The OP service-account token has to live somewhere on each runner host. This
is the one thing this design *cannot* eliminate; it collapses N secrets to 1.
Acceptable bootstrap paths in priority order:

1. File on the runner host loaded at process start (`/etc/miren/op-token`,
   mode 0600, owned by the runner user). Ops's responsibility, same shape as
   the existing miren-runner credentials.
2. systemd `EnvironmentFile=` for `make`-managed deployments.
3. Cloud-provider instance metadata for hosted setups (later).

The token is read once at runner startup, held in memory, never logged, never
serialized.

## Persistent cache

The resolver registry sits in front of an on-disk cache so a runner that just
restarted — or a 1P backend that's briefly unavailable — can still boot
sandboxes with the last-known values. This trades some surface area (resolved
values briefly persist to local disk) for real availability.

**Location.** `/var/lib/miren/runner/secrets-cache/`, mode 0700, owned by the
runner user. One file per URI, named by `sha256(uri)` to avoid filename
escaping. Atomic writes via tmp + rename.

**Entry format (CBOR or JSON):**

```
{
  "uri":                       "op://Production/litellm/db_url",
  "value":                     "...",
  "fetched_at":                "2026-05-07T14:22:01Z",
  "last_used_at":              "2026-05-07T14:22:01Z",
  "restarts_since_last_use":   0,
  "resolver":                  "onepassword/v1"
}
```

Stored unencrypted in v1 (see [v2: encryption at rest](#v2-encryption-at-rest)).

**Resolution flow at sandbox boot:**

1. **In-memory hit, fresh** (within in-memory TTL, default 30s): use it.
2. **Miss or in-memory stale**: call the live resolver.
   - On success: write through to in-memory + disk cache, return value.
   - On failure: fall through to the disk cache.
3. **Disk-cache hit**: use the value, log a structured warning (`secrets.cache.stale_fallback` with URI and `fetched_at`), emit a metric. Sandbox boots, operator gets paged.
4. **No cache, resolver failed**: hard fail; the sandbox doesn't boot. Surface in `m sandbox describe`.

**Eviction.** Per-entry counter `restarts_since_last_use`. On every
successful lookup the counter resets to 0; on every runner startup the
counter increments for every entry not touched during the previous run.
Entries with counter ≥ 2 are evicted at startup. A URI in continuous use
across restarts stays cached forever; a URI that disappears from `app.toml`
gracefully ages out within two runner restarts.

**Bounds.** Soft per-host bound on cache size (default 1000 entries) as a
safety net against runaway URI churn. Cache size is operational hygiene, not
a correctness concern.

**Logging.** Resolution failures emit `WARN`-level structured logs in two
distinct cases, both with sandbox ID, URI, and resolver name (never the
value):

- `secrets.resolver.unavailable` — the resolver itself failed (network,
  auth, malformed response). The operator's signal that a *source* is down.
- `secrets.value.unavailable` — the resolver succeeded but returned no value
  for the URI (item deleted, field renamed). The operator's signal that a
  *specific key* needs attention.

These warn even when the disk cache absorbs the failure (i.e. the sandbox
still boots) — silent fallback would mask exactly the conditions an operator
needs to see.

**Operator controls.** Part of v1 scope:

- `m runner secrets cache flush` — evict all entries on a single runner.
- `m runner secrets cache flush <uri>` — evict one entry.

Useful after a forced secret rotation where you need every runner to
re-fetch on next sandbox restart.

**What this does NOT do.** The cache is per-runner; there is no replication
or coordination between runners. That's deliberate — each runner is a
separate trust boundary, and consistency falls out of "every resolver returns
the same value for the same URI." If a runner's cache disagrees with another
runner's cache, the next successful upstream fetch reconciles them.

### v2: encryption at rest

Cache files in v1 are mode-0700 plaintext on the runner host. Same effective
exposure as the OP service-account token sitting next to them — anyone with
root on the host already has both. Plaintext keeps v1 simple and debuggable.

For v2, AEAD-encrypt entries with a key derived from `/etc/machine-id` plus
a runner-provisioned per-install salt. Buys defense-in-depth against:

- Stolen disk images (the cache file is unreadable on a different host).
- Snapshots and backups that capture `/var/lib/miren` without the OP token.

Estimated cost: a few hundred lines, plus a key-derivation/rotation story
(what happens when `machine-id` changes, e.g. after a reimage). Worth doing
when there's a second resolver in tree or a customer with a compliance ask
that names "encrypted at rest" specifically.

## Token bootstrap (the unavoidable secret)

The OP service-account token has to live somewhere on each runner host. This
is the one thing this design *cannot* eliminate; it collapses N secrets to 1.
Acceptable bootstrap paths in priority order:

1. File on the runner host loaded at process start (`/etc/miren/op-token`,
   mode 0600, owned by the runner user). Ops's responsibility, same shape as
   the existing miren-runner credentials.
2. systemd `EnvironmentFile=` for `make`-managed deployments.
3. Cloud-provider instance metadata for hosted setups (later).

The token is read once at runner startup, held in memory, never logged, never
serialized.

## Failure modes

| Scenario | Behavior |
|---|---|
| Resolver itself fails (network/auth), **cache hit** | Use cached value. Emit `secrets.resolver.unavailable` WARN. Sandbox boots. |
| Resolver succeeds but value missing for URI, **cache hit** | Use cached value. Emit `secrets.value.unavailable` WARN. Sandbox boots. |
| Either failure, **no cache** | Sandbox transitions to `failed`, error surfaced in `m sandbox describe`. **Don't** fall back to empty string — that's the silent-failure mode `port_timeout` validation was added to prevent. |
| 1P API unreachable, runner just restarted | Disk cache survives the restart; resolution serves from cache + emits the `resolver.unavailable` warn. |
| Token missing/expired at runner startup | Runner startup fails fast with a clear error pointing at the token file. Don't start the runner half-configured. |
| Token expired *after* startup, cache hit | Sandbox boots from cache; `resolver.unavailable` warn is the operator's signal to rotate. |
| In-memory TTL expired during sandbox lifetime | No-op. Resolution happens at boot only; in-flight sandboxes keep their resolved env. Rotation happens on the next restart. |
| Required secret with `value_from` resolves to empty string | Treat as `value.unavailable` (consistent with the existing `required: true` semantics on env vars). |
| Disk cache file corrupted | Treat as miss; fall through to the live resolver. Log a warning. |

## Why not the alternatives

- **Pre-deploy CLI shim** that resolves `op://` refs in `app.toml` before
  `m deploy`. Zero runtime changes — but resolved values land in etcd. Defeats
  the stated goal.
- **Container entrypoint shim** that fetches secrets at process start. No
  Miren changes, but requires baking `op` into every image or a shared mount.
  Pushes the integration into every app instead of solving it once.
- **Server-side resolution at deploy time** in `buildVersionConfig` (build.go:572).
  Simpler to build, breaks the "never persisted" property.
- **Re-using the addon framework** (`pkg/addon/framework.go`). Addons create
  their *own* sandbox pools — wrong shape for "intercept env on someone else's
  sandbox." Worth borrowing the registry pattern, not the framework itself.

## Open questions

1. **Audit logging.** Beyond the WARN-level resolver/value-unavailable logs,
   should every successful resolution also emit a structured audit event
   (which sandbox, which URI, which resolver, never the value)? Easy add;
   probably wait for a compliance ask before turning it on by default.

## Resolved decisions

- **`SandboxSpec.EnvFrom` field vs. URI sentinel.** Structured field. New
  `env_from` component on the sandbox spec carrying `{ key, value_from }`.
  Schema bump in `api/compute/schema.yml` is a one-time cost; sentinel
  formats become a maintenance liability when a third value source shows up.
- **Multiple resolvers per scheme.** No — one resolver per scheme. Per-tenant
  routing belongs *inside* the URI, not in the registry.
- **Deploy-time validation of unknown schemes.** Server-side. The build
  server validates every `value_from` URI against the cluster's union of
  registered schemes and rejects upfront with a clear error naming the
  available schemes. Mirrors how `AppConfig.Validate()` already catches
  `port_timeout` typos at deploy time rather than sandbox boot. Per-runner
  heterogeneity (only some runners have a given resolver registered) is
  handled by making *runners* refuse to start when they see a scheme they
  can't handle — the misconfigured runner fails its own startup, deploy
  validation against the union stays simple.

## Rough sequencing

1. Land `value_from` field + parse-time validation in `appconfig` (no resolver
   yet — field is accepted but nothing reads it).
2. Add `EnvFrom` to `SandboxSpec`, plumb through `build.go` and `launcher.go`.
3. Add the empty `pkg/secrets` registry + the on-disk cache (with the
   restarts-since-last-use eviction sweep at runner startup) + the one call
   site in `sandbox.go`. Now the runtime supports the *concept* of resolved
   env, with no providers wired up.
4. Add `pkg/secrets/onepassword/` (Shape A) + runner config + token bootstrap.
5. Add `m runner secrets cache flush [<uri>]` CLI commands.
6. Docs + a minimal integration test using the OP CLI in a fixture, plus a
   cache-fallback test that simulates resolver failure with a warm cache and
   asserts the WARN logs fire.

Steps 1–3 are the "core" change and are independently useful (e.g., a Vault
provider could be added by anyone without touching core again). Step 4 is the
1P-specific work and could be its own repo if we go that way.
