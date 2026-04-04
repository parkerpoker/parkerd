# Trust Model

This document describes the trust model implemented today in this repository.

For the protocol surface, see [protocol.md](./protocol.md). For money movement, see [money-flows.md](./money-flows.md). For architecture, see [architecture.md](./architecture.md).

## Short Version

The current model is:

- wallet, peer, protocol, and transport private keys stay local to each daemon profile
- the browser/controller and indexer remain non-custodial
- monetary truth is the latest accepted `CustodyState`, not replicated UI state
- the host is a proposer and sequencer, not a unilateral money authority
- deterministic contested-pot recovery uses pre-signed recovery bundles over the shared pot CSV exit
- turn timeouts now default to a `chain-challenge` fallback instead of immediate timeout payout on new tables
- in the current heads-up runtime, once a custody-backed betting or payout step is accepted, the counterparty should not be able to claw that accepted result back through a later cash-out or exit
- operator outage affects liveness and visibility, not ownership of the latest accepted custody claim
- the accepted v1 liveness tradeoff is eventual deterministic recovery after `U`, not immediate forced recovery at `D`

## Money Authority

The core trust boundary change in the current runtime is:

- `LatestCustodyState` is authoritative
- `LatestSnapshot`, `LatestFullySignedSnapshot`, `PublicState`, and local table-funds state are derived

That matters because:

- cash-out and exit read from custody state first
- replay validation rejects tables that diverge from accepted custody history
- failover resumes from the latest custody checkpoint

If a replica has a prettier UI projection but the wrong custody chain, that replica is wrong.

That accepted chain can prove history through two offline proof surfaces:

- `SettlementWitness` for ordinary real Ark batches
- stored `RecoveryBundles` plus executed `RecoveryWitness` for deterministic recovery transitions
- stored `ChallengeBundle` plus executed `ChallengeWitness` for `turn-challenge-open` and challenge-resolved turn transitions

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

Compromising a player's local wallet key still compromises that player's funds.

## Host Authority

The host still has real responsibility for:

- join acceptance
- action ordering
- transcript progression
- liveness timers
- replication
- failover sequencing

The host is not the unilateral money sequencer.

For user-initiated transitions, honest replicas do not trust a host-authored summary of intent. They derive the expected successor locally from:

- the latest accepted custody state
- the signed `nativeActionRequest` or `nativeFundsRequest`

Current runtime guarantee:

- `action` successors now exact-match the locally derived next custody state, including `ActionDeadlineAt`, `ChallengeAnchor`, `TranscriptRoot`, and the derived public money state hash, using the latest accepted custody state plus the signed `nativeActionRequest`
- `timeout`, `blind-post`, `cash-out`, `emergency-exit`, and the other host-derived non-action successors such as `buy-in-lock`, `showdown-payout`, and `carry-forward` also exact-match those locally derived custody bindings
- deadline derivation uses the accepted table timing profile (`actionTimeoutMs`, `handProtocolTimeoutMs`, `nextHandDelayMs`) instead of the local daemon's current mock/real mode, so accepted replay stays stable across local settlement-mode changes

In practice, that means the host is a proposer of the next valid state, not the sole owner of monetary truth.

## Chain-Challenge Timing Model

The new turn-timeout fallback changes the trust story for betting turns.

For new tables:

- the ordinary fast path is still the same deterministic finite-menu selection plus cooperative Ark settlement
- after the ordinary action deadline `D`, the fallback is no longer an immediate timeout payout
- instead, Parker can open a pre-signed onchain `turn-challenge-open` spend into a dedicated `TurnChallengeRef`

Important current guarantees and non-guarantees:

- `chain-challenge` removes Bob's immediate timeout-payout path at `D`
- before `D + C`, only the option-resolution bundles are valid
- at `D + C`, the timeout-resolution bundle also becomes valid without requiring fresh cooperation from the non-acting side
- after `D + C`, a late option-resolution and the timeout-resolution can both be valid; confirmation order decides if both are published then
- the current design does not prove from chain data alone that Alice selected her option before `D`

That last point is intentional in the current `best possible now` version. Candidate intent acks and any operator acceptance receipt still matter for the ordinary cooperative Ark path, but they are no longer the thing suppressing timeout or proving timely turn selection under the challenge fallback. Those fairness properties now come only from the pre-signed `D` and `D + C` chain-challenge envelope.

