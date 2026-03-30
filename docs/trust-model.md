# Trust Model

This document describes the trust model implemented today in this repository.

For the protocol surface, see [protocol.md](./protocol.md). For money movement, see [money-flows.md](./money-flows.md). For architecture, see [architecture.md](./architecture.md).

## Short Version

The current model is:

- wallet, peer, protocol, and transport private keys stay local to each daemon profile
- the browser/controller and indexer remain non-custodial
- monetary truth is the latest accepted `CustodyState`, not replicated UI state
- the host is a proposer and sequencer, not a unilateral money authority
- operator outage affects liveness and visibility, not ownership of the latest accepted custody claim

## Money Authority

The core trust boundary change in the current runtime is:

- `LatestCustodyState` is authoritative
- `LatestSnapshot`, `LatestFullySignedSnapshot`, `PublicState`, and local table-funds state are derived

That matters because:

- cash-out and exit read from custody state first
- replay validation rejects tables that diverge from accepted custody history
- failover resumes from the latest custody checkpoint

If a replica has a prettier UI projection but the wrong custody chain, that replica is wrong.

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

But the host is no longer supposed to be the unilateral money sequencer.

The host cannot legitimately advance money state without producing a custody successor that is bound to:

- the previous custody state hash
- the active transcript root
- the current decision index
- the acting player
- timeout policy and deadline
- the derived public money state hash

In practice, that means the host is a proposer of the next valid state, not the sole owner of monetary truth.

## Cooperative Approval And Dead Players

Only seated players with locked funds participate in custody approval.

Witnesses and the indexer do not authorize spend.

Current runtime behavior:

- cooperative money changes collect seated-player approvals
- timeout successors can exclude a dead non-cooperating player when resolving that timeout path
- reveal/private-delivery/showdown timeout refunds uncontested stack instead of gifting it to the surviving player
- remote signers validate the prebuilt custody transition and authorized output set before signing PSBT or tree-signing requests

This keeps liveness from depending on continued cooperation by a player who has already lost eligibility in the contested portion of the hand.

## Derived State And Replay Guarantees

Peers do not blindly persist host-pushed tables.

Accepted state is checked against:

- signed transport/auth envelopes
- hand transcript replay
- public-state replay
- historical event continuity
- historical snapshot continuity
- historical custody continuity

In real-settlement mode those checks also include live Ark/indexer validation of accepted custody refs, including tapscript-to-output binding for any declared taproot custody refs. The current implementation validates against live services rather than carrying a fully self-contained offline inclusion-proof bundle.

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
- replay or approval of new Ark-settled successors may stall because live verification is part of acceptance

Again, this is a liveness issue. The accepted custody checkpoint remains the reference claim.

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
- treat live Ark/indexer verification as part of current acceptance and signing safety
- treat controller and indexer outages as liveness failures, not custody failures
