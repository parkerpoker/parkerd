# Poker Mesh Protocol

This document describes the protocol implemented today in this repository. It stays protocol-focused: wire format, canonical state, signing rules, settlement checkpoints, and failover behavior.

## Document Scope

- Protocol consensus lives inside the daemon mesh implemented by [`apps/daemon`](../apps/daemon) and [`packages/daemon-runtime`](../packages/daemon-runtime).
- [`apps/indexer`](../apps/indexer) is an optional public ingest/read model.
- [`apps/controller`](../apps/controller) is a local HTTP and SSE control plane for one machine. It is outside consensus.
- [`apps/web`](../apps/web) is a browser UI that can read either the public indexer or the local controller, but it is not a consensus peer.
- The indexer and UI are outside consensus. They cannot append canonical gameplay events, sign settlement checkpoints, or finalize money movement.
- For component topology and runtime boundaries, see [architecture.md](./architecture.md).
- For guarantees, trust assumptions, and failure consequences, see [trust-model.md](./trust-model.md).

## Current Runtime Split

The protocol described here is exercised through the current runtime split:

- `apps/daemon` owns long-running process behavior, peer transport, canonical event replay, settlement coordination, and local persistence.
- `apps/cli` only controls the local daemon over profile-local Unix-socket RPC.
- `apps/controller` only controls the local daemon over localhost HTTP and SSE backed by that same RPC.
- `apps/indexer` is optional and only ingests signed public advertisements and public updates, then serves a read model over HTTP.
- `apps/web` is a hybrid UI that can query the public indexer and the local controller, but never joins the mesh.

Primary implementation units:

- `packages/protocol/src/index.ts`
- `packages/daemon-runtime/src/meshRuntime.ts`
- `packages/daemon-runtime/src/peerTransport.ts`
- `packages/daemon-runtime/src/daemonProcess.ts`
- `packages/settlement/src/index.ts`
- `packages/daemon-runtime/src/tableFundsStateStore.ts`
- `integration/mesh-regtest.ts`

## Scope

- Protocol version: `poker/v1`
- Canonical gameplay transport: direct daemon-to-daemon WebSockets on `/mesh`
- Canonical state: signed `SignedTableEvent` history plus signed cooperative settlement-boundary snapshots
- Settlement layer: Arkade-backed `TableFundsProvider` with wallet-local custody
- Implemented game variant: heads-up Texas Hold'em

The localhost controller HTTP and SSE routes are intentionally out of scope for protocol consensus. They are a local control plane, not a protocol wire format between peers.

Although shared schemas allow more seats and more dealer modes, the current runtime only starts hands when exactly two bankroll participants are seated, and it only uses `host-dealer-v1`.

## Identity Model

Each daemon uses three identities:

- peer identity for transport handshake and peer addressability
- protocol identity for poker participation and canonical signatures
- wallet identity for funds ownership, join binding, and table-funds receipts

### Identity binding

`IdentityBinding` binds one wallet identity to one peer/protocol identity for one table:

- `tableId`
- `peerId`
- `protocolId`
- `protocolPubkeyHex`
- `walletPlayerId`
- `walletPubkeyHex`
- `signedAt`
- `signatureHex`

The wallet identity signs the unsigned binding body. Hosts verify this binding when handling `join-request`.

## Canonical Structured Signing

All structured signatures in the poker protocol use the same canonicalization and hashing rules implemented in `packages/settlement/src/index.ts`.

### Canonicalization

`stableStringify()` first canonicalizes input data, then applies `JSON.stringify()` to the canonical form.

Canonicalization rules:

- `null` stays `null`
- strings and booleans stay unchanged
- finite numbers stay numeric
- non-finite numbers become `null`
- `bigint` becomes a base-10 string
- `Date` becomes an ISO-8601 string
- arrays preserve order; `undefined`, functions, and symbols inside arrays become `null`
- typed-array views become lowercase hex strings
- objects drop keys whose values are `undefined`, functions, or symbols
- remaining object keys are sorted lexicographically before serialization

