# Current Architecture Audit

This document captures the coordinator-era Parker architecture as it exists in the repository before the daemon-first mesh migration.

## Runtime Inventory

| Path | Language / framework | Responsibility | Network-facing? | Touches keys / signed data / money state? | Current role |
| --- | --- | --- | --- | --- | --- |
| `apps/cli` | TypeScript / Node | Local daemon wrapper, CLI commands, player bootstrap, wallet actions, websocket client | Both | Yes | daemon + CLI |
| `apps/server` | TypeScript / Fastify + WebSocket + SQLite | Central table coordinator, websocket relay, canonical table state, timeout sweeps, transcript persistence | Yes | Yes | server |
| `apps/web` | TypeScript / React + Vite | Browser wallet UI, websocket client, coordinator-driven table UX | Yes | Yes | web client |
| `packages/protocol` | TypeScript / Zod | Shared HTTP and websocket schemas | Shared | Yes | shared |
| `packages/game-engine` | TypeScript | Hold'em rules, deterministic deck derivation, showdown scoring | Local / shared | Yes | shared |
| `packages/settlement` | TypeScript | Arkade wallet helpers, signing, timeout delegation helpers, mock/live settlement seams | Shared | Yes | shared |

## Current Module Responsibilities

### `apps/server`

- [`apps/server/src/app.ts`](../../apps/server/src/app.ts) exposes:
  - `POST /api/tables`
  - `POST /api/tables/join`
  - `GET /api/tables/by-invite/:inviteCode`
  - `GET /api/tables/:tableId`
  - `GET /api/tables/:tableId/transcript`
  - `POST /api/tables/:tableId/commitments`
  - `POST /api/tables/:tableId/delegations`
  - `GET /ws`
- [`apps/server/src/service.ts`](../../apps/server/src/service.ts) owns:
  - invite generation
  - seat reservation
  - escrow descriptor creation
  - commitment collection
  - hand creation
  - signed-action processing
  - timeout sweeps
  - transcript construction
- [`apps/server/src/db.ts`](../../apps/server/src/db.ts) persists:
  - snapshots
  - current hand state
  - checkpoints
  - delegations
  - transcript events

### `apps/cli`

- [`apps/cli/src/daemonProcess.ts`](../../apps/cli/src/daemonProcess.ts) is a local Unix-socket RPC daemon. Today it wraps a single `ParkerPlayerClient`.
- [`apps/cli/src/playerClient.ts`](../../apps/cli/src/playerClient.ts) is still coordinator-centric:
  - HTTP for create / join / snapshot / transcript / commitments / delegations
  - websocket for live table presence, peer relay, and signed actions
- [`apps/cli/src/tableSocketClient.ts`](../../apps/cli/src/tableSocketClient.ts) is a websocket-only table transport.
- [`apps/cli/src/walletRuntime.ts`](../../apps/cli/src/walletRuntime.ts) handles wallet bootstrap, Arkade deposit / onboard / offboard / withdrawal, and timeout delegation signing.
- [`apps/cli/src/index.ts`](../../apps/cli/src/index.ts) is the primary human control surface and proxies almost everything through the local daemon.

### `apps/web`

- [`apps/web/src/App.tsx`](../../apps/web/src/App.tsx) uses the same coordinator APIs as the CLI.
- [`apps/web/src/hooks/useTableSocket.ts`](../../apps/web/src/hooks/useTableSocket.ts) duplicates the websocket transport assumptions used by the CLI.
- The web app derives hole cards locally from shared reveals, which means both players can derive the full deck once both seeds are revealed.

### Shared packages

- [`packages/protocol/src/index.ts`](../../packages/protocol/src/index.ts) defines the current canonical shapes for:
  - tables
  - seat reservations
  - commitments
  - signed game actions
  - checkpoints
  - delegations
  - websocket messages
- [`packages/game-engine/src/holdem.ts`](../../packages/game-engine/src/holdem.ts) mutates deterministic hand state from actions and resolves showdown.
- [`packages/settlement/src/index.ts`](../../packages/settlement/src/index.ts) provides:
  - secp256k1 identity generation
  - message and checkpoint signing
  - timeout delegation signing
  - Arkade wallet connection helpers
  - mock settlement provider

