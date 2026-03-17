# Trust Model

## Honest v1 Trust Statement

The mesh migration removes the central app coordinator from live gameplay, but it does **not** pretend to solve every trust problem.

## What v1 does guarantee

- wallet keys remain local to each user's daemon
- canonical gameplay is replicated as signed, append-only events
- host-only unsigned actions cannot create a valid cashout path
- witnesses can preserve logs and snapshots for between-hand failover
- the website and public indexers are optional and read-only

## What v1 still trusts

- Arkade operator availability still matters for live offchain Arkade operations
- `HostDealerV1` trusts the non-playing host with hidden-card privacy
- unfinished hands are not forced on Arkade; hard failures roll back to the last cooperative checkpoint

## What is explicitly not claimed

- no dealerless mental-poker privacy
- no browser-native peer parity
- no claim that the read-only website is part of gameplay consensus
- no claim that the current Arkade table-lock adapter is production-ready for mainnet

## Operational Guidance

- use regtest / signet / controlled deployments first
- prefer public tables with a witness
- prefer non-playing hosts for `HostDealerV1`
- treat the latest fully signed cooperative checkpoint as the last enforceable money state
