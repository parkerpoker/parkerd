# Trust Model

This document describes the current trust model implemented in this repository. It is intentionally current-state only.

The short version is:

- wallet keys stay local to each daemon
- the daemon mesh, signed event log, and fully signed cooperative snapshots define enforceable money state
- the indexer is informational only
- the local browser UI can instruct the daemon through the controller, but it still does not hold keys or sign protocol messages
- `host-dealer-v1` still places meaningful trust in the host for hidden-card privacy and fair dealing

For wire/state rules, see [protocol.md](./protocol.md). For component topology, see [architecture.md](./architecture.md).

## Security Boundary Summary

- Consensus lives in the daemon mesh, not in the indexer or UI.
- The localhost controller is not part of consensus. It is a local control plane for one machine.
- Money movement is bounded by fully signed settlement snapshots plus each player's local Arkade table-funds state.
- Unfinished hands are not forced onto Arkade. The last fully signed settlement-boundary snapshot is the last enforceable money state.
- Public ads and public hand updates can inform humans, but they cannot create, authorize, or finalize bankroll changes.
- The browser UI can trigger local wallet and table actions through the controller, but only the daemon can authorize and execute them with local keys.

## Assets

### Wallet keys

Each profile has a local wallet identity. The daemon uses it for:

- Arkade wallet ownership
- join identity binding
- signed table-funds receipts

If a wallet private key is compromised, the attacker can control that player's bankroll and produce valid wallet-level receipts for that player.

### Bankroll state

Each seated player keeps local table-funds state for:

- prepared VTXOs
- managed table VTXOs
- current local table balance
- checkpoint records
- cash-out and emergency-exit receipts

This state is local to the player's daemon and is required for checkpoint recording, renewal, cash-out, and emergency exit.

### Canonical event log

`SignedTableEvent` history is the authoritative gameplay transcript. It determines:

- seating
- host epochs
- gameplay ordering
- public state evolution
- failover decisions

If two peers hold the same valid event history, they can deterministically replay the same canonical state.

### Signed snapshots

`CooperativeTableSnapshot` is the enforceable money checkpoint. A snapshot only becomes fully authoritative when it carries signatures from:

- the current host
- every seated player
- every configured witness

That fully signed snapshot is the boundary used for checkpoint recording, cash-out, and emergency exit.

### Public metadata

Signed public advertisements and derived public updates expose information such as:

- table name and stakes
- host identity
- witness count
- public board state
- public chip counts
- showdown hole cards when revealed

This data is useful for discovery and spectatorship, but it is not a money-authorizing asset.

## Actors

### Player daemon

A player daemon:

- owns the player's wallet keys locally
- prepares and confirms buy-ins locally
- signs join intents, action intents, and settlement snapshots
- records checkpoints and executes cash-out or emergency exit for that player

Players do not directly append canonical actions. They submit action requests to the current host.

### Host daemon

A host daemon:

- creates the table
- validates joins and buy-ins
- orders canonical gameplay events
- performs trusted dealing in `host-dealer-v1`
- collects settlement snapshot signatures
- publishes optional public ads and public updates when configured

The host is a protocol participant and part of every snapshot quorum. In the current dealing model, it also has privileged visibility into hidden-card material.

### Witness daemon

A witness daemon:

- signs host leases
- receives and stores canonical events and snapshots
- verifies host heartbeats
- can initiate failover if the host disappears
- can become the next host after a successful failover quorum

Witnesses improve recoverability, but they do not remove trust in the host for hidden-card privacy.

### Indexer

The optional indexer:

- accepts signed public advertisements
- accepts derived public table updates
- stores them in a public read model
- serves them over HTTP to daemons and the web UI

The indexer does not participate in consensus, does not hold wallet keys, and cannot move funds.

### UI

The web UI:

- reads the indexer HTTP API for public spectator state
- reads the localhost controller for local profile, wallet, gameplay, and settlement state
- can instruct the local daemon through the controller

The UI still does not:

- hold wallet or protocol private keys
- sign protocol objects
- read profile JSON directly
- talk directly to the daemon mesh

### Local controller

The localhost controller:

- binds to loopback only
- translates browser-safe HTTP and SSE into daemon RPC
- enforces browser-origin and custom-header checks
- does not own money, keys, or consensus state

### Arkade operator

Arkade and related backing services remain relevant because the current settlement adapter depends on them for:

- wallet onboarding and offboarding
- offchain table-position construction
- checkpoint transfer execution
- some cash-out and emergency-exit paths

The protocol narrows when Arkade state can matter, but it does not eliminate availability dependence on the operator-backed infrastructure.

## Guarantees

### Wallet custody stays local

Player wallet private keys are generated and stored locally per daemon profile. Neither the host, witness, indexer, nor UI needs those keys to participate in the protocol.

### Canonical gameplay is signed and replayable

Canonical gameplay is an append-only stream of signed events. Peers verify signatures, event hashes, ordering, and semantic rules before accepting an event into canonical state.

### Settlement is pinned to cooperative snapshots

Bets and street transitions do not directly move Arkade custody. The enforceable money boundary is the latest fully signed settlement-boundary snapshot.

### Invalid host-only unsigned state cannot force cash-out

The host cannot unilaterally invent a valid settlement outcome with unsigned local state. Cash-out and emergency exit require:

- a fully signed cooperative snapshot
- a matching locally recorded checkpoint hash in the player's table-funds state

