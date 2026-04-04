# Money Flows Deep Dive

This document describes how money moves in Parker.

For card confidentiality and transcript flow, see [dealerless.md](./dealerless.md). For the wire surface, see [protocol.md](./protocol.md). For trust boundaries, see [trust-model.md](./trust-model.md).

## Short Version

Parker treats Ark-backed table custody as the monetary source of truth.

The key rules are:

- `LatestCustodyState` is the authoritative money checkpoint
- `LatestSnapshot` and `LatestFullySignedSnapshot` are derived replay/debug projections
- seat lock, blind posting, betting actions, timeout successors, settled payouts, cash-out, and emergency exit are custody transitions
- accepted action and funds history replays from canonical signed request objects, not host-authored summaries or `ActionLog`
- accepted Ark-settled custody history replays from the stored settlement witness bundle in `CustodyProof`, not from live Ark/indexer lookups
- deterministic contested-pot recovery uses pre-signed CSV recovery bundles over the shared pot exit
- accepted timeout/showdown history can therefore replay either from `SettlementWitness`, from stored `RecoveryBundles` plus an executed `RecoveryWitness`, or from `ChallengeBundle` plus `ChallengeWitness` for the chain-challenge path
- in the heads-up runtime, once a custody-backed betting or payout step is accepted, the losing player cannot later cash out or emergency-exit a larger pre-loss claim
- zero-exposure successors like `check` can advance custody through a non-settlement transition that reuses the same refs
- `meshRenew` is not a money-moving primitive; continuing play means carrying forward the latest stack claims
- local table-funds state is `arkade-table-funds/v1`, which records custody state hashes, Ark ids, and VTXO refs instead of local-only receipts
- `walletSummary()` presents Ark wallet funds plus locally recorded custody-backed table-funds buckets as separate totals
- second-based challenge escapes validate from accepted timestamps, while block-based challenge escapes validate from exact open and escape confirmation heights plus the live chain tip
- accepted table state does not carry live tip height or transaction confirmation heights for challenge escape replay

Mock-settlement mode synthesizes Ark ids for tests, but the runtime model and checkpoints are custody-first either way.

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
- `CustodySettlementWitness`
- `CustodyRecoveryBundle`
- `CustodyRecoveryWitness`
- `CustodyChallengeBundle`
- `CustodyChallengeWitness`
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

The proof surface depends on how the transition finalized:

- ordinary real Ark completion commits to `CustodyProof.SettlementWitness`
- deterministic recovery-ready source transitions store `CustodyProof.RecoveryBundles`
- executed fallback `timeout` or `showdown-payout` transitions commit to `CustodyProof.RecoveryWitness`
- chain-challenge open, resolution, timeout, and escape transitions commit to `CustodyProof.ChallengeBundle` and `CustodyProof.ChallengeWitness`

### 3. Derived projections

These are not money authority:

- `PublicState`
- `LatestSnapshot`
- `LatestFullySignedSnapshot`
- local table-funds entries

They exist for replay, UI, and operator/debug workflows. `arkade-table-funds/v1` is a derived local receipt ledger written after custody-backed operations succeed. Cash-out, exit, renew/carry-forward, availability, and historical validation all key off custody state first.

## Join And Buy-In Lock

`JoinTable(inviteCode, buyInSats)` builds a funded buy-in bundle from real wallet refs before the host can accept the seat.

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

In other words, buy-in lock is the first custody checkpoint for the table, not just a local overlay convention.

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

For accepted historical replay, real Ark-settled successors validate from the stored `CustodyProof.SettlementWitness` bundle:

- `proofPsbt`
- finalized `commitmentTx`
- stored batch expiry
- finalized `vtxoTree`
- optional `connectorTree`

That witness bundle is authoritative replay proof. The duplicated top-level summary fields (`arkIntentId`, `arkTxid`, `finalizedAt`, `vtxoRefs`) remain as convenience summaries and must stay consistent with the witness-derived result.

### Zero-exposure successors

Not every accepted custody step needs a fresh Ark batch.

For actions like `check`, or for timeout auto-check when no stack or pot claims change, Parker advances the custody chain without forcing a new Ark batch. In that case the runtime:

1. carries forward the latest accepted custody refs
2. updates the custody binding fields such as transcript root, decision index, acting player, and legal-actions hash
3. finalizes a non-settlement custody transition with approvals and replay validation, but without an Ark spend bundle

Validation scope: `action`, `timeout`, and `blind-post` successors exact-match the locally derived custody bindings, including `ActionDeadlineAt`, `ChallengeAnchor`, and `TranscriptRoot`. That deadline derivation uses the accepted table timing config, not the local daemon's settlement mode. The same strict binding-field equality is also enforced for `cash-out`, `emergency-exit`, and the other host-derived non-action successors such as `buy-in-lock`, `showdown-payout`, and `carry-forward`.

### Timeout successors

Action deadlines are carried in custody state, not only in host-local timers.

Timeout validation is also local-derivation-first:

- the runtime rejects timeout successors before the accepted custody deadline
- it derives auto-check vs auto-fold from the accepted timeout policy and the current legal actions
- it rejects any timeout successor whose supplied resolution disagrees with that local derivation

Timeout behavior:

- if `check` is legal, timeout can auto-check
- otherwise timeout auto-folds
- reveal/private-delivery/showdown timeout makes the missing player dead for contested pots while refunding unmatched uncontested chips
- only deterministic money-resolving timeout states get stored recovery bundles; in v1 that means action auto-fold, `showdown-reveal` timeout, or settled `showdown-payout`
- those bundles become executable only after the unilateral exit delay `U`

Timeout-driven successors may exclude the dead player from the approval set for the successor that resolves the hand.

For deterministic contested pots, the source accepted transition stores the fully signed recovery PSBT before it is considered complete. If later live timeout finalization cannot complete, the host can wait for `U`, execute that stored PSBT over the shared pot CSV exit, and append the same semantic `timeout` transition with `RecoveryWitness` instead of `SettlementWitness`.

That execution uses the ordinary unilateral-exit broadcaster. In practice the recovering daemon therefore needs the same small on-chain fee-bump reserve that Parker already assumes for unilateral exits; the bundle removes the need for fresh cooperative signatures, not the need to relay the recovery package.

### Turn challenge fallback

When the table uses `turnTimeoutMode = "chain-challenge"`, the same accepted turn menu also carries a `ChallengeEnvelope`.

That envelope contains four fully signed onchain PSBT families:

- one `turn-challenge-open` bundle that spends every live stack ref and pot ref through the predeclared `D` locktime leaf and reissues the full live bankroll into one `TurnChallengeRef`
- one option-resolution bundle per finite turn option, each spending `TurnChallengeRef` through its cooperative player-only leaf
- one timeout-resolution bundle that also spends `TurnChallengeRef` through the cooperative player-only leaf, but with transaction locktime `D + C`
- one escape bundle that spends `TurnChallengeRef` through its CSV exit leaf

The money model changes shape but not ownership:

- before `turn-challenge-open`, money is distributed across the ordinary stack refs and pot refs
- after `turn-challenge-open`, the full live bankroll is represented by one `TurnChallengeRef`
- after an option-resolution, timeout-resolution, or escape spend, money returns to the ordinary stack/pot layout described by the accepted successor transition

Challenge escape maturity depends on the CSV type:

- second-based CSV uses `PendingTurnChallenge.escapeEligibleAt` and replay compares `ChallengeWitness.executedAt` against that accepted timestamp
- block-based CSV leaves `PendingTurnChallenge.escapeEligibleAt` empty and computes readiness from the accepted open transaction height plus the CSV block delay
- local escape readiness queries the accepted open txid through the wallet explorer surface, requires that transaction to be confirmed, computes `eligibleHeight = openConfirmedHeight + csvBlocks`, and requires the live chain tip to be at least that height
- accepted replay queries chain status for both the open txid and the escape txid and requires the escape confirmation height to be at least `eligibleHeight`
- if Parker cannot verify those heights, local escape resolution and accepted replay both fail closed

### Settled payouts

`internal/game` is side-pot aware and produces N-player-capable contribution structures even though runtime table creation rejects `seatCount > 2`.

