# Current Protocol Surface

This document describes the protocol surface implemented today in this repository.

For component topology, see [architecture.md](./architecture.md). For dealerless transcript flow, see [dealerless.md](./dealerless.md). For money movement, see [money-flows.md](./money-flows.md). For trust assumptions, see [trust-model.md](./trust-model.md).

## Short Version

The current runtime is `poker/v2`.

The important protocol changes in this generation are:

- money authority is the latest accepted `CustodyState`
- local funds bookkeeping is `arkade-table-funds/v2`
- join requests include funded buy-in refs
- action requests are bound to `prevCustodyStateHash`
- host sequencing is proposer-only; action and result events are appended only after custody finalization succeeds
- the game engine is N-player-capable for money logic, but runtime table creation is still capped at 2 seats

## Runtime Surfaces

The implementation is split across four protocol surfaces:

1. local daemon RPC over profile-local Unix sockets
2. optional localhost controller HTTP/SSE
3. peer-to-peer framed transport over `parker://` endpoints
4. optional public indexer ingest/read HTTP

Only the local daemon RPC and peer transport are required for direct table play.

## Local Daemon RPC

The daemon still exposes the existing local RPC methods used by the CLI and controller, including:

- `meshCreateTable`
- `meshTableJoin`
- `meshGetTable`
- `meshSendAction`
- `meshRotateHost`
- `meshCashOut`
- `meshRenew`
- `meshExit`
- `walletSummary`
- `walletOnboard`
- `walletOffboard`

The local transport is still newline-delimited JSON request/response/event envelopes over the profile socket.

`meshRenew` remains on the control surface for compatibility, but the current runtime implements it as a carry-forward acknowledgment over the latest custody state rather than as a separate money-moving protocol step.

## Peer Transport

Peer endpoints are still advertised as:

- `parker://<host>:<port>`

Transport envelopes remain:

- protocol-key signed
- encrypted with X25519 plus AES-256-GCM once the sender knows the recipient transport key
- request/response framed as one NDJSON envelope each way per connection

## Current Peer Message Types

The runtime currently handles:

- `peer.manifest.get`
- `peer.manifest`
- `table.state.pull`
- `table.state.push`
- `table.join.request`
- `table.join.response`
- `table.action.request`
- `table.action.response`
- `table.funds.request`
- `table.funds.response`
- `table.custody.request`
- `table.custody.response`
- `table.custody.sign.request`
- `table.custody.sign.response`
- `table.custody.signer.prepare.request`
- `table.custody.signer.prepare.response`
- `table.custody.signer.start.request`
- `table.custody.signer.start.response`
- `table.custody.signer.nonces.request`
- `table.custody.signer.nonces.response`
- `table.custody.signer.aggregated_nonces.request`
- `table.custody.signer.aggregated_nonces.response`
- `table.hand.request`
- `table.hand.response`
- `ack`
- `nack`

The new pieces in the custody generation are the `table.funds.*`, `table.custody.*`, `table.custody.sign*`, and `table.custody.signer.*` routes plus the tighter coupling between table sync, action acceptance, and custody finalization.

## Authoritative Table State

`nativeTableState` now carries both the derived UI/debug views and the custody history:

- `CustodyTransitions`
- `LatestCustodyState`
- `Events`
- `LatestSnapshot`
- `LatestFullySignedSnapshot`
- `PublicState`

Interpretation:

- `LatestCustodyState` is monetary truth
- `CustodyTransitions` are the accepted money-history chain
- `Events`, `PublicState`, and snapshots are derived projections that must replay against that chain

Peers reject accepted tables that:

- rewrite custody history
- roll back `custodySeq`
- tamper with transcript-bound public state
- rewrite snapshots or events
- break replay between transcript, gameplay state, public state, and custody state

## Join Contract

`table.join.request` now includes:

