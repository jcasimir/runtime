---
title: Elixir on Miren
description: Deploy Elixir and Phoenix apps on Miren. Miren detects mix.exs, builds a Mix release, and runs it on a slim image, no Dockerfile required.
keywords: [elixir, phoenix, mix release, otp, ecto, liveview, secret_key_base, deploy]
---

import CliCommand from '@site/src/components/CliCommand';

# Elixir on Miren

Miren auto-detects Elixir apps from `mix.exs`, builds a [Mix
release](https://hexdocs.pm/mix/Mix.Tasks.Release.html), and ships just the release on a
slim Debian image. Phoenix apps get their assets built and their endpoint started
automatically. No Dockerfile required.

:::tip[Let your agent do this]
Ask your AI coding agent to "set up this Phoenix app on Miren" after installing the
[Miren agent skills](../agent-skills.md). It wires up the database addon and secrets and
deploys, using this page as its reference.
:::

## Does this source build need a Dockerfile?

No. Miren detects Elixir from `mix.exs` and builds the release for you. Provide a
`Dockerfile.miren` only for custom build steps. See
[Using Dockerfile.miren](./index.md#using-dockerfilemiren).

## Set up the app

From your project root:

<CliCommand context="client">
```miren
miren init
miren deploy
```
</CliCommand>

For a Phoenix app, `miren init` generates and stores `SECRET_KEY_BASE` for you. If
the app uses DNSCluster, it configures
`DNS_CLUSTER_QUERY=web.app.miren`. Set `PHX_HOST` to your public route hostname
before deploying: Phoenix uses it for LiveView websocket origin checks. If the
app requires `DATABASE_URL`, configure a database addon or supply the URL before
deploying (see [Environment variables](#environment-variables)).

### Build process

Miren builds on the [hexpm/elixir](https://hub.docker.com/r/hexpm/elixir) image with
`MIX_ENV=prod`:

1. `mix deps.get --only prod` and `mix deps.compile`, from `mix.exs`, `mix.lock` and
   `config/` alone, so dependency builds are cached across code changes.
2. `mix compile`.
3. Install npm dependencies from `assets/package.json` (using `npm ci` when a
   `package-lock.json` is present), then run `mix assets.deploy` if your `mix.exs`
   defines that alias (Phoenix apps do). Umbrella apps under `apps/` get the same
   asset steps, each in its own directory.
4. `mix release`.

The release bundles the Erlang runtime, so the final image is `debian:bookworm-slim`
plus the release at `/app`. The Elixir toolchain and your source stay behind.
The builder includes `build-essential` and Git for native and Git dependencies;
dependencies compile on the cluster, not on your laptop.

### Versions

Miren picks the Elixir and Erlang/OTP versions in this order:

1. `[build] version` in `.miren/app.toml`
2. The `elixir` (and `erlang`) entries in `.tool-versions` or `mise.toml`
3. The default, **Elixir 1.19 on Erlang/OTP 28**

Versions take the forms version managers use: `1.18`, `1.18.4`, or `1.18.4-otp-27`.
An `erlang` entry picks the OTP when the Elixir version doesn't. Miren builds
Elixir 1.16 through 1.20, each on its most recent patch release, so `1.18.4` builds on
1.18.5. The build output says exactly which versions it used.

```toml
[build]
version = "1.18-otp-26"
```

For an exact pairing outside that set, set `version` to a full
[hexpm/elixir tag](https://hub.docker.com/r/hexpm/elixir/tags) on Debian bookworm,
such as `1.18.4-erlang-27.3.4.18-debian-bookworm-20260918-slim`.

### Release name

Miren builds the release named in a `releases:` block in `mix.exs`, or, without one,
the default release named after your OTP app (the `app:` in `project/0`). Umbrella
projects need a `releases:` block.

For umbrella Phoenix apps, Miren installs npm dependencies from each
`apps/*/assets/package.json` and runs `mix assets.deploy` in each app that defines
the alias, after compilation and before assembling the release. No `onbuild`
workaround is needed.

### Start command

The web service runs `/app/bin/<release> start`. For DNSCluster apps, Miren
uses the node name `<release>_<MIREN_RUNTIME_VERSION>@<instance IP>` by default; explicit
`RELEASE_NODE` and `RELEASE_DISTRIBUTION` values take precedence. For Phoenix, Miren sets
`PHX_SERVER=true` so the endpoint starts, and the `runtime.exs` from `mix phx.new`
already binds `$PORT` on all interfaces. Other apps should read `PORT` from the
environment and bind `0.0.0.0`.

## Environment variables

A production Phoenix app needs a database and a few secrets, and they must exist
**before the app boots**: `config/runtime.exs` raises on a missing `DATABASE_URL` or
`SECRET_KEY_BASE`.

:::warning[Set secrets before the app serves traffic]
A web app autoscales to zero, so a deploy can report success without ever starting an
instance. The missing-secret error only surfaces when the first request tries to boot
one, and the instance crashes. Configure the addon and secrets first.
:::

### Database via an addon

The simplest way to get `DATABASE_URL` is a managed Postgres [addon](../addons.md).
Miren provisions it and injects the connection string (plus `PG*` variables)
automatically. Declare it in `.miren/app.toml`:

```toml
[addons.miren-postgresql]
variant = "small"
```

### Secrets and settings

`miren init` stores a generated `SECRET_KEY_BASE`. Set your public hostname with `miren env set`,
where `-s` masks secrets in output and logs:

<CliCommand context="client">
```miren
miren env set -e PHX_HOST=my_app.example.com
```
</CliCommand>

| Variable | Required | Notes |
|----------|----------|-------|
| `DATABASE_URL` | Yes | Injected by the `miren-postgresql` addon |
| `SECRET_KEY_BASE` | Yes | Generated by `miren init`, or `mix phx.gen.secret` |
| `PHX_HOST` | For LiveView | Set to your public route hostname before deploy. Phoenix uses it for URLs and the websocket origin check |
| `PHX_SERVER` | Set by Miren | Starts the endpoint in the release |
| `PORT` | No | Injected by Miren |
| `POOL_SIZE` | No | DB pool size, defaults to 10 |
| `DNS_CLUSTER_QUERY` | For DNSCluster apps | Defaults to `web.app.miren` on `miren init` |
| `RELEASE_COOKIE` | No | Mix bundles a cookie in the release; set only to override it for custom clustering |

Miren also scans your code and config for `System.fetch_env!/1` and
`System.get_env/1` and reports what it finds. A `fetch_env!` or a `get_env(...) ||
raise` counts as required.

See [App Configuration: Environment Variables](../app-configuration.md#environment-variables).

## Migrations

Generate Phoenix's release helpers locally with `mix phx.gen.release` and commit
them. When a Phoenix app contains `rel/overlays/bin/migrate`, Miren uses
`/app/bin/migrate` as a deploy-triggered task automatically, except for apps
using `ecto_sqlite3`. It runs once against
the new release before rollout; a failure blocks activation. Mix copies the
overlay into the release, so no Mix toolchain is needed in production.

SQLite apps must manage migrations in the service that mounts their database
disk, for example during startup before serving requests. Deploy tasks and
`miren app run` do not mount service disks, so they cannot migrate the database
provided by the `miren-sqlite` addon. Adding a manual migration task does not
give it access to that disk.

Explicit `[tasks.migrate]` configuration takes precedence, including a manual
trigger. Any explicit deploy-triggered task also suppresses the automatic task,
so an existing migration hook under another name isn't duplicated. For example:

```toml
[tasks.migrate]
command = '/app/bin/my_app eval "MyApp.Release.migrate()"'
trigger = "deploy"
```

Without the generated helper, declare a task that calls your release module as
above. Miren does not generate application code or run `mix ecto.migrate` in the
release. See [Tasks](../tasks.md).

For a network database, you can also run the generated helper manually:

<CliCommand context="client">
```miren
miren app run -a my_app -- /app/bin/migrate
```
</CliCommand>

## Clustering

A Phoenix app is stateless by default, so you can run multiple replicas behind Miren's
load balancer. When the app depends on DNSCluster and reads `DNS_CLUSTER_QUERY`,
`miren init` configures discovery at `web.app.miren`. Miren names nodes
`<release>_<MIREN_RUNTIME_VERSION>@<instance IP>`. DNSCluster uses the local node's
name before `@` when connecting to discovered IPs, so old and new deployments
remain separate even when DNS returns both sets of replicas during rollout.

Replicas running the same image share the cookie bundled by Mix in the release.
Miren no longer requires or generates a separate `RELEASE_COOKIE`; existing
configured cookies remain valid and are not removed. Node names provide rollout
isolation, not a security boundary. Custom node naming or discovery must agree on
the node name and cookie.

The bundled cookie can be read by anyone who can pull the release image. A
holder who can also reach Erlang distribution ports can execute code on the
node. Restrict image and network access; set `RELEASE_COOKIE` as a sensitive
environment variable if image readers must not have clustering credentials.

## Metrics

Phoenix's generated telemetry module defines metrics but needs an exporter to
expose them. Add
[`telemetry_metrics_prometheus`](https://hexdocs.pm/telemetry_metrics_prometheus/TelemetryMetricsPrometheus.html)
as a production dependency, and add the exporter to your application supervisor:

```elixir
{TelemetryMetricsPrometheus, metrics: MyAppWeb.Telemetry.metrics(), port: 9568}
```

It serves `/metrics` on port 9568. Keep your application's HTTP port and add
the exporter as a TCP port so it does not become Phoenix's `PORT`. For an app
using port 3000, configure `.miren/app.toml` as follows; preserve your existing
HTTP port if it differs:

```toml
[[services.web.ports]]
port = 3000
name = "http"
type = "http"

[[services.web.ports]]
port = 9568
name = "metrics"
type = "tcp"

[services.web.metrics]
enabled = true
port = 9568
path = "/metrics"
interval = "30s"
```

The TCP port type does not change the scraper's HTTP requests to `/metrics`.
The exporter must listen on an interface reachable from the private overlay,
not only loopback. Your cluster also needs a managed metrics remote-write
destination. See [Observability](../observability.md#managed-application-metrics).

## Agent quick reference

- **Detection:** `mix.exs` in the project
- **Version:** `[build] version`, else `.tool-versions` / `mise.toml`, else Elixir 1.19 / OTP 28; forms like `1.18`, `1.18.4-otp-27`, or a full bookworm hexpm tag
- **Build:** `deps.get`, `deps.compile`, `compile`, `assets.deploy` (if aliased), `release`, all with `MIX_ENV=prod`
- **Release:** the `releases:` entry in `mix.exs`, else the OTP app name; umbrellas need `releases:`
- **Umbrella Phoenix:** npm dependencies and `assets.deploy` aliases under `apps/*` are handled automatically before release
- **Start command:** `/app/bin/<release> start` (version-scoped, IP-named for DNSCluster apps); `PHX_SERVER=true` is set for Phoenix
- **Secrets:** `SECRET_KEY_BASE` generated by `miren init`; the release bundles its clustering cookie; set `PHX_HOST` to the route host for LiveView
- **Database:** `[addons.miren-postgresql]` injects `DATABASE_URL` (and `PG*`)
- **Migrations:** automatic deploy task when Phoenix's `rel/overlays/bin/migrate` exists, except for `ecto_sqlite3`; explicit migration/deploy tasks take precedence; SQLite migrations must run where the service disk is mounted
- **Metrics:** add a Prometheus exporter and configure `[services.web.metrics]`
- **Dockerfile:** not needed; add `Dockerfile.miren` only for custom builds

## Next steps

- [Addons](../addons.md): managed Postgres and other backing services
- [App Configuration](../app-configuration.md): customize `.miren/app.toml`
- [Deployment](../deployment.md): how deploys build and activate
