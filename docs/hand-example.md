# Hand Example Walkthrough

This document gives a concrete, end-to-end view of how money moves through a Parker heads-up table.

It is meant to complement:

- [money-flows.md](./money-flows.md)
- [protocol.md](./protocol.md)
- [dealerless.md](./dealerless.md)
- [trust-model.md](./trust-model.md)

The goal here is not to restate every type or every code path. The goal is to show, step by step, what actually happens to funds from:

- wallet funds before join
- buy-in lock
- blind posting
- betting
- card dealing and reveal phases
- showdown payout
- cash-out
- fold
- timeout / abandonment
- emergency exit

## Reading Guide

There are three different things to track during a hand:

| Layer | What it means | Does it move money? |
| --- | --- | --- |
| Wallet layer | Ark wallet spendable VTXOs outside the table | Yes, but outside table custody |
| Custody layer | The table's authoritative money state in `LatestCustodyState` | Yes, this is the table money truth |
| Transcript layer | Dealerless commitments, reveals, private delivery, board openings, showdown reveals | No, this is game/fairness data, not money movement by itself |

The most important rule is:

- cards do not move money directly
- signed actions and settled hand results move money by producing a new custody state
- the latest accepted custody state is the authoritative money checkpoint

## Example Assumptions

The examples below are illustrative but match the implemented runtime shape.

- Two players: Alice and Bob
- `seatCount = 2`
- Each player buys in for `4,000` playable sats
- In real Ark mode, each seat also locks an extra fee reserve `R`
- Each stack claim is therefore backed by `4,000 + R` sats, not just `4,000`
- Blind examples use `100 / 200` for easy arithmetic
- In a real Ark table, Parker may raise blind defaults so the opening blind pot clears the minimum offchain output floor; the worked arithmetic below is illustrative, not a claim about the only allowed blind size

Important note on `R`:

- Parker carries the fee reserve inside the same custody-backed stack claim
- the exact reserve depends on Ark fee config and runtime settings
- in real artifacts, the reserve can be much larger than the visible in-game stack
- when a player cashes out, the player receives their remaining stack plus their remaining reserve, minus the cash-out batch fee

Important note on deterministic recovery:

- v1 does not rely on an output-inspecting tapscript branch to detect who won a contested pot
- instead, when an accepted heads-up source transition leaves live contested pot refs and a later timeout or payout result is already objective, Parker stores a fully signed recovery PSBT over the shared pot CSV exit
- if the cooperative path later stalls, that stored bundle can execute after `U` and produce the same ordinary winner-owned stack refs the cooperative successor would have created

Notation used below:

- `W_A0`: Alice wallet ref before joining
- `S_A0`: Alice table stack ref
- `P_0`: current pot ref
- `A`, `B`, `O`: Alice pubkey, Bob pubkey, Ark/operator signer pubkey
- `U`: unilateral exit delay
- `D`: current action deadline

## Which Steps Create Ark Settlement

This is the first thing to keep straight when reading a hand:

| Transition or phase | Ark batch required? | Why |
| --- | --- | --- |
| `buy-in-lock` | Usually yes | New table custody ref is created from wallet funding refs |
| `blind-post` | Yes | Stack refs and pot refs change |
| `action` with `call`, `bet`, `raise`, all-in, money-changing `fold` | Yes | Stack or pot refs change |
| `action` with `check` | No | Same refs can be carried forward |
| `timeout` with auto-check | No | Same refs can be carried forward |
| `timeout` with auto-fold that changes money ownership | Usually yes | Pot and winner stack refs change |
| `showdown-payout` | Yes if needed | Pot is redistributed into winner stack claims |
| `cash-out` | Yes | Latest stack claim is spent back to wallet |
| `emergency-exit` | No new cooperative batch | Uses the exact source refs plus a local exit execution proof |
| Dealerless card protocol phases | No | They affect hidden/public card knowledge, not custody refs |

The runtime rule is simple:

- if canonical money refs change, Parker builds a new Ark settlement plan
- if canonical money refs do not change, Parker can advance custody with approvals and replay validation, but without a new Ark batch

## The Taproot Trees Parker Builds

Parker uses taproot script-path outputs for custody refs.

The internal key is intentionally unspendable, so these outputs are spent through declared tapscript leaves, not through a hidden key-path shortcut.