- `BuyInSats`
- `FundingRefs`
- `FundingTotalSats`
- `WalletPlayerID`
- `WalletPubkeyHex`
- `ArkAddress`
- `Peer`
- `ProtocolID`
- wallet-signed `IdentityBinding`

The host will not accept `SeatLocked` unless:

- identity binding validates
- funding refs are present
- funding total covers the buy-in
- funding refs are not duplicated
- in real-settlement mode, funding refs verify live on Ark and remain acceptable spend inputs

Seat lock is then committed as a `buy-in-lock` custody transition.

## Action Contract

`table.action.request` now includes:

- action payload
- `handId`
- `epoch`
- `decisionIndex`
- `prevCustodyStateHash`
- wallet signature over the action plus that custody base hash

The host rejects the action if the request is not anchored to the latest custody checkpoint.

For an accepted action, the host:

1. validates the current turn against transcript/game state
2. applies Hold'em rules
3. builds the next custody transition
4. collects required approvals
5. finalizes custody
6. appends `PlayerAction`

`PlayerAction` therefore reflects an already-finalized custody step.

This also covers zero-exposure successors such as `check` or timeout auto-check. Those still advance `custodySeq`, but if the successor reuses the same refs and needs no Ark spend inputs, the runtime finalizes a non-settlement custody transition instead of forcing a batch.

## Custody Transition Contract

The custody layer lives in `internal/tablecustody`.

`CustodyState` binds money to game context through:

- `tableId`
- `epoch`
- `handId`
- `handNumber`
- `custodySeq`
- `decisionIndex`
- `prevStateHash`
- `transcriptRoot`
- `publicStateHash`
- `actingPlayerId`
- `legalActionsHash`
- `timeoutPolicy`
- `actionDeadlineAt`
- `challengeAnchor`
- stack claims
- structural side-pot slices

Transition kinds currently used by the runtime include:

- `buy-in-lock`
- `blind-post`
- `action`
- `timeout`
- `showdown-payout`
- `cash-out`
- `emergency-exit`

In real-settlement mode, peer approval and replay validation do more than hash-chain checks. They verify Ark-linked refs live against Ark/indexer state, including amount/script identity and tapscript-to-output binding for any declared taproot custody refs. The current implementation relies on live verification, not a separate offline inclusion-proof bundle.

## Hand And Money Sequencing

The active runtime order is:

1. seat lock custody
2. hand creation and blind-post custody
3. dealerless transcript flow
4. betting actions and timeout successors, each custody-backed
5. settled replay/snapshot derivation
6. payout custody only when the latest custody state does not already reflect the settled public money state
7. `HandResult`

That means:

- transcript-only steps can exist without a money transition
- stake-changing steps fail closed if custody cannot finalize
- `HandResult` is appended after the money checkpoint for that result is established

## Failover And Continuation

The runtime still uses host heartbeat plus protocol deadlines for liveness:

- host heartbeat interval: `1000ms`
- host failure timeout: `3500ms`
- next-hand delay: `1000ms`

But failover now resumes from the latest accepted custody state, not from a snapshot overlay.

Witness or player failover behavior:

- best-effort sync the latest accepted table copy from known participants
- rotate host authority when heartbeat or protocol deadlines require it
- continue from the latest custody checkpoint
- if a player is dead for a timeout successor, exclude that dead player from the successor approval set when appropriate

## Runtime Scope And Limits

The money model and side-pot logic are N-player-capable, but the active dealerless runtime currently enforces:

- `seatCount <= 2`

Tables above 2 seats are rejected until a separate multi-player dealing/privacy protocol exists.

## Practical Reading

The safest way to interpret the current protocol is:

- transport envelopes authenticate and encrypt peer traffic
- custody state, not snapshots, is the money-finality object
- the host proposes transitions and orchestrates replication
- money-changing steps are accepted only after custody finalization
- real-mode peer approvals and replay validate Ark-linked refs against live Ark/indexer state before signing or persistence
- non-host peers replay transcript, public state, snapshot history, and custody history before persistence
