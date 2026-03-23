# Trust Model

This document describes the trust model implemented today in this repository. It is intentionally current-state only.

For wire surfaces, see [protocol.md](./protocol.md). For component topology, see [architecture.md](./architecture.md).

## Short Version

- wallet keys stay local to each daemon profile
- the browser UI and controller do not hold wallet or protocol private keys
- the current Go runtime is still host-authoritative for gameplay progression, hidden-card privacy, and replicated table state
- the runtime signs events, advertisements, snapshots, and funds operations, but it does not yet enforce the stronger multi-party verification model described in older design docs
- the indexer is informational only and does not authorize money movement

## Security Boundary Summary

- Local wallet custody lives in each daemon profile.
- The localhost controller is a local control plane, not a money-authorizing peer.
- The browser UI can trigger local actions, but only the daemon owns signing, persistence, and wallet operations.
- The indexer and public UI are optional read surfaces only.
- The host or failover successor is currently trusted as the active table authority.
- Signed events and snapshots exist, but remote peers do not yet independently enforce full cryptographic replay and quorum validation on every replicated object.

## Assets

### Wallet keys

Each profile has a local wallet identity. The daemon uses it for:

- wallet custody
- local table-funds operations
- signed renew, cash-out, and emergency-exit receipts

If a wallet private key is compromised, that player's bankroll can be controlled by the attacker.

### Protocol and peer keys

Each profile also has local peer and protocol identities used for:

- peer addressability
- event signing
- snapshot signing
- advertisement signing

These keys stay local to the daemon profile, not in the browser.

### Local table copy

Each daemon stores its own replica of the table, including:

- table config
- seats
- public state
- signed events
- snapshots
- private hand material relevant to that profile

This replica is operationally important because failover and funds operations use it directly.

### Table-funds state

Each daemon keeps local table-funds state for:

- buy-in amounts
- checkpoint hashes
- wallet-signed operation receipts
- local cash-out and exit status

### Public metadata

Public advertisements and updates expose:

- table name and stakes
- host identity and witness count
- occupied seats
- public game state

This information is useful for discovery and spectatorship, but it is not wallet custody.

## Actors

### Player daemon

A player daemon:

- owns local wallet, peer, and protocol keys
- joins tables
- submits actions to the host
- stores a replicated table copy
- performs local renew, cash-out, and emergency-exit operations

### Host daemon

A host daemon:

- creates tables
- accepts joins
- sequences gameplay
- deals hidden cards in `host-dealer-v1`
- appends events
- builds snapshots
- replicates table state to other peers
- publishes public state when configured

### Witness daemon

A witness daemon:

- stores replicated table copies
- watches host heartbeat freshness
- can take over when witnesses are configured

If no witnesses are configured, failover falls back to the seated player with the lowest peer ID.

### Local controller

The localhost controller:

- exposes browser-safe HTTP and SSE
- enforces origin and custom-header checks
- forwards structured actions to the local daemon
- does not own keys or funds

### Indexer

The optional indexer:

- stores public ads and updates
- serves public table views over HTTP
- does not join gameplay authority
- does not hold wallet keys
- does not authorize money movement

### Web UI

The web UI:

- reads public state from the indexer or controller proxy
- reads local daemon state through the controller
- can instruct the local daemon through the controller

It still does not:

- hold wallet or protocol private keys
- talk to peer `/native/*` routes directly
- talk to the Unix socket directly

## Guarantees Provided Today

### Wallet custody stays local

Player wallet private keys are generated and stored locally per daemon profile. Neither the host, witness, indexer, nor browser needs those keys to participate.

### Browser access is mediated

The browser only reaches the daemon through the localhost controller, which:

- binds to loopback by default
- checks allowed origins
- requires the `X-Parker-Local-Controller` header for browser requests

### Local funds operations are signed

Renew, cash-out, and exit receipts are signed with the local wallet key and include the current checkpoint hash recorded by the daemon.

### Public surfaces are non-custodial

The indexer and public browser surfaces can be stale, missing, or maliciously curated without gaining the ability to sign wallet operations on behalf of a player.

## Important Current Limitations

### Host authority is stronger than the older design docs implied

The current Go runtime is not yet a fully verified signed mesh.

Today the host is trusted to:

- accept joins
- accept actions
- advance gameplay
- replicate accurate table state
- provide the current table copy to non-host peers

### Peer requests use detached signatures

Join, action, and table-sync requests between daemons are JSON over HTTP with detached request signatures. The runtime still relies on HTTP transport semantics rather than an encrypted peer-to-peer channel.

### Replicated state is not fully re-verified on receipt

Peers accept replicated table state through host polling and `/native/table/sync` after request-level auth and monotonicity checks. The current runtime does not yet re-run a full cryptographic replay and semantic verification pass before persisting that state.

### Snapshot quorum is not yet multi-party enforced

The field name `latestFullySignedSnapshot` is historical. In current code, the runtime populates it with the latest locally built snapshot, and that snapshot currently carries only the builder's signature.

That means:

- snapshot hashes are still useful local checkpoints
- but they are not yet the multi-party settlement proofs described in older design docs

### The indexer does not verify signatures before storage

The indexer validates required fields, but it does not currently cryptographically verify advertisements or updates before storing them.

## Trust Assumptions

### Trust the host for hidden-card privacy and dealing integrity

Current dealing is host-run. The host:

- generates the deck seed
- knows all hole cards during the hand
- determines the public hand progression

This is not dealerless mental poker.

### Trust the host or failover successor for replicated table truth

Because non-host peers currently rely on replicated table copies rather than a fully verified signed mesh, the active host or failover successor is effectively the authoritative table source.

### Trust each local machine to protect its own secrets

Each participant machine is trusted to protect:

- wallet keys
- protocol and peer keys
- persisted table state
- local private hand material
- controller-origin protections when the browser UI is in use

### Arkade availability still matters

The poker runtime can continue to track state locally, but renew, cash-out, exit, and some wallet flows still depend on Arkade-backed services.

## Failure Outcomes

### Host loss between hands

If the host stops updating heartbeat timestamps:

- witnesses can take over when configured
- otherwise the seated player with the lowest peer ID can take over
- the new host continues from the latest stored snapshot

This is operational recovery, not a byzantine-proof consensus change.

### Host loss mid-hand

If failover occurs during an active hand and a snapshot exists:

- the new host appends `HandAbort`
- public state rolls back to the latest stored snapshot
- the table continues from there

Because the snapshot is not yet a multi-party quorum proof, that rollback trust is only as strong as the replicated table state already accepted by the peers.

### Witness loss

If witnesses are configured and disappear:

- a player cannot take over while witnesses are still configured
- automatic failover can stall
- gameplay may continue until the host itself fails

### Indexer loss

If the indexer is down:

- direct table play can continue
- public discovery and spectatorship degrade or disappear
- no wallet authority is lost

### UI loss

If the web UI is unavailable:

- daemon-to-daemon play can continue
- CLI control still works
- no wallet keys are exposed by that failure

### Arkade outage

If Arkade-backed services are unavailable:

- wallet onboarding/offboarding may fail
- renew, cash-out, or exit flows may stall
- local poker state can still exist, but some real funds operations may have to wait

## Practical Reading

The safest way to interpret the current system is:

- trust each daemon with its own wallet custody
- treat the host or failover successor as the practical gameplay authority
- treat events and snapshots as signed audit artifacts, not yet as fully verified multi-party consensus proofs
- do not trust the indexer or UI with money authority
- do not over-claim privacy or fairness against a malicious host in `host-dealer-v1`