## Current Trust Boundaries

### Wallet creation and key storage

- CLI profiles store the wallet private key in [`apps/cli/src/profileStore.ts`](../../apps/cli/src/profileStore.ts).
- Browser mock/live keys live in the service worker / local storage via [`apps/web/src/lib/walletClient.ts`](../../apps/web/src/lib/walletClient.ts).
- The server never stores private keys.

### Arkade address generation and signing

- Local only, via [`packages/settlement/src/index.ts`](../../packages/settlement/src/index.ts) and [`apps/cli/src/walletRuntime.ts`](../../apps/cli/src/walletRuntime.ts).

### Buy-in flow

- Server creates the table and later joins the second player.
- Server builds the escrow descriptor in [`apps/server/src/service.ts`](../../apps/server/src/service.ts).
- The buy-in is a server-mediated transition; there is no daemon-to-daemon buy-in lock protocol yet.

### Game creation, lobby discovery, seat assignment

- All current creation and discovery run through HTTP endpoints on `apps/server`.
- Invite codes are generated by the server.
- Seat assignment is server-owned mutable state.

### Card shuffling / deterministic dealing

- Both players commit and reveal randomness through the server.
- The final deck seed is derived from both reveals with [`deriveDeckSeed`](../../packages/game-engine/src/rng.ts).
- This is auditable, but not private once both reveals exist.

### Action ordering and timeout enforcement

- The server applies actions in order through [`processSignedAction`](../../apps/server/src/service.ts).
- The server enforces action timeouts through [`runTimeoutSweep`](../../apps/server/src/service.ts).

### Hand settlement, spectator streaming, reconnect / resume

- Hand settlement is calculated server-side from the game engine.
- There is no separate spectator pipeline; the websocket broadcasts the full table snapshot/checkpoint stream.
- Reconnect is only a websocket re-identify against the same coordinator.

### Persistence and replay

- SQLite stores mutable snapshots plus current hand state.
- Transcript events exist, but canonical recovery is not event-sourced yet because the server also stores mutable hand state and mutable table snapshots.

## Current Canonical State

- The canonical table state lives on the coordinator in SQLite.
- The active hand state is mutable and stored separately from checkpoints.
- Player actions are signed, but server sequencing itself is not hash-linked or epoch-scoped.
- A server crash mid-hand loses the current in-memory websocket topology and relies on the persisted mutable hand state, not an append-only canonical log.

## Audit Answers

### Which current modules can stay mostly unchanged?

- [`packages/game-engine`](../../packages/game-engine) can stay as the rules engine.
- [`packages/settlement`](../../packages/settlement) can stay as the Arkade-facing wallet/signing layer if its trust boundaries are made explicit.
- The local daemon RPC mechanism in [`apps/cli/src/daemonProcess.ts`](../../apps/cli/src/daemonProcess.ts) can stay as the agent/human control surface.

### Which modules must be split because they assume a central coordinator?

- [`apps/server/src/service.ts`](../../apps/server/src/service.ts)
- [`apps/cli/src/playerClient.ts`](../../apps/cli/src/playerClient.ts)
- [`apps/cli/src/tableSocketClient.ts`](../../apps/cli/src/tableSocketClient.ts)
- [`apps/web/src/hooks/useTableSocket.ts`](../../apps/web/src/hooks/useTableSocket.ts)

### Which data must become signed append-only events?

- table lifecycle transitions
- host lease / epoch changes
- join and seat acceptance
- buy-in lock confirmation
- hand lifecycle transitions
- action acceptance
- showdown reveal
- hand result / abort
- host failover

### Which data can stay local-only?

- wallet private keys
- peer transport private keys
- private hole-card decrypt state
- Arkade wallet internals
- known-peer caches / relay hints

### Where does the current design leak trust into the central server?

- canonical sequencing
- seat assignment
- hand lifecycle transitions
- timeout enforcement
- canonical persistence
- relay of all peer messaging

### Minimum adapter layer needed for migration

- a transport interface that can back:
  - direct request / response
  - per-table event stream
  - public announcement broadcast
- an event log/snapshot boundary between table logic and transport
- a server compatibility adapter for legacy websocket flow during transition
