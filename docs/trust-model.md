# Trust Model

This document describes the trust model in this repository.

For the protocol surface, see [protocol.md](./protocol.md). For money movement, see [money-flows.md](./money-flows.md). For architecture, see [architecture.md](./architecture.md).

## Short Version

The model is:

- wallet, peer, protocol, and transport private keys stay local to each daemon profile
- the browser/controller and indexer remain non-custodial
- monetary truth is the latest accepted `CustodyState`, not replicated UI state
- the host is a proposer and sequencer, not a unilateral money authority
- the acting player is the only party that can lock an ordinary turn option through `SelectionAuth`
- public pending-turn replication carries compact menu metadata and, after lock, exactly one selected bundle
- deterministic contested-pot recovery uses pre-signed recovery bundles over the shared pot CSV exit
- before action lock, turn timeouts use a `chain-challenge` fallback instead of immediate timeout payout
- after action lock, recovery uses the replicated selected bundle plus `settlementDeadlineAt`
- in the heads-up runtime, once a custody-backed betting or payout step is accepted, the counterparty does not get a second funds path back to a pre-loss balance
- operator outage affects liveness and visibility, not ownership of the latest accepted custody claim
- deterministic pot recovery still uses eventual execution after `U`, not an immediate forced recovery at `D`

## Money Authority

The core trust boundary is:

- `LatestCustodyState` is authoritative
- `LatestSnapshot`, `LatestFullySignedSnapshot`, `PublicState`, and local table-funds state are derived

That matters because:

- cash-out and exit read from custody state first
- replay validation rejects tables that diverge from accepted custody history
- failover resumes from the latest custody checkpoint

If a replica has a prettier UI projection but the wrong custody chain, that replica is wrong.

That accepted chain can prove history through three offline proof surfaces:

- `SettlementWitness` for ordinary real Ark batches
- stored `RecoveryBundles` plus executed `RecoveryWitness` for deterministic recovery transitions
- stored `ChallengeBundle` plus executed `ChallengeWitness` for `turn-challenge-open`, challenge-resolved turn transitions, and `turn-challenge-escape`

## Keys And Local Secrets

Each daemon profile locally holds:

- wallet private key
- peer identity key
- protocol identity key
- transport private key
- local private hand material

Those secrets do not move to:

- the browser client
- the localhost controller
- the indexer
- witnesses acting only as auditors/replicas

Compromising a player's local wallet key compromises that player's funds.

## Host Authority

The host has responsibility for:

- join acceptance
- action publication ordering
- transcript progression
- liveness timers
- replication
- failover sequencing

The host is not the unilateral money sequencer and it is not the authority that chooses an ordinary betting option.

For user-initiated transitions, honest replicas do not trust a host-authored summary of intent. They derive the expected successor locally from:

- the latest accepted custody state
- the signed `nativeActionRequest` or `nativeFundsRequest`

For ordinary betting turns, the acting player chooses exactly one candidate by signing `SelectionAuth` over:

- table id
- epoch
- hand id
- decision index
- previous custody state hash
- turn anchor hash
- candidate hash
- action deadline

The host validates that signature, locks that exact candidate, persists the selected bundle in replicated pending-turn state, and acknowledges the lock with `ActionLockedAck`. The acting player then settles the locked bundle locally and sends a signed `ActionSettlementRequest` carrying the fully settled transition and witness data. The host persists that exact settled request in pending-turn state until publication. Accepted custody history still advances only after the fully witnessed transition is replay-valid.

Runtime guarantee:

- `action` successors exact-match the locally derived next custody state, including `ActionDeadlineAt`, `ChallengeAnchor`, `TranscriptRoot`, and the derived public money state hash, using the latest accepted custody state plus the signed `nativeActionRequest`
- `timeout`, `blind-post`, `cash-out`, `emergency-exit`, and the other host-derived non-action successors such as `buy-in-lock`, `showdown-payout`, and `carry-forward` also exact-match those locally derived custody bindings
- deadline derivation uses the accepted table timing profile (`actionTimeoutMs`, `handProtocolTimeoutMs`, `nextHandDelayMs`) instead of the local daemon's mock/real settlement mode, so accepted replay stays stable across local settlement-mode changes
- before lock, sibling pre-signed action bundles remain local to the acting player and current host rather than public table replication
- after lock, the selected bundle is the only recoverable ordinary action for that turn
- publication authority moves only through host failover, not through action selection itself