### Hash and signature primitive

For any structured payload:

1. Build the unsigned payload by removing the signature field or signature collection.
2. Serialize with `stableStringify()`.
3. Hash the UTF-8 bytes with SHA-256.
4. Sign the digest with compact secp256k1.
5. Encode the signature as hex.

Verification repeats the same canonicalization and hashing steps. Any post-sign mutation of the signed body invalidates the signature.

### Exact unsigned payloads

The runtime signs and verifies these exact bodies:

- `hello`: `{ alias, peerId, peerPubkeyHex, peerUrl, protocolId, protocolPubkeyHex, roles, sentAt }`
- `heartbeat`: `{ epoch, hostPeerId, leaseExpiry, sentAt, tableId }`
- `SignedTableAdvertisement`: advertisement object without `hostSignatureHex`
- `IdentityBinding`: binding object without `signatureHex`
- `PlayerJoinIntent`: all intent fields except `signatureHex`
- `BuyInConfirm`: `{ confirmedAt, playerId, receipt: unsignedFundsOperation(receipt), tableId }`
- `PlayerActionIntent`: all intent fields except `signatureHex`
- `TableFundsOperation`: operation object without `signatureHex`
- `SignedTableEvent`: event envelope without `signature`
- `CooperativeTableSnapshot`: snapshot without `signatures`
- `HostLease`: lease without `signatures`
- `HostFailoverAcceptance`: acceptance without `signatureHex`

`unsignedFundsOperation()` canonicalizes the receipt after removing `signatureHex`. Hosts use that exact form when verifying `buy-in-confirm`.

## Transport

### WebSocket mesh

Each daemon listens on:

- `ws://<host>:<port>/mesh`

Known peers are stored locally. A provisional peer entry may exist before `hello`, but a connection is not considered identified until a valid signed `hello` is received.

### Wire frames

All traffic is a `MeshWireFrame`:

- `hello`
- `request`
- `response`
- `event`
- `heartbeat`
- `public-ad`
- `public-update`
- `relay-forward`

All inbound frames are schema-parsed before handling.

### `hello`

`hello` must be the first meaningful frame received on a socket. Any non-`hello` frame received before peer identification is rejected.

The receiver verifies the peer-identity signature over the unsigned `hello` payload. On success, the transport:

- marks the peer as confirmed
- stores `peerId`, `peerUrl`, `alias`, `roles`, and `protocolPubkeyHex`
- replaces any provisional entry that used the same `peerUrl`

### `heartbeat`

`heartbeat` is not a canonical table event. It is a signed liveness frame from the current host.

Receiver behavior:

- ignore if `tableId` is unknown
- ignore if `hostPeerId` does not equal `currentHostPeerId`
- ignore if `epoch` does not equal `currentEpoch`
- resolve the current host protocol pubkey
- verify the structured signature over `{ epoch, hostPeerId, leaseExpiry, sentAt, tableId }`
- update `lastHostHeartbeatAt` only if verification succeeds

Invalid heartbeats are ignored and never affect failover timing.

### Public ads and relay frames

- `public-ad` is verified against `hostProtocolPubkeyHex` and then cached locally
- inbound `public-update` frames are ignored by the current mesh runtime
- `relay-forward` is a best-effort forwarder to an already-open target socket and is not part of consensus

## Request / Response Protocol

`request` and `response` frames coordinate side effects that do not themselves define canonical ordering.

### Supported requests

- `join-request`
- `buy-in-confirm`
- `action-request`
- `snapshot-sign-request`
- `lease-sign-request`
- `failover-accept-request`
- `peer-cache-request`
- `public-table-list-request`

### Join request validation

`join-request` carries:

- `intent: PlayerJoinIntent`
- `preparedBuyIn: TableFundsOperation`

The host verifies:

