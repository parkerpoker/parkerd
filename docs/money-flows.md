# Money Flows Deep Dive

This document describes how money moves in the current Parker implementation in this repository.

For card confidentiality and transcript flow, see [dealerless.md](./dealerless.md). For the wire surface, see [protocol.md](./protocol.md). For trust boundaries, see [trust-model.md](./trust-model.md).

## Short Version

Parker now treats Ark-backed table custody as the monetary source of truth.

The important current-state rules are:

- `LatestCustodyState` is the authoritative money checkpoint
- `LatestSnapshot` and `LatestFullySignedSnapshot` are derived replay/debug projections
- seat lock, blind posting, betting actions, timeout successors, settled payouts, cash-out, and emergency exit are custody transitions
- accepted action and funds history replays from canonical signed request objects, not host-authored summaries or `ActionLog`
- zero-exposure successors like `check` can still advance custody through a non-settlement transition that reuses the same refs
- `meshRenew` is no longer a money-moving primitive; continuing play means carrying forward the latest stack claims
- local table-funds state is now `arkade-table-funds/v2`, which records custody state hashes, Ark ids, and VTXO refs instead of local-only receipts
- `walletSummary()` presents Ark wallet funds plus locally recorded custody-backed table-funds buckets as separate totals

In mock-settlement mode the runtime still synthesizes Ark ids for tests, but the runtime model and checkpoints are custody-first either way.

## Monetary Layers

### 1. Wallet spendable funds

`internal/wallet/runtime.go` talks to the Ark SDK and exposes:

- Ark spendable balance
- boarding balance
- VTXO listing
- custody intent registration
- off-chain sends
- transaction signing
- signer-session helpers

This is the spendable wallet pool outside a table.

### 2. Table custody

`internal/tablecustody` defines the authoritative money model:

- `CustodyState`
- `StackClaim`
- `PotSlice`
- `CustodyTransition`
- `CustodyProof`
- `TimeoutResolution`

`CustodyState` binds money to gameplay by hashing:

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
- structural side-pot claims

If two replicas disagree about money, `LatestCustodyState` wins and the divergent replica is rejected.

### 3. Derived projections

These are no longer money authority:

- `PublicState`
- `LatestSnapshot`
- `LatestFullySignedSnapshot`
- local table-funds entries

They exist for replay, UI, and operator/debug workflows. `arkade-table-funds/v2` is a derived local receipt ledger written after custody-backed operations succeed. Cash-out, exit, renew/carry-forward, availability, and historical validation all key off custody state first.

## Join And Buy-In Lock

`JoinTable(inviteCode, buyInSats)` now builds a funded buy-in bundle from real wallet refs before the host can accept the seat.

The join payload includes:

- `BuyInSats`
- `FundingRefs`
- `FundingTotalSats`
- `WalletPlayerID`
- `WalletPubkeyHex`
- `ArkAddress`
- `ProtocolID`
- `Peer`
- wallet-signed `IdentityBinding`

The host:

1. validates the identity binding
2. validates funding refs and rejects empty, duplicate, or insufficient funding
3. verifies the funding refs live on Ark in real-settlement mode
4. appends the seat
5. finalizes a `buy-in-lock` custody transition
6. appends `SeatLocked`
7. once the table is full, derives ready state and schedules the hand

In other words, buy-in lock is no longer just a local overlay convention. It is the first custody checkpoint for the table.

## Per-Hand Money Movement

### Hand start

The host starts a hand from the latest custody stack claims, not from a snapshot overlay.

When `CreateHoldemHand(...)` posts blinds, Parker immediately finalizes a `blind-post` custody transition. The hand does not become an accepted table event until that transition succeeds.

If custody finalization fails, the hand fails closed.

### Betting actions

For every stake-changing action, Parker:

1. validates that the signed request references the latest custody base hash
2. applies Hold'em rules in `internal/game`
3. derives the expected semantic successor locally from the latest accepted state plus that signed request
4. builds the next `CustodyTransition`
5. verifies Ark/output authorization separately when the successor needs settlement material
6. collects required approvals
7. finalizes the custody step
8. appends `PlayerAction`

That applies to:

- call
- bet
- raise
- all-in
- fold
- timeout auto-check
- timeout auto-fold

`PlayerAction` is therefore downstream of custody finalization, not the other way around, and its canonical payload is the full signed `nativeActionRequest`.

Those same semantic checks run before:

- custody approval
- PSBT signing
- signer-session prepare

### Zero-exposure successors

Not every accepted custody step needs a fresh Ark batch.

For actions like `check`, or for timeout auto-check when no stack or pot claims change, Parker still advances the custody chain without forcing a new Ark batch. In that case the runtime:

