---
title: "miren app disable"
sidebar_label: "app disable"
description: "Hold every service of an app at zero instances"
---

# miren app disable

Hold every service of an app at zero instances

Disable turns an app off. Every service is held at zero instances: running sandboxes shut down, nothing boots on a deploy, and HTTP requests get a 503. The app keeps its versions, routes, environment, disks, and addons, so `miren app enable` brings it back as it was.

Disabled is an explicit state, so `miren app list` and `miren app status` report it as such rather than as an app that failed to start.

:::note[Tasks still run]
Disable stops services, not tasks. `miren app run` and scheduled tasks keep working, which leaves room for a backup or a migration while the app is off.
:::

## Usage

```bash
miren app disable [flags]
```

## Flags

- `--reason, -r` — Why the app is disabled, shown by app status

## Config Options

- `--cluster, -C` — Cluster name
- `--config` — Path to the config file

## App Options

- `--app, -a` — Application name
- `--dir, -d` — Directory to run from (default: `.`)

## Global Options

- `--options` — Path to file containing options
- `--server-address` — Server address to connect to (default: `127.0.0.1:8443`)
- `--verbose, -v` — Enable verbose output

## Examples

**Disable an app:**

```bash
miren app disable -a myapp --reason "Moved to the new cluster"
```

## See also

- [`miren app`](./app.md)
