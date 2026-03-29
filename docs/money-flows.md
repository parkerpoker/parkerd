# Money Flows Deep Dive

This document describes how money moves in the current Parker implementation in this repository.

For card confidentiality and transcript flow, see [dealerless.md](./dealerless.md). For the wire surface, see [protocol.md](./protocol.md). For trust boundaries, see [trust-model.md](./trust-model.md).

## Short Version

Parker now treats Ark-backed table custody as the monetary source of truth.

The important current-state rules are:

- `LatestCustodyState` is the authoritative money checkpoint
- `LatestSnapshot` and `LatestFullySignedSnapshot` are derived replay/debug projections
- seat lock, blind posting, betting actions, timeout successors, settled payouts, cash-out, and emergency exit are custody transitions
- `meshRenew` is no longer a money-moving primitive; continuing play means carrying forward the latest stack claims
- local table-funds state is now `arkade-table-funds/v2`, which records custody state hashes, Ark ids, and VTXO refs instead of local-only receipts
- `walletSummary()` presents wallet funds plus table-locked custody claims as separate buckets

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

They exist for replay, UI, and operator/debug workflows, but cash-out, exit, renew/carry-forward, availability, and historical validation all now key off custody state first.

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
3. appends the seat
4. finalizes a `buy-in-lock` custody transition
5. appends `SeatLocked`
6. once the table is full, derives ready state and schedules the hand

In other words, buy-in lock is no longer just a local overlay convention. It is the first custody checkpoint for the table.

## Per-Hand Money Movement

### Hand start

The host starts a hand from the latest custody stack claims, not from a snapshot overlay.

When `CreateHoldemHand(...)` posts blinds, Parker immediately finalizes a `blind-post` custody transition. The hand does not become an accepted table event until that transition succeeds.

If custody finalization fails, the hand fails closed.

### Betting actions

For every stake-changing action, Parker:

1. validates that the request references the latest custody base hash
2. applies Hold'em rules in `internal/game`
3. builds the next `CustodyTransition`
4. collects required approvals
5. finalizes the custody step
6. appends `PlayerAction`

That applies to:

- call
- bet
- raise
- all-in
- fold
- timeout auto-check
- timeout auto-fold

`PlayerAction` is therefore downstream of custody finalization, not the other way around.

### Timeout successors

Action deadlines are carried in custody state, not only in host-local timers.

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

`arkade-table-funds/v2` operations carry:

- `stateHash`
- `prevStateHash`
- `custodySeq`
- `arkIntentId`
- `arkTxid`
- `vtxoRefs`
- `exitProofRef`

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
- `TableLockedSats` is value currently locked into accepted table custody claims
- `PendingExitSats` is value reserved for exit/cash-out completion
- `AvailableSats` is spendable wallet balance net of currently table-locked and pending-exit commitments

This is presentation over real custody state plus wallet funds, not a security overlay.

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
- operator or indexer outages affect liveness and visibility, not ownership of the latest accepted custody claim
