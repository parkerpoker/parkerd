# Parker

Parker is a peer-to-peer poker workspace built around private heads-up tables, deterministic commit/reveal dealing, and non-custodial bankroll coordination. The repo now supports both Mutinynet and local Nigiri/regtest flows. The repo is structured as an npm workspace with:

- `apps/web` for the React/Vite client and wallet service worker
- `apps/cli` for the Node CLI player client and local harness
- `apps/server` for the Fastify coordinator, WebSocket relay, and persisted checkpoints
- `packages/protocol` for shared schemas and message contracts
- `packages/game-engine` for deterministic shuffling, Hold'em rules, and showdown logic
- `packages/settlement` for identity/signing helpers, escrow descriptors, and mock/live adapter seams

## Current shape

- The checked-in root [`.env.example`](/Users/danieldresner/src/arkade_fun/.env.example) covers both Mutinynet and local Nigiri/regtest settings for the server, web app, and CLI.
- The settlement layer is split so the UI/server already speak in Arkade-style concepts: wallet summary, Lightning swaps, table escrow, checkpoints, and timeout delegations.
- The server never keeps user private keys. It coordinates tables, mirrors transcripts, persists checkpoints, and runs timeout sweeps against posted delegations.

## Requirements

- Node 22+ is expected. The current repo pins this in [`.nvmrc`](/Users/danieldresner/src/arkade_fun/.nvmrc).
- Copy [`.env.example`](/Users/danieldresner/src/arkade_fun/.env.example) to [`.env`](/Users/danieldresner/src/arkade_fun/.env) if you want to switch between Mutinynet and local Nigiri/regtest settings.

## Commands

```bash
npm install
npm test
npm run typecheck
npm run build
```

For development, run the server and web app in separate terminals:

```bash
npm run dev:server
npm run dev:web
```

The CLI exposes wallet, table, peer, scenario, and harness commands:

```bash
npm run dev:cli -- --help
npm run dev:cli -- bootstrap --profile alpha "Alpha"
npm run dev:cli -- create-table --profile alpha
npm run dev:cli -- join-table --profile beta ABCD1234
npm run dev:cli -- interactive --profile alpha
```

## Local Nigiri Flow

Start Nigiri with Ark + Lightning and point Parker at local regtest endpoints:

```bash
nigiri start --ark --ln --ci
cp .env.example .env
```

Set these local values in [`.env`](/Users/danieldresner/src/arkade_fun/.env):

```bash
PARKER_NETWORK=regtest
PARKER_SERVER_URL=http://127.0.0.1:3020
PARKER_WEBSOCKET_URL=ws://127.0.0.1:3020/ws
PARKER_ARK_SERVER_URL=http://127.0.0.1:7070
PARKER_BOLTZ_URL=http://127.0.0.1:9069
VITE_NETWORK=regtest
VITE_ARK_SERVER_URL=http://127.0.0.1:7070
VITE_BOLTZ_URL=http://127.0.0.1:9069
```

Then run the server and a CLI player:

```bash
npm run dev:server
npm run dev:cli -- bootstrap --profile alpha "Alpha"
npm run dev:cli -- wallet --profile alpha
npm run dev:cli -- faucet --profile alpha 100000
npm run dev:cli -- onboard --profile alpha
```

The checked-in example harness scenario is [apps/cli/examples/regtest-heads-up.json](/Users/danieldresner/src/arkade_fun/apps/cli/examples/regtest-heads-up.json):

```bash
npm run dev:cli -- run-harness --scenario-file apps/cli/examples/regtest-heads-up.json
```

## Notes

- The client registers a service worker from [apps/web/src/wallet-service-worker.ts](/Users/danieldresner/src/arkade_fun/apps/web/src/wallet-service-worker.ts) to keep the local identity and wallet summary outside React state.
- Hole cards are hidden in checkpoints until showdown. Each client derives its own cards locally from the shared commitment/reveal material.
- The CLI daemon and the web app both use the shared table WebSocket for relayed peer messages; there is no separate WebRTC transport anymore.
- The default mock mode still keeps the full workspace buildable and testable without live network dependencies.
