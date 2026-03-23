#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

BASE="${BASE:-/tmp/parker-browser-2p-$$}"
INDEXER_PORT="${INDEXER_PORT:-}"
ALICE_PEER_PORT="${ALICE_PEER_PORT:-}"
BOB_PEER_PORT="${BOB_PEER_PORT:-}"
ALICE_UI_PORT="${ALICE_UI_PORT:-}"
BOB_UI_PORT="${BOB_UI_PORT:-}"
FAUCET_SATS="${FAUCET_SATS:-100000}"
NO_OPEN="${NO_OPEN:-0}"
BROWSER_APP="${BROWSER_APP:-Google Chrome}"
NIGIRI_DATADIR="${NIGIRI_DATADIR:-$HOME/Library/Application Support/Nigiri/parker-browser/$(printf '%s' "$BASE" | tr '/:' '__')}"

ALICE_ROOT="$BASE/alice"
BOB_ROOT="$BASE/bob"

ensure_node_toolchain() {
  local candidate
  for candidate in /opt/homebrew/bin /usr/local/bin "$HOME"/.nvm/versions/node/*/bin; do
    [[ -d "$candidate" ]] || continue
    case ":$PATH:" in
      *":$candidate:"*) ;;
      *) export PATH="$candidate:$PATH" ;;
    esac
  done

  command -v node >/dev/null 2>&1 || {
    echo "node must be available on PATH to run poker-regtest-ui-2p." >&2
    exit 1
  }
  command -v npm >/dev/null 2>&1 || {
    echo "npm must be available on PATH to run poker-regtest-ui-2p." >&2
    exit 1
  }
  command -v nigiri >/dev/null 2>&1 || {
    echo "nigiri must be available on PATH to run poker-regtest-ui-2p." >&2
    exit 1
  }
}

cleanup() {
  set +e
  stop_pid() {
    local pid="$1"
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  }
  [[ -n "${ALICE_CONTROLLER_PID:-}" ]] && stop_pid "$ALICE_CONTROLLER_PID"
  [[ -n "${BOB_CONTROLLER_PID:-}" ]] && stop_pid "$BOB_CONTROLLER_PID"
  [[ -n "${ALICE_DAEMON_PID:-}" ]] && stop_pid "$ALICE_DAEMON_PID"
  [[ -n "${BOB_DAEMON_PID:-}" ]] && stop_pid "$BOB_DAEMON_PID"
  [[ -n "${INDEXER_PID:-}" ]] && stop_pid "$INDEXER_PID"
  [[ -n "${NIGIRI_START_PID:-}" ]] && stop_pid "$NIGIRI_START_PID"
  nigiri --datadir "$NIGIRI_DATADIR" stop >/dev/null 2>&1 || true
}
trap cleanup EXIT

ensure_node_toolchain

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

wait_for_http() {
  local url="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-1}"
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if curl -fsS "$url" >/dev/null 2>&1; then
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
    status="$(nigiri --datadir "$NIGIRI_DATADIR" arkd wallet status 2>/dev/null || true)"
    if [[ "$status" == *"unlocked: true"* && "$status" == *"synced: true"* ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

seed_ark_liquidity() {
  wait_for_ark_wallet 60 >/dev/null
  local address
  address="$(nigiri --datadir "$NIGIRI_DATADIR" arkd wallet address | tr -d '\r' | tail -n 1)"
  [[ -n "$address" ]]
  for _ in {1..10}; do
    nigiri --datadir "$NIGIRI_DATADIR" faucet "$address" >/dev/null
  done
}

actor_flags() {
  local actor_root="$1"
  ACTOR_FLAGS=(
    --network regtest
    --indexer-url "http://127.0.0.1:${INDEXER_PORT}"
    --ark-server-url http://127.0.0.1:7070
    --boltz-url http://127.0.0.1:9069
    --nigiri-datadir "$NIGIRI_DATADIR"
    --daemon-dir "$actor_root/daemons"
    --profile-dir "$actor_root/profiles"
    --run-dir "$actor_root/runs"
  )
}

pcli_actor() {
  local actor_root="$1"
  local profile_name="$2"
  shift 2
  actor_flags "$actor_root"
  node --import tsx apps/cli/src/index.ts "$@" "${ACTOR_FLAGS[@]}" --profile "$profile_name"
}

start_daemon() {
  local label="$1"
  local actor_root="$2"
  local profile_name="$3"
  local peer_port="$4"
  mkdir -p "$actor_root"/{daemons,profiles,runs}
  actor_flags "$actor_root"
  node --import tsx apps/daemon/src/index.ts \
    "${ACTOR_FLAGS[@]}" \
    --profile "$profile_name" \
    --mode player \
    --peer-port "$peer_port" >"$BASE/${label}.daemon.log" 2>&1 &
  printf '%s\n' "$!"
}

start_controller() {
  local label="$1"
  local actor_root="$2"
  local controller_port="$3"
  PARKER_NETWORK=regtest \
  PARKER_CONTROLLER_PORT="$controller_port" \
  PARKER_INDEXER_URL="http://127.0.0.1:${INDEXER_PORT}" \
  PARKER_ARK_SERVER_URL=http://127.0.0.1:7070 \
  PARKER_BOLTZ_URL=http://127.0.0.1:9069 \
  PARKER_NIGIRI_DATADIR="$NIGIRI_DATADIR" \
  PARKER_DAEMON_DIR="$actor_root/daemons" \
  PARKER_PROFILE_DIR="$actor_root/profiles" \
  PARKER_RUN_DIR="$actor_root/runs" \
    node --import tsx apps/controller/src/index.ts >"$BASE/${label}.controller.log" 2>&1 &
  printf '%s\n' "$!"
}

open_tabs() {
  local alice_url="$1"
  local bob_url="$2"
  if [[ "$NO_OPEN" == "1" ]]; then
    return 0
  fi
  if command -v open >/dev/null 2>&1; then
    if open -a "$BROWSER_APP" "$alice_url" "$bob_url" >/dev/null 2>&1; then
      return 0
    fi
  fi
  echo "Open these URLs manually in Chrome:"
  echo "  Alice: $alice_url"
  echo "  Bob:   $bob_url"
}

INDEXER_PORT="${INDEXER_PORT:-$(free_port)}"
ALICE_PEER_PORT="${ALICE_PEER_PORT:-$(free_port)}"
BOB_PEER_PORT="${BOB_PEER_PORT:-$(free_port)}"
ALICE_UI_PORT="${ALICE_UI_PORT:-$(free_port)}"
BOB_UI_PORT="${BOB_UI_PORT:-$(free_port)}"

rm -rf "$BASE"
mkdir -p "$BASE"
mkdir -p "$ALICE_ROOT"/{daemons,profiles,runs}
mkdir -p "$BOB_ROOT"/{daemons,profiles,runs}
mkdir -p "$NIGIRI_DATADIR"

echo "Building the web bundle for controller-served UIs..."
npm run build -w @parker/web >/dev/null

echo "Starting Nigiri..."
nigiri --datadir "$NIGIRI_DATADIR" start --ark --ln --ci >"$BASE/nigiri.log" 2>&1 &
NIGIRI_START_PID=$!
wait_for_http "http://127.0.0.1:7070/v1/info" 120 1
seed_ark_liquidity

echo "Starting indexer on :${INDEXER_PORT}..."
HOST=127.0.0.1 PORT="$INDEXER_PORT" PARKER_NETWORK=regtest \
  node --import tsx apps/indexer/src/index.ts >"$BASE/indexer.log" 2>&1 &
INDEXER_PID=$!
wait_for_http "http://127.0.0.1:${INDEXER_PORT}/health" 30 1

echo "Starting Alice and Bob daemons..."
ALICE_DAEMON_PID="$(start_daemon alice "$ALICE_ROOT" alice "$ALICE_PEER_PORT")"
BOB_DAEMON_PID="$(start_daemon bob "$BOB_ROOT" bob "$BOB_PEER_PORT")"
sleep 2

echo "Bootstrapping Alice and Bob..."
pcli_actor "$ALICE_ROOT" alice bootstrap Alice --json >/dev/null
pcli_actor "$BOB_ROOT" bob bootstrap Bob --json >/dev/null

echo "Funding and onboarding wallets..."
pcli_actor "$ALICE_ROOT" alice wallet faucet "$FAUCET_SATS" --json >/dev/null
pcli_actor "$ALICE_ROOT" alice wallet onboard --json >/dev/null
pcli_actor "$BOB_ROOT" bob wallet faucet "$FAUCET_SATS" --json >/dev/null
pcli_actor "$BOB_ROOT" bob wallet onboard --json >/dev/null

echo "Starting controller UIs..."
ALICE_CONTROLLER_PID="$(start_controller alice "$ALICE_ROOT" "$ALICE_UI_PORT")"
BOB_CONTROLLER_PID="$(start_controller bob "$BOB_ROOT" "$BOB_UI_PORT")"

ALICE_URL="http://127.0.0.1:${ALICE_UI_PORT}"
BOB_URL="http://127.0.0.1:${BOB_UI_PORT}"

wait_for_http "${ALICE_URL}/health" 30 1
wait_for_http "${BOB_URL}/health" 30 1

open_tabs "$ALICE_URL" "$BOB_URL"

cat <<EOF

Two player browser UIs are ready:
  Alice: $ALICE_URL
  Bob:   $BOB_URL

State directory:
  $BASE

Suggested flow:
  1. Use Alice's tab to create a table.
  2. Copy the invite code from Alice's result panel.
  3. Paste it into Bob's "Join by invite" field.
  4. Play the hand from the two tabs.

Press Ctrl-C to stop the daemons, controllers, indexer, and Nigiri.
EOF

while true; do
  sleep 5
done