- `IdentityBinding.signatureHex`
- the protocol signature on the join intent
- the transport peer ID matches `intent.peerId`
- `preparedBuyIn.signatureHex`
- `preparedBuyIn.provider === "arkade-table-funds/v1"` in the canonical Arkade path
- `preparedBuyIn.kind === "buy-in-prepared"`
- `preparedBuyIn.status === "prepared"`
- `preparedBuyIn.playerId === intent.player.playerId`
- `preparedBuyIn.tableId === tableId`
- `preparedBuyIn.networkId === table.networkId`
- `preparedBuyIn.signerPubkeyHex === intent.identityBinding.walletPubkeyHex`
- `preparedBuyIn.amountSats` is inside table buy-in bounds

If accepted, the host appends `JoinRequest`, sends the existing canonical backlog to the joiner, and then appends `JoinAccepted`. If no seat is available, the host appends `JoinRejected`.

### Buy-in confirmation validation

`buy-in-confirm` carries a `BuyInConfirm` with:

- `tableId`
- `playerId`
- `confirmedAt`
- `receipt`
- `signatureHex`

The host verifies:

- the confirming peer matches the pending seated player
- the protocol signature over `{ confirmedAt, playerId, receipt: unsignedFundsOperation(receipt), tableId }`
- `receipt.signatureHex`
- `receipt.provider === "arkade-table-funds/v1"` in the canonical Arkade path
- `receipt.kind === "buy-in-locked"`
- `receipt.status === "locked"`
- `receipt.signerPubkeyHex === pending.walletPubkeyHex`

On success, the host appends `SeatLocked` and `BuyInLocked`. Once all seats are locked, the host emits `TableReady`, captures a settlement-boundary snapshot, and schedules the first hand.

### Action request validation

Players do not append canonical actions directly. They send `action-request` to the current host.

The host verifies:

- the protocol signature on `PlayerActionIntent`
- the referenced hand is currently active
- hand setup is complete (`handSetupInFlight === false`)
- the transport peer matches the acting seat

The host then appends `PlayerAction`, runs the game engine, and appends either `ActionAccepted` or `ActionRejected`.

### Snapshot sign request validation

The collector sends `snapshot-sign-request` with a candidate `CooperativeTableSnapshot`.

The receiver:

- waits briefly for local replay to reach `snapshot.latestEventHash`
- recomputes the expected snapshot body from local canonical state
- verifies every supplied snapshot signature cryptographically
- requires the collector's own signature to already be present
- signs the unsigned snapshot only if the snapshot body exactly matches local state

### Lease sign request validation

For a known table, the signer verifies the lease body and all supplied signatures before adding its own signature.

For an unknown table, only the epoch-1 bootstrap lease can be signed, and only if:

- the signer is included in `lease.witnessSet`
- the request contains a valid host signature
- the host signature matches a known host peer's protocol pubkey

During bootstrap, a witness may still be referenced by a provisional peer ID. The lease builder resolves that provisional entry to the witness's final signed peer ID and rebuilds the lease before final quorum enforcement.

### Failover acceptance request validation

The responder verifies:

- `currentEpoch === local currentEpoch`
- `proposedEpoch === local currentEpoch + 1`
- the local peer is part of the failover quorum
- the requester is the current host or a configured witness

If valid, the responder returns a signed `HostFailoverAcceptance`.

## Canonical Event Log

The authoritative protocol history is the `SignedTableEvent` stream.

### Envelope

Every event contains:

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

The canonical event hash is:

- `sha256(stableStringify(unsignedEvent))`

where `unsignedEvent` is the event without `signature`.

### Local append rules

When the local daemon appends an event:

- `epoch` is `body.newEpoch` for `HostRotated`, otherwise `currentEpoch`
- `seq` is `1` if no event exists yet in that epoch, otherwise `lastSeqInEpoch + 1`
- `prevEventHash` is the current `lastEventHash`, even across epoch boundaries

The first event of a new epoch therefore still points back to the last accepted hash from the previous epoch.

