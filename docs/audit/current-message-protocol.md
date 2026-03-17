# Current Message Protocol Inventory

## HTTP Endpoints

| Endpoint | Sender -> receiver | Schema | Auth / signature | Idempotency | Replay protection | Persistence | Error handling |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /api/tables` | CLI/Web -> server | `createTableRequestSchema` | none | not idempotent | none | snapshot persisted | throws request error text |
| `POST /api/tables/join` | CLI/Web -> server | `joinTableRequestSchema` | none | not idempotent | none | snapshot persisted | throws request error text |
| `GET /api/tables/by-invite/:inviteCode` | CLI/Web -> server | path param | none | idempotent read | none | read only | `404` if missing |
| `GET /api/tables/:tableId` | CLI/Web -> server | path param | none | idempotent read | none | read only | throws if missing |
| `GET /api/tables/:tableId/transcript` | CLI/Web -> server | path param | none | idempotent read | none | reads checkpoints + events | throws if missing |
| `POST /api/tables/:tableId/commitments` | CLI/Web -> server | `deckCommitmentSchema` | none | replace-by-seat semantics | none | snapshot/checkpoint persisted | request parse / thrown error |
| `POST /api/tables/:tableId/delegations` | CLI/Web -> server | `timeoutDelegationSchema` | delegation carries signature hex but server does not re-verify against canonical epoch/seq | replace-by-id semantics | none | delegation + snapshot persisted | request parse / thrown error |

## WebSocket Messages

### Client -> server

| Message | Sender -> receiver | Schema | Auth / signature | Idempotency | Replay protection | Persistence | Error handling |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `identify` | CLI/Web -> server | `clientSocketEventSchema` | none | repeated identify replaces socket slot | none | no | websocket close on malformed input |
| `peer-message` | CLI/Web -> server | `clientSocketEventSchema` | none | no | none | no | dropped if target absent |
| `signed-action` | CLI/Web -> server | `signedGameActionSchema` | action payload signed by player key, but sequencing is server-owned | no | only `clientSeq`, not canonical | transcript event + mutable hand/checkpoint | thrown errors bubble |
| `checkpoint` | CLI/Web -> server | `tableCheckpointSchema` | checkpoint signatures included, but server only relays | no | none | no | no explicit nack |
| `heartbeat` | CLI/Web -> server | `clientSocketEventSchema` | none | repeated heartbeat | none | no | ignored if table/player unknown |

### Server -> client

| Message | Sender -> receiver | Schema | Auth / signature | Idempotency | Replay protection | Persistence | Error handling |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `table-snapshot` | server -> CLI/Web | `serverSocketEventSchema` | none | latest snapshot wins | none | server snapshot is persisted | no explicit versioning |
| `peer-message` | server -> CLI/Web | `serverSocketEventSchema` | none | no | none | not persisted | no nack |
| `presence` | server -> CLI/Web | `serverSocketEventSchema` | none | latest status wins | none | not persisted | no nack |
| `checkpoint` | server -> CLI/Web | `serverSocketEventSchema` | signatures array included | latest checkpoint wins | none | persisted in DB | no epoch / monotonic guard |
| `error` | server -> CLI/Web | `serverSocketEventSchema` | none | n/a | none | not persisted | plain error payload |

## Protocol Observations

- The current protocol validates JSON shapes, but canonical game history is not hash-linked.
- Player action signatures exist, but the server's own sequencing decisions are not signed.
- Replay protection is incomplete:
  - no epoch concept
  - no `prevEventHash`
  - no canonical monotonic sequence shared by all replicas
- Persistence is mixed:
  - checkpoints and transcript events are append-only-ish
  - snapshots and active hand state are mutable
- Errors are primarily request-time thrown exceptions; there is no durable protocol-level reject event model.
