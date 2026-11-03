# window-keeper

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that keeps every Claude (and, optionally,
Codex) account's 5-hour usage window running. A window starts with the first request after the previous one ended, so an idle
account's countdown doesn't start until you use it. window-keeper sends one tiny request (1 output
token) through the account as soon as its window ends. That way a reset is never more than 5 hours away.

With several accounts of a provider it **staggers** their windows: their starts are spaced evenly (2h30 apart with two
accounts), so their resets are spread out too. If a burst exhausts every account, the next refill comes
within 5h/N instead of up to 5h. Staggering never costs capacity: a window only waits while its account
is idle, and real traffic through that account starts it anyway.

The plugin reads each account's usage endpoint, which is free, every few minutes, and again when a window
ends: Claude's `/api/oauth/usage`, Codex's `wham/usage`. It sends the starting request through the proxy's
normal path for that provider, pinned to the account. Some Codex plans don't list the 5-hour window in
`wham/usage`; for those, the window's end is read from the `x-codex-*` headers of the starting request.
Codex free accounts have only a monthly limit, so there's nothing to start: they get paused for a day.

## Config

```yaml
plugins:
  configs:
    window-keeper:
      enabled: true
      mode: active          # or observe: plan and report, never send
      stagger: true         # default true; each provider is staggered on its own
      claude: true          # default true
      claude_model: claude-haiku-5-5
      codex: false          # default false: Codex is opt-in
      codex_model: gpt-6-luna
      accounts: ""          # comma-separated auth IDs or emails (a YAML list works too); empty = every account
      check_interval: 5m
      tolerance: 15m        # how far from the ideal spacing a start may be
```

Accounts of a switched-off provider are still read and shown in the status, but never started. An account
is skipped while its longer limit (weekly, or monthly on some Codex plans) is used up. It is also paused for
an hour if a start didn't open a window, or for a day if it reports no 5-hour limit at all.

## Management API

- `GET /v0/management/window-keeper/status`: each account's window, planned start and last start.
- `POST /v0/management/window-keeper/start?account=<auth id or email>`: start one account's window now, even
  if its provider is switched off.

## Build

`./build.sh [PLUGIN_DIR]` runs the tests and builds `dist/window-keeper.so` in `golang:1.26-bookworm`.
It is built against CLIProxyAPI v8.0.48 (C ABI v1, RPC schema v9). Restart CLIProxyAPI after installing.