In code, the taproot output key is built from:

- an unspendable internal key
- the merkle root of the closure scripts

### 1. Player Stack Ref

Player stack refs are built by `stackOutputSpec(...)` using Ark's `NewDefaultVtxoScript(...)`.

Outside a chain-challenge turn, that gives a two-leaf tree:

```text
Leaf 1: cooperative rebind
  <A> OP_CHECKSIGVERIFY <O> OP_CHECKSIG

Leaf 2: unilateral exit after delay U
  <U> OP_CHECKSEQUENCEVERIFY OP_DROP <A> OP_CHECKSIG
```

Meaning:

- during ordinary play, Alice plus the Ark/operator signer cooperate to rebind Alice's stack into the next accepted custody output
- if cooperation breaks down long enough, Alice can use the delayed CSV branch to redeem her own stack ref unilaterally

### 2. Shared Pot Ref

Pot refs are built by `potOutputSpec(...)`.

For an active pot in an unfinished heads-up hand, Parker usually builds three kinds of leaves:

```text
Leaf 1: cooperative in-hand spend
  <A> OP_CHECKSIGVERIFY <B> OP_CHECKSIGVERIFY <O> OP_CHECKSIG

Leaf 2: timeout spend after deadline D
  <D> OP_CHECKLOCKTIMEVERIFY OP_DROP <non-defaulting players> ... <O>

Leaf 3: delayed shared exit after U
  <U> OP_CHECKSEQUENCEVERIFY OP_DROP <eligible players>
```

In the common case where both players are still eligible and Alice is the current actor, that expands to:

```text
Leaf 1: <A> CHECKSIGVERIFY <B> CHECKSIGVERIFY <O> CHECKSIG
Leaf 2: <D> CLTV DROP <B> CHECKSIGVERIFY <O> CHECKSIG   if Alice is the current actor
Leaf 3: <U> CSV DROP <A> CHECKSIGVERIFY <B> CHECKSIG
```

Important details:

- the cooperative leaf requires every required player signer plus the operator signer
- the timeout leaf excludes the player who missed the deadline, so the non-defaulting side can keep progress moving after `D`
- the delayed exit leaf uses the pot's `EligiblePlayerIDs`, not the host's opinion
- for a still-contested heads-up pot, `EligiblePlayerIDs` is usually both players, so this CSV leaf is a shared delayed-exit path, not a unilateral winner path
- because a live contested pot still requires the eligible player set, one player cannot simply peel the pot out mid-hand
- once the contest is resolved by fold, timeout, or showdown payout, the pot should be converted into winner-owned stack refs; those stack refs then have the simpler single-player stack exit leaf

### 3. Chain-Challenge Open Leaf And `TurnChallengeRef`

When the table uses `turnTimeoutMode = "chain-challenge"` and a turn is actionable, every live stack ref and pot ref also carries a `turn-challenge-open` leaf keyed to the action deadline `D`.

In heads-up, that extra source-ref leaf is:

```text
<D> OP_CHECKLOCKTIMEVERIFY OP_DROP <A> OP_CHECKSIGVERIFY <B> OP_CHECKSIG
```

Meaning:

- after `D`, the full active player set can spend the live bankroll into the challenge path
- the open leaf is player-only; it does not include the operator signer
- Parker uses that leaf only for `turn-challenge-open`, which consumes every live stack ref and pot ref and reissues the full live bankroll into one `TurnChallengeRef`

`TurnChallengeRef` itself uses a separate two-leaf tree:

```text
Leaf 1: cooperative challenge resolution
  <A> OP_CHECKSIGVERIFY <B> OP_CHECKSIG

Leaf 2: challenge escape after delay U
  <U> OP_CHECKSEQUENCEVERIFY OP_DROP <A> OP_CHECKSIGVERIFY <B> OP_CHECKSIG
```

Meaning:

- option-resolution bundles and the timeout-resolution bundle spend `TurnChallengeRef` through the cooperative player-only leaf
- the escape bundle spends `TurnChallengeRef` through the CSV leaf
- timeout is enforced by the timeout-resolution transaction locktime `D + C`, not by a dedicated timeout leaf on `TurnChallengeRef`
- the operator signer is absent from `TurnChallengeRef`; once money is in that ref, challenge resolution is governed by the accepted player-only envelope

