# Protocol

This document describes the current daemon-first poker protocol implemented in this repo.

It is a v1 implementation document, not a future-looking idealized spec. Where the code is intentionally incomplete, this doc says so directly.

## Scope

- protocol version: `poker/v1`
- canonical gameplay transport today: direct daemon-to-daemon WebSocket connections on `/mesh`
- canonical state model: signed, append-only table events plus signed cooperative snapshots
- public read path: optional indexer HTTP ingest plus read-only website APIs

This is the protocol implemented by:

- [index.ts](/Users/danieldresner/src/arkade_fun/packages/protocol/src/index.ts)
- [meshRuntime.ts](/Users/danieldresner/src/arkade_fun/apps/cli/src/meshRuntime.ts)
- [peerTransport.ts](/Users/danieldresner/src/arkade_fun/apps/cli/src/peerTransport.ts)
- [index.ts](/Users/danieldresner/src/arkade_fun/packages/settlement/src/index.ts)

## Goals

- keep wallet custody local to each daemon
- make gameplay reconstructable from persisted local state
- let hosts sequence hands without becoming custodians
- allow witnesses to preserve liveness and auditability
- keep website and public indexers outside consensus

## Identity Model

Each daemon uses three identities.


| Identity          | Purpose                                        | Signs                                                                                                             |
| ----------------- | ---------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| peer identity     | network addressability and transport handshake | `hello` wire frames                                                                                               |
| protocol identity | poker protocol participation                   | signed table events, join intents, action intents, snapshot/lease/failover signatures, advertisements, heartbeats |
| wallet identity   | funds and custody binding                      | identity bindings and table funds operations                                                                      |


### Identity binding

`IdentityBinding` explicitly ties a wallet identity to a peer/protocol identity for one table:

- `tableId`
- `peerId`
- `protocolId`
- `protocolPubkeyHex`
- `walletPlayerId`
- `walletPubkeyHex`
- `signedAt`
- `signatureHex`

The wallet key signs this binding. Hosts verify it during `join-request`.

### Signature primitive

Unless noted otherwise, protocol signatures use the same structured-data rule:

- serialize the unsigned payload with `stableStringify()`
- hash the serialized bytes with SHA-256
- sign the digest with compact secp256k1
- carry the signature as hex plus the signer public key in the object or enclosing envelope

This is the signing path implemented by `signStructuredData()` and `verifyStructuredData()` in [index.ts](/Users/danieldresner/src/arkade_fun/packages/settlement/src/index.ts).

### Signer matrix


| Object                     | Signed by                                         | Verified by                     | Current behavior                                                                                                    |
| -------------------------- | ------------------------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `hello`                    | peer identity                                     | transport                       | required before a provisional peer becomes confirmed                                                                |
| `IdentityBinding`          | wallet identity                                   | host on `join-request`          | binds wallet identity to peer and protocol identity for one table                                                   |
| `PlayerJoinIntent`         | protocol identity                                 | host                            | proves the joiner controls the advertised protocol key                                                              |
| `BuyInConfirm`             | protocol identity                                 | host                            | signs `{ tableId, playerId, confirmedAt, receipt-without-signature }`                                               |
| `PlayerActionIntent`       | protocol identity                                 | host                            | host accepts or rejects the action, then emits canonical events                                                     |
| `SignedTableEvent`         | protocol identity                                 | every receiver                  | this is the canonical state-changing signature path                                                                 |
| `TableSnapshotSignature`   | protocol identity of host, players, and witnesses | host/witness collector          | signatures are gathered and counted for completeness; received signatures are not yet re-verified cryptographically |
| `HostLeaseSignature`       | protocol identity                                 | host/witness collector          | collected best-effort and carried in the lease                                                                      |
| `HostFailoverAcceptance`   | protocol identity                                 | witness/host collector          | carried into failover events; not yet re-verified on receipt                                                        |
| `SignedTableAdvertisement` | host protocol identity                            | public readers/indexers         | used for public table discovery                                                                                     |
| `TableFundsOperation`      | wallet identity used by the funds provider        | currently the local daemon path | signed money receipt for buy-in, renewal, cashout, and exit operations                                              |
| `heartbeat`                | host protocol identity                            | receivers                       | carried today, but not yet signature-checked on receipt                                                             |


## Transport

### Current transport

Today the mesh uses direct WebSocket connections. Each daemon listens on:

- `ws://<peerHost>:<peerPort>/mesh`

Known peers are persisted locally and can start from bootstrap hints. A provisional peer ID derived from `peerUrl` may be used briefly during bootstrap, then replaced by the real peer ID once a signed `hello` is received.

