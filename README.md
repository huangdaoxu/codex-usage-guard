# codex-usage-guard

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) **usage plugin**.
It stops routing new requests to a Codex account once its 5-hour or weekly usage
crosses a threshold, then re-enables the account automatically after the window
rolls off.

This repo also hosts a **plugin registry** (`registry.json`) so CLIProxyAPI
nodes can install the plugin remotely with just the management key.

## What it does

On every Codex response it reads `x-codex-primary-used-percent` (5h) and
`x-codex-secondary-used-percent` (weekly). If either is at/above its threshold it
sets `disabled: true` on that account's auth file (with a private marker), and a
background loop re-enables it once the window resets. It only ever re-enables
accounts it disabled itself.

## Install on a node (remote, via the management key)

1. Point the node at this registry once (`PUT /v0/management/config.yaml`):

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     store-sources:
       - "https://raw.githubusercontent.com/huangdaoxu/codex-usage-guard/main/registry.json"
     configs:
       codex-usage-guard:
         enabled: true
         priority: 1
         primary_percent: 90
         secondary_percent: 90
         min_active: 1
         dry_run: false
   ```

2. Install it (downloads, checksum-verifies, hot-loads — no restart):

   ```bash
   curl -X POST https://<node>/v0/management/plugin-store/codex-usage-guard/install \
        -H "Authorization: Bearer $MGMT_KEY"
   ```

## Configuration

| key | meaning | default |
|---|---|---|
| `primary_percent` | 5h-window threshold (%, 0 disables) | 90 |
| `secondary_percent` | weekly-window threshold (%, 0 disables) | 90 |
| `primary_resume_minutes` | keep disabled this long after a 5h trip | 300 |
| `secondary_resume_minutes` | keep disabled this long after a weekly trip | 10080 |
| `min_active` | never drop the node below this many active accounts | 0 |
| `cooldown_seconds` | debounce repeated action on one account | 120 |
| `scan_interval_seconds` | how often the resume loop runs | 60 |
| `dry_run` | log only, change nothing | false |

Change any value live with `PATCH /v0/management/plugins/codex-usage-guard/config`
— it reconfigures in place, no restart.

## Build

One source builds to every platform via `go build -buildmode=c-shared` (cgo).
The library basename must equal the plugin id (`codex-usage-guard`).

```bash
make dll     # windows/amd64 (needs mingw-w64)
make so      # linux/amd64
make dylib   # macOS
```

Release artifacts are zips named `codex-usage-guard-<goos>-<goarch>.zip`
containing `codex-usage-guard.dll` (etc.) at the zip root.

## License

MIT