### 4. How Parker Chooses the Leaf

When Parker prepares a live cooperative spend, it parses the ref's tapscript tree and picks the leaf whose signer set matches the current successor:

- ordinary live updates pick the cooperative leaf
- timeout successors prefer the CLTV timeout leaf
- deterministic contested-pot recovery instead uses the shared CSV exit leaf through the stored pre-signed bundle
- `turn-challenge-open` spends each live input through its `D` CLTV open leaf
- option-resolution and challenge-timeout bundles spend `TurnChallengeRef` through the cooperative player-only leaf
- challenge escape spends `TurnChallengeRef` through its CSV leaf

Live cooperative spends use `selectCustodySpendPath(...)`. Deterministic pot recovery uses the dedicated shared-pot CSV selector in `selectPotCSVExitSpendPath(...)`.

## What Cards Do And Do Not Do

Dealerless card flow changes transcript state, not money state.

These phases do not create Ark batches by themselves:

- `fairness-commit`
- `fairness-reveal`
- `finalization`
- `private-delivery-share`
- `board-share`
- `board-open`
- `showdown-reveal`

They matter because:

- they determine whether the hand is valid
- they determine what each player's legal action set is
- they determine the final showdown winner

But money only moves when Parker turns the accepted game state into the next custody state.

## Worked Example 1: Normal Hand To Showdown To Cash-Out

### Step 0: Funds Before Joining

Before the table exists, each player just has wallet-side Ark refs:

```text
Alice wallet: W_A0 = 4,000 + R
Bob wallet:   W_B0 = 4,000 + R
```

These are not yet table funds.

### Step 1: Alice Joins And Locks Her Buy-In

Alice sends a join payload containing:

- `BuyInSats = 4,000`
- `FundingRefs = [W_A0]`
- wallet identity binding
- wallet pubkey and Ark address

In real mode, the host checks that `W_A0` is:

- indexed on Ark
- spendable
- script-consistent with its declared tapscripts
- not already locked elsewhere in the table

Then Parker finalizes a `buy-in-lock` custody transition.

Unlike later live-hand custody steps, `buy-in-lock` is admitted from the signed join payload plus live funding checks. It is not yet a "every seated player signs the successor" step.

Example transaction shape:

```text
Inputs:
  W_A0

Outputs:
  S_A0 = Alice stack ref backing:
    amountSats = 4,000
    reservedFeeSats = R
    backed amount = 4,000 + R
```

Money meaning:

- Alice's wallet ref is reserved for the table rather than unrelated wallet use
- the table has an authoritative Alice stack claim
- the new output uses the default stack tapscript tree shown above

### Step 2: Bob Joins And Locks His Buy-In

Same flow for Bob:

```text
Inputs:
  W_B0

Outputs:
  S_B0 = Bob stack ref backing:
    amountSats = 4,000
    reservedFeeSats = R
    backed amount = 4,000 + R
```

At this point the table custody state is roughly:

```text
Alice stack claim = 4,000 (+ R reserve)
Bob stack claim   = 4,000 (+ R reserve)
Pot slices        = none
```

### Step 3: Hand Start And Blind Post

When the hand is created, Parker posts blinds immediately as a `blind-post` custody transition.

Assume:

- Alice is small blind: `100`
- Bob is big blind: `200`

Before:

```text
Alice stack = 4,000
Bob stack   = 4,000
Pot         = 0
```

After blind-post:

```text
Alice stack = 3,900
Bob stack   = 3,800
Pot         = 300
```

Example transaction shape:

```text
Inputs:
  S_A0
  S_B0

Outputs:
  S_A1 = Alice stack ref backing 3,900 + reserve
  S_B1 = Bob stack ref backing 3,800 + reserve
  P_0  = main pot ref backing 300
```

Script meaning:

- `S_A1` and `S_B1` are player stack trees
- `P_0` is a pot tree with:
  - cooperative `Alice + Bob + operator`
  - timeout branch keyed to the current actor and deadline
  - delayed shared exit branch for eligible players

### Step 4: Dealerless Card Flow

Now the hand runs through:

- commitment
- reveal
- finalization
- private delivery

No money moves here.

The only things changing are:

- transcript records
- transcript root
- public hand state
- the set of legal actions