### Wire frames

All peer traffic is wrapped in a `MeshWireFrame`.


| Kind            | Purpose                                                              |
| --------------- | -------------------------------------------------------------------- |
| `hello`         | signed transport handshake                                           |
| `request`       | point-to-point RPC request                                           |
| `response`      | RPC response or error                                                |
| `event`         | canonical signed table event                                         |
| `heartbeat`     | host liveness hint                                                   |
| `public-ad`     | signed public table advertisement                                    |
| `public-update` | public spectator/update frame; currently reserved in runtime         |
| `relay-forward` | wrapper for relay forwarding; placeholder for optional relay support |


### `hello`

`hello` is signed by the peer identity and includes:

- `peerId`
- `peerPubkeyHex`
- `protocolId`
- `protocolPubkeyHex`
- `alias`
- `peerUrl`
- `roles`
- `sentAt`
- `signatureHex`

The transport verifies this signature before accepting the peer as confirmed.

## Canonical Event Log

The canonical protocol is the `SignedTableEvent` stream.

### Envelope

Every canonical event includes:

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

### Event hash

The runtime computes the event hash as:

- `sha256(stableStringify(unsignedEvent))`

where `unsignedEvent` is the envelope without `signature`.

### Canonicality rules

The runtime applies these rules when receiving events:

- schema must parse
- signature must verify against `senderProtocolPubkeyHex`
- duplicate events are ignored by event hash
- stale epochs are ignored
- future events are queued if their predecessor has not arrived yet
- `prevEventHash` must match the last accepted canonical event hash
- `seq` must be monotonic within an epoch and restart at `1` on a new epoch

Only accepted events become canonical state.

## Event Families

### Table lifecycle

Schema supports:

- `TableAnnounce`
- `TableWithdraw`
- `JoinRequest`
- `JoinAccepted`
- `JoinRejected`
- `SeatProposal`
- `SeatLocked`
- `BuyInRequested`
- `BuyInLocked`
- `TableReady`
- `TableClosed`

Current runtime actively emits:

- `TableAnnounce`
- `JoinRequest`
- `JoinAccepted`
- `JoinRejected`
- `SeatLocked`
- `BuyInLocked`
- `TableReady`

Reserved but not currently emitted by the mesh runtime:

- `TableWithdraw`
- `SeatProposal`
- `BuyInRequested`
- `TableClosed`

### Hand lifecycle

Schema supports:

- `HandStart`
- `DealerCommit`
- `PrivateCardDelivery`
- `StreetStart`
- `PlayerAction`
- `ActionAccepted`
- `ActionRejected`
- `StreetClosed`
- `ShowdownReveal`
- `HandResult`
- `HandAbort`

Current host flow is:

1. `HandStart`
2. `DealerCommit`
3. `PrivateCardDelivery` for each seated player
4. `StreetStart`
5. `PlayerAction`
6. `ActionAccepted` or `ActionRejected`
7. `StreetClosed` and next `StreetStart` when the phase changes
8. optional `ShowdownReveal`
9. `HandResult`

`HandAbort` is used after host failover if a hand was live and the table must roll back to the last fully signed checkpoint.

### Host and witness lifecycle

Schema supports:

- `HostLeaseGranted`
- `HostHeartbeat`
- `WitnessSnapshot`
- `HostFailoverProposed`
- `HostFailoverAccepted`
- `HostRotated`

Current runtime actively emits:

- `HostLeaseGranted`
- `WitnessSnapshot`
- `HostFailoverProposed`
- `HostFailoverAccepted`
- `HostRotated`

Host liveness itself is carried over the separate `heartbeat` wire frame, not the `HostHeartbeat` event body.

### Public data events

Schema supports:

- `PublicTableSnapshot`
- `PublicHandUpdate`
- `PublicShowdownReveal`

These exist in the shared schema but are not currently used as canonical table events. Public data is published through indexer HTTP ingest instead.

## Requests and Responses

`request` / `response` frames are used for coordination that does not itself define canonical order.

### Supported requests


| Request                     | Purpose                                | Verified by receiver                                                         |
| --------------------------- | -------------------------------------- | ---------------------------------------------------------------------------- |
| `join-request`              | ask current host for a seat            | wallet identity binding and protocol signature on the join intent            |
| `buy-in-confirm`            | confirm local buy-in lock              | protocol signature over unsigned receipt payload                             |
| `action-request`            | submit a player action                 | protocol signature over the action intent                                    |
| `snapshot-sign-request`     | ask a peer to co-sign a snapshot       | no prior verification by receiver; receiver signs the supplied snapshot body |
| `lease-sign-request`        | ask witness/host to sign a host lease  | no prior verification by receiver beyond local availability                  |
| `failover-accept-request`   | ask peers to acknowledge host rotation | no prior verification by receiver beyond local availability                  |
| `peer-cache-request`        | fetch known peer hints                 | none                                                                         |
| `public-table-list-request` | fetch cached advertisements            | none                                                                         |


