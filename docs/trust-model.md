# Trust Model

This document describes the trust model implemented today in this repository. It is intentionally current-state only.

For wire surfaces, see [protocol.md](./protocol.md). For component topology, see [architecture.md](./architecture.md). For the dealerless hand flow itself, see [dealerless.md](./dealerless.md). For chip and wallet movement, see [money-flows.md](./money-flows.md).

## Short Version

- wallet, peer, protocol, and transport private keys stay local to each daemon profile
- the browser client and controller do not hold wallet, protocol, or transport private keys
- the current Go runtime is coordinator-led for sequencing and failover, but hidden-card confidentiality now comes from dealerless transcript flow with seat-local secrets
- the runtime signs transport envelopes, request-auth payloads, events, advertisements, snapshots, and funds operations, and remote peers now replay accepted transcript/public/ledger state before persistence, but it does not yet enforce the stronger multi-party quorum model described in older design docs
- the indexer is informational only and does not authorize money movement

## Security Boundary Summary

- Local wallet custody lives in each daemon profile.
- The localhost controller is a local control plane, not a money-authorizing peer.
- The browser client can trigger local actions, but only the daemon owns peer transport, signing, persistence, and wallet operations.
- The indexer and public browser surfaces are optional read surfaces only.
- The host or failover successor is currently trusted as the active coordinator for ordering, heartbeat handling, and timeout/failover flow.
- Signed transport envelopes, events, snapshots, and hand transcripts are replay-checked before persistence, but the runtime does not yet enforce a multi-party snapshot quorum or broader consensus proof on every replicated object.

## Assets

### Wallet keys

Each profile has a local wallet identity. The daemon uses it for:

- wallet custody
- local table-funds operations
- signed renew, cash-out, and emergency-exit receipts

If a wallet private key is compromised, that player's bankroll can be controlled by the attacker.

### Peer, protocol, and transport keys

Each profile also has local peer, protocol, and transport identities used for:

- peer addressability
- transport-envelope signing via the protocol key
- transport body decryption for traffic addressed to that daemon
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

These are runtime table roles, not separate network stacks. The same daemon binary can appear as a host, witness-listed participant, or player depending on table state and configuration.

### Player daemon

A player daemon:

- owns local wallet, peer, protocol, and transport keys
- joins tables
- submits actions to the host
- stores a replicated table copy
- performs local renew, cash-out, and emergency-exit operations

### Host daemon

A host daemon:

- creates tables
- accepts joins
- sequences gameplay and protocol deadlines
- coordinates `dealerless-transcript-v1` hand setup and public progression
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

### Browser client

The browser client:

- reads public state from the indexer or controller proxy
- reads local daemon state through the controller
- can instruct the local daemon through the controller

It still does not:

- hold wallet, protocol, or transport private keys
- talk to peer `parker://` transport directly
- talk to the Unix socket directly

## Guarantees Provided Today

### Wallet custody stays local

Player wallet private keys are generated and stored locally per daemon profile. Neither the host, witness, indexer, nor browser needs those keys to participate.

### Browser access is mediated

The browser only reaches the daemon through the localhost controller, which:

- binds to loopback by default
- checks allowed origins
- requires the `X-Parker-Local-Controller` header for browser requests

### Peer transport auth stays in the daemon

Daemon-to-daemon envelopes are signed with the protocol key, and join/action/fetch/sync payloads carry wallet- or protocol-level auth when needed.

The browser and controller never assemble or sign those peer messages directly.

### Local funds operations are signed

Renew, cash-out, and exit receipts are signed with the local wallet key and include the current checkpoint hash recorded by the daemon.

### Public surfaces are non-custodial

The indexer and public browser surfaces can be stale, missing, or maliciously curated without gaining the ability to sign wallet operations on behalf of a player.

## Important Current Limitations

### Coordinator authority is still stronger than the older design docs implied

The current Go runtime is not yet a fully verified signed mesh.

Today the coordinator is trusted to:

- accept joins
- accept actions
- order gameplay and timeout/failover transitions
- replicate accurate table state
- provide the current table copy to non-host peers

### Signed peer transport does not remove coordinator authority

Daemon-to-daemon traffic no longer uses HTTP peer routes. Peers exchange signed transport envelopes over direct `parker://` TCP links, and request/response bodies are encrypted once the sender knows the recipient transport public key.

Join requests still rely on wallet-signed identity bindings, action requests on wallet-signed turn bindings, and sync on a host protocol signature over the table hash. That authenticates messages, but it does not by itself make the runtime a fully trustless verified mesh.

### Replicated state is replay-verified, but not quorum-finalized

Peers accept replicated table state through host polling (`table.state.pull`) and host pushes (`table.state.push`) after envelope verification, request-level auth, transcript replay, public-state replay, and historical-ledger validation. The current runtime still does not collect or enforce a multi-party signature quorum before treating a snapshot as the latest local checkpoint.

### Snapshot quorum is not yet multi-party enforced

The field name `latestFullySignedSnapshot` is historical. In current code, the runtime populates it with the latest locally built snapshot, and that snapshot currently carries only the builder's signature.

That means:

- snapshot hashes are still useful local checkpoints
- but they are not yet the multi-party settlement proofs described in older design docs

### The indexer does not verify signatures before storage

The indexer validates required fields, but it does not currently cryptographically verify advertisements or updates before storing them.

## Trust Assumptions

### Do not trust the host with non-owned hole cards

Current dealing is `dealerless-transcript-v1`. The coordinator:

- orders the hand transcript and timeout transitions
- is not supposed to receive plaintext non-owned hole cards from honest peers in the first place
- does not derive or persist plaintext non-owned hole cards
- only sees encrypted deck state, partial ciphertext shares, and later intentionally public card opens for other seats

Each owning daemon can decrypt and locally store only its own hole cards after private delivery. Honest daemons exchange only partial decryptions for non-public cards, not plaintext hole-card values.

### Trust the host or failover successor for ordering and liveness

Because the coordinator still orders joins, actions, timeouts, and failover, liveness and sequencing still depend on the active host or failover successor. Replicated state is no longer accepted blindly, but it is still not backed by a multi-party consensus layer.

### Trust each local machine to protect its own secrets

Each participant machine is trusted to protect:

- wallet keys
- peer, protocol, and transport keys
- persisted table state
- local private hand material
- controller-origin protections when a browser client is in use

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

### Browser-client loss

If a browser client is unavailable:

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
- treat the host or failover successor as the practical gameplay coordinator
- treat events, transcripts, and snapshots as signed and replay-verified audit artifacts, not yet as fully quorum-finalized consensus proofs
- do not trust the indexer or browser client with money authority
- do not over-claim beyond the current dealerless transcript plus single-builder snapshot model
