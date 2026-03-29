# Dealerless Hand Flow

This document describes the dealerless poker flow implemented today in this repository. It is intentionally current-state only.

For broader system context, see [architecture.md](./architecture.md). For wire surfaces, see [protocol.md](./protocol.md). For trust boundaries, see [trust-model.md](./trust-model.md).

## Summary

Parker currently runs heads-up Hold'em with a coordinator-led, dealerless card protocol named `dealerless-transcript-v1`.

The key distinction is:

- fairness comes from commit/reveal plus replayable transcript validation
- confidentiality comes from layered mental-poker encryption and seat-local decryption

The coordinator orders messages, appends events, replicates state, and drives timeout/failover. It does not get a special randomness role, and it must not persist plaintext non-owned hole cards.

Only the owning seat's daemon decrypts and stores that seat's hole cards in plaintext locally after private delivery.

## Goals

- no daemon should persist plaintext non-owned hole cards
- each player should be able to validate the shuffle and reveal path from a signed transcript
- failover should resume from snapshot plus transcript, not from a host-only plaintext deck secret
- timeout handling should be explicit and should abort or forfeit hands when required transcript messages are missing

## Current Scope

- heads-up Hold'em only
- witnesses are failover/audit participants, not entropy contributors
- the host may also occupy a seat
- betting legality, bankroll, and settlement remain the existing runtime logic
- snapshot quorum is still not multi-party enforced

## Public Versus Private State

### Replicated / public hand state

Replicated table state carries:

- public betting state
- the active hand state machine
- the transcript records and transcript root
- encrypted deck stages and encrypted final deck material
- partial decryptions for private delivery and board opening
- public board cards once a board street is opened
- showdown hole cards only when they are intentionally revealed

Replicated state must not carry:

- plaintext non-owned hole cards
- shuffle seeds
- lock private exponents
- any seat-local secret needed to finish another player's decryption

### Seat-local private state

Each daemon keeps private state for its own profile only:

- local shuffle seed
- local mental-poker private exponent
- local mental-poker public exponent
- locally decrypted hole cards for its own seat
- lightweight audit metadata keyed by hand

Owner-local plaintext is expected. Plaintext hole cards outside the owning daemon are a bug.

## Core Objects

### Mental deck

The deck starts as an ordered list of encoded card values. Each seat:

1. encrypts every card with that seat's mental-poker public exponent
2. applies a deterministic shuffle derived from that seat's local shuffle seed

Replaying all reveals in seat order reconstructs the same encrypted final deck for every verifier.

### Fairness commitment

Before reveal, each seat commits to:

- `tableId`
- `handNumber`
- `seatIndex`
- `playerId`
- `phase`
- `shuffleSeedHex`
- `lockPublicExponentHex`

That commitment prevents a seat from changing its shuffle seed or lock key after seeing another reveal.

### Hand transcript

Every hand message is appended into a hash-chained transcript. Each record carries a `stepHash`, and the transcript carries a rolling `rootHash`.

The transcript root is exposed publicly and is also checked against snapshots for the same hand.

## Transcript Record Kinds

The current runtime appends these logical record kinds:

- `fairness-commit`
- `fairness-reveal`
- `finalization`
- `private-delivery-share`
- `board-share`
- `board-open`
- `showdown-reveal`

Together they prove:

- which shuffle commitments were made
- which reveals were opened
- what final encrypted deck was produced
- which partial decryptions were provided for hole cards and board cards
- which cards were finally opened to the table

## Hand Lifecycle

### 1. Commitment phase

Each seat generates local hand secrets:

- a fresh shuffle seed
- a fresh mental-poker keypair

The daemon stores those secrets only in its local private state. The seat then contributes a `fairness-commit` record to the transcript.

The coordinator does not learn the secret values from this step.

### 2. Reveal phase

Each seat reveals:

- the shuffle seed
- the mental-poker public exponent
- the per-seat replayed deck stage and stage root

Reveals are ordered by seat index. Later seats cannot reveal until earlier-seat reveals are present.

Once all reveals are present, any verifier can replay the encrypted deck locally and derive the same final encrypted deck.

### 3. Finalization record

After all reveals are present, the coordinator appends a `finalization` transcript record containing the replayed final encrypted deck and its root, then advances the live hand into private delivery.

This is the point where the transcript fully binds the encrypted deck that the rest of the hand will use.

### 4. Private delivery

