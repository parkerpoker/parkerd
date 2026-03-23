# Current Protocol Surface

This document describes the protocol and wire surface implemented today in this repository. It is intentionally current-state only.

For component topology, see [architecture.md](./architecture.md). For guarantees and trust assumptions, see [trust-model.md](./trust-model.md).

## Scope

The active implementation is split across:

- [`cmd/parker-daemon`](../cmd/parker-daemon/main.go) and [`internal/native_runtime.go`](../internal/native_runtime.go) for daemon behavior and peer-to-peer table sync
- [`internal/proxy_daemon.go`](../internal/proxy_daemon.go) and [`internal/client.go`](../internal/client.go) for local daemon RPC
- [`cmd/parker-controller`](../cmd/parker-controller/main.go) and [`internal/controller/app.go`](../internal/controller/app.go) for localhost browser control
- [`cmd/parker-indexer`](../cmd/parker-indexer/main.go) and [`internal/indexer/app.go`](../internal/indexer/app.go) for public ingest and spectator reads
- [`internal/settlementcore/core.go`](../internal/settlementcore/core.go) for canonical JSON hashing and signatures
- [`internal/native_types.go`](../internal/native_types.go) for the main runtime object shapes

The browser UI in `apps/web` is outside peer-to-peer protocol consensus.

## Current Runtime Split

The current implementation has three distinct protocol surfaces:

1. local daemon RPC over Unix-socket NDJSON
2. peer-to-peer JSON over HTTP endpoints under `/native/*`
3. public indexer ingest/read over HTTP

Only the first two are required for direct table play.

## Local Daemon RPC

The CLI and controller both talk to the daemon through profile-local Unix-socket RPC.

### Socket path

The socket path is:

- `<daemonDir>/<profile>.sock`

### Envelope types

The local wire envelopes live in [`internal/protocol.go`](../internal/protocol.go):

- `RequestEnvelope`
- `ResponseEnvelope`
- `EventEnvelope`

All frames are newline-delimited JSON.

### Supported methods

The active daemon methods are declared in [`internal/rpc/protocol.go`](../internal/rpc/protocol.go):

- `bootstrap`
- `meshBootstrapPeer`
- `meshCashOut`
- `meshCreateTable`
- `meshExit`
- `meshGetTable`
- `meshNetworkPeers`
- `meshPublicTables`
- `meshRenew`
- `meshRotateHost`
- `meshSendAction`
- `meshTableAnnounce`
- `meshTableJoin`
- `ping`
- `status`
- `stop`
- `watch`
- `walletDeposit`
- `walletFaucet`
- `walletOffboard`
- `walletOnboard`
- `walletSummary`
- `walletWithdraw`

### Watch semantics

`watch` behaves as follows:

1. the daemon sends one `response` frame containing the current state
2. the connection stays open
3. the daemon later emits `event` frames of type `log` and `state`

This is the contract used by both the CLI watcher and the controller's SSE bridge.

## Peer-To-Peer Runtime Protocol

The current Go runtime does not implement the older signed WebSocket mesh described in earlier design docs. Instead, it exchanges JSON over HTTP.

### Peer address format

Peer URLs are still stored and advertised as:

- `ws://<host>:<port>/mesh`

for compatibility, but the runtime converts those URLs to an HTTP base internally before making requests.

### Peer endpoints

The active peer routes are registered in [`internal/native_runtime.go`](../internal/native_runtime.go):

- `GET /native/peer`
- `POST /native/table/join`
- `POST /native/table/action`
- `POST /native/table/sync`
- `GET /native/table/{tableId}`

### Practical behavior

- `GET /native/peer` returns the peer's current advertised identity and role information.
- `POST /native/table/join` sends a signed join payload to the current host and returns the updated recipient-scoped table state.
- `POST /native/table/action` sends a signed action payload to the current host and returns the updated recipient-scoped table state.
- `POST /native/table/sync` pushes a signed recipient-scoped table copy to another peer.
- `GET /native/table/{tableId}` fetches the host's current recipient-scoped table copy.

### Host heartbeat and sync timing

The runtime tracks liveness with the table field `LastHostHeartbeatAt`.

Current timing constants in [`internal/native_store.go`](../internal/native_store.go):

- host heartbeat interval: `1000ms`
- host failure timeout: `3500ms`
- non-host host-poll interval: `1s`
- next-hand delay after settlement: `1000ms`

The current host updates its own table copy on each heartbeat tick. Non-host peers poll the host table periodically and can trigger failover when the heartbeat goes stale.

## Current Signed Objects

The runtime uses canonical JSON signatures from [`internal/settlementcore/core.go`](../internal/settlementcore/core.go) for these object families:

- public table advertisements
- canonical events
- snapshots
- table-funds operations

These signatures use deterministic canonicalization plus SHA-256 and compact secp256k1 signatures.

### Canonicalization rules

`StableStringify()` canonicalizes structured data before hashing.

