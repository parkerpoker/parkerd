# Parker

Parker is a Go-first poker workspace built around:

- Go daemons for gameplay, wallet actions, local control, and public indexing
- a thin local CLI that controls one daemon over Unix-socket RPC
- a thin local controller that exposes browser-safe HTTP and SSE for the local daemon
- an optional public indexer for table ads and spectator reads
- a React/Vite web app in `apps/web`, which is the only remaining TypeScript product surface
- Arkade-backed bankroll movement on Mutinynet or local Nigiri regtest

## Documentation

- [docs/architecture.md](./docs/architecture.md): current component topology, runtime boundaries, deployment shapes, and recovery flows
- [docs/protocol.md](./docs/protocol.md): current local RPC surface, peer-to-peer endpoints, signed objects, and public read flow
- [docs/trust-model.md](./docs/trust-model.md): current guarantees, trust assumptions, security boundaries, and known gaps
- [docs/go-parker-daemon-parity.md](./docs/go-parker-daemon-parity.md): Go runtime status, preserved contracts, and remaining hardening work

## Repository Layout

- `cmd/parker-daemon`: Go daemon entrypoint
- `cmd/parker-cli`: Go CLI entrypoint
- `cmd/parker-controller`: Go localhost controller entrypoint
- `cmd/parker-indexer`: Go public indexer entrypoint
- `internal/`: native runtime, RPC client/server envelopes, storage, wallet integration, controller/indexer apps, game logic, and settlement helpers
- `apps/web`: React/Vite browser app
- `scripts/`: launch wrappers plus local dev and regtest harnesses

## Runtime Boundary

- The daemon owns wallet access, peer transport, table state, event appends, snapshots, settlement operations, and persistence.
- The CLI and controller do not run gameplay logic. They only talk to the local daemon.
- The indexer is outside consensus. It stores public advertisements and public updates for discovery and spectatorship.
- The web app is a hybrid UI:
  - public reads come from the indexer or the controller's proxy routes
  - local wallet, table, gameplay, and settlement actions go to the localhost controller
- The browser never holds private keys or talks to the Unix socket directly.

## Requirements

- Go toolchain compatible with [`go.mod`](./go.mod) (currently `go 1.25.3`)
- Node `22+`
- `npm install`
- `nigiri` available locally for regtest flows

## Install And Validate

```bash
npm install
go test ./...
npm run typecheck
npm run build
npm run test
```

Notes:

- `npm run build` and `npm run typecheck` only target `apps/web`.
- `npm run test` runs `go test ./...` plus the web typecheck.
- The `scripts/bin/*` launchers build the Go binaries on demand and then execute them.

## Development Entrypoints

```bash
./scripts/bin/parker-cli help
./scripts/bin/parker-daemon --profile alpha --mode player
./scripts/bin/parker-controller
./scripts/bin/parker-indexer
npm run dev:web
npm run dev:local
```

## CLI Examples

```bash
./scripts/bin/parker-cli bootstrap Alpha --profile alpha
./scripts/bin/parker-cli wallet summary --profile alpha
./scripts/bin/parker-cli daemon start --profile alpha
./scripts/bin/parker-cli table public --profile alpha
```

## Local Regtest

The relevant local values are:

```bash
PARKER_NETWORK=regtest
PARKER_CONTROLLER_PORT=3030
PARKER_INDEXER_URL=http://127.0.0.1:3020
PARKER_ARK_SERVER_URL=http://127.0.0.1:7070
PARKER_BOLTZ_URL=http://127.0.0.1:9069
VITE_NETWORK=regtest
VITE_ARK_SERVER_URL=http://127.0.0.1:7070
VITE_BOLTZ_URL=http://127.0.0.1:9069
```

One-command local dev stack:

```bash
npm run dev:local
```

That launcher starts the Go controller, Go indexer, the web UI, and the standard `host` / `witness` / `alice` / `bob` Go daemons. When `PARKER_NETWORK=regtest`, it also starts Nigiri.

One-command local poker simulation:

```bash
make poker-regtest-round
```

That target starts Nigiri, the indexer, four segregated Go daemons (`host`, `witness`, `alice`, `bob`), funds the player wallets on regtest, creates a table, auto-plays a hand, and cashes both players out.

Two isolated browser player tabs:

```bash
make poker-regtest-ui-2p
```

That target starts Nigiri, the public indexer, two isolated player daemons and controller-served UIs for `alice` and `bob`, then opens both UIs in Chrome on separate localhost ports so their browser state stays independent. Set `NO_OPEN=1` if you only want the URLs printed.

## Notes

- All backend binaries in active use are now Go.
- The web app and some local orchestration scripts still use Node/TypeScript.
- Public table discovery is optional and read-only.
- The localhost controller is a local control plane, not part of consensus.
- The protocol/trust docs describe the current Go implementation, including the places where it is still simpler than the older signed-mesh design.