Parker may create non-money custody updates later that bind those fields, but the card protocol itself does not respend the pot or stack refs.

### Ordinary Action Staging Before Money Moves

Every ordinary betting action in the examples below follows the same locked-action protocol before Parker accepts the resulting custody transition:

1. The host prebuilds the full candidate bundles locally and replicates only the compact public turn menu.
2. The acting player chooses exactly one deterministic option by signing `SelectionAuth` over the table id, epoch, hand id, decision index, previous custody state hash, turn anchor hash, candidate hash, and action deadline.
3. The host validates that binding, locks that exact candidate, replicates only the selected bundle, and acknowledges the lock with `ActionLockedAck`.
4. The acting player settles that locked bundle locally, signs `ActionSettlementRequest`, and the host persists that exact settled request until it publishes the accepted `action` transition.

The examples below show the accepted money result of each action after that lock, settlement, and publication flow completes.

### Step 5: Alice Calls Preflop

Alice completes the blind by calling `100`.

Before:

```text
Alice stack = 3,900
Bob stack   = 3,800
Pot         = 300
```

After:

```text
Alice stack = 3,800
Bob stack   = 3,800
Pot         = 400
```

Example transaction shape:

```text
Inputs:
  S_A1
  P_0

Outputs:
  S_A2 = Alice stack ref backing 3,800 + reserve
  P_1  = pot ref backing 400
```

Notice what did not move:

- Bob's stack ref did not need to be respent because Bob's stack amount did not change

That is a recurring pattern in Parker:

- only the changed refs need to be consumed and recreated

### Step 6: Flop Cards Open

The flop comes through:

- `board-share`
- `board-open`

No money moves here.

The custody state may bind the new transcript root and public state hash, but no Ark batch is required just because community cards were revealed.

### Step 7: Bob Bets The Flop

Bob bets `300`.

Before:

```text
Alice stack = 3,800
Bob stack   = 3,800
Pot         = 400
```

After:

```text
Alice stack = 3,800
Bob stack   = 3,500
Pot         = 700
```

Example transaction shape:

```text
Inputs:
  S_B1
  P_1

Outputs:
  S_B2 = Bob stack ref backing 3,500 + reserve
  P_2  = pot ref backing 700
```

### Step 8: Alice Calls The Flop Bet

Alice calls the `300`.

Before:

```text
Alice stack = 3,800
Bob stack   = 3,500
Pot         = 700
```

After:

```text
Alice stack = 3,500
Bob stack   = 3,500
Pot         = 1,000
```

Example transaction shape:

```text
Inputs:
  S_A2
  P_2

Outputs:
  S_A3 = Alice stack ref backing 3,500 + reserve
  P_3  = pot ref backing 1,000
```

### Step 9: Turn Check, River Check

Suppose both remaining streets are checked through.

These are accepted custody transitions, but they are zero-exposure transitions:

- the transcript root changes
- the legal-action hash changes
- the action deadline changes
- the custody sequence advances
- the stack refs and pot refs stay the same

So Parker records accepted custody, but does not need a new Ark batch.

Before and after the turn check:

```text
Alice stack = 3,500
Bob stack   = 3,500
Pot         = 1,000
Refs        = unchanged
```

Same for the river check.

### Step 10: Showdown Reveal

Both players reveal as required.

Again:

- transcript data changes
- card knowledge changes
- no money moves yet

Assume the hand settles with Alice winning the full `1,000` pot.

### Step 11: Showdown Payout

This is where the pot is actually redistributed into winner-owned stack claims.

Before:

```text
Alice stack = 3,500
Bob stack   = 3,500
Pot         = 1,000
```

After:

```text
Alice stack = 4,500
Bob stack   = 3,500
Pot         = 0
```

Example transaction shape:

```text
Inputs:
  S_A3
  P_3

Outputs:
  S_A4 = Alice stack ref backing 4,500 + remaining reserve
```

Bob's unchanged stack ref can be carried forward.

This is usually a `showdown-payout` transition, unless the immediately prior accepted custody state already exactly matches the settled public money state.

### Step 12: Hand Result

After the final money checkpoint exists, Parker appends `HandResult`.

That ordering matters:

- first the monetary checkpoint is accepted
- then the UI/result event is appended

### Step 13: Alice Cashes Out

Cash-out is a cooperative table-to-wallet off-ramp from the latest accepted stack claim.