Important rules:

- `null` stays `null`
- strings and booleans stay unchanged
- finite numbers stay numeric
- non-finite numbers become `null`
- `time.Time` becomes a UTC millisecond timestamp string
- arrays preserve order
- maps and structs are serialized with lexicographically sorted keys
- unsupported or invalid values collapse to `null`-like canonical forms

### Event signing

`appendEvent()` in [`internal/native_runtime.go`](../internal/native_runtime.go) signs the event body after removing the `signature` field.

Each `NativeSignedTableEvent` includes:

- `protocolVersion`
- `networkId`
- `tableId`
- `handId`
- `epoch`
- `seq`
- `prevEventHash`
- `messageType`
- `senderPeerId`
- `senderRole`
- `senderProtocolPubkeyHex`
- `timestamp`
- `body`
- `signature`

Current event append behavior:

- `seq` is the current event slice length
- `prevEventHash` points to the previous event hash when present
- the host appends gameplay events during normal play
- failover hosts append `HostRotated` and optionally `HandAbort`

### Snapshot signing

`buildSnapshot()` signs the snapshot body after removing `signatures`.

The current snapshot body includes:

- `snapshotId`
- `tableId`
- `epoch`
- `handId`
- `handNumber`
- `phase`
- `seatedPlayers`
- `chipBalances`
- `potSats`
- `sidePots`
- `turnIndex`
- `livePlayerIds`
- `foldedPlayerIds`
- `dealerCommitmentRoot`
- `previousSnapshotHash`
- `latestEventHash`
- `createdAt`

Current implementation detail:

- snapshots currently contain the local builder's signature only
- the runtime still stores them in fields named `LatestSnapshot` and `LatestFullySignedSnapshot`
- no multi-party snapshot quorum is enforced in the current Go runtime

### Funds-operation signing

`buildFundsOperation()` signs wallet-level table-funds operations after removing `signatureHex`.

The current provider id is:

- `arkade-table-funds/v1`

Operations are used for:

- renewals
- cash-out
- emergency exit

## Table Lifecycle

### Table creation

The host daemon:

1. creates a `NativeMeshTableConfig`
2. generates an invite code containing table and host information
3. optionally builds a signed advertisement for public tables
4. appends `TableAnnounce`
5. persists and replicates the table

### Join flow

The current join flow is:

1. the player decodes the invite and checks wallet availability
2. the player sends a wallet-signed `POST /native/table/join` with an identity binding to the host
3. the host appends `SeatLocked`
4. when the table reaches seat count, the host marks it `ready`
5. the host builds a snapshot, appends `TableReady`, and schedules the first hand
6. the updated table is replicated to peers with active-hand secrets redacted per recipient

The current Go runtime does not send separate signed `join-request` / `buy-in-confirm` envelopes between peers.

### Action flow

The current action flow is:

1. the seated player sends a wallet-signed `POST /native/table/action` to the host; the signed payload is bound to the current per-hand `decisionIndex` (`len(ActionLog)`) so it is valid only for that exact turn
2. the host applies the action through the Hold'em engine
3. the host appends `PlayerAction`
4. when the hand settles, the host builds a snapshot and appends `HandResult`
5. the updated table is replicated to peers with only the caller's hole cards included in replicated state

The current Go runtime does not exchange separate signed `action-request` envelopes between peers.

### Failover

Failover is driven from replicated table state rather than a separate lease-signing protocol.

Current rules:

- witnesses can take over when configured
- if no witnesses are configured, the seated player with the lowest peer ID is the failover candidate
- the new host appends `HostRotated`
- if a hand was in progress and a snapshot exists, the new host appends `HandAbort`, restores from that snapshot, and schedules the next hand

## Public Indexer Protocol

The public indexer surface is implemented in [`internal/indexer/app.go`](../internal/indexer/app.go).

### Ingest routes

- `POST /api/indexer/table-ads`
- `POST /api/indexer/table-updates`

### Public read routes

- `GET /api/public/tables`
- `GET /api/public/tables/{tableId}`

The host publishes:

- a signed advertisement
- `PublicTableSnapshot` updates derived from the current public state

The controller can proxy the public read routes for the browser.

## Current Limitations

These points are important for reading the current implementation accurately:

- peer join, action, and sync requests carry detached signatures over JSON payloads, but transport remains HTTP and peer URLs still downgrade from `ws://` to `http://` internally
- peers accept replicated table state through `/native/table/sync` and host polling after request-level auth and monotonicity checks; they do not currently re-run a full event-by-event cryptographic replay before persistence
- snapshots are signed, but the runtime does not yet collect or enforce a multi-party signature quorum
- the `latestFullySignedSnapshot` field name is historical; in current code it is populated with the latest locally built snapshot
- the indexer validates required fields, but it does not verify advertisement or update signatures before storing them

Those limitations are reflected again in [trust-model.md](./trust-model.md), because they materially affect the system's real trust boundary today.
