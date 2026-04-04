# Current Protocol Surface

This document describes the protocol surface implemented today in this repository.

For component topology, see [architecture.md](./architecture.md). For dealerless transcript flow, see [dealerless.md](./dealerless.md). For money movement, see [money-flows.md](./money-flows.md). For trust assumptions, see [trust-model.md](./trust-model.md).

## Short Version

The current runtime is `poker/v1`.

The important protocol properties are:

- money authority is the latest accepted `CustodyState`
- local funds bookkeeping is `arkade-table-funds/v1`
- join requests include funded buy-in refs
- action and funds requests are bound to `prevCustodyStateHash`
- approval/request hashing strips recovery bundles and recovery witnesses until the final accepted proof is assembled
- accepted `PlayerAction`, `CashOut`, and `EmergencyExit` events carry the full signed initiator request as the canonical payload
- host sequencing is proposer-only; action and result events are appended only after custody finalization succeeds
- deterministic contested-pot recovery uses fully signed CSV recovery bundles over the existing shared pot exit leaf
- accepted custody history can prove either a live Ark batch path or a stored recovery-bundle path
- in the current heads-up runtime, accepted betting and payout steps become the new cash-out and exit baseline; later funds requests are evaluated against the latest custody state, not a pre-loss balance
- the game engine is N-player-capable for money logic, but runtime table creation is still capped at 2 seats

## Recovery Semantics

The deterministic recovery path is deliberately narrower than the normal cooperative batch path.

Bundles are only stored when the current accepted transition leaves contested pot refs and the next money result is objective:

- action timeout that must auto-fold
- `showdown-reveal` timeout that kills the missing player for the contested pot
- settled `showdown-payout` timeout

They are not stored for auto-check states, because those states do not yet determine a winner-take-all money result. Earlier protocol-timeout phases such as `private-delivery` still fail closed in v1 unless or until the runtime reaches one of the objectively money-resolving states above.

Each stored bundle carries:

- semantic successor metadata
- source pot refs
- a fully signed PSBT using the pot's shared CSV leaf
- the exact authorized outputs
- the earliest execution time after the unilateral delay `U`

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
- `meshOpenTurnChallenge`
- `meshResolveTurnChallenge`
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

For user-initiated transitions, `table.custody.request`, `table.custody.sign.request`, and `table.custody.signer.prepare.request` carry a transition authorizer object with the full signed `nativeActionRequest` or `nativeFundsRequest`. Later signer start/nonces/aggregated-nonces steps continue from the already-validated transition hash and stored signer session.

## Authoritative Table State

`nativeTableState` carries both the derived UI/debug views and the custody history:

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

`table.join.request` includes:

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

`table.action.request` includes:

- action payload
- current `challengeAnchor`
- `handId`
- `epoch`
- `decisionIndex`
- `prevCustodyStateHash`
- current `transcriptRoot`
- wallet signature over the action plus that custody base hash

The host rejects the action if the request is not anchored to the latest custody checkpoint.
It also rejects the action if the signed transcript bindings do not match the locally accepted table state.

It also rejects any proposed custody successor unless the daemon can derive the same semantic successor locally from:

- the latest accepted local table state
- the signed `nativeActionRequest`

For an accepted action, the host:

1. validates the current turn against transcript/game state
2. applies Hold'em rules
3. rebuilds the exact next custody successor, including deadline and transcript bindings
4. collects required approvals
5. finalizes custody
6. appends `PlayerAction`

`PlayerAction` therefore reflects an already-finalized custody step, and its canonical initiator payload is the full signed `nativeActionRequest`, not a host-authored summary.

This also covers zero-exposure successors such as `check` or timeout auto-check. Those still advance `custodySeq`, but if the successor reuses the same refs and needs no Ark spend inputs, the runtime finalizes a non-settlement custody transition instead of forcing a batch.

Custody timing is protocol-configured. Table config carries `actionTimeoutMs`, `handProtocolTimeoutMs`, and `nextHandDelayMs`, and semantic replay uses those accepted table values instead of the local daemon's current mock-vs-real settlement mode.

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
- `actionTimeoutMs`
- `handProtocolTimeoutMs`
- `nextHandDelayMs`
- `actionDeadlineAt`
- `challengeAnchor`
- stack claims
- structural side-pot slices

Transition kinds currently used by the runtime include:

- `buy-in-lock`
- `blind-post`
- `action`
- `timeout`
- `turn-challenge-open`
- `showdown-payout`
- `cash-out`
- `emergency-exit`

User-initiated transition validation is explicitly layered:

1. semantic successor validation
2. Ark/output authorization and proof validation

Semantic successor validation derives the expected next custody step locally from the last accepted state plus the relevant signed initiator request:

- `action` uses the embedded `nativeActionRequest` and local `game.ApplyHoldemAction(...)`
- `timeout` derives the timeout resolution locally from the prior custody deadline and timeout policy
- `cash-out` and `emergency-exit` use the embedded `nativeFundsRequest`
- `blind-post`, `showdown-payout`, and `carry-forward` are rebuilt locally with no host-authored semantic input