Each seat partially decrypts the opponent's hole-card positions with its own private exponent and sends those partial ciphertexts as a `private-delivery-share`.

For heads-up play:

- seat 0 sends a private-delivery share to seat 1
- seat 1 sends a private-delivery share to seat 0

The recipient combines:

- the opponent's partial ciphertexts from the transcript
- the recipient's own private exponent from local private state

That second local decryption step yields the recipient's plaintext hole cards. Those cards are then stored only in the recipient daemon's private state.

The runtime does not allow replicated tables to advance into `preflop` or later unless the required private-delivery shares are present.

### 5. Betting

After private delivery is complete, the Hold'em state activates and betting starts at `preflop`.

Betting itself is still the existing signed action flow:

- players send signed actions to the current coordinator
- the coordinator validates turn legality and appends `PlayerAction`
- peers replay the accepted public hand state before persistence

This dealerless document is about card handling, not betting semantics.

### 6. Board opening

For flop, turn, and river, the runtime introduces explicit reveal phases:

- `flop-reveal`
- `turn-reveal`
- `river-reveal`

At each reveal phase:

1. each seat publishes a `board-share` partial decryption for the required board positions
2. once both board shares exist, a seat that can finish the decryption locally appends `board-open` with the plaintext board cards
3. peers verify the opened board against the two board shares

Only the opened public board becomes plaintext replicated state.

### 7. Showdown

At `showdown-reveal`, each live seat reveals its own hole cards by locally decrypting the opponent's earlier `private-delivery-share` for that seat and appending `showdown-reveal`.

Peers validate each showdown reveal against the prior private-delivery record before using it for settlement.

Folded seats do not need to reveal at showdown.

### 8. Settlement

After all required showdown reveals are present:

- the runtime settles the hand
- appends `HandResult`
- builds a snapshot
- schedules the next hand

The snapshot and public state are then tied back to the verified transcript root and event ledger.

## Validation On Receipt

Remote peers do not blindly persist replicated tables.

Before writing an accepted table, the runtime:

1. replays the transcript root and validates transcript semantics
2. reconstructs the replayed public hand state
3. validates historical events and snapshots
4. validates host-transition rules and endpoint identity
5. rejects tables that advanced without required protocol records such as private delivery

This means a coordinator can still order messages and affect liveness, but it cannot publish an arbitrary self-consistent hidden-card story and have peers accept it unchecked.

## Timeout And Abort Rules

Dealerless hand setup uses explicit deadlines for:

- commitment
- reveal
- private delivery
- flop/turn/river reveal
- showdown reveal

If a required record is missing when the deadline expires:

- a single missing seat is force-folded through `HandAbort`
- multiple missing seats cause the hand to abort and roll back to the latest snapshot

Peers can also force failover when protocol deadlines expire, so a still-heartbeating but stalling coordinator cannot hold the hand indefinitely.

## Coordinator Role

The current host/coordinator is responsible for:

- accepting joins and actions
- ordering transcript contributions
- appending events
- replicating table state
- driving protocol deadlines
- handling failover

The coordinator is not supposed to:

- choose extra randomness outside its own seat contribution
- see other players' plaintext hole cards
- store other players' plaintext hole cards
- bypass transcript validation on receiving peers

The host may also be a seat, but when it acts as a seat it follows the same commitment, reveal, and action rules as any other player.

## What Failover Uses

Failover does not rely on a host-only deck secret.

The new coordinator resumes from:

- the replicated snapshot history
- the replicated event history
- the replicated hand transcript
- locally derived deadline handling

If the in-progress hand is still replayable from transcript plus snapshot, it can continue. If required transcript steps are missing or invalid, the failover path aborts the hand explicitly.

## Current Limitations

- this is still heads-up only
- snapshots are not quorum-signed by multiple parties
- the active coordinator still controls ordering and therefore liveness pressure
- witnesses do not add randomness
- public spectatorship still sees only the public board and public state, not a separate delayed dealerless proof surface

## Practical Reading

The safest way to interpret the current implementation is:

- fairness of shuffle/opening is transcript-based and replay-checkable
- confidentiality of hole cards is encryption-based and seat-local
- coordinator trust is now mainly about sequencing, timeout behavior, and liveness, not about holding everyone's hidden cards
- the protocol is stronger than the old host-dealer flow, but it is not yet a quorum-finalized multi-party consensus system
