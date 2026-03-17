# Current Risk Register

## High Risk

### Coordinator is the live control-plane single point of failure

- Evidence:
  - [`apps/server/src/app.ts`](../../apps/server/src/app.ts)
  - [`apps/server/src/service.ts`](../../apps/server/src/service.ts)
  - [`apps/cli/src/tableSocketClient.ts`](../../apps/cli/src/tableSocketClient.ts)
- Impact:
  - no gameplay without the coordinator
  - no peer-to-peer continuity if the websocket relay disappears

### Hidden cards are not actually private after both reveals

- Evidence:
  - [`packages/game-engine/src/rng.ts`](../../packages/game-engine/src/rng.ts)
  - [`apps/web/src/App.tsx`](../../apps/web/src/App.tsx)
  - [`apps/cli/src/playerClient.ts`](../../apps/cli/src/playerClient.ts)
- Impact:
  - both players can derive the full deck once reveals are posted
  - current deterministic dealing is auditable, but not suitable as a truthful private-card model

### Canonical state is mutable and coordinator-owned

- Evidence:
  - [`apps/server/src/db.ts`](../../apps/server/src/db.ts)
  - [`apps/server/src/service.ts`](../../apps/server/src/service.ts)
- Impact:
  - replay and failover depend on coordinator-side mutable state
  - no daemon can independently reconstruct canonical table history from signed events alone

### No epoch / hash-link / replay defense at the canonical table layer

- Evidence:
  - [`packages/protocol/src/index.ts`](../../packages/protocol/src/index.ts)
  - [`apps/server/src/service.ts`](../../apps/server/src/service.ts)
- Impact:
  - stale or duplicated messages are only partially defended
  - host rotation is impossible without inventing an epoch boundary

## Medium Risk

### Timeout enforcement is centralized

- Evidence:
  - [`apps/server/src/service.ts`](../../apps/server/src/service.ts)
- Impact:
  - the coordinator decides when timeout folds occur
  - timeout liveness disappears with the server

### Reconnect is coordinator resume, not peer replay

- Evidence:
  - [`apps/server/src/app.ts`](../../apps/server/src/app.ts)
  - [`apps/cli/src/playerClient.ts`](../../apps/cli/src/playerClient.ts)
- Impact:
  - reconnect assumes the same coordinator remains reachable
  - clients do not reconstruct state from peer-replicated logs

### Website and CLI share the same write path

- Evidence:
  - [`apps/web/src/lib/api.ts`](../../apps/web/src/lib/api.ts)
  - [`apps/cli/src/api.ts`](../../apps/cli/src/api.ts)
- Impact:
  - browser UX is coupled to the same coordinator that drives gameplay
  - there is no clean read-only website path today

## Lower Risk / Existing Strengths

### Wallet custody is already local

- Evidence:
  - [`apps/cli/src/walletRuntime.ts`](../../apps/cli/src/walletRuntime.ts)
  - [`packages/settlement/src/index.ts`](../../packages/settlement/src/index.ts)

### Game rules are already transport-separable

- Evidence:
  - [`packages/game-engine/src/holdem.ts`](../../packages/game-engine/src/holdem.ts)

### The repo already has a local daemon control surface

- Evidence:
  - [`apps/cli/src/daemonProcess.ts`](../../apps/cli/src/daemonProcess.ts)
  - [`apps/cli/src/daemonClient.ts`](../../apps/cli/src/daemonClient.ts)