Ark/output-shape validation is a separate mandatory layer. In real-settlement mode, peers still verify Ark-linked refs, authorized output sets, and proof material even after the semantic successor matches.

Accepted history can replay two proof surfaces offline.

The first is the ordinary real Ark batch path through `CustodyProof.SettlementWitness`. That witness bundle includes:

- `arkIntentId`
- `arkTxid`
- `finalizedAt`
- `proofPsbt`
- `commitmentTx`
- `batchExpiryType`
- `batchExpiryValue`
- `vtxoTree`
- optional `connectorTree`

Accepted replay re-derives the authorized spend paths and batch outputs from the previous custody state plus the accepted transition, validates the witness bundle offline, and exact-matches the witness-derived refs against `NextState` and `Proof.VTXORefs`.

The second is the deterministic recovery path:

- the source accepted transition stores one or more `CustodyProof.RecoveryBundles`
- the executed `timeout` or `showdown-payout` transition carries `CustodyProof.RecoveryWitness`
- replay validates the stored signed PSBT, the exact source pot refs, the CSV leaf/sequence, the authorized outputs, and the recovery transaction metadata
- replay then derives the winner-owned stack refs from the PSBT itself and exact-matches them against `NextState` and `Proof.VTXORefs`

Hashing and approval semantics follow the same split:

- `HashCustodyRequest` intentionally strips recovery bundles and recovery witnesses
- once the bundle is attached to the accepted source transition, the final transition hash commits to it
- recovery execution later appends a normal semantic `timeout` or `showdown-payout` transition whose proof commits to the executed `RecoveryWitness`

Live Ark/indexer checks remain in the current protocol only where liveness or spendability matters, such as join funding admission and other interactive safety checks.

## Turn Challenge Fallback

New tables now default `turnTimeoutMode` to `chain-challenge`. Older accepted tables that omit the field are still interpreted as legacy `direct` timeout tables for backward compatibility.

`PendingTurnMenu` now carries two parallel artifacts for the same deterministic finite menu:

- the ordinary prebuilt finite-menu candidate bundles used by the cooperative Ark fast path
- a deterministic `ChallengeEnvelope` containing:
  - one fully signed onchain `turn-challenge-open` bundle at locktime `D`
  - one fully signed option-resolution bundle per menu option
  - one fully signed timeout-resolution bundle at locktime `D + C`

`turn-challenge-open` always consumes the full live turn bankroll:

- all current stack refs
- all current pot refs

It reissues the entire live bankroll into one dedicated `TurnChallengeRef`. The challenge path is intentionally a full reissuance path, not a delta path.

While `PendingTurnChallenge` exists:

- the hand decision index, acting player, and legal finite menu stay fixed
- the logical balances remain the same
- the custody source for fallback resolution is `PendingTurnChallenge.challengeRef`, not the per-stack or per-pot refs in the pre-open state
- ordinary `meshSendAction` is disabled for that turn

Resolution then splits:

- `meshResolveTurnChallenge` with an option id executes the matching pre-signed option-resolution bundle and appends the ordinary `PlayerAction` event for that option
- `meshResolveTurnChallenge` with `optionId="timeout"` executes the pre-signed timeout-resolution bundle once `timeoutEligibleAt` has passed
- host tick also auto-finalizes the timeout-resolution bundle after `D + C` if no option bundle resolved first

The accepted transition kind after challenge resolution is still `action` or `timeout`. What changes is the proof surface:

- `turn-challenge-open`, challenge-resolved `action`, and challenge-resolved `timeout` transitions carry `CustodyProof.ChallengeBundle` plus `CustodyProof.ChallengeWitness`
- those transitions do not depend on Ark intent registration, `CandidateIntentAck`, or live Ark batch registration

The ordinary cooperative fast path is unchanged. Selected ordinary finite-menu candidates still carry `SignedProofPSBT` and `RegisterMessage`, and Parker still uses Ark intent registration plus the resulting settlement witness when that fast path actually finalizes first.

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
- accepted action and funds replay uses player-signed request objects, not host-authored `ActionLog` summaries
- later `CashOut` and `EmergencyExit` requests are evaluated against that accepted custody result, not against the pre-action or pre-payout balance
- `EmergencyExit` is only available from a settled hand, so it is not a protocol path for pulling contested chips out of a live hand

## Failover And Continuation

The runtime still uses host heartbeat plus protocol deadlines for liveness:

- host heartbeat interval: `1000ms`
- host failure timeout: `3500ms`
- next-hand delay: `1000ms`

Failover resumes from the latest accepted custody state, not from a snapshot overlay.

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
- semantic successor validation and Ark/output validation are distinct required checks
- real-mode peer approvals still use live Ark/indexer checks when current liveness or spendability matters, while accepted-history replay validates stored settlement witness bundles before persistence
- non-host peers replay transcript, public state, snapshot history, and custody history before persistence
