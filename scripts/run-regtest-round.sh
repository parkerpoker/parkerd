#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

BASE="${BASE:-/tmp/parker-auto-2p}"
SERVER_PORT="${SERVER_PORT:-3020}"
HOST_PORT="${HOST_PORT:-7101}"
WITNESS_PORT="${WITNESS_PORT:-7102}"
ALICE_PORT="${ALICE_PORT:-7103}"
BOB_PORT="${BOB_PORT:-7104}"
BUY_IN_SATS="${BUY_IN_SATS:-4000}"
FAUCET_SATS="${FAUCET_SATS:-100000}"

rm -rf "$BASE"
mkdir -p "$BASE"/{daemons,profiles,runs}

common_flags=(
  --network regtest
  --server-url "http://127.0.0.1:${SERVER_PORT}"
  --indexer-url "http://127.0.0.1:${SERVER_PORT}"
  --websocket-url "ws://127.0.0.1:${SERVER_PORT}/ws"
  --ark-server-url http://127.0.0.1:7070
  --boltz-url http://127.0.0.1:9069
  --daemon-dir "$BASE/daemons"
  --profile-dir "$BASE/profiles"
  --run-dir "$BASE/runs"
)

cleanup() {
  set +e
  [[ -n "${HOST_DAEMON_PID:-}" ]] && kill "$HOST_DAEMON_PID" 2>/dev/null
  [[ -n "${WITNESS_DAEMON_PID:-}" ]] && kill "$WITNESS_DAEMON_PID" 2>/dev/null
  [[ -n "${ALICE_DAEMON_PID:-}" ]] && kill "$ALICE_DAEMON_PID" 2>/dev/null
  [[ -n "${BOB_DAEMON_PID:-}" ]] && kill "$BOB_DAEMON_PID" 2>/dev/null
  [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null
  nigiri stop >/dev/null 2>&1 || true
}
trap cleanup EXIT

pcli() {
  node --import tsx apps/cli/src/index.ts "${common_flags[@]}" "$@"
}

pdaemon() {
  node --import tsx apps/cli/src/daemonEntry.ts "${common_flags[@]}" "$@"
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

echo "Starting Nigiri..."
nigiri start --ark --ln --ci

echo "Starting API server on :${SERVER_PORT}..."
HOST=127.0.0.1 PORT="$SERVER_PORT" PARKER_NETWORK=regtest WEBSOCKET_URL="ws://127.0.0.1:${SERVER_PORT}/ws" \
  node --import tsx apps/server/src/index.ts >"$BASE/server.log" 2>&1 &
SERVER_PID=$!

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

WITNESS_PEER_URL="$(printf '%s' "$WITNESS_BOOT" | json_field mesh.peerUrl)"
WITNESS_PEER_ID="$(printf '%s' "$WITNESS_BOOT" | json_field mesh.peerId)"
ALICE_PLAYER_ID="$(printf '%s' "$ALICE_BOOT" | json_field mesh.walletPlayerId)"
BOB_PLAYER_ID="$(printf '%s' "$BOB_BOOT" | json_field mesh.walletPlayerId)"

echo "Funding wallets..."
pcli faucet "$FAUCET_SATS" --profile alice --json >/dev/null
pcli onboard               --profile alice --json >/dev/null
pcli faucet "$FAUCET_SATS" --profile bob   --json >/dev/null
pcli onboard               --profile bob   --json >/dev/null

echo "Connecting host to witness..."
pcli network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null

echo "Creating table..."
CREATE_JSON="$(
  BASE="$BASE" SERVER_PORT="$SERVER_PORT" WITNESS_PEER_ID="$WITNESS_PEER_ID" \
  node --import tsx --input-type=module <<'EOF'
import { resolveCliRuntimeConfig } from "./apps/cli/src/config.ts";
import { DaemonRpcClient } from "./apps/cli/src/daemonClient.ts";

const cfg = resolveCliRuntimeConfig({
  network: "regtest",
  "server-url": `http://127.0.0.1:${process.env.SERVER_PORT}`,
  "indexer-url": `http://127.0.0.1:${process.env.SERVER_PORT}`,
  "websocket-url": `ws://127.0.0.1:${process.env.SERVER_PORT}/ws`,
  "ark-server-url": "http://127.0.0.1:7070",
  "boltz-url": "http://127.0.0.1:9069",
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

BASE="$BASE" SERVER_PORT="$SERVER_PORT" TABLE_ID="$TABLE_ID" ALICE_PLAYER_ID="$ALICE_PLAYER_ID" BOB_PLAYER_ID="$BOB_PLAYER_ID" \
node --import tsx --input-type=module <<'EOF'
import { resolveCliRuntimeConfig } from "./apps/cli/src/config.ts";
import { DaemonRpcClient } from "./apps/cli/src/daemonClient.ts";

const cfg = resolveCliRuntimeConfig({
  network: "regtest",
  "server-url": `http://127.0.0.1:${process.env.SERVER_PORT}`,
  "indexer-url": `http://127.0.0.1:${process.env.SERVER_PORT}`,
  "websocket-url": `ws://127.0.0.1:${process.env.SERVER_PORT}/ws`,
  "ark-server-url": "http://127.0.0.1:7070",
  "boltz-url": "http://127.0.0.1:9069",
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
