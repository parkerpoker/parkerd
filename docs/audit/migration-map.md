# Migration Map

## Current -> Target Mapping

| Current module | Current responsibility | Migration action | Target module / location |
| --- | --- | --- | --- |
| `apps/server/src/service.ts` | Canonical gameplay coordinator | Split and minimize | host runtime in daemon; server becomes optional public indexer |
| `apps/cli/src/playerClient.ts` | HTTP + websocket coordinator client | Keep only as legacy adapter | daemon mesh runtime + legacy transport adapter |
| `apps/cli/src/tableSocketClient.ts` | websocket hot path | Wrap as compatibility transport | transport adapter boundary |
| `apps/web/src/App.tsx` | write-capable gameplay UI | convert to read-only observer | website / indexer view |
| `packages/game-engine` | rules engine | retain | rules-only engine under daemon-controlled table runtime |
| `packages/protocol` | coordinator-era schemas | extend | append-only signed event schemas + public indexer schemas |
| `packages/settlement` | wallet + signing helpers | retain and extend | wallet/funds provider boundary |
| `apps/server/src/db.ts` | mutable snapshots + transcript | retain only for optional public read model and legacy compatibility | public indexer store |

## Minimal Refactor Sequence

1. Audit docs
2. Extract transport seam so coordinator websocket is only an adapter
3. Add signed event envelope + append-only local store in the daemon
4. Make a daemon-hosted table controller own canonical table events
5. Add direct daemon-to-daemon transport
6. Add witness replication and host rotation
7. Add funds-provider boundary and checkpoint-based cashout
8. Convert server to public indexer duties
9. Convert website to read-only observer duties

## Reusable Pieces

- `createHoldemHand`, `applyHoldemAction`, and `toCheckpointShape`
- local Unix-socket daemon RPC
- Arkade wallet bootstrap / signing helpers
- Zod-based shared protocol package

## Expected Splits

### Split `service.ts`

Into:

- transport-agnostic table event application
- host-only orchestration
- public indexer publication
- legacy server adapter

### Split `playerClient.ts`

Into:

- local wallet / profile actions
- peer transport client
- table-role controller
- legacy coordinator adapter

### Split website responsibilities

Into:

- optional read-only public browser
- no required signing, joining, or gameplay sequencing