### Supported responses

- `join-response`
- `buy-in-response`
- `action-response`
- `snapshot-sign-response`
- `lease-sign-response`
- `failover-accept-response`
- `peer-cache-response`
- `public-table-list-response`

Errors are returned as `response` frames with `ok: false` and an `error` string.

## Table Join Protocol

### Private invite

Private table invites carry:

- `protocolVersion`
- `networkId`
- `tableId`
- `hostPeerId`
- `hostPeerUrl`
- optional `relayPeerId`

### Join sequence

1. player prepares a local buy-in lock via `TableFundsProvider.prepareBuyIn()`
2. player sends `join-request` with:
  - `PlayerJoinIntent`
  - `preparedBuyIn`
3. host verifies:
  - wallet identity binding
  - protocol signature on the join intent
4. host appends `JoinRequest`
5. host chooses a seat and appends `JoinAccepted`, or appends `JoinRejected`
6. host sends the current canonical backlog to the joiner before `JoinAccepted`
7. player confirms local buy-in and sends `buy-in-confirm`
8. host verifies the confirmation signature and appends:
  - `SeatLocked`
  - `BuyInLocked`
9. when all seats are locked, host appends `TableReady`

## Hand Protocol

### Dealer mode

The current runtime implements only:

- `host-dealer-v1`

The host:

- generates the deck locally
- commits to the deck root
- privately sends hole cards to each player
- reveals public board state through canonical events
- may reveal full audit material at showdown

This is intentionally a trusted-host dealing model. The host should not also play in the same table.

### Private card delivery

Hole cards are sent in `PrivateCardDelivery` as a `PrivateCardEnvelope`.

The current encryption scheme is:

- ECDH-style shared secret derived from sender protocol private key and recipient protocol public key
- scope-bound key derivation using `<tableId>:<handId>:cards`
- XOR keystream encryption with SHA-256-based blocks
- SHA-256 authenticity tag

This is suitable for local/test v1 behavior but should not be treated as a production-audited AEAD design.

### Action ordering

Players do not append canonical actions directly. They submit `action-request` to the host.

The host:

- verifies the action signature
- checks the live hand state
- appends `PlayerAction`
- runs the game engine
- appends `ActionAccepted` or `ActionRejected`

The host remains the sequencer, but only signed events become canonical.

## Snapshots and Money Checkpoints

The protocol uses `CooperativeTableSnapshot` for durable money-safe checkpoints.

### Snapshot fields

Each snapshot includes:

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
- `signatures`

### Snapshot cadence

Current runtime captures snapshots at:

- `TableReady`
- hand start
- street transitions
- `HandResult`
- post-abort recovery after failover

### Fully signed snapshot

A snapshot is considered fully signed when it contains signatures from:

- all seated player peer IDs
- all witness peer IDs
- the local non-player host/witness signer when applicable

The last fully signed snapshot is the last enforceable money state for v1.

### Funds operations

The shared protocol defines `TableFundsOperation` for:

- `buy-in-prepared`
- `buy-in-locked`
- `checkpoint-recorded`
- `cashout`
- `close-table`
- `renewal`
- `emergency-exit`

In the current runtime:

- buy-in confirmations are reflected in canonical `BuyInLocked` events
- `HandResult` may reference a `checkpointHash`
- cashout, renewal, and emergency exit are executed via `TableFundsProvider` on the local daemon
- the provider boundary exists for an Arkade-backed implementation, but the current default behavior in local flows is still the signed mock adapter

### Money movement model

The runtime uses a table-balance-lock model, not per-bet Arkade settlement.

1. each player locks a buy-in into table participation state through `TableFundsProvider`
2. once a hand begins, betting changes only the app-layer chip ledger in canonical events and snapshots
3. the latest fully signed cooperative snapshot is the enforceable balance checkpoint for v1
4. no host-only unsigned decision can directly move funds

This means the poker rules engine stays in the app protocol, while money safety is anchored to signed cooperative checkpoints and local wallet-controlled cashout or exit flows.

### Buy-in flow

