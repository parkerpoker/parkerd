#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
NIGIRI_BIN="$ROOT_DIR/scripts/bin/nigiri"

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
  --datadir ""
  --daemon-dir ""
  --profile-dir ""
  --run-dir ""
)

ensure_toolchains() {
  local candidate
  for candidate in /opt/homebrew/bin /usr/local/bin "$HOME"/.gvm/gos/go1.24.0/bin; do
    [[ -d "$candidate" ]] || continue
    case ":$PATH:" in
      *":$candidate:"*) ;;
      *) export PATH="$candidate:$PATH" ;;
    esac
  done

  "$NIGIRI_BIN" --version >/dev/null 2>&1 || {
    echo "nigiri must be available on PATH to run poker-regtest-round." >&2
    exit 1
  }
  command -v curl >/dev/null 2>&1 || {
    echo "curl must be available on PATH to run poker-regtest-round." >&2
    exit 1
  }
  if ! command -v go >/dev/null 2>&1 && [[ ! -x /opt/homebrew/bin/go ]] && [[ ! -x "$HOME/.gvm/gos/go1.24.0/bin/go" ]]; then
    echo "go must be available on PATH to run parker-cli and parker-daemon." >&2
    exit 1
  fi
}

cleanup() {
  set +e
  terminate_pid() {
    local pid="$1"
    [[ -n "$pid" ]] || return 0
    kill "$pid" 2>/dev/null || true
    for ((attempt = 0; attempt < 20; attempt += 1)); do
      if ! kill -0 "$pid" 2>/dev/null; then
        return 0
      fi
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
  }

  collect_run_daemon_pids() {
    local metadata pid command_line
    for metadata in "$BASE"/daemons/*.json; do
      [[ -f "$metadata" ]] || continue
      pid="$(json_field pid <"$metadata" 2>/dev/null || true)"
      [[ -n "$pid" ]] && printf '%s\n' "$pid"
    done

    ps -axo pid=,command= 2>/dev/null | while read -r pid command_line; do
      if [[ "$command_line" == *"parker-daemon-go"* ]] && [[ "$command_line" == *"--daemon-dir $BASE/daemons"* ]]; then
        printf '%s\n' "$pid"
      fi
    done
  }

  if [[ -d "$BASE/daemons" ]]; then
    "$ROOT_DIR/scripts/bin/parker-cli" daemon stop "${common_flags[@]}" --profile host >/dev/null 2>&1 || true
    "$ROOT_DIR/scripts/bin/parker-cli" daemon stop "${common_flags[@]}" --profile witness >/dev/null 2>&1 || true
    "$ROOT_DIR/scripts/bin/parker-cli" daemon stop "${common_flags[@]}" --profile alice >/dev/null 2>&1 || true
    "$ROOT_DIR/scripts/bin/parker-cli" daemon stop "${common_flags[@]}" --profile bob >/dev/null 2>&1 || true
  fi

  collect_run_daemon_pids | sort -u | while read -r pid; do
    terminate_pid "$pid"
  done

  [[ -n "${NIGIRI_START_PID:-}" ]] && terminate_pid "$NIGIRI_START_PID"
  [[ -n "${HOST_DAEMON_PID:-}" ]] && terminate_pid "$HOST_DAEMON_PID"
  [[ -n "${WITNESS_DAEMON_PID:-}" ]] && terminate_pid "$WITNESS_DAEMON_PID"
  [[ -n "${ALICE_DAEMON_PID:-}" ]] && terminate_pid "$ALICE_DAEMON_PID"
  [[ -n "${BOB_DAEMON_PID:-}" ]] && terminate_pid "$BOB_DAEMON_PID"
  [[ -n "${INDEXER_PID:-}" ]] && terminate_pid "$INDEXER_PID"
  "$NIGIRI_BIN" --datadir "$NIGIRI_DATADIR" stop >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM HUP

ensure_toolchains

pcli() {
  local command="$1"
  shift
  "$ROOT_DIR/scripts/bin/parker-cli" "$command" "${common_flags[@]}" "$@"
}

pdaemon() {
  "$ROOT_DIR/scripts/bin/parker-daemon" "${common_flags[@]}" "$@"
}

pdevtool() {
  "$ROOT_DIR/scripts/bin/parker-devtool" "$@"
}

json_field() {
  pdevtool json-field "$1"
}

assert_go_daemon_active() {
  local profile="$1"
  local metadata_path="$BASE/daemons/${profile}.json"
  local log_path="$BASE/daemons/${profile}.log"
  local pid=""
  local command_line=""

  for ((attempt = 0; attempt < 40; attempt += 1)); do
    if [[ -f "$metadata_path" ]]; then
      pid="$(json_field pid <"$metadata_path")"
      if [[ -n "$pid" ]]; then
        command_line="$(ps -p "$pid" -o command= 2>/dev/null || true)"
        if [[ "$command_line" == *"parker-daemon-go"* ]] && [[ -f "$log_path" ]] && grep -q 'go parker daemon starting' "$log_path"; then
          return 0
        fi
      fi
    fi
    sleep 0.25
  done

  echo "expected Go daemon evidence for profile $profile; pid=${pid:-missing} command=${command_line:-missing}" >&2
  return 1
}

wait_for_daemon_reachable() {
  local profile="$1"
  local status reachable

  for ((attempt = 0; attempt < 80; attempt += 1)); do
    if status="$(pcli daemon status --profile "$profile" --json 2>/dev/null)"; then
      reachable="$(printf '%s' "$status" | json_field data.reachable)"
      if [[ "$reachable" == "true" ]]; then
        return 0
      fi
    fi
    sleep 0.25
  done

  echo "timed out waiting for daemon $profile to become reachable" >&2
  return 1
}

nigiri_cmd() {
  "$NIGIRI_BIN" --datadir "$NIGIRI_DATADIR" "$@"
}

free_port() {
  pdevtool free-port
}

wait_for_http_json() {
  local url="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-1}"
  local body
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if body="$(curl -fsS "$url" 2>/dev/null)" && [[ -n "$body" ]]; then
      printf '%s\n' "$body"
      return 0
    fi
    sleep "$sleep_seconds"
  done
  return 1
}

wait_for_ark_wallet() {
  local attempts="${1:-240}"
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
  wait_for_ark_wallet 240 >/dev/null
  local address
  address="$(nigiri_cmd arkd wallet address | tr -d '\r' | tail -n 1)"
  [[ -n "$address" ]]
  for _ in {1..10}; do
    nigiri_cmd faucet "$address" >/dev/null
  done
}

wait_for_ark_ready() {
  local attempts="${1:-120}"
  local body=""
  local signer_pubkey=""
  local forfeit_pubkey=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if body="$(curl -fsS http://127.0.0.1:7070/v1/info 2>/dev/null)" && [[ -n "$body" ]]; then
      signer_pubkey="$(printf '%s' "$body" | json_field signerPubkey)"
      forfeit_pubkey="$(printf '%s' "$body" | json_field forfeitPubkey)"
      if [[ -n "$signer_pubkey" && "$signer_pubkey" != "null" ]]; then
        printf '%s\n' "$body"
        return 0
      fi
    fi
    sleep 1
  done

  echo "timed out waiting for Ark server signer pubkey" >&2
  return 1
}

select_table_action() {
  pdevtool select-table-action --alice "$ALICE_PLAYER_ID" --bob "$BOB_PLAYER_ID"
}

send_table_action_with_retry() {
  local actor="$1"
  local action="$2"
  local amount="${3:-}"
  local output=""
  local args=(table action "$action" --table-id "$TABLE_ID" --profile "$actor" --json)
  if [[ -n "$amount" ]]; then
    args=(table action "$action" "$amount" --table-id "$TABLE_ID" --profile "$actor" --json)
  fi

  for ((attempt = 0; attempt < 60; attempt += 1)); do
    if output="$(pcli "${args[@]}" 2>&1)"; then
      return 0
    fi
    if [[ "$output" == *"cannot act while"* || "$output" == *"hand is still starting"* || "$output" == *"hand is not active"* ]]; then
      sleep 0.15
      continue
    fi
    printf '%s\n' "$output" >&2
    return 1
  done

  echo "timed out waiting to send action $action for $actor" >&2
  return 1
}

play_hand_automatically() {
  local state_json=""
  local hand_id=""
  local phase=""
  local action_line=""
  local actor=""
  local action=""
  local amount=""
  local current_bet=""
  local pot_sats=""

  for ((turn = 0; turn < 30; turn += 1)); do
    state_json="$(pcli table watch "$TABLE_ID" --profile alice --json)"
    hand_id="$(printf '%s' "$state_json" | json_field data.publicState.handId)"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase)"
    if [[ -z "$hand_id" || -z "$phase" || "$phase" == "null" ]]; then
      sleep 0.25
      continue
    fi
    if [[ "$phase" == "settled" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi

    action_line="$(printf '%s' "$state_json" | select_table_action)"
    if [[ -z "$action_line" ]]; then
      sleep 0.25
      continue
    fi
    if [[ "$action_line" == "settled" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi

    actor=""
    action=""
    amount=""
    read -r actor action amount <<<"$action_line"
    current_bet="$(printf '%s' "$state_json" | json_field data.publicState.currentBetSats)"
    pot_sats="$(printf '%s' "$state_json" | json_field data.publicState.potSats)"
    printf '{"actor":"%s","currentBetSats":%s,"payload":{"type":"%s"%s},"phase":"%s","potSats":%s}\n' \
      "$actor" \
      "${current_bet:-0}" \
      "$action" \
      "$(if [[ -n "$amount" ]]; then printf ',"totalSats":%s' "$amount"; fi)" \
      "$phase" \
      "${pot_sats:-0}"
    send_table_action_with_retry "$actor" "$action" "$amount"
    sleep 0.4
  done

  echo "hand did not settle in time" >&2
  return 1
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
  --datadir "$BASE/data"
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
wait_for_ark_ready 120 >/dev/null
kill "$NIGIRI_START_PID" 2>/dev/null || true
wait "$NIGIRI_START_PID" 2>/dev/null || true

echo "Starting public indexer on :${INDEXER_PORT}..."
HOST=127.0.0.1 PORT="$INDEXER_PORT" PARKER_NETWORK=regtest PARKER_DATADIR="$BASE/indexer" \
  "$ROOT_DIR/scripts/bin/parker-indexer" >"$BASE/indexer.log" 2>&1 &
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

assert_go_daemon_active host
assert_go_daemon_active witness
assert_go_daemon_active alice
assert_go_daemon_active bob
wait_for_daemon_reachable host
wait_for_daemon_reachable witness
wait_for_daemon_reachable alice
wait_for_daemon_reachable bob
echo "Verified Go daemon ownership via metadata PID, startup banner, and live socket reachability."

echo "Bootstrapping identities..."
HOST_BOOT="$(pcli bootstrap Host --profile host --json)"
WITNESS_BOOT="$(pcli bootstrap Witness --profile witness --json)"
ALICE_BOOT="$(pcli bootstrap Alice --profile alice --json)"
BOB_BOOT="$(pcli bootstrap Bob --profile bob --json)"
WITNESS_STATUS="$(pcli daemon status --profile witness --json)"

WITNESS_PEER_URL="$(printf '%s' "$WITNESS_STATUS" | json_field data.metadata.peerUrl)"
WITNESS_PEER_ID="$(printf '%s' "$WITNESS_BOOT" | json_field data.transport.peer.peerId)"
ALICE_PLAYER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.transport.peer.walletPlayerId)"
BOB_PLAYER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.transport.peer.walletPlayerId)"

echo "Funding wallets..."
pcli wallet faucet "$FAUCET_SATS" --profile alice --json >/dev/null
pcli wallet onboard               --profile alice --json >/dev/null
pcli wallet faucet "$FAUCET_SATS" --profile bob   --json >/dev/null
pcli wallet onboard               --profile bob   --json >/dev/null

echo "Connecting host to witness..."
pcli network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null

echo "Creating table..."
CREATE_JSON="$(pcli table create --name auto-regtest-table --witness-peer-ids "$WITNESS_PEER_ID" --profile host --json)"

INVITE_CODE="$(printf '%s' "$CREATE_JSON" | json_field data.inviteCode)"
TABLE_ID="$(printf '%s' "$CREATE_JSON" | json_field data.table.tableId)"

echo "TABLE_ID=$TABLE_ID"

echo "Joining players..."
pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile alice --json >/dev/null
pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile bob   --json >/dev/null

echo "Playing one hand automatically..."
play_hand_automatically >/dev/null

echo "Final table state:"
pcli table watch "$TABLE_ID" --profile alice --json

echo "Cashing out..."
pcli funds cashout "$TABLE_ID" --profile alice --json
pcli funds cashout "$TABLE_ID" --profile bob   --json

echo "Final wallet summaries:"
pcli wallet --profile alice --json
pcli wallet --profile bob   --json

echo "Done. Logs are under $BASE"
