#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

BASE="${BASE:-/tmp/parker-auto-2p-$$}"
INDEXER_PORT="${INDEXER_PORT:-}"
HOST_PORT="${HOST_PORT:-}"
WITNESS_PORT="${WITNESS_PORT:-}"
ALICE_PORT="${ALICE_PORT:-}"
BOB_PORT="${BOB_PORT:-}"
BUY_IN_SATS="${BUY_IN_SATS:-4000}"
FAUCET_SATS="${FAUCET_SATS:-100000}"
NIGIRI_DATADIR="${NIGIRI_DATADIR:-$HOME/Library/Application Support/Nigiri/parker-auto/$(printf '%s' "$BASE" | tr '/:' '__')}"

common_flags=(
  --network regtest
  --indexer-url ""
  --ark-server-url http://127.0.0.1:7070
  --boltz-url http://127.0.0.1:9069
  --daemon-dir ""
  --profile-dir ""
  --run-dir ""
)

cleanup() {
  set +e
  stop_pid() {
    local pid="$1"
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  }
  [[ -n "${NIGIRI_START_PID:-}" ]] && stop_pid "$NIGIRI_START_PID"
  [[ -n "${HOST_DAEMON_PID:-}" ]] && stop_pid "$HOST_DAEMON_PID"
  [[ -n "${WITNESS_DAEMON_PID:-}" ]] && stop_pid "$WITNESS_DAEMON_PID"
  [[ -n "${ALICE_DAEMON_PID:-}" ]] && stop_pid "$ALICE_DAEMON_PID"
  [[ -n "${BOB_DAEMON_PID:-}" ]] && stop_pid "$BOB_DAEMON_PID"
  [[ -n "${INDEXER_PID:-}" ]] && stop_pid "$INDEXER_PID"
  nigiri --datadir "$NIGIRI_DATADIR" stop >/dev/null 2>&1 || true
}
trap cleanup EXIT

pcli() {
  local command="$1"
  shift
  node --import tsx apps/cli/src/index.ts "$command" "${common_flags[@]}" "$@"
}

pdaemon() {
  node --import tsx apps/daemon/src/index.ts "${common_flags[@]}" "$@"
}

json_field() {
  node --input-type=module -e '
    const path = process.argv[1].split(".");
    let s = "";
    process.stdin.on("data", d => s += d);
    process.stdin.on("end", () => {
      let v = JSON.parse(s);
      for (const k of path) v = v?.[k];
      if (typeof v === "object") console.log(JSON.stringify(v));
      else console.log(v ?? "");
    });
  ' "$1"
}

nigiri_cmd() {
  nigiri --datadir "$NIGIRI_DATADIR" "$@"
}

free_port() {
  node --input-type=module -e '
    import { createServer } from "node:net";
    const server = createServer();
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        process.exit(1);
        return;
      }
      console.log(address.port);
      server.close();
    });
  '
}

wait_for_http_json() {
  local url="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-1}"
  local body
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if body="$(curl -sS "$url" 2>/dev/null)" && [[ -n "$body" ]]; then
      printf '%s\n' "$body"
      return 0
    fi
    sleep "$sleep_seconds"
  done
  return 1
}

wait_for_ark_wallet() {
  local attempts="${1:-60}"
  local status
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    status="$(nigiri_cmd arkd wallet status 2>/dev/null || true)"
    if [[ "$status" == *"unlocked: true"* && "$status" == *"synced: true"* ]]; then
      printf '%s\n' "$status"
      return 0
    fi
    sleep 1
  done
  return 1
}

seed_ark_liquidity() {
  wait_for_ark_wallet 60 >/dev/null
  local address
  address="$(nigiri_cmd arkd wallet address | tr -d '\r' | tail -n 1)"
  [[ -n "$address" ]]
  for _ in {1..10}; do
    nigiri_cmd faucet "$address" >/dev/null
  done
}

INDEXER_PORT="${INDEXER_PORT:-$(free_port)}"
HOST_PORT="${HOST_PORT:-$(free_port)}"
WITNESS_PORT="${WITNESS_PORT:-$(free_port)}"
ALICE_PORT="${ALICE_PORT:-$(free_port)}"
BOB_PORT="${BOB_PORT:-$(free_port)}"

rm -rf "$BASE"
mkdir -p "$BASE"/{daemons,profiles,runs}
mkdir -p "$NIGIRI_DATADIR"