1. carries forward the latest accepted custody refs
2. updates the custody binding fields such as transcript root, decision index, acting player, and legal-actions hash
3. finalizes a non-settlement custody transition with approvals and replay validation, but without an Ark spend bundle

Current validation scope: `action`, `timeout`, and `blind-post` successors exact-match the locally derived custody bindings, including `ActionDeadlineAt`, `ChallengeAnchor`, and `TranscriptRoot`. That deadline derivation uses the accepted table timing config, not the local daemon's current settlement mode. The same strict binding-field equality is also enforced for `cash-out`, `emergency-exit`, and the other host-derived non-action successors such as `buy-in-lock`, `showdown-payout`, and `carry-forward`.

### Timeout successors

Action deadlines are carried in custody state, not only in host-local timers.

Timeout validation is also local-derivation-first:

- the runtime rejects timeout successors before the accepted custody deadline
- it derives auto-check vs auto-fold from the accepted timeout policy and the current legal actions
- it rejects any timeout successor whose supplied resolution disagrees with that local derivation

Current timeout behavior:

- if `check` is legal, timeout can auto-check
- otherwise timeout auto-folds
- reveal/private-delivery/showdown timeout makes the missing player dead for contested pots while refunding unmatched uncontested chips

Timeout-driven successors may exclude the dead player from the approval set for the successor that resolves the hand.

### Settled payouts

`internal/game` is now side-pot aware and produces N-player-capable contribution structures even though runtime table creation still rejects `seatCount > 2`.

The custody layer carries:

- per-player stack claims
- deterministic side-pot slices
- eligibility sets
- odd-chip ordering

If the latest custody state already matches the settled public money state, Parker reuses that state as the final monetary checkpoint. Otherwise it finalizes an explicit `showdown-payout` custody transition.

## Cash-Out, Exit, And Continue

### Cash-out and emergency exit

`meshCashOut` and `meshExit` now read from `LatestCustodyState`, not from `LatestFullySignedSnapshot`.

For both flows, Parker first validates the canonical signed `nativeFundsRequest` against the latest accepted custody state, derives the expected successor locally, and only then finalizes a `cash-out` or `emergency-exit` custody transition. After that succeeds, it appends:

- a canonical `CashOut` or `EmergencyExit` event containing the full signed `nativeFundsRequest`
- a derived local `arkade-table-funds/v2` receipt for wallet availability, UI, and operator/debug accounting

Those `arkade-table-funds/v2` operations carry:

- `stateHash`
- `prevStateHash`
- `custodySeq`
- `arkIntentId`
- `arkTxid`
- `vtxoRefs`
- `exitProofRef`

Emergency exit also has a unilateral wallet path: the wallet runtime can redeem custody refs through their redemption branches and sweep the result into the player wallet when cooperative completion is unavailable.

### Continue playing

`meshRenew` is no longer a separate money authority step.

Continuing play means:

- keep the latest custody state
- carry its stack claims into the next hand
- treat the response as a carry-forward acknowledgment

There is no separate local renewal receipt that changes monetary truth.

## Wallet Summary Presentation

`walletSummary()` now exposes:

- `WalletSpendableSats`
- `TableLockedSats`
- `PendingExitSats`
- `AvailableSats`
- `TotalSats`

Interpretation:

- `WalletSpendableSats` is the Ark wallet's spendable balance
- `TableLockedSats` is value currently recorded in local `arkade-table-funds/v2` entries with `pending-lock` or `locked` status
- `PendingExitSats` is value recorded in local `arkade-table-funds/v2` entries awaiting exit/cash-out completion
- `AvailableSats` is spendable wallet balance net of currently table-locked and pending-exit commitments

This is presentation over the Ark wallet balance plus the local custody-backed funds ledger, not a security overlay and not a direct scan of `LatestCustodyState`.

## Current Runtime Scope

The money model is N-player-capable, but table creation still enforces heads-up runtime only:

- `seatCount > 2` is rejected in the current dealerless runtime
- side-pot and payout code is already generalized
- a separate multi-player dealing/privacy protocol is still required before multi-seat runtime play is enabled

## Practical Reading

The safest way to read the implementation today is:

- wallet spendable funds live in Ark
- table money truth lives in `LatestCustodyState`
- snapshots and public state are derived from accepted custody-backed gameplay
- every real exposure change is supposed to fail closed if custody cannot be finalized
- poker-semantic successor validation and Ark/output-shape validation are distinct required checks
- real-mode approval and replay validate Ark-linked refs live, including tapscript-to-output binding for declared taproot custody refs
- operator or indexer outages affect liveness and visibility, not ownership of the latest accepted custody claim