## Cooperative Approval And Dead Players

Only seated players with locked funds participate in custody approval.

Witnesses and the indexer do not authorize spend.

Current runtime behavior:

- cooperative money changes collect seated-player approvals
- timeout successors can exclude a dead non-cooperating player when resolving that timeout path
- reveal/private-delivery/showdown timeout refunds uncontested stack instead of gifting it to the surviving player
- remote signers validate the prebuilt custody transition semantically before approval, PSBT signing, or signer-session prepare
- Ark/output authorization and Ark-proof validation remain a separate mandatory layer after semantic validation
- deterministic action-timeout and showdown-timeout bundles are pre-signed while the source transition is still cooperative, then executed later only if the live path stalls
- challenge-open, option-resolution, and timeout-resolution bundles are fully signed onchain transactions and do not depend on live Ark intent registration

This keeps liveness from depending on continued cooperation by a player who has already lost eligibility in the contested portion of the hand.

## Heads-Up Counterparty Guarantee

In the current `seatCount <= 2` runtime, the practical money guarantee against the other player is:

- once a betting or payout step has finalized as an accepted custody transition, later `cash-out` and `emergency-exit` requests are evaluated against that resulting custody state, not against an older pre-loss balance
- `cash-out` and `emergency-exit` derive from the acting player's latest accepted stack claim, not from snapshots or stale local overlays
- `emergency-exit` is rejected while a hand is still live, so a player cannot pull contested chips out of an in-progress hand and then claim them unilaterally

This is not a blanket "trustless under every failure" claim:

- host, operator, controller, or indexer outages can still stall progress
- compromising a local wallet or protocol key still compromises that player's own funds

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

`ReplayValidated` remains telemetry/debug metadata only. It is not treated as proof on its own.

This means a malicious or stale proposer can still waste time or withhold progress, but it should not be able to silently rewrite accepted money history without being rejected by honest peers.

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
- the new host resumes from the latest accepted custody checkpoint

### Host loss mid-hand

If the host disappears mid-hand:

- failover first attempts to sync the latest accepted table from known participants
- the successor resumes from the latest accepted custody state
- if timeout logic resolves the hand, the missing player can become dead for the contested pots without losing uncontested stack

### Operator or indexer outage

If the operator, controller, or indexer is down:

- discovery or UI surfaces may degrade
- new transitions may stall
- wallet-side cash-out completion may stall

But that is a liveness problem. It does not change who owns the latest accepted custody claim.

### Ark service outage

If Ark-backed services are unavailable:

- new custody transitions may fail closed
- onboarding/offboarding may stall
- exit completion may stall
- live spendability checks such as join funding admission can still stall

Accepted historical replay of already-settled Ark custody steps can still succeed from the stored witness material even while Ark services are unavailable. Deterministic contested pots can also recover after `U` if the required recovery bundle was already stored on the source transition. Again, the remaining failures are liveness issues, not ownership changes.

Remaining v1 stall cases are explicit:

- auto-check states do not get winner-take-all recovery bundles
- non-deterministic missing-card situations can still fail closed
- the daemon broadcasting a stored recovery bundle still needs enough ordinary unilateral-exit fee-bump liquidity to relay the recovery package after `U`
- personal wallet exit/cash-out still depends on the ordinary stack-owned path after the pot has already been resolved

## Important Current Limits

The repo is stronger than the old local-overlay model, but it still has implementation limits:

- mock settlement mode is still used in tests and can synthesize Ark ids
- the dealerless runtime still rejects `seatCount > 2`
- multi-player money logic exists, but multi-player dealing/privacy runtime does not yet

So the right reading is:

- stronger money authority than before
- better failover safety than snapshot-led rollback
- still not a finished multi-player production protocol

## Practical Reading

The safest way to interpret the current system is:

- trust each daemon to protect its own secrets
- trust the host to propose and sequence, not to define money alone
- trust custody replay more than snapshots or UI projections
- treat stored settlement witnesses and stored recovery witnesses as the proof for accepted history, and live Ark/indexer checks as a current liveness/spendability safety layer
- treat controller and indexer outages as liveness failures, not custody failures