common_flags=(
  --network regtest
  --indexer-url "http://127.0.0.1:${INDEXER_PORT}"
  --ark-server-url http://127.0.0.1:7070
  --boltz-url http://127.0.0.1:9069
  --nigiri-datadir "$NIGIRI_DATADIR"
  --daemon-dir "$BASE/daemons"
  --profile-dir "$BASE/profiles"
  --run-dir "$BASE/runs"
)

echo "Starting Nigiri..."
nigiri_cmd start --ark --ln --ci >"$BASE/nigiri-start.log" 2>&1 &
NIGIRI_START_PID=$!
wait_for_http_json "http://127.0.0.1:7070/v1/info" 120 1 >/dev/null
seed_ark_liquidity
kill "$NIGIRI_START_PID" 2>/dev/null || true
wait "$NIGIRI_START_PID" 2>/dev/null || true

echo "Starting public indexer on :${INDEXER_PORT}..."
HOST=127.0.0.1 PORT="$INDEXER_PORT" PARKER_NETWORK=regtest \
  node --import tsx apps/indexer/src/index.ts >"$BASE/indexer.log" 2>&1 &
INDEXER_PID=$!

sleep 2

echo "Starting daemons..."
pdaemon --profile host    --mode host    --peer-port "$HOST_PORT" >"$BASE/host.log" 2>&1 &
HOST_DAEMON_PID=$!
pdaemon --profile witness --mode witness --peer-port "$WITNESS_PORT" >"$BASE/witness.log" 2>&1 &
WITNESS_DAEMON_PID=$!
pdaemon --profile alice   --mode player  --peer-port "$ALICE_PORT" >"$BASE/alice.log" 2>&1 &
ALICE_DAEMON_PID=$!
pdaemon --profile bob     --mode player  --peer-port "$BOB_PORT" >"$BASE/bob.log" 2>&1 &
BOB_DAEMON_PID=$!

sleep 2

echo "Bootstrapping identities..."
HOST_BOOT="$(pcli bootstrap Host --profile host --json)"
WITNESS_BOOT="$(pcli bootstrap Witness --profile witness --json)"
ALICE_BOOT="$(pcli bootstrap Alice --profile alice --json)"
BOB_BOOT="$(pcli bootstrap Bob --profile bob --json)"
WITNESS_STATUS="$(pcli daemon status --profile witness --json)"

WITNESS_PEER_URL="$(printf '%s' "$WITNESS_STATUS" | json_field data.metadata.peerUrl)"
WITNESS_PEER_ID="$(printf '%s' "$WITNESS_BOOT" | json_field data.mesh.peerId)"
ALICE_PLAYER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.mesh.walletPlayerId)"
BOB_PLAYER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.mesh.walletPlayerId)"

echo "Funding wallets..."
pcli wallet faucet "$FAUCET_SATS" --profile alice --json >/dev/null
pcli wallet onboard               --profile alice --json >/dev/null
pcli wallet faucet "$FAUCET_SATS" --profile bob   --json >/dev/null
pcli wallet onboard               --profile bob   --json >/dev/null

echo "Connecting host to witness..."
pcli network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null

echo "Creating table..."
CREATE_JSON="$(
  BASE="$BASE" INDEXER_PORT="$INDEXER_PORT" NIGIRI_DATADIR="$NIGIRI_DATADIR" WITNESS_PEER_ID="$WITNESS_PEER_ID" \
  node --import tsx --input-type=module <<'EOF'
import { DaemonRpcClient, resolveCliRuntimeConfig } from "@parker/daemon-runtime";

const cfg = resolveCliRuntimeConfig({
  network: "regtest",
  "indexer-url": `http://127.0.0.1:${process.env.INDEXER_PORT}`,
  "ark-server-url": "http://127.0.0.1:7070",
  "boltz-url": "http://127.0.0.1:9069",
  "nigiri-datadir": process.env.NIGIRI_DATADIR,
  "daemon-dir": `${process.env.BASE}/daemons`,
  "profile-dir": `${process.env.BASE}/profiles`,
  "run-dir": `${process.env.BASE}/runs`,
});

const client = new DaemonRpcClient("host", cfg);
const result = await client.meshCreateTable({
  name: "auto-regtest-table",
  public: false,
  witnessPeerIds: [process.env.WITNESS_PEER_ID],
});
console.log(JSON.stringify(result));
EOF
)"

INVITE_CODE="$(printf '%s' "$CREATE_JSON" | json_field inviteCode)"
TABLE_ID="$(printf '%s' "$CREATE_JSON" | json_field table.tableId)"

echo "TABLE_ID=$TABLE_ID"