1. Before joining, the player daemon calls `prepareBuyIn(tableId, playerId, amountSats)`.
2. The provider returns a signed `TableFundsOperation` with `kind: buy-in-prepared`, `provider`, `networkId`, `amountSats`, optional `vtxoExpiry`, `signerPubkeyHex`, and `signatureHex`.
3. The player includes that receipt in `join-request` together with the signed `PlayerJoinIntent`.
4. After seat acceptance, the player daemon confirms the lock locally with `confirmBuyIn(...)` and sends `buy-in-confirm`.
5. `buy-in-confirm` is signed by the player's protocol identity over `{ tableId, playerId, confirmedAt, receipt-without-signature }`.
6. The host verifies that protocol signature, then appends `SeatLocked` and `BuyInLocked`.
7. The locked receipt is stored in local `buyInReceipts` and its `amountSats` becomes the player's initial chip balance at `TableReady`.

### In-hand balance updates

During a hand there is no Arkade call per bet, raise, or fold.

1. players submit signed action intents to the host
2. the host sequences canonical `PlayerAction`, `ActionAccepted`, `ActionRejected`, `StreetClosed`, and `HandResult` events
3. chip balances, pots, and side-pot state live in the event-derived public state and in cooperative snapshots
4. `HandResult` may carry a `checkpointHash`, but the authoritative money checkpoint remains the latest fully signed snapshot

This is why an unfinished hand is rolled back on hard failure instead of trying to force speculative intra-hand balances onto the settlement layer.

### Cooperative checkpoint and cash-out flow

1. At `TableReady`, hand start, street transitions, `HandResult`, and post-abort recovery, the host or witness captures a `CooperativeTableSnapshot`.
2. The collector signs the unsigned snapshot and requests additional snapshot signatures from seated players and witnesses.
3. The snapshot hash is `sha256(stableStringify(unsignedSnapshot))`.
4. Once the runtime sees signatures from the required peers, that snapshot becomes `latestFullySignedSnapshot`.
5. `cashOut()` reads the player's balance from `latestFullySignedSnapshot`, computes `checkpointHash` from that snapshot, and calls `cooperativeCashOut(tableId, playerId, balance, checkpointHash)`.
6. The returned `TableFundsOperation` has `kind: cashout` and references that `checkpointHash`.
7. The same interface also supports `cooperativeCloseTable(...)`, although the current runtime path is centered on per-player cashout.

### Renewal and expiry

The funds provider tracks an expiry window per table position using `vtxoExpiry`.

1. prepared and locked buy-in receipts include an expiry timestamp
2. `renewFunds()` calls `renewTablePositions(tableId)`
3. the provider returns signed `renewal` operations and extends `vtxoExpiry`
4. the daemon updates local receipts and can warn before table positions expire

Renewal is local money-state maintenance. It is not currently emitted as a canonical table event.

### Emergency exit

1. `emergencyExit()` requires a `latestFullySignedSnapshot`
2. the daemon computes the exiting player's balance and the snapshot hash
3. it calls `emergencyExit(tableId, playerId, lastCheckpointHash, amountSats)`
4. the returned `TableFundsOperation` has `kind: emergency-exit` and references the last cooperative checkpoint hash

The protocol therefore treats the latest fully signed checkpoint as the last enforceable money state. Any hand that never reached that checkpoint boundary is canceled for settlement purposes.

### Ark / Arkade usage

The codebase uses Arkade in two distinct layers.

1. Wallet and payment flows already integrate with Arkade for wallet summary, boarding, and withdrawals through helpers such as `connectArkadeWallet()`, `getArkadeWalletSummary()`, `onboardArkadeFunds()`, `offboardArkadeFunds()`, `createArkadeDepositQuote()`, and `submitArkadeWithdrawal()`.
2. Table gameplay uses the `TableFundsProvider` boundary so the poker protocol can lock buy-ins, track cooperative checkpoints, renew positions, cash out, and emergency exit without embedding Arkade calls directly into the game engine.

This matches the intended Ark model in the repo: Arkade is the custody and settlement layer around cooperative checkpoints, not the place where every poker action is adjudicated.

Today that table-funds boundary is only partially wired to live Arkade behavior:

- `createArkadeTableFundsProvider()` currently returns the same `SignedTableFundsProvider` implementation as the mock path, with provider name `arkade-table-funds/v1` and a longer default expiry window
- `TableFundsOperation` receipts are real signed protocol objects, but they are not yet backed by live per-table Ark contract execution
- `recordCheckpoint()` exists on the interface, but the runtime does not yet call it when snapshots are captured
- current cashout, renewal, and emergency-exit flows therefore produce signed local receipts that reference cooperative checkpoint hashes, rather than driving a full `arkd` table contract lifecycle