Only the acting player is required to approve a `cash-out` transition.

Suppose Alice cashes out first.

Before:

```text
Alice latest stack claim = 4,500 + remaining reserve
Bob latest stack claim   = 3,500 + remaining reserve
```

Example transaction shape:

```text
Inputs:
  S_A4

Outputs:
  wallet-return(Alice Ark address) =
    4,500 + remaining reserve - cash-out batch fee
```

After the custody transition:

```text
Alice stack claim = 0, status = completed
Bob stack claim   = unchanged
```

The important point is that cash-out spends the latest accepted stack claim, not an older pre-hand or pre-loss balance.

## Worked Example 2: Player Folds Before Showdown

This is the simplest "who wins the pot?" example.

Start from the blind-post state:

```text
Alice stack = 3,900
Bob stack   = 3,800
Pot         = 300
```

Alice folds immediately.

Because the fold ends the hand, the signed `action` transition itself can already be the final money checkpoint.

After the fold:

```text
Alice stack = 3,900
Bob stack   = 4,100
Pot         = 0
```

Example transaction shape:

```text
Inputs:
  S_B1
  P_0

Outputs:
  S_B_fold = Bob stack ref backing 4,100 + reserve
```

Alice's stack ref can be carried forward unchanged because folding does not spend her remaining stack.

This is why folding does not require trusting Bob:

- Alice's signed fold action is replayed locally
- the next custody state is derived locally
- the only valid next money state is "Bob gets the pot, Alice keeps the rest"
- once that transition is accepted, Alice cannot later cash out the old contested pot

Often there is no separate `showdown-payout` after a money-finalizing fold, because the fold action already produced the settled money state.

## Chain-Challenge Example: Exact Height-Based Escape Validation

Suppose the table uses `turnTimeoutMode = "chain-challenge"` and the hand reaches:

```text
Alice stack = 3,800
Bob stack   = 3,500
Pot         = 700
Alice to act facing Bob's 300 flop bet
Action deadline = D
Challenge window = C
Challenge escape CSV = 12 blocks   (illustrative block-based example)
```

### Step 1: Live refs before the challenge opens

At this decision, every live ref already carries the `turn-challenge-open` leaf keyed to `D`.

Example live scripts:

```text
Alice stack ref
  Leaf 1: <A> CHECKSIGVERIFY <O> CHECKSIG
  Leaf 2: <D> CLTV DROP <A> CHECKSIGVERIFY <B> CHECKSIG
  Leaf 3: <U> CSV DROP <A> CHECKSIG

Bob stack ref
  Leaf 1: <B> CHECKSIGVERIFY <O> CHECKSIG
  Leaf 2: <D> CLTV DROP <A> CHECKSIGVERIFY <B> CHECKSIG
  Leaf 3: <U> CSV DROP <B> CHECKSIG

Shared pot ref
  Leaf 1: <A> CHECKSIGVERIFY <B> CHECKSIGVERIFY <O> CHECKSIG
  Leaf 2: <D> CLTV DROP <A> CHECKSIGVERIFY <B> CHECKSIG
  Leaf 3: <D> CLTV DROP <B> CHECKSIGVERIFY <O> CHECKSIG   if Alice is the defaulting actor
  Leaf 4: <U> CSV DROP <A> CHECKSIGVERIFY <B> CHECKSIG
```

The important property is that the open leaf is uniform across all live refs:

- it is keyed to `D`
- it is signed only by the active player set
- it lets Parker collapse the full live bankroll into one `TurnChallengeRef`

### Step 2: `turn-challenge-open` reissues the full live bankroll

If the turn is still unlocked when `D` passes, Parker can execute the pre-signed `turn-challenge-open` bundle.

Example transaction shape:

```text
Inputs:
  S_A2
  S_B2
  P_2

Outputs:
  TC_0 = TurnChallengeRef backing:
    Alice stack 3,800
    Bob stack   3,500
    main pot      700
    plus the same fee reserve already carried by the source stack refs
  anchor output
```

Bundle meaning:

- `Kind = turn-challenge-open`
- `SourceRefs = [S_A2, S_B2, P_2]`
- `TxLocktime = D`
- `SignedPSBT` spends every input through its `turn-challenge-open` leaf
- `CustodyProof.ChallengeWitness.TransactionID = tx_open` after broadcast