### Acceptance rules

Inbound event processing is:

1. Parse against `signedTableEventSchema`.
2. Verify the event signature against `senderProtocolPubkeyHex`.
3. Compute `eventHash`.
4. Ignore duplicates already in `events` or `pendingEvents`.
5. Classify ordering:
   - ignore if `event.epoch < currentEpoch`
   - ignore if `event.epoch === currentEpoch` and `event.seq < expectedSeq`
   - queue if `event.epoch === currentEpoch` and `event.seq > expectedSeq`
   - queue if `event.epoch > currentEpoch` and `event.seq !== 1`
   - queue if `prevEventHash !== lastEventHash`
   - otherwise accept
6. For accepted events, run semantic validation.
7. Commit the event, update derived state, then repeatedly re-check queued events.

If a queued event later becomes admissible but fails semantic validation, it is discarded and never enters canonical state.

### Semantic validation rules

Additional event-specific rules are enforced before an event becomes canonical:

- `TableAnnounce`: must be emitted by `table.hostPeerId` as `senderRole: "host"`
- `JoinRequest`, `JoinAccepted`, `JoinRejected`: must be emitted by the current host and carry a valid signed join intent
- `SeatLocked`: must be emitted by the current host and match a pending reservation
- `BuyInLocked`: must be emitted by the current host and carry a valid locked funds receipt
- gameplay events (`TableReady`, `HandStart`, `DealerCommit`, `PrivateCardDelivery`, `StreetStart`, `PlayerAction`, `ActionAccepted`, `ActionRejected`, `StreetClosed`, `ShowdownReveal`, `HandResult`, `HandAbort`, `TableClosed`): must be emitted by the current host
- `HostLeaseGranted`: must be emitted by the lease holder and carry a fully valid lease
- `WitnessSnapshot`: must be emitted by the current host or a configured witness and carry a fully valid snapshot
- `HostFailoverProposed`: must be emitted by the current host or a configured witness, and `previousHostPeerId` must equal the current host
- `HostFailoverAccepted`: must be emitted by a configured witness collector and carry a valid acceptance signature
- `HostRotated`: must be emitted by the current host or a configured witness, must advance the epoch by exactly one, and must carry a fully valid lease whose `hostPeerId` matches `newHostPeerId`

## Table Lifecycle

### Table creation

The host emits:

1. `TableAnnounce`
2. `HostLeaseGranted`

The initial lease is signed by the host plus every configured witness.

### Seating and readiness

The current flow is:

1. player locally prepares buy-in funds
2. player sends `join-request`
3. host appends `JoinRequest`
4. host appends `JoinAccepted` or `JoinRejected`
5. player locally confirms the prepared buy-in
6. player sends `buy-in-confirm`
7. host appends `SeatLocked`
8. host appends `BuyInLocked`
9. once all seats are locked, host appends `TableReady`

The first `TableReady` snapshot is a settlement boundary with `phase: null`.

## Hand Protocol

### Dealer mode

The implemented dealer mode is `host-dealer-v1`.

The host:

- generates the deck locally
- commits to the deck root
- privately encrypts hole cards to each player
- sequences all public actions and board state
- reveals full audit material in `ShowdownReveal` when no player folded before settlement

This is a trusted-host dealing model, not dealerless mental poker.

### Private card delivery

`PrivateCardDelivery` carries a `PrivateCardEnvelope` encrypted with:

- an ECDH-style shared secret between sender protocol private key and recipient protocol public key
- a scope string of `<tableId>:<handId>:cards`
- a SHA-256-derived XOR keystream
- a SHA-256 authenticity tag

The recipient decrypts only after the event is canonical.

### Hand sequencing

The host begins a hand only if:

- it is the current host
- the table is not closed
- exactly two players are seated
- no active unsettled hand is already in progress

The canonical sequence is:

1. `HandStart`
2. `DealerCommit`
3. one `PrivateCardDelivery` per seated player
4. initial `StreetStart`
5. repeated `PlayerAction` plus `ActionAccepted` or `ActionRejected`
6. `StreetClosed` then next `StreetStart` when phase changes
7. optional `ShowdownReveal`
8. `HandResult`

`handSetupInFlight` prevents action acceptance until the initial hand-start sequence is fully emitted.

### Settlement completion

When the game engine reaches `phase === "settled"`:

- `ShowdownReveal` is emitted if nobody folded before settlement
- `HandResult` is emitted with the final `publicState`
- a settlement-boundary snapshot is scheduled
- the next hand is scheduled after a short delay

No mid-hand snapshots are currently emitted. Settlement-boundary snapshots are created only after:

- `TableReady`
- `HandResult`
- `HandAbort` rollback

## Cooperative Snapshots

`CooperativeTableSnapshot` is the enforceable settlement checkpoint object.

### Snapshot body

The runtime reconstructs snapshot bodies directly from canonical public state:

- `snapshotId`
- `tableId`
- `epoch`
- `handId`
- `handNumber`
- `phase`
- `seatedPlayers`
- `chipBalances`
- `potSats`
- `sidePots` (currently always `[]`)
- `turnIndex`
- `livePlayerIds`
- `foldedPlayerIds`
- `dealerCommitmentRoot`
- `previousSnapshotHash`
- `latestEventHash`
- `createdAt`

`previousSnapshotHash` is the hash of the last stored snapshot, not merely the last fully signed one.

### Snapshot hash

The snapshot hash used as the settlement checkpoint hash is:

- `sha256(stableStringify(unsignedSnapshot))`

where `unsignedSnapshot` is the snapshot without `signatures`.

### Snapshot signature rules

Every supplied signature is verified against the unsigned snapshot.

Additional signer rules:

- host signature must come from `currentHostPeerId`
- player signature must come from a currently seated player peer ID
- witness signature must come from a peer ID in `witnessSet`
- duplicate signer peer IDs are forbidden
- signer pubkeys must match the runtime's resolved protocol pubkeys for those peers

### Snapshot quorum

A snapshot is fully signed only if it contains signatures from these exact peer IDs:

- the current host
- every seated player
- every configured witness

The runtime does not accept "enough witnesses"; it requires the configured witness peer IDs specifically.

### Settlement boundary

Only snapshots with:

- `phase === null`, or
- `phase === "settled"`

can become `latestFullySignedSnapshot`.

`cashOut()` and `emergencyExit()` always use the hash and balances from `latestFullySignedSnapshot`. Unfinished hands therefore never affect settlement.

## Arkade Table Funds Model

The canonical funds provider name is:

- `arkade-table-funds/v1`

Each seated player daemon controls its own Arkade wallet and local per-table funds state.

### Local funds state

Per table, the provider stores:

- `preparedVtxos`: spendable Arkade VTXOs reserved for a pending buy-in
- `managedVtxos`: the local live table position
- `amountSats`: local table balance currently represented by `managedVtxos`
- `vtxoExpiry`: minimum expiry across the managed position
- `checkpoint`: latest recorded checkpoint state
- `cashoutTxid`
- `emergencyExitTxid`

The local table-funds state is persisted at:

- `<daemonDir>/<profile>.table-funds.json`

### `prepareBuyIn`

The provider:

1. loads spendable Arkade VTXOs
2. deterministically selects enough spendable VTXOs to cover the requested amount
3. stores them in `preparedVtxos`
4. sets `vtxoExpiry` to the earliest selected expiry
5. returns a signed `TableFundsOperation` with:
   - `kind: "buy-in-prepared"`
   - `status: "prepared"`
   - `amountSats`
   - `vtxoExpiry`

### `confirmBuyIn`

The provider:

1. verifies the `buy-in-prepared` receipt signature
2. verifies the reserved VTXO set still covers the prepared amount
3. rebalances those VTXOs into an exact local table position
4. moves the position into `managedVtxos`
5. returns a signed `TableFundsOperation` with:
   - `kind: "buy-in-locked"`
   - `status: "locked"`
   - the live `vtxoExpiry`

