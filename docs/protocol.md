# Current Protocol Surface

This document describes the protocol and wire surface implemented today in this repository. It is intentionally current-state only.

For component topology, see [architecture.md](./architecture.md). For guarantees and trust assumptions, see [trust-model.md](./trust-model.md).

## Scope

The active implementation is split across:

- [`cmd/parker-daemon`](../cmd/parker-daemon/main.go), [`internal/mesh_runtime.go`](../internal/mesh_runtime.go), and [`internal/transport_wire.go`](../internal/transport_wire.go) for daemon behavior and peer-to-peer table sync
- [`internal/proxy_daemon.go`](../internal/proxy_daemon.go) and [`internal/client.go`](../internal/client.go) for local daemon RPC
- [`cmd/parker-controller`](../cmd/parker-controller/main.go) and [`internal/controller/app.go`](../internal/controller/app.go) for localhost browser control
- [`cmd/parker-indexer`](../cmd/parker-indexer/main.go) and [`internal/indexer/app.go`](../internal/indexer/app.go) for public ingest and spectator reads
- [`internal/settlementcore/core.go`](../internal/settlementcore/core.go) for canonical JSON hashing and signatures
- [`internal/native_types.go`](../internal/native_types.go) for the main runtime object shapes

Any browser client using the localhost controller is outside peer-to-peer protocol consensus.

## Current Runtime Split

The current implementation has four distinct protocol surfaces:

1. local daemon RPC over Unix-socket NDJSON
2. localhost controller HTTP and SSE for browser-safe local control
3. peer-to-peer framed TCP messages over `parker://` endpoints with signed transport envelopes
4. public indexer ingest/read over HTTP

Only the local daemon RPC and peer transport are required for direct CLI-driven table play. The controller and indexer are optional browser/public surfaces layered on top.

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

## Local Controller HTTP Surface

The optional localhost controller in [`internal/controller/app.go`](../internal/controller/app.go) adapts daemon RPC into browser-safe HTTP and SSE.

### Route shape

The controller exposes:

- `GET /health`
- profile discovery and daemon status routes under `/api/local/profiles`
- daemon-backed wallet, network, and table routes under `/api/local/profiles/{profile}/...`
- `GET /api/local/profiles/{profile}/watch` as an SSE bridge over daemon `watch`
- `GET /api/public/tables` and `GET /api/public/tables/{tableId}` as optional indexer proxies

### Access control

For `/api/local/*` routes:

- the request must include `X-Parker-Local-Controller`
- if an `Origin` header is present, it must match the controller's configured allowlist

The controller does not speak peer transport itself. It forwards local requests to the profile daemon over the Unix socket described above.

## Peer-To-Peer Runtime Protocol

The current Go runtime does not implement the older signed WebSocket mesh described in earlier design docs. Instead, it exchanges signed transport envelopes over a framed TCP transport.

### Peer address format

Peer endpoints are stored and advertised as:

- `parker://<host>:<port>`

The runtime dials those endpoints directly over TCP and exchanges one newline-delimited JSON request envelope followed by one newline-delimited JSON response envelope per connection.

The dialer also accepts:

- `tcp://<host>:<port>` for direct TCP bootstrap
- `tor://<host>:<port>` for Tor-routed bootstrap

If the target host is an onion address, or the scheme is `tor://`, the runtime dials through Tor SOCKS when Tor is enabled.

### Transport envelope auth and confidentiality

All peer transport messages use [`TransportEnvelope`](../internal/transport_types.go):

- every envelope is signed by the sender's protocol key
- the `peer.manifest.get` bootstrap exchange is signed but typically unencrypted, because it establishes the recipient transport key
- once the sender knows the recipient transport public key, request and response bodies are encrypted with an ephemeral X25519 shared secret plus AES-256-GCM (`x25519-aes256gcm`)
- the receiver verifies the envelope signature and body hash before dispatch

### Peer message types

The active peer transport handlers live in [`internal/mesh_runtime.go`](../internal/mesh_runtime.go) and [`internal/transport_wire.go`](../internal/transport_wire.go):

- `peer.manifest.get`
- `peer.manifest`
- `table.state.pull`
- `table.state.push`
- `table.join.request`
- `table.join.response`
- `table.action.request`
- `table.action.response`
- `ack`
- `nack`

