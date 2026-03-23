# Parker

Parker is a daemon-mesh poker workspace built around:

- daemon-owned table state and settlement
- a thin local CLI that only controls the daemon over local RPC
- a thin local controller service that exposes browser-safe HTTP and SSE on top of the daemon RPC
- an optional public indexer for table ads and read-only spectator views
- Arkade-backed bankroll movement on Mutinynet or local Nigiri regtest

## Documentation

- [docs/protocol.md](./docs/protocol.md): wire format, canonical event/snapshot rules, settlement boundaries, and failover semantics
- [docs/trust-model.md](./docs/trust-model.md): guarantees, trust assumptions, privacy tradeoffs, and failure outcomes
- [docs/architecture.md](./docs/architecture.md): current component topology, runtime boundaries, deployment shapes, and recovery flows

## Workspace Layout

- `apps/daemon`: long-running daemon process
- `apps/cli`: operator CLI for wallet, network, table, funds, and daemon control
- `apps/controller`: loopback-only browser controller for the local daemon
- `apps/indexer`: optional public indexer and read-only HTTP API
- `apps/web`: hybrid public browser plus local controller UI
- `packages/daemon-runtime`: shared daemon RPC/runtime implementation
- `packages/protocol`: shared schemas and message contracts
- `packages/game-engine`: deterministic Hold'em logic
- `packages/settlement`: Arkade wallet and settlement helpers

## Runtime Boundary

- The daemon owns wallet access, peer transport, canonical event replay, snapshots, settlement, and persistence.
- The CLI and controller do not run gameplay logic. They only talk to the local daemon.
- The indexer is not part of consensus. It stores signed public table ads and public updates for discovery.
- The web app is a hybrid UI:
  - public reads come from the indexer
  - local wallet, table, gameplay, and settlement actions go to the localhost controller
- The browser never holds private keys, talks to the Unix socket directly, or joins the mesh.

## Requirements

- Node `22+`
- `npm install`
- `nigiri` available locally for regtest flows

## Workspace Commands

```bash
npm install
npm run build
npm run typecheck
npm run test
```

Development entrypoints:

```bash
npm run dev:daemon
npm run dev:cli -- help
npm run dev:controller
npm run dev:indexer
npm run dev:web
npm run dev:local
```

## CLI Examples

```bash
npm run dev:cli -- bootstrap Alpha --profile alpha
npm run dev:cli -- wallet summary --profile alpha
npm run dev:cli -- daemon start --profile alpha
npm run dev:cli -- table public --profile alpha
```

## Local Regtest

Copy `.env.example` to `.env` if you want defaults for local development. The relevant local values are:

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

That launcher starts the controller, the web UI, the optional indexer, the standard `host`/`witness`/`alice`/`bob` daemons, and Nigiri when the network is `regtest`.

One-command local poker simulation:

```bash
make poker-regtest-round
```

That target starts Nigiri, the indexer, four segregated daemons (`host`, `witness`, `alice`, `bob`), funds the two player wallets on regtest, creates a table, auto-plays a hand, and cashes both players out.

Two isolated browser player tabs:

```bash
make poker-regtest-ui-2p
```

That target starts Nigiri, the public indexer, two isolated player daemons and controller-served UIs for `alice` and `bob`, then opens both UIs in Chrome on separate localhost ports so their browser state stays independent. Set `NO_OPEN=1` if you only want the URLs printed.

The larger integration harness is also checked in:

```bash
npm run test:mesh-regtest
```

## Notes

- Public table discovery is optional and read-only.
- The daemon mesh is the gameplay authority.
- The localhost controller is a local control plane, not part of consensus.
- Old coordinator-era server and websocket gameplay paths have been removed from the active runtime.