echo "Joining players..."
pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile alice --json >/dev/null
pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile bob   --json >/dev/null

echo "Playing one hand automatically..."

BASE="$BASE" INDEXER_PORT="$INDEXER_PORT" NIGIRI_DATADIR="$NIGIRI_DATADIR" TABLE_ID="$TABLE_ID" ALICE_PLAYER_ID="$ALICE_PLAYER_ID" BOB_PLAYER_ID="$BOB_PLAYER_ID" \
node --import tsx --input-type=module <<'EOF'
import { DaemonRpcClient, resolveCliRuntimeConfig } from "@parker/daemon-runtime";

const cfg = resolveCliRuntimeConfig({
  network: "regtest",
  "indexer-url": `http://127.0.0.1:${process.env.INDEXER_PORT}`,
  "ark-server-url": "http://127.0.0.1:7070",
  "boltz-url": "http://127.0.0.1:9069",
  "nigiri-datadir": process.env.NIGIRI_DATADIR,
  "daemon-dir": `${process.env.BASE}/daemons`,
  "profile-dir": `${process.env.BASE}/profiles`,
  "run-dir": `${process.env.BASE}/runs`,
});

const tableId = process.env.TABLE_ID;
const alicePlayerId = process.env.ALICE_PLAYER_ID;
const bobPlayerId = process.env.BOB_PLAYER_ID;

const alice = new DaemonRpcClient("alice", cfg);
const bob = new DaemonRpcClient("bob", cfg);

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function actingProfile(publicState) {
  const actingSeat = publicState.actingSeatIndex;
  const aliceSeat = publicState.seatedPlayers.find((p) => p.playerId === alicePlayerId);
  const bobSeat = publicState.seatedPlayers.find((p) => p.playerId === bobPlayerId);
  if (!aliceSeat || !bobSeat) {
    throw new Error("missing seats");
  }
  return actingSeat === aliceSeat.seatIndex ? "alice" : "bob";
}

function nextAction(publicState, actorPlayerId) {
  const contribution = publicState.roundContributions[actorPlayerId] ?? 0;
  const toCall = Math.max(0, publicState.currentBetSats - contribution);
  if (publicState.phase === "preflop" && toCall === 0) {
    return { type: "bet", totalSats: Math.max(publicState.minRaiseToSats || 800, 800) };
  }
  if (toCall > 0) {
    return { type: "call" };
  }
  return { type: "check" };
}

async function sendActionWithRetry(client, payload) {
  for (let attempt = 0; attempt < 60; attempt += 1) {
    try {
      await client.meshSendAction(payload, tableId);
      return;
    } catch (error) {
      const message = String(error?.message ?? error);
      if (
        message.includes("cannot act while") ||
        message.includes("hand is still starting") ||
        message.includes("hand is not active")
      ) {
        await delay(150);
        continue;
      }
      throw error;
    }
  }
  throw new Error(`timed out waiting to send action ${payload.type}`);
}

for (let turn = 0; turn < 30; turn += 1) {
  const state = await alice.meshGetTable(tableId);
  const publicState = state.publicState;
  if (!publicState) {
    await delay(250);
    continue;
  }
  if (!publicState.handId || publicState.phase === null) {
    await delay(250);
    continue;
  }
  if (publicState.phase === "settled") {
    console.log(JSON.stringify({
      result: "hand-settled",
      balances: state.latestFullySignedSnapshot?.chipBalances ?? publicState.chipBalances,
      snapshotId: state.latestFullySignedSnapshot?.snapshotId ?? null,
    }, null, 2));
    process.exit(0);
  }

  const actor = actingProfile(publicState);
  const actorPlayerId = actor === "alice" ? alicePlayerId : bobPlayerId;
  const client = actor === "alice" ? alice : bob;
  const payload = nextAction(publicState, actorPlayerId);

  console.log(JSON.stringify({
    actor,
    currentBetSats: publicState.currentBetSats,
    payload,
    phase: publicState.phase,
    potSats: publicState.potSats,
  }));

  await sendActionWithRetry(client, payload);
  await delay(400);
}

throw new Error("hand did not settle in time");
EOF

echo "Final table state:"
pcli table watch "$TABLE_ID" --profile alice --json

echo "Cashing out..."
pcli funds cashout "$TABLE_ID" --profile alice --json
pcli funds cashout "$TABLE_ID" --profile bob   --json

echo "Final wallet summaries:"
pcli wallet --profile alice --json
pcli wallet --profile bob   --json

echo "Done. Logs are under $BASE"