So the protocol already uses Arkade as the architectural money boundary, but the current implementation is still an honest v1 scaffold rather than a production-complete Ark table contract integration.

## Host Lease and Failover

### Heartbeats

Current timing constants:

- heartbeat interval: `1000ms`
- host failure timeout: `3500ms`
- lease duration: `4000ms`

The host sends `heartbeat` wire frames to players and witnesses.

Important implementation note:

- heartbeats carry a signature today
- receivers currently use them as liveness hints by `tableId` and `epoch`
- the runtime does not yet cryptographically verify heartbeat signatures on receipt

### Lease flow

A `HostLease` carries:

- `tableId`
- `epoch`
- `hostPeerId`
- `witnessSet`
- `leaseStart`
- `leaseExpiry`
- `signatures`

Host creation emits:

- `HostLeaseGranted`

Rotation emits:

- `HostFailoverProposed`
- zero or more `HostFailoverAccepted`
- `HostRotated`

### Failover behavior

Current v1 policy is:

- between hands: witnesses can rotate the host and continue
- mid-hand: new host aborts the hand and rolls back to the last fully signed snapshot

The witness runtime triggers failover after missed heartbeats. Failover acknowledgements and witness lease signatures are currently best-effort; the runtime does not enforce a strict quorum before rotating.

## Public Discovery and Spectator Protocol

### Signed public advertisement

`SignedTableAdvertisement` includes:

- `protocolVersion`
- `networkId`
- `tableId`
- `hostPeerId`
- optional `hostPeerUrl`
- `tableName`
- blind sizes in `stakes`
- `currency`
- `seatCount`
- `occupiedSeats`
- `spectatorsAllowed`
- `hostModeCapabilities`
- `witnessCount`
- `buyInMinSats`
- `buyInMaxSats`
- `visibility`
- optional geographic / latency hints
- `adExpiresAt`
- `hostProtocolPubkeyHex`
- `hostSignatureHex`

Public advertisements are distributed by:

- `public-ad` wire frames
- `POST /api/indexer/table-ads`

### Public updates

Public indexer update types are:

- `PublicTableSnapshot`
- `PublicHandUpdate`
- `PublicShowdownReveal`

HTTP ingest and read endpoints are:

- `POST /api/indexer/table-updates`
- `GET /api/public/tables`
- `GET /api/public/tables/:tableId`

The website is read-only against these endpoints.

## Persistence and Replay

Each daemon persists mesh protocol state locally.

### Mesh store

Per-profile mesh state is stored under:

- `tables/<tableId>/events.ndjson`
- `tables/<tableId>/snapshots.ndjson`
- `tables/<tableId>/private-state.json`
- `public-ads.json`

### Profile store

The profile file stores:

- peer and protocol private keys
- wallet private key reference
- known peers
- mesh table references
- current mesh table selection

### Replay

On startup, the runtime:

1. loads persisted tables
2. loads events, snapshots, and private state
3. replays events in order into a fresh context
4. reconstructs the latest canonical public state and snapshot pointers

This is the basis for restart recovery.

## Verification Coverage and Current Gaps

### Verified today

- `hello` signatures
- canonical `SignedTableEvent` signatures
- wallet `IdentityBinding` signatures
- join intent signatures
- buy-in confirmation signatures
- player action signatures
- locally created `TableFundsOperation` signatures

### Not fully verified yet

- heartbeat signatures are not checked on receipt
- snapshot signatures are counted for completeness, but not cryptographically re-verified when received
- lease signatures are collected and carried, but not cryptographically re-verified when received
- failover acceptance signatures are carried into events, but not cryptographically re-verified when received
- `preparedBuyIn` is carried in `join-request`, but the host does not yet use it as a canonical verified money object
- hosts verify the player's protocol signature over `buy-in-confirm`, but do not separately verify the underlying `TableFundsOperation.signatureHex`
- the Arkade-named table funds provider is still the local signed adapter, not a live per-table Ark contract executor
- `public-update` wire frames and several schema variants remain reserved for future work

### Honest trust statement

The strongest current protocol guarantees come from:

- local wallet custody
- signed canonical event history
- fully signed cooperative snapshots
- host failover bounded by checkpoint rollback

The protocol does not yet claim:

- dealerless hidden-card privacy
- browser-native peer equivalence
- libp2p/DHT/NAT traversal in the current implementation
- production-complete Arkade table contracts
- full cryptographic validation for every auxiliary signature path

