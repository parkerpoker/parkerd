# Open Questions

These questions stay visible because the migration intentionally keeps some trust/model decisions explicit instead of pretending they are solved.

## Active Questions

- Can the current deterministic dealing code be wrapped behind a `DealerEngine` without a major rewrite?
  - Current answer: partially. The rules engine is reusable, but current commit/reveal dealing is not an honest private-card model.
- Does the current game engine already support append-only action logs, or does it mutate state in place?
  - Current answer: it mutates state in place and returns a cloned next state; the canonical event log still needs to live above it.
- Should witness be required for all public tables, or only recommended?
  - Current answer: recommended by default in code, but not globally enforced for private tables.
- What is the smallest viable cooperative checkpoint cadence for money safety without too much signing overhead?
  - Current answer: implemented at buy-in lock, hand start, street boundaries, hand result, cashout, and host rotation.
- Should public tables require non-playing hosts in v1?
  - Current answer: yes for `HostDealerV1`.
- Should private tables be allowed without witnesses?
  - Current answer: yes, but failover guarantees degrade.
- Should public spectators be delayed by one hand or by time?
  - Current answer: delayed by one completed hand in the public indexer path.
- What alias / identity model should public players and AI agents use?
  - Current answer: aliases remain user-chosen strings bound to protocol keys per table session.
- What exact Arkade contract pattern is best for the first buy-in lock implementation?
  - Current answer: still open. The code now isolates this behind `TableFundsProvider` so the first concrete contract can land without rewriting gameplay.