After this point:

- the ordinary stack refs and pot refs are gone
- the full live bankroll is represented by one `TurnChallengeRef`
- accepted table state records the bundle hash and the witness txid, but not any live chain-height observation

### Step 3: The pre-signed PSBT chain

The same accepted `ChallengeEnvelope` carries four PSBT families over the same semantic turn menu:

```text
1. Open bundle
   SourceRefs  = all live stack refs + pot refs
   Spend path  = each input's turn-challenge-open leaf
   TxLocktime  = D
   Outputs     = TurnChallengeRef + anchor

2. Option-resolution bundle (one per option)
   SourceRefs  = [TurnChallengeRef]
   Spend path  = TurnChallengeRef cooperative leaf
   TxLocktime  = 0
   Outputs     = exact successor stack/pot refs for that option + anchor

3. Timeout-resolution bundle
   SourceRefs  = [TurnChallengeRef]
   Spend path  = TurnChallengeRef cooperative leaf
   TxLocktime  = D + C
   Outputs     = exact timeout successor stack/pot refs + anchor

4. Escape bundle
   SourceRefs  = [TurnChallengeRef]
   Spend path  = TurnChallengeRef CSV leaf
   TxLocktime  = 0
   Input seq   = BIP68(U)
   Outputs     = exact `turn-challenge-escape` successor refs + anchor
```

Two implementation details matter here:

- option-resolution and timeout-resolution both spend the cooperative player-only leaf on `TurnChallengeRef`
- escape uses the CSV leaf, so maturity depends on the challenge ref's actual confirmation height rather than on an estimated wall-clock delay when the CSV unit is blocks

### Step 4: Local block-height readiness

Suppose the challenge ref escape leaf uses a block-based CSV delay of `12` blocks and `tx_open` confirms at block height `250000`.

Parker computes:

```text
eligibleHeight = 250000 + 12 = 250012
```

Local readiness for `meshResolveTurnChallenge(..., "escape")` is:

1. find the accepted `turn-challenge-open` transition
2. read `openTxID = ChallengeWitness.TransactionID`
3. query explorer `/tx/{openTxID}/status`
4. reject if `tx_open` is unconfirmed
5. compute `eligibleHeight = openConfirmedHeight + csvBlocks`
6. query explorer `/blocks/tip/height`
7. reject unless `tipHeight >= eligibleHeight`

So:

- if the live tip is `250011`, escape is rejected locally
- if the live tip is `250012`, escape is locally ready

For a block-based CSV example:

- `PendingTurnChallenge.escapeEligibleAt` stays empty
- `NativeTableLocalView.TurnChallengeChain` exposes the local-only fields `openTxID`, `openConfirmedHeight`, `escapeEligibleHeight`, `chainTipHeight`, and `escapeReady`

### Step 5: Accepted replay of the escape transaction

When the accepted escape transition appears, replay uses exact heights rather than accepted timestamps:

1. read `tx_open` from the accepted `turn-challenge-open` witness
2. read `tx_escape` from the accepted `turn-challenge-escape` witness
3. query explorer status for both transactions
4. reject if either transaction is unconfirmed
5. reject unless `escape.BlockHeight >= open.BlockHeight + 12`
6. reject if either height cannot be verified

Accepted table state does not store:

- live chain tip height
- `tx_open` confirmation height
- `tx_escape` confirmation height

Those observations stay local and are re-queried when Parker checks readiness or replays accepted history.

Second-based CSV keeps the timestamp path:

- `PendingTurnChallenge.escapeEligibleAt` is populated
- local readiness compares wall-clock time to that accepted timestamp
- replay compares `ChallengeWitness.executedAt` against the accepted `escapeEligibleAt`

## Worked Example 3: A Player Abandons The Hand During An Action Window

Suppose the hand is at:

```text
Alice stack = 3,800
Bob stack   = 3,500
Pot         = 700
Alice to act facing Bob's 300 flop bet
```

The semantic timeout result below is the same successor that the pre-signed challenge timeout bundle authorizes at `D + C`.

If Alice disappears and the timeout path executes:

- Parker derives the timeout result locally
- if `check` is legal, timeout becomes `check`
- otherwise timeout becomes `fold`

Here `check` is not legal, so timeout becomes fold.