In practice, that means the host is a proposer of the next valid state, not the sole owner of monetary truth.

## Chain-Challenge Timing Model

The chain-challenge fallback changes the trust story for betting turns.

The ordinary turn flow has two stages:

- the acting player chooses a deterministic candidate before the action deadline `D`
- that exact locked candidate is then settled and published as an accepted custody transition

The `chain-challenge` fallback applies only while the turn is still unlocked. If no valid `SelectionAuth` lock exists by `D`, Parker can open a pre-signed onchain `turn-challenge-open` spend into a dedicated `TurnChallengeRef`.

Guarantees and non-guarantees:

- `chain-challenge` removes Bob's immediate timeout-payout path at `D`
- `SelectionAuth` is the only authority that chooses an ordinary action candidate
- `ActionLockedAck` records host acceptance of that exact candidate but does not advance accepted custody history
- once a candidate is locked, recovery uses that locked selected bundle rather than timeout fold/check substitution
- before `D + C`, only the option-resolution bundles are valid
- at `D + C`, the timeout-resolution bundle also becomes valid without requiring fresh cooperation from the non-acting side
- after `D + C`, a late option-resolution and the timeout-resolution can both be valid; confirmation order decides if both are published then
- the design does not prove from chain data alone that Alice selected her option before `D`
- second-based challenge escapes validate maturity from accepted timestamps
- block-based challenge escapes validate maturity from the accepted `turn-challenge-open` transaction height, the live chain tip for local readiness, and either the accepted escape transaction confirmation height or, while that escape remains unconfirmed, the live chain tip plus a visible escape transaction
- accepted table state does not carry chain tip height or transaction confirmation heights
- if Parker cannot verify the required block heights for a block-based challenge escape, local resolution and accepted replay fail closed

Ordinary timeout suppression comes from locked-selection state, not from Ark registration receipts or `CandidateIntentAck`. Under the challenge fallback, onchain sequencing comes from the pre-signed `D` and `D + C` challenge bundles.

## Cooperative Approval And Dead Players

Only seated players with locked funds participate in custody approval.

Witnesses and the indexer do not authorize spend.

Runtime behavior:

- cooperative money changes collect seated-player approvals
- timeout successors can exclude a dead non-cooperating player when resolving that timeout path
- reveal/private-delivery/showdown timeout refunds uncontested stack instead of gifting it to the surviving player
- remote signers validate the prebuilt custody transition semantically before approval, PSBT signing, or signer-session prepare
- Ark/output authorization and Ark-proof validation remain a separate mandatory layer after semantic validation
- deterministic action-timeout and showdown-timeout bundles are pre-signed while the source transition is cooperative, then executed later only if the live path stalls
- challenge-open, option-resolution, and timeout-resolution bundles are fully signed onchain transactions and do not depend on live Ark intent registration
- second-based challenge escapes are validated from accepted timestamps, while block-based challenge escapes are validated from the live open confirmation height plus either the escape confirmation height or a visible unconfirmed escape together with the local chain tip
- Parker does not treat accepted table state as authoritative for chain-height observations; those tip and confirmation-height checks stay local and must be re-verified when needed
- if Parker cannot verify the required block heights for a block-based challenge escape, both local resolution and accepted replay fail closed

This keeps liveness from depending on continued cooperation by a player who has already lost eligibility in the contested portion of the hand.

## Heads-Up Counterparty Guarantee

In the `seatCount <= 2` runtime, the practical money guarantee against the other player is:

- once a betting or payout step has finalized as an accepted custody transition, later `cash-out` and `emergency-exit` requests are evaluated against that resulting custody state, not against an older pre-loss balance
- `cash-out` and `emergency-exit` derive from the acting player's latest accepted stack claim, not from snapshots or stale local overlays
- `emergency-exit` is rejected while a hand is live, so a player cannot pull contested chips out of an in-progress hand and then claim them unilaterally

This is not a blanket "trustless under every failure" claim:

- host, operator, controller, or indexer outages can stall progress
- compromising a local wallet or protocol key compromises that player's own funds

## Derived State And Replay Guarantees