The resulting locked receipt is the receipt the host accepts into canonical state.

### `recordCheckpoint`

This is the bridge between cooperative snapshots and Arkade state.

When a fully signed settlement-boundary snapshot is committed, every seated local daemon:

1. computes `checkpointHash = snapshotHash(snapshot)`
2. compares new snapshot balances to the previously recorded balances, or to initial buy-ins if no checkpoint exists yet
3. derives a deterministic transfer plan sorted by player ID
4. if the local player is a loser at this checkpoint:
   - consumes the full current `managedVtxos` set as inputs to an Arkade offchain transfer
   - emits one output to the winner `arkAddress`
   - emits one local change output for the loser's new balance
   - records the returned Arkade transaction ID on the completed transfer entries
5. if the local player is a winner:
   - waits for incoming spendable Arkade VTXOs from the losers' checkpoint transfers
   - accepts both `settled` and `preconfirmed` incoming VTXOs as valid managed position material
   - merges those VTXOs into `managedVtxos`
6. stores the checkpoint record locally with:
   - `checkpointHash`
   - `balances`
   - `participants`
   - transfer list and status

The local recorded checkpoint must match the latest cooperative snapshot hash before cash-out or emergency exit is allowed.

### `renewTablePositions`

`renewTablePositions(tableId)` rebalances the current local table position to the same amount, refreshing the managed VTXO set and `vtxoExpiry`. It returns a signed `renewal` receipt.

Renewals are local settlement maintenance and are not canonical table events.

### `cooperativeCashOut`

`cashOut()` is only allowed when:

- `latestFullySignedSnapshot` exists
- the local provider has recorded the same `checkpointHash`
- the requested amount equals the locally recorded Arkade table balance

If the exact local table position is already present as spendable Arkade VTXOs matching the checkpoint balance, the provider completes cash-out by clearing the table association locally and returns a signed `cashout` receipt.

If the balance is zero, the provider clears local table state and returns a zero-valued completed receipt without calling Arkade settlement.

If the managed position is not already spendable, the provider falls back to Arkade settlement to the wallet boarding address and retries known transient operator errors before failing.

### `emergencyExit`

`emergencyExit()` uses the same checkpoint hash and balance source as `cashOut()`: the latest fully signed cooperative snapshot.

The provider:

1. verifies the local recorded checkpoint matches `lastCheckpointHash`
2. if the exact managed position is already present as spendable Arkade VTXOs matching `amountSats`, clears the table association locally and returns a signed `emergency-exit` receipt
3. otherwise attempts Arkade settlement to the wallet boarding address
4. retries Arkade settle on known transient errors
5. otherwise falls back to Arkade VTXO recovery only if the SDK exposes `VtxoManager`

If the balance is zero, the provider clears local state and returns a zero-valued `emergency-exit` receipt.

### Money-safety rule

No bet, raise, fold, or street transition directly changes Arkade custody. Only:

- buy-in lock
- checkpoint recording
- renewal
- cooperative cash-out
- emergency exit

touch Arkade state.

The enforceable money boundary is therefore the latest fully signed cooperative settlement snapshot.

### Advisory `HandResult.checkpointHash`

`HandResult` may carry a `checkpointHash`, but it is advisory only. The runtime settles from the hash of the latest fully signed settlement-boundary snapshot created after `HandResult` or rollback, not from the `HandResult` field itself.

## Host Lease, Heartbeats, and Failover

### Timing constants

Current runtime constants are:

- heartbeat interval: `1000ms`
- lease duration: `4000ms`
- host-failure timeout: `3500ms`
- auto-next-hand delay: `1000ms`

### Lease format and quorum

`HostLease` contains:

- `tableId`
- `epoch`
- `hostPeerId`
- `witnessSet`
- `leaseStart`
- `leaseExpiry`
- `signatures`