The timeout resolution looks like:

```text
ActionType               = fold
ActingPlayerID           = Alice
DeadPlayerIDs            = [Alice]
LostEligibilityPlayerIDs = [Alice]
```

Money result:

```text
Alice stack = 3,800
Bob stack   = 4,200
Pot         = 0
```

Example transaction shape:

```text
Inputs:
  S_B2
  P_2

Outputs:
  S_B_timeout = Bob stack ref backing 4,200 + reserve
```

Two important enforcement details are at work here:

- Alice is excluded from the timeout successor approval set
- the previous pot ref already had a timeout leaf that matched "non-defaulting side + operator after deadline D"

So Bob does not need Alice to sign after Alice has already missed the deadline and lost eligibility on the contested money.

The accepted source transition that left `P_2` live also stores a deterministic recovery bundle:

- it is attached to that accepted source transition before the source step is treated as complete
- it spends `P_2` through the shared CSV pot exit, not through a special winner-detecting tapscript branch
- it is fully signed before the source transition is accepted
- it exact-commits to `S_B_timeout` as the only money-resolving custody output, plus the required anchor output
- it becomes executable only after `U`

So if the live cooperative timeout finalization fails after `D`, the table can recover later by:

1. waiting until the stored bundle's `EarliestExecuteAt`
2. broadcasting the pre-signed recovery PSBT
3. appending the same semantic `timeout` transition with `RecoveryWitness` instead of `SettlementWitness`

The semantic money story does not change. Only the proof surface changes.

If timeout had resolved to `check` instead:

- the hand would advance
- the custody sequence would advance
- the refs could stay unchanged
- no new Ark batch would be required
- no winner-take-all recovery bundle would exist yet, because no objective redistribution had happened

## Worked Example 4: A Player Abandons During Showdown Reveal

Now suppose betting is complete:

```text
Alice stack = 3,500
Bob stack   = 3,500
Pot         = 1,000
Phase       = showdown-reveal
```

Bob fails to provide the required reveal.

Dealerless behavior:

- Parker appends `HandAbort`
- the missing seat is force-folded
- the hand settles

Custody behavior:

- Parker derives a `TimeoutResolution` marking Bob dead / ineligible for the contested pot
- Parker finalizes a `showdown-payout` transition if the current custody state does not already match the settled money state

If Alice is the only eligible winner, the result becomes:

```text
Alice stack = 4,500
Bob stack   = 3,500
Pot         = 0
```

Example transaction shape:

```text
Inputs:
  S_A3
  P_3

Outputs:
  S_A_timeout_win = Alice stack ref backing 4,500 + reserve
```

Again, the missing player is excluded where appropriate from the payout successor approval set.

One subtle but important rule:

- showdown-reveal timeout refunds uncontested stack
- earlier protocol failures such as `private-delivery` fail closed unless or until the runtime reaches a later objective money-resolving state
- the timeout path does not gift uncontested chips to the surviving player

So the timeout only transfers the actually contested money.

If the live cooperative `showdown-payout` cannot complete, Parker handles this exactly the same way as the action-timeout case:

- the accepted source transition already stored a fully signed recovery bundle over the shared pot CSV exit
- that bundle only exists because the money result is objective once the missing showdown participant is dead
- after `U`, the host can execute that stored PSBT and append the ordinary semantic `showdown-payout` transition with `RecoveryWitness`

That recovered payout lands in an ordinary winner-owned stack ref. It is not a direct wallet payout.

## What If More Than One Player Is Missing?

Parker does not invent hidden-card-dependent winners when the missing data is too large.

If multiple seats are missing required protocol records:

- the hand is aborted
- Parker restores the latest fully signed snapshot
- funds stay at the latest accepted custody checkpoint

That is a liveness failure, not a "host guesses who won" path.

## Emergency Exit Versus Cash-Out

Ordinary cash-out is cooperative:

- it starts from the latest accepted stack claim
- it builds a normal cash-out custody transition
- it produces a wallet-return output

Emergency exit is the fallback:

- it is only allowed from a settled hand
- it must carry the exact current source refs
- it uses a local exit execution proof
- it records an `ExitProofRef`
- it can redeem the current refs through their redemption / exit branches when cooperative completion is unavailable

The emergency-exit proof is validated against:

- the latest custody state hash
- the exact source refs
- the exit tx ids or sweep tx id

So it is not "I claim I owned something earlier." It is "I can prove I am exiting the exact refs from the latest accepted custody checkpoint."

## How The Protocol Enforces That The Winner Gets The Funds

There are five layers to that answer.

### 1. The host does not define the money result alone

For actions, timeouts, cash-out, and exits, honest peers derive the expected successor locally from:

- the latest accepted custody state
- the signed `nativeActionRequest` or `nativeFundsRequest`
- local Hold'em rules

So the host is proposing the next state, not inventing it unilaterally.

### 2. The accepted successor is hash-bound and approval-bound

Each accepted custody transition binds:

- the previous state hash
- the next state hash
- the action deadline
- transcript root
- public money state hash
- approval signatures

That means a later counterparty cannot quietly replace "Alice wins 1,000" with "the old pot still exists."

### 3. The pot output scripts themselves encode the allowed signer sets

While the pot is still live:

- cooperative branches require the required player set plus the operator signer
- timeout branches allow the non-defaulting side plus the operator after `D`
- delayed exit branches require the eligible player set after `U`

So the winner path is not merely a database update. It is reflected in the taproot leaves used to spend the previous pot ref.

### 4. After payout, the pot disappears and the winner gets a winner-owned stack ref

Once a fold or showdown payout is accepted:

- the contested pot ref is gone
- the winner's value is in the winner's stack claim
- that stack claim uses the winner's own stack script, not the old shared-pot script

From that point on, later cash-out or emergency-exit flows read the post-win stack claim, not the pre-win pot.

### 5. Real Ark batches and recovery bundles are witness-backed and replayable

When a real batch is required, Parker stores a settlement witness bundle in `CustodyProof.SettlementWitness`:

- `proofPsbt`
- finalized `commitmentTx`
- batch expiry type/value
- finalized `vtxoTree`
- optional `connectorTree`

Later accepted-history replay:

- rebuilds the authorized spend plan from the prior state plus the accepted transition
- validates the witness bundle offline
- checks that the witness-derived refs match `NextState` and `Proof.VTXORefs`

So already-accepted history does not need a fresh Ark lookup to prove who ended up owning what.

Deterministic recovery uses a second proof surface:

- the source transition stores `CustodyProof.RecoveryBundles`
- the executed `timeout` or `showdown-payout` transition stores `CustodyProof.RecoveryWitness`
- replay validates the stored signed PSBT, executes the finalized witness against the CSV leaf offline, and checks the exact source pot refs, authorized outputs, and recovery tx metadata
- replay then derives the winner-owned stack refs from the recovery PSBT itself and exact-matches them against `NextState` and `Proof.VTXORefs`

So the important distinction is:

- `SettlementWitness` proves "this money moved through a live Ark batch"
- `RecoveryWitness` proves "this money moved through the pre-signed deterministic CSV recovery bundle"

## Where Forfeits Fit

The Ark batch path also prepares signed forfeit transactions for consumed inputs when a connector tree is present.

At a high level:

- the finalized batch gives Parker a commitment transaction and connector leaves
- for each consumed custody input, Parker can build a forfeit tx spending:
  - the old VTXO input
  - the matching connector output
- that forfeit tx pays the Ark forfeit address plus an anchor output
- the input spend uses the same selected tapscript branch Parker authorized for that transition

The point of this machinery is not "the host chooses the winner."

The point is:

- the old inputs are tied to the authorized transition path
- the new outputs are the only accepted next ownership state
- the connector / forfeit machinery is part of Ark's batch safety and failure handling around those spent inputs

## One-Line Summary Of The Money Story

The cleanest way to think about Parker money flow is:

- wallet refs are locked into table stack refs at join
- blinds and bets turn stack refs into updated stack refs plus shared pot refs
- cards change transcript state, not money state
- fold, timeout, or showdown convert the pot ref into winner-owned stack refs
- cash-out spends the latest winner or loser stack ref back to wallet
- emergency exit redeems the exact latest accepted refs when cooperation fails

If you want, the next useful follow-up would be a second doc with diagrams for:

- the exact custody states after each step as JSON-like objects
- a tap-tree diagram for each live ref in the worked examples
- a side-by-side mapping from `game.HoldemState` to `CustodyState`