### Witnesses improve recovery

When witnesses are configured and available, they can preserve canonical history, detect missing heartbeats, gather failover acceptances, and drive host rotation without relying on the failed host.

### Public surfaces are informational only

The indexer and the public browser view can display stale, missing, or maliciously curated public information without gaining the ability to authorize bankroll changes. They cannot create valid `SignedTableEvent`, `CooperativeTableSnapshot`, or `TableFundsOperation` records on behalf of players.

### Local browser control is still bounded by the daemon

The local browser can ask the controller to move funds, join tables, or submit actions, but:

- the request only reaches `127.0.0.1`
- the controller only forwards structured routes
- the daemon still decides whether the action is valid
- the daemon still owns every signing and persistence step

## Trust Assumptions

### The host is trusted for hidden-card privacy in `host-dealer-v1`

Current dealing is host-run. The host:

- generates the deck
- knows the hole cards before showdown
- controls private card delivery

That means the host can learn or misuse hidden-card information. The protocol does not currently provide dealerless mental-poker privacy or cryptographic fairness against a malicious dealer.

This is why the practical recommendation remains:

- prefer a non-playing host for public games
- treat the host as trusted with respect to hidden-card secrecy and dealing integrity

### Witness presence matters for failover and fresh checkpoints

The runtime only lets a witness initiate failover. If no witness is configured, or a configured witness is unavailable:

- automatic host failover does not happen
- manual `rotateHost()` cannot be initiated by players or the current host
- fresh fully signed snapshots may stop if a configured witness is missing from the exact snapshot quorum

Existing fully signed snapshots remain valid, but recoverability degrades.

### Arkade availability still matters

The daemon mesh defines who should own what, but the current settlement adapter still depends on Arkade-backed services for several wallet and settlement operations. If Arkade or related services are down, money movement may stall even though the signed poker state remains intact.

### Operator choices still shape public exposure

The host decides whether a table is public and whether to publish to an indexer. If the table is public, the public path exposes metadata and public game state by design.

### Local machine security still matters

Each daemon is trusted to protect:

- local private keys
- persisted event and snapshot history
- local Arkade table-funds state

When the controller UI is in use, the same machine is also trusted to protect:

- allowed local browser origins
- the loopback controller process
- the browser tab from malicious local extensions or injected same-origin content

Compromise of a participating machine can compromise that participant's role and secrets.

## Host-Dealer Privacy Tradeoff

The current privacy model is asymmetrical:

- the host learns deck order and player hole cards
- the receiving player learns only their own hole cards during the hand
- witnesses replicate public history and snapshots, but do not receive hidden-card plaintext during ordinary play
- the indexer and UI only see public state plus showdown reveals when they are published

This is better than pushing hidden-card handling into a browser spectator surface, but it is not equal-peer privacy. The system is intentionally honest about that tradeoff.

## Non-Goals

Version 1 does not claim:

- dealerless or trustless hidden-card privacy
- browser-native daemon or wallet custody
- browser-native mesh participation
- trustless public discovery through the indexer
- enforceability of unfinished mid-hand state on Arkade
- that the current Arkade table-funds adapter is hardened for production mainnet use
- that witnesses or indexers can override a player's local wallet custody

## Failure Outcomes

### Host crash between hands

If a witness is present and heartbeats stop:

- the witness can gather failover acceptances
- the witness can install a new host lease for the next epoch
- the table can continue from the last canonical settlement boundary

No unfinished-hand rollback is needed in this case.

### Host crash mid-hand

If failover occurs during an active unsettled hand and a fully signed settlement snapshot already exists:

- the new host appends `HandAbort`
- public state rolls back to the latest fully signed settlement-boundary snapshot
- a new settlement snapshot is collected over that rolled-back state

The interrupted hand is discarded as unenforceable. The last signed checkpoint remains the money truth.

### Witness loss

If a configured witness disappears:

- the current host may still continue ordering gameplay
- but automatic failover is lost because only witnesses initiate failover
- and new fully signed snapshots can stall because the runtime requires every configured witness in the exact snapshot quorum

The practical effect is that the table may keep playing while losing the ability to advance its enforceable checkpoint boundary safely.

### Indexer loss

If the indexer is down or unreachable:

- gameplay consensus continues in the daemon mesh
- players can still use direct invites and the canonical mesh
- public discovery and read-only spectatorship degrade or disappear

No bankroll movement is authorized by the indexer, so its loss is informational rather than custodial.

### UI loss

If the web UI is unavailable:

- gameplay continues
- indexer ingest can continue
- there is no direct effect on wallet custody or canonical state

The UI is optional display surface only.

### Arkade outage

If Arkade-backed services are unavailable:

- wallet onboarding/offboarding may fail
- buy-in preparation or confirmation may fail
- checkpoint recording may stall
- renewals may fail
- cash-out or emergency exit may require waiting for operator availability unless the exact spendable local position is already available

Signed event history and fully signed snapshots still preserve who should own what, but some real money movement paths may be blocked until the backing services recover.

## Practical Reading

The safest way to interpret the current system is:

- trust the daemon mesh for canonical poker state
- trust fully signed snapshots for enforceable money state
- trust each player daemon to protect its own keys and local funds state
- do not trust the indexer or UI with money authority
- do not over-claim privacy against a malicious host in `host-dealer-v1`