### Practical behavior

- `peer.manifest.get` requests the peer's current advertised identity, peer URL, wallet player id, protocol id, and transport public key.
- `table.join.request` sends a join payload to the current host inside a signed transport envelope and returns the updated recipient-scoped table state.
- `table.action.request` sends an action payload to the current host inside a signed transport envelope and returns the updated recipient-scoped table state.
- `table.state.push` pushes a signed recipient-scoped table copy to another peer.
- `table.state.pull` fetches the host's current recipient-scoped table copy; the request can include a fresh wallet-signed fetch auth payload so the caller receives its own hidden cards back.

### Host heartbeat and sync timing

The runtime tracks liveness with the table field `LastHostHeartbeatAt`.

Current timing constants in [`internal/mesh_store.go`](../internal/mesh_store.go):

- host heartbeat interval: `1000ms`
- host failure timeout: `3500ms`
- non-host host-poll interval: `1s`
- next-hand delay after settlement: `1000ms`

The current host updates its own table copy on each heartbeat tick. Non-host peers poll the host table periodically and can trigger failover when the heartbeat goes stale.

## Current Signed Objects

The runtime uses canonical JSON signatures from [`internal/settlementcore/core.go`](../internal/settlementcore/core.go) for these object families:

- peer transport envelopes
- identity bindings and request-auth payloads
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

`appendEvent()` in [`internal/mesh_runtime.go`](../internal/mesh_runtime.go) signs the full event object after removing the `signature` field.

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

`buildSnapshot()` signs the full snapshot object after removing `signatures`.

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

### Request-level auth payloads

In addition to transport-envelope signatures, the runtime uses these request-specific signed payloads:

- `settlementcore.IdentityBinding` in join requests, signed by the player's wallet key and binding `tableId`, `peerId`, `peerUrl`, `protocolId`, and wallet identity together
- `nativeActionAuthPayload()` in action requests, signed by the player's wallet key and bound to `tableId`, `playerId`, `handId`, `epoch`, `decisionIndex`, and `signedAt`
- `nativeTableFetchAuthPayload()` in table-pull requests, signed by the player's wallet key when the caller wants recipient-scoped hidden-card visibility
- `nativeTableSyncAuthPayload()` in table-push requests, signed by the current host's protocol key over the table hash and send timestamp

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
2. the player builds `nativeJoinRequest`, including a wallet-signed identity binding over the claimed peer, protocol, and wallet identity
3. the player sends that request inside `table.join.request` to the host's `parker://` endpoint
4. the host verifies the identity binding and checks that the claimed peer endpoint serves the same peer/protocol/wallet identity
5. the host appends `SeatLocked`
6. when the table reaches seat count, the host marks it `ready`
7. the host builds a snapshot, appends `TableReady`, and schedules the first hand
8. the updated table is replicated to peers with active-hand secrets redacted per recipient

There is no separate buy-in-confirm side channel beyond the join request, signed events, and replicated table state.

### Action flow

The current action flow is:

1. the seated player builds `nativeActionRequest`; its wallet signature is bound to the current `tableId`, `handId`, `epoch`, and per-hand `decisionIndex` (`len(ActionLog)`)
2. the player sends that request inside `table.action.request` to the current host's `parker://` endpoint
3. the host verifies the wallet signature and current turn binding, then applies the action through the Hold'em engine
4. the host appends `PlayerAction`
5. when the hand settles, the host builds a snapshot and appends `HandResult`
6. the updated table is replicated to peers with only the caller's hole cards included in replicated state

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

- peer join, action, fetch, and sync now travel inside signed transport envelopes over direct TCP `parker://` links; join/action/fetch/sync payloads still add their own wallet- or protocol-level auth objects where needed
- peers accept replicated table state through `table.state.push` and host polling via `table.state.pull` after envelope verification, request-level auth, and monotonicity checks; they do not currently re-run a full event-by-event cryptographic replay before persistence
- snapshots are signed, but the runtime does not yet collect or enforce a multi-party signature quorum
- the `latestFullySignedSnapshot` field name is historical; in current code it is populated with the latest locally built snapshot
- the indexer validates required fields, but it does not verify advertisement or update signatures before storing them

Those limitations are reflected again in [trust-model.md](./trust-model.md), because they materially affect the system's real trust boundary today.
