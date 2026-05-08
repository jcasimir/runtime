# External Secret Resolution (Sketch)

**Status:** sketch
**Branch:** `jcasimir/op-secrets-sketch`
**Author:** jcasimir

## Goal

Let `app.toml` reference secrets by URI (initially `op://vault/item/field`)
without ever writing the resolved value to disk, the manifest, or etcd.
Resolution happens on the runner at sandbox boot. The 1Password integration
itself should live outside the core runtime — minimally invasive, plugin-shaped.

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
| Resolver returns error at sandbox boot | Sandbox transitions to `failed`, error surfaced in `m sandbox describe`. **Don't** fall back to empty string — that's the silent-failure mode `port_timeout` validation was added to prevent. |
| 1P API unreachable | Same as above unless cache is warm. |
| Token missing/expired | Runner startup fails fast with a clear error pointing at the token file. Don't start the runner half-configured. |
| Cache TTL expired during sandbox lifetime | No-op. Resolution happens at boot only; in-flight sandboxes keep their resolved env. Rotation happens on the next restart. |
| Required secret with `value_from` resolves to empty string | Treat as resolution failure (consistent with the existing `required: true` semantics on env vars). |

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

1. **`SandboxSpec.EnvFrom` field vs. URI sentinel.** A new `EnvFrom` slice on
   the spec is cleanest and survives schema migrations; a sentinel format
   (`"DATABASE_URL=value_from:op://..."`) is two fewer schema bumps. Lean
   toward the structured field — it's a one-time cost.
2. **Caching policy.** Per-runner LRU keyed by URI, short TTL (default 30s)
   so back-to-back sandbox starts during a deploy don't hammer the OP API.
   Cache is purely a perf concern — correctness doesn't depend on it.
3. **Multiple resolvers per scheme?** Probably not. One registered resolver
   per scheme keeps the dispatch trivial.
4. **CLI ergonomics for non-OP users.** `value_from` with no registered
   resolver should be a clear error at deploy time, not a silent fallback to
   empty string. (Handled in #1 above.)
5. **Audit logging.** Should resolution emit an audit event (which sandbox,
   which URI, which resolver) without including the value? Probably yes,
   structured-logging only — easy add, ignore for v1.

## Rough sequencing

1. Land `value_from` field + parse-time validation in `appconfig` (no resolver
   yet — field is accepted but nothing reads it).
2. Add `EnvFrom` to `SandboxSpec`, plumb through `build.go` and `launcher.go`.
3. Add the empty `pkg/secrets` registry + the one call site in `sandbox.go`.
   Now the runtime supports the *concept* of resolved env, with no providers.
4. Add `pkg/secrets/onepassword/` (Shape A) + runner config + token bootstrap.
5. Docs + a minimal integration test using the OP CLI in a fixture.

Steps 1–3 are the "core" change and are independently useful (e.g., a Vault
provider could be added by anyone without touching core again). Step 4 is the
1P-specific work and could be its own repo if we go that way.
