# Go Parker Daemon Contract

The active Parker daemon and CLI path is now Go-first.

## Active Launch Path

- `scripts/bin/parker-daemon` always builds and runs `cmd/parker-daemon`.
- `scripts/bin/parker-cli` always builds and runs `cmd/parker-cli`.
- `packages/daemon-runtime/src/daemonClient.ts` autostarts through `scripts/bin/parker-daemon`, so TS consumers such as the controller and web apps still talk to the Go daemon.
- `apps/daemon/src/index.ts` and `apps/cli/src/index.ts` remain as legacy TypeScript reference implementations and are no longer selected by top-level wrappers.

## Preserved Local Contract

- Unix domain socket + NDJSON request/response/event envelopes.
- RPC method names from `packages/daemon-runtime/src/daemonProtocol.ts`.
- Watch handshake semantics: one `response` frame with current state, then long-lived `log` and `state` events.
- Profile metadata file shape, lifecycle states, and 5 second heartbeat cadence.
- Detached autostart behavior and stale PID cleanup semantics.
- Stable daemon socket, log, metadata, and state directory paths.

## Repo Wiring Rules

- Top-level `make` flows use the Go wrappers only; there is no runtime selector for the CLI or daemon on that path.
- Setting `PARKER_DAEMON_IMPL=ts` or `PARKER_CLI_IMPL=ts` against the top-level wrappers now fails fast.
- Legacy TypeScript daemon code is retained for reference and direct manual execution only.

## Remaining Migration Work

- Keep porting mesh/runtime slices until the unchanged root acceptance flows pass end to end.
- Retire or remove legacy proxy scaffolding once the native runtime no longer needs parity scaffolding nearby.