Peers do not blindly persist host-pushed tables.

Accepted state is checked against:

- signed transport/auth envelopes
- hand transcript replay
- public-state replay
- historical event continuity, including embedded initiator signatures and custody anchors
- historical snapshot continuity
- historical custody continuity

In real-settlement mode those checks replay accepted custody transitions from whichever proof surface the history actually used. `SettlementWitness` carries the proof PSBT, finalized commitment transaction, batch expiry, and finalized VTXO tree for the ordinary Ark batch path. `RecoveryWitness` points at a stored signed recovery bundle, source pot refs, and recovery broadcast metadata. If those stored artifacts are intact, accepted history replays without live Ark/indexer availability.

The challenge path has one extra split:

- second-based CSV challenge escapes replay from the accepted timestamps alone
- block-based CSV challenge escapes require live verification of the accepted open tx height and accepted escape tx height, and replay rejects the history if those heights cannot be checked or do not satisfy the CSV delay exactly

`ReplayValidated` remains telemetry/debug metadata only. It is not treated as proof on its own.

This means a malicious or stale proposer can waste time or withhold progress, but it should not be able to silently rewrite accepted money history without being rejected by honest peers.

## Non-Custodial Components

### Local controller

The localhost controller is a local control plane only.

It:

- proxies daemon RPC
- enforces browser-facing origin/header checks
- does not hold custody keys
- does not speak peer transport as an authority

### Browser client

The browser can request local actions through the controller, but it does not hold:

- wallet keys
- protocol keys
- transport keys

### Indexer

The indexer is informational only.

It can affect:

- discovery
- spectatorship
- public visibility

It does not authorize money movement.

## Failure Model

### Host loss between hands

If the host disappears between hands:

- witnesses can take over when configured
- otherwise the eligible seated player fallback can take over
- the successor host resumes from the latest accepted custody checkpoint

### Host loss mid-hand

If the host disappears mid-hand:

- failover first attempts to sync the latest accepted table from known participants
- the successor resumes from the latest accepted custody state and the replicated pending-turn lock state
- if the turn is unlocked, the successor can continue the ordinary lock flow or open `turn-challenge-open` after the action deadline
- if the turn is locked and the acting player already settled, the successor can publish that exact settled transition
- if the turn is locked and the acting player disappears before settlement, the successor can settle the replicated selected bundle after `settlementDeadlineAt`
- if timeout logic resolves the hand, the missing player can become dead for the contested pots without losing uncontested stack

### Operator or indexer outage

If the operator, controller, or indexer is down:

- discovery or UI surfaces may degrade
- transitions may stall
- wallet-side cash-out completion may stall

But that is a liveness problem. It does not change who owns the latest accepted custody claim.

### Ark service outage

If Ark-backed services are unavailable:

- custody transitions may fail closed
- onboarding/offboarding may stall
- exit completion may stall
- live spendability checks such as join funding admission can stall

Accepted historical replay of already-settled Ark custody steps succeeds from the stored witness material even while Ark services are unavailable. Deterministic contested pots also recover after `U` if the required recovery bundle is already stored on the source transition. Again, the remaining failures are liveness issues, not ownership changes.

Stall cases include:

- auto-check states do not get winner-take-all recovery bundles
- non-deterministic missing-card situations fail closed
- the daemon broadcasting a stored recovery bundle needs enough ordinary unilateral-exit fee-bump liquidity to relay the recovery package after `U`
- personal wallet exit/cash-out depends on the ordinary stack-owned path after the pot has already been resolved

## Important Limits

Implementation limits are:

- mock settlement mode is used in tests and can synthesize Ark ids
- the dealerless runtime rejects `seatCount > 2`
- multi-player money logic exists, but multi-player dealing/privacy runtime does not yet

So the right reading is:

- custody-backed money authority
- failover safety from replay-valid checkpoints rather than snapshot-led rollback
- no multi-player production runtime

## Practical Reading

The safest way to interpret the system is:

- trust each daemon to protect its own secrets
- trust the host to propose and sequence, not to define money alone
- trust custody replay more than snapshots or UI projections
- treat stored settlement witnesses and stored recovery witnesses as the proof for accepted history, and live Ark/indexer checks as a liveness/spendability safety layer
- treat controller and indexer outages as liveness failures, not custody failures