The custody layer carries:

- per-player stack claims
- deterministic side-pot slices
- eligibility sets
- odd-chip ordering

If the latest custody state already matches the settled public money state, Parker reuses that state as the final monetary checkpoint. Otherwise it finalizes an explicit `showdown-payout` custody transition.

When showdown payout is already objective but the live cooperative path fails, the same fallback exists: the stored recovery bundle can resolve the contested pot after `U` into the exact winner-owned stack refs the cooperative payout would have produced.

## Cash-Out, Exit, And Continue

### Cash-out and emergency exit

`meshCashOut` and `meshExit` read from `LatestCustodyState`, not from `LatestFullySignedSnapshot`.

For both flows, Parker first validates the canonical signed `nativeFundsRequest` against the latest accepted custody state, derives the expected successor locally, and only then finalizes a `cash-out` or `emergency-exit` custody transition. After that succeeds, it appends:

- a canonical `CashOut` or `EmergencyExit` event containing the full signed `nativeFundsRequest`
- a derived local `arkade-table-funds/v1` receipt for wallet availability, UI, and operator/debug accounting

Operationally, that means:

- both flows spend from the acting player's latest accepted stack claim in `LatestCustodyState`, not from a stale pre-hand or pre-bet balance
- `meshExit` is rejected while a hand is live, so contested pot money cannot be pulled out mid-hand through the unilateral exit path
- emergency-exit requests must carry the exact source refs used by the local exit execution proof

Those `arkade-table-funds/v1` operations carry:

- `stateHash`
- `prevStateHash`
- `custodySeq`
- `arkIntentId`
- `arkTxid`
- `vtxoRefs`
- `exitProofRef`

Emergency exit also has a unilateral wallet path: the wallet runtime can redeem custody refs through their redemption branches and sweep the result into the player wallet when cooperative completion is unavailable.

### Continue playing

`meshRenew` is not a separate money authority step.

Continuing play means:

- keep the latest custody state
- carry its stack claims into the next hand
- treat the response as a carry-forward acknowledgment

There is no separate local renewal receipt that changes monetary truth.

## Wallet Summary Presentation

`walletSummary()` exposes:

- `WalletSpendableSats`
- `TableLockedSats`
- `PendingExitSats`
- `AvailableSats`
- `TotalSats`

Interpretation:

- `WalletSpendableSats` is the Ark wallet's spendable balance
- `TableLockedSats` is value recorded in local `arkade-table-funds/v1` entries with `pending-lock` or `locked` status
- `PendingExitSats` is value recorded in local `arkade-table-funds/v1` entries awaiting exit/cash-out completion
- `AvailableSats` is spendable wallet balance net of table-locked and pending-exit commitments

This is presentation over the Ark wallet balance plus the local custody-backed funds ledger, not a security overlay and not a direct scan of `LatestCustodyState`.

## Runtime Scope

The money model is N-player-capable, but table creation enforces heads-up runtime only:

- `seatCount > 2` is rejected in the dealerless runtime
- side-pot and payout code is already generalized
- a separate multi-player dealing/privacy protocol is required before multi-seat runtime play is enabled

## Practical Reading

The safest way to read the implementation is:

- wallet spendable funds live in Ark
- table money truth lives in `LatestCustodyState`
- snapshots and public state are derived from accepted custody-backed gameplay
- every real exposure change is supposed to fail closed if custody cannot be finalized
- poker-semantic successor validation and Ark/output-shape validation are distinct required checks
- real-mode approval uses live Ark/indexer checks when liveness or spendability matters, but accepted-history replay validates stored settlement witness bundles offline
- deterministic recovery replay validates stored recovery bundles and executed recovery witnesses offline, with no live Ark/indexer lookup
- challenge replay validates stored challenge bundles and executed challenge witnesses offline, with live chain lookups only for the exact block-height checks required by block-based CSV escape
- in the heads-up runtime, once a betting or payout step is accepted into custody history, the other player cannot later cash out or emergency-exit a larger pre-loss claim
- operator or indexer outages affect liveness and visibility, not ownership of the latest accepted custody claim
