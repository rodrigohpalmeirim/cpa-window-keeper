# window-keeper

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that keeps every Claude account's 5-hour
usage window running. A window starts with the first request after the previous one ended, so an idle
account's countdown doesn't start until you use it. window-keeper sends one tiny request (1 output
token) through the account as soon as its window ends. That way a reset is never more than 5 hours away.

With several accounts it **staggers** the windows: their starts are spaced evenly (2h30 apart with two
accounts), so their resets are spread out too. If a burst exhausts every account, the next refill comes
within 5h/N instead of up to 5h. Staggering never costs capacity: a window only waits while its account
is idle, and real traffic through that account starts it anyway.

The plugin reads each account's `/api/oauth/usage`, which is free, every few minutes, and again when a
window ends. It sends the starting request through the proxy's normal Claude path, pinned to that
account.

## Config

```yaml
plugins:
  configs:
    window-keeper:
      enabled: true
      mode: active          # or observe: plan and report, never send
      stagger: true         # default true
      model: claude-haiku-5-5
      accounts: []          # auth IDs or emails; empty = every Claude account
      check_interval: 5m
      tolerance: 15m        # how far from the ideal spacing a start may be
```

An account is skipped while its weekly limit is at 100%. It is also paused for an hour if a start didn't
open a window, or for 6 hours if it reports no 5-hour limit at all.

## Management API

- `GET /v0/management/window-keeper/status`: each account's window, planned start and last start.
- `POST /v0/management/window-keeper/start?account=<auth id or email>`: start one account's window now.

## Build

`./build.sh [PLUGIN_DIR]` runs the tests and builds `dist/window-keeper.so` in `golang:1.26-bookworm`.
It is built against CLIProxyAPI v8.0.48 (C ABI v1, RPC schema v9). Restart CLIProxyAPI after installing.