The unsigned lease body is signed by:

- the host as `signerRole: "host"`
- every configured witness as `signerRole: "witness"`

`witnessSet` excludes the host peer ID. Lease quorum is exact: the host plus every peer ID listed in `witnessSet` must sign.

### Host failover quorum

Failover acceptance quorum is the exact set:

- every configured witness
- every currently seated player

The previous host is not part of the acceptance quorum.

### Failover trigger

Only a witness initiates failover in the current runtime:

- automatically after missing signed heartbeats for more than `3500ms`
- manually via `rotateHost()` when called on a witness daemon

Manual rotation from the current host is rejected so the witness quorum remains mandatory.

### Failover sequence

The witness proposer:

1. appends `HostFailoverProposed`
2. creates and appends its own `HostFailoverAccepted`
3. requests signed failover acceptances from every other peer in the failover quorum
4. verifies every returned acceptance cryptographically
5. requires signatures from every exact quorum peer ID
6. builds a new host lease for `epoch + 1`
7. requires the new host lease to be signed by the new host plus every configured witness
8. appends `HostRotated`

`HostRotated` is always the first event in the new epoch (`seq = 1`) and keeps `prevEventHash` chained to the last accepted event of the previous epoch.

### Mid-hand failover

If failover occurs while:

- `publicState.phase` is non-null
- `publicState.phase !== "settled"`
- `latestFullySignedSnapshot` exists

then the new host:

1. appends `HandAbort`
2. rolls public state back to `latestFullySignedSnapshot`
3. clears the active hand
4. emits a new settlement-boundary snapshot over the rolled-back state

The new host does not auto-start another hand until rollback is complete.

### Between-hand failover

If failover happens at a clean settlement boundary, the new host rotates, installs the new lease, and schedules the next hand.

## Public Discovery Surface

Public discovery is intentionally non-canonical.

- Public tables are represented by signed `SignedTableAdvertisement` objects.
- If a host marks a table `public`, the daemon can send the advertisement plus derived `PublicTableUpdate` records to the optional indexer over HTTP.
- The daemon can also broadcast `public-ad` frames to known peers, and it can query the indexer for `meshPublicTables()`.
- None of those public discovery or read-model paths define canonical money state.

The current daemon publishes these public update types to the indexer:

- `PublicTableSnapshot`
- `PublicHandUpdate`
- `PublicShowdownReveal`

Those updates are derived from canonical daemon state. They are informative, not authoritative.

## Persistence and Replay

Per daemon, the mesh runtime persists:

- `tables/<tableId>/events.ndjson`
- `tables/<tableId>/snapshots.ndjson`
- `tables/<tableId>/private-state.json`
- `public-ads.json`

The profile store also persists:

- peer, protocol, and wallet keys
- known peers
- mesh table references including table config, host URL, epoch, and local role
- current selected mesh table

On startup, the runtime:

1. loads persisted table references
2. loads events, snapshots, and private state
3. replays events in stored order into a fresh context
4. reconstructs `currentEpoch`, `lastEventHash`, `publicState`, `witnessSet`, and buy-in receipts
5. restores `latestFullySignedSnapshot` by scanning persisted snapshots for the newest settlement-boundary snapshot with the full required signer set

Because replay uses the same canonical event hashing, signature verification, and semantic rules, it is deterministic across peers that hold the same canonical event history.

## Reserved or Non-Canonical Surface

The shared schema still defines several variants that are not part of the current canonical runtime path, including:

- `TableWithdraw`
- `SeatProposal`
- `BuyInRequested`
- `HostHeartbeat` event bodies
- `PublicTableSnapshot`, `PublicHandUpdate`, and `PublicShowdownReveal` as canonical events
- dealer modes other than `host-dealer-v1`

The repository also contains a separate mock settlement provider for isolated development. It is not part of the Arkade-backed protocol described here and is not used by the regtest integration harness.
