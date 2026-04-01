# Parker

`parkerd` is the daemon workspace that implements the [Parker protocol](./docs/protocol.md) for local poker runtime, localhost control, and optional public indexing.

The browser app has been split out into the sibling repo `../controller-web`. `parkerd` no longer contains or serves a bundled web app.

## Documentation

- [docs/architecture.md](./docs/architecture.md): current component topology, runtime boundaries, deployment shapes, and recovery flows
- [docs/protocol.md](./docs/protocol.md): current local RPC surface, peer-to-peer endpoints, signed objects, and public read flow
- [docs/trust-model.md](./docs/trust-model.md): current guarantees, trust assumptions, security boundaries, and known gaps

## Repository Layout

- `cmd/parker-daemon`: Go daemon entrypoint
- `cmd/parker-cli`: Go CLI entrypoint
- `cmd/parker-controller`: Go localhost controller entrypoint
- `cmd/parker-indexer`: Go public indexer entrypoint
- `cmd/parker-devtool`: Go helper used by local regtest harness scripts
- `internal/`: daemon runtime, controller/indexer apps, transport, storage, wallet integration, and game logic
- `scripts/`: launcher wrappers and regtest harnesses

## Runtime Boundary

- The daemon owns wallet access, peer transport, table state, event appends, snapshots, settlement operations, and persistence.
- The CLI and controller are thin local control planes over the daemon's Unix-socket RPC.
- The indexer is outside consensus. It stores public advertisements and public updates for discovery and spectatorship.
- Browser clients live outside this repository and talk to the daemon only through the localhost controller.

## Requirements

- Go toolchain compatible with [`go.mod`](./go.mod) (currently `go 1.25.3`)
- `curl` for local harness scripts
- `nigiri` for regtest flows

## Validate

```bash
go test ./...
```

The `scripts/bin/*` launchers build the Go binaries on demand and then execute them.

## Development Entrypoints

```bash
./scripts/bin/parker-cli help
./scripts/bin/parker-daemon --profile alpha --mode player
./scripts/bin/parker-controller
./scripts/bin/parker-indexer
```

## Browser Development

Run the controller from this repo:

```bash
./scripts/bin/parker-controller
```

Then run the extracted browser app from the sibling repo:

```bash
cd ../controller-web
npm install
npm run dev
```

By default, the browser app serves on `http://127.0.0.1:3010` and proxies to:

- the controller at `http://127.0.0.1:3030`
- the indexer at `http://127.0.0.1:3020`

## CLI Examples

```bash
./scripts/bin/parker-cli bootstrap Alpha --profile alpha
./scripts/bin/parker-cli wallet summary --profile alpha
./scripts/bin/parker-cli daemon start --profile alpha
./scripts/bin/parker-cli table public --profile alpha
```

## Local Regtest

Useful local values:

```bash
PARKER_NETWORK=regtest
PARKER_CONTROLLER_PORT=3030
PARKER_INDEXER_URL=http://127.0.0.1:3020
PARKER_ARK_SERVER_URL=http://127.0.0.1:7070
PARKER_BOLTZ_URL=http://127.0.0.1:9069
```

Managed local stack:

```bash
make local
```

That target rebuilds the local binaries, starts Nigiri, the indexer, the localhost controller, and three local daemons: `witness`, `alice`, and `bob`. `HOST_PROFILE` chooses which player runs in host mode, and defaults to `alice`.

For example:

```bash
HOST_PROFILE=bob make local
```

Useful lifecycle commands:

```bash
make local-down
make deps
make deps-down
make host
make host-down
make witness
make witness-down
make alice
make alice-down
make bob
make bob-down
make fund-alice
make fund-bob
```

The `host` target starts whichever player matches `HOST_PROFILE`, and the `alice` or `bob` targets also switch to host mode automatically when that player is selected.

The managed stack keeps its runtime under `.tmp/local-regtest`, including a stable exported env file at `.tmp/local-regtest/runtime.env`.

One-command local poker simulation:

```bash
make poker-regtest-round
```

That target still starts Nigiri, the indexer, four segregated Go daemons (`host`, `witness`, `alice`, `bob`), funds the player wallets on regtest, creates a table, auto-plays a hand, and cashes both players out.
