# Parker

Parker is a Mutinynet-first poker workspace built around private heads-up tables, deterministic commit/reveal dealing, and non-custodial bankroll coordination. The repo is structured as an npm workspace with:

- `apps/web` for the React/Vite client and wallet service worker
- `apps/server` for the Fastify coordinator, signaling, and persisted checkpoints
- `packages/protocol` for shared schemas and message contracts
- `packages/game-engine` for deterministic shuffling, Hold'em rules, and showdown logic
- `packages/settlement` for identity/signing helpers, escrow descriptors, and mock/live adapter seams

## Current shape

- The checked-in root [`.env.example`](/Users/danieldresner/src/arkade_fun/.env.example) shows the Mutinynet settings the web app reads, and a local [`.env`](/Users/danieldresner/src/arkade_fun/.env) can switch between live and mock settlement.
- The settlement layer is split so the UI/server already speak in Arkade-style concepts: wallet summary, Lightning swaps, table escrow, checkpoints, and timeout delegations.
- The server never keeps user private keys. It coordinates tables, mirrors transcripts, persists checkpoints, and runs timeout sweeps against posted delegations.

## Requirements

- Node 22+ is expected. The current repo pins this in [`.nvmrc`](/Users/danieldresner/src/arkade_fun/.nvmrc).
- Copy [`.env.example`](/Users/danieldresner/src/arkade_fun/.env.example) to [`.env`](/Users/danieldresner/src/arkade_fun/.env) if you want to change the local Mutinynet settings.

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

The current checked-in [`.env`](/Users/danieldresner/src/arkade_fun/.env) is already set for live Mutinynet wallet and Boltz flows:

```bash
VITE_USE_MOCK_SETTLEMENT=false
VITE_ARK_SERVER_URL=https://mutinynet.arkade.sh
VITE_BOLTZ_URL=https://api.boltz.mutinynet.arkade.sh
```

## Notes

- The client registers a service worker from [apps/web/src/wallet-service-worker.ts](/Users/danieldresner/src/arkade_fun/apps/web/src/wallet-service-worker.ts) to keep the local identity and wallet summary outside React state.
- Hole cards are hidden in checkpoints until showdown. Each client derives its own cards locally from the shared commitment/reveal material.
- The current live Arkade/Boltz path is intentionally isolated behind the settlement package. The default mock mode keeps the full workspace buildable and testable without live network dependencies.
