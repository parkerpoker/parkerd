#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
NIGIRI_BIN="$ROOT_DIR/scripts/bin/nigiri"
DOCKER_COMPOSE_BIN="$ROOT_DIR/scripts/bin/docker-compose"

BASE="${BASE:-/tmp/parker-auto-2p-$$}"
INDEXER_PORT="${INDEXER_PORT:-}"
HOST_PORT="${HOST_PORT:-}"
WITNESS_PORT="${WITNESS_PORT:-}"
ALICE_PORT="${ALICE_PORT:-}"
BOB_PORT="${BOB_PORT:-}"
USE_TOR="${USE_TOR:-false}"
PCLI_TIMEOUT_SECONDS="${PCLI_TIMEOUT_SECONDS:-}"
TOR_TARGET_HOST="${TOR_TARGET_HOST:-host.docker.internal}"
HOST_TOR_SOCKS_PORT="${HOST_TOR_SOCKS_PORT:-}"
HOST_TOR_CONTROL_PORT="${HOST_TOR_CONTROL_PORT:-}"
WITNESS_TOR_SOCKS_PORT="${WITNESS_TOR_SOCKS_PORT:-}"
WITNESS_TOR_CONTROL_PORT="${WITNESS_TOR_CONTROL_PORT:-}"
ALICE_TOR_SOCKS_PORT="${ALICE_TOR_SOCKS_PORT:-}"
ALICE_TOR_CONTROL_PORT="${ALICE_TOR_CONTROL_PORT:-}"
BOB_TOR_SOCKS_PORT="${BOB_TOR_SOCKS_PORT:-}"
BOB_TOR_CONTROL_PORT="${BOB_TOR_CONTROL_PORT:-}"
BUY_IN_SATS="${BUY_IN_SATS:-4000}"
FAUCET_SATS="${FAUCET_SATS:-100000}"
NIGIRI_DATADIR="${NIGIRI_DATADIR:-$HOME/Library/Application Support/Nigiri/parker-auto/$(printf '%s' "$BASE" | tr '/:' '__')}"
TOR_PROJECT="parker-round-$(printf '%s' "$BASE" | tr -cs '[:alnum:]' '-')"
TOR_COMPOSE_FILE="$BASE/tor/docker-compose.yml"
TOR_STATE_BASE="${TOR_STATE_BASE:-$ROOT_DIR/.tmp/tor-round/$TOR_PROJECT}"

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

tor_enabled() {
  case "$(printf '%s' "$USE_TOR" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes) return 0 ;;
    *) return 1 ;;
  esac
}

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
  if tor_enabled; then
    "$DOCKER_COMPOSE_BIN" version >/dev/null 2>&1 || {
      echo "docker compose must be available on PATH to run poker-regtest-round in Tor mode." >&2
      exit 1
    }
    command -v nc >/dev/null 2>&1 || {
      echo "nc must be available on PATH to wait for Tor bootstrap." >&2
      exit 1
    }
  fi
}

terminate_pid() {
  local pid="$1"
  local i
  [[ -n "$pid" ]] || return 0
  kill "$pid" 2>/dev/null || true
  for ((i = 0; i < 20; i += 1)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  kill -9 "$pid" 2>/dev/null || true
}

cleanup() {
  set +e
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
  if tor_enabled && [[ -f "$TOR_COMPOSE_FILE" ]]; then
    run_with_timeout 15 "$DOCKER_COMPOSE_BIN" -f "$TOR_COMPOSE_FILE" -p "$TOR_PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  stop_nigiri_stack || true
  rm -rf "$TOR_STATE_BASE"
}
trap cleanup EXIT INT TERM HUP

ensure_toolchains

command_timeout_seconds() {
  if [[ -n "$PCLI_TIMEOUT_SECONDS" ]]; then
    printf '%s\n' "$PCLI_TIMEOUT_SECONDS"
    return 0
  fi
  if tor_enabled; then
    printf '30\n'
    return 0
  fi
  printf '10\n'
}

run_with_timeout() {
  local timeout_seconds="$1"
  shift

  LC_ALL=C LANG=C LC_CTYPE=C /usr/bin/perl -e '
    use strict;
    use warnings;

    my $timeout = shift @ARGV;
    my $pid = fork();
    die "fork failed\n" unless defined $pid;

    if ($pid == 0) {
      exec @ARGV or die "exec failed: $!\n";
    }

    local $SIG{ALRM} = sub {
      kill "TERM", $pid;
      select undef, undef, undef, 0.25;
      kill "KILL", $pid;
      print STDERR "command timed out after ${timeout}s\n";
      exit 124;
    };

    alarm $timeout;
    waitpid($pid, 0);
    alarm 0;
    exit($? >> 8);
  ' "$timeout_seconds" "$@"
}

pcli() {
  local command="$1"
  shift
  run_with_timeout "$(command_timeout_seconds)" "$ROOT_DIR/scripts/bin/parker-cli" "$command" "${common_flags[@]}" "$@"
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

docker_compose() {
  "$DOCKER_COMPOSE_BIN" -f "$TOR_COMPOSE_FILE" -p "$TOR_PROJECT" "$@"
}

wait_for_file() {
  local path="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-0.5}"
  local i
  for ((i = 0; i < attempts; i += 1)); do
    if [[ -f "$path" ]]; then
      return 0
    fi
    sleep "$sleep_seconds"
  done
  echo "timed out waiting for file $path" >&2
  return 1
}

wait_for_tcp_port() {
  local host="$1"
  local port="$2"
  local attempts="${3:-120}"
  local sleep_seconds="${4:-0.5}"
  local i
  for ((i = 0; i < attempts; i += 1)); do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      return 0
    fi
    sleep "$sleep_seconds"
  done
  echo "timed out waiting for TCP $host:$port" >&2
  return 1
}

tor_cookie_hex() {
  od -An -tx1 -v "$1" | tr -d ' \n'
}

query_tor_bootstrap() {
  local control_port="$1"
  local cookie_path="$2"
  local cookie_hex

  cookie_hex="$(tor_cookie_hex "$cookie_path")"
  {
    printf 'AUTHENTICATE %s\r\n' "$cookie_hex"
    printf 'GETINFO status/bootstrap-phase\r\n'
    printf 'QUIT\r\n'
  } | nc 127.0.0.1 "$control_port" 2>/dev/null || true
}

wait_for_tor_bootstrap() {
  local label="$1"
  local control_port="$2"
  local cookie_path="$3"
  local response=""

  wait_for_file "$cookie_path" 240 0.5
  wait_for_tcp_port 127.0.0.1 "$control_port" 240 0.5

  for ((attempt = 0; attempt < 360; attempt += 1)); do
    response="$(query_tor_bootstrap "$control_port" "$cookie_path")"
    if [[ "$response" == *"250-status/bootstrap-phase="* ]] && [[ "$response" == *"PROGRESS=100"* ]]; then
      return 0
    fi
    sleep 1
  done

  echo "timed out waiting for Tor bootstrap for $label on control port $control_port" >&2
  printf '%s\n' "$response" >&2
  return 1
}

write_tor_compose_file() {
  mkdir -p "$TOR_STATE_BASE/host" "$TOR_STATE_BASE/witness" "$TOR_STATE_BASE/alice" "$TOR_STATE_BASE/bob"
  cat >"$TOR_COMPOSE_FILE" <<EOF
services:
  tor-host:
    build:
      context: $ROOT_DIR/ops/tor
    ports:
      - "127.0.0.1:${HOST_TOR_SOCKS_PORT}:9050"
      - "127.0.0.1:${HOST_TOR_CONTROL_PORT}:9051"
  tor-witness:
    build:
      context: $ROOT_DIR/ops/tor
    ports:
      - "127.0.0.1:${WITNESS_TOR_SOCKS_PORT}:9050"
      - "127.0.0.1:${WITNESS_TOR_CONTROL_PORT}:9051"
  tor-alice:
    build:
      context: $ROOT_DIR/ops/tor
    ports:
      - "127.0.0.1:${ALICE_TOR_SOCKS_PORT}:9050"
      - "127.0.0.1:${ALICE_TOR_CONTROL_PORT}:9051"
  tor-bob:
    build:
      context: $ROOT_DIR/ops/tor
    ports:
      - "127.0.0.1:${BOB_TOR_SOCKS_PORT}:9050"
      - "127.0.0.1:${BOB_TOR_CONTROL_PORT}:9051"
EOF
}

copy_tor_cookie() {
  local service="$1"
  local destination="$2"
  local container_id=""

  mkdir -p "$(dirname "$destination")"
  for ((attempt = 0; attempt < 120; attempt += 1)); do
    container_id="$(docker_compose ps -q "$service" 2>/dev/null || true)"
    if [[ -n "$container_id" ]] && docker cp "${container_id}:/var/lib/tor/control_auth_cookie" "$destination" >/dev/null 2>&1; then
      chmod 600 "$destination" 2>/dev/null || true
      return 0
    fi
    sleep 0.5
  done

  echo "timed out copying Tor cookie for $service" >&2
  return 1
}

start_tor_sidecars() {
  write_tor_compose_file
  docker_compose up -d --build
  copy_tor_cookie tor-host "$TOR_STATE_BASE/host/control_auth_cookie"
  copy_tor_cookie tor-witness "$TOR_STATE_BASE/witness/control_auth_cookie"
  copy_tor_cookie tor-alice "$TOR_STATE_BASE/alice/control_auth_cookie"
  copy_tor_cookie tor-bob "$TOR_STATE_BASE/bob/control_auth_cookie"
  wait_for_tor_bootstrap host "$HOST_TOR_CONTROL_PORT" "$TOR_STATE_BASE/host/control_auth_cookie"
  wait_for_tor_bootstrap witness "$WITNESS_TOR_CONTROL_PORT" "$TOR_STATE_BASE/witness/control_auth_cookie"
  wait_for_tor_bootstrap alice "$ALICE_TOR_CONTROL_PORT" "$TOR_STATE_BASE/alice/control_auth_cookie"
  wait_for_tor_bootstrap bob "$BOB_TOR_CONTROL_PORT" "$TOR_STATE_BASE/bob/control_auth_cookie"
}

wait_for_peer_url() {
  local profile="$1"
  local require_onion="${2:-false}"
  local status peer_url

  for ((attempt = 0; attempt < 240; attempt += 1)); do
    if status="$(pcli daemon status --profile "$profile" --json 2>/dev/null)"; then
      peer_url="$(printf '%s' "$status" | json_field data.metadata.peerUrl)"
      if [[ -n "$peer_url" && "$peer_url" != "null" ]]; then
        if [[ "$require_onion" != "true" || "$peer_url" == parker://*.onion:* ]]; then
          printf '%s\n' "$peer_url"
          return 0
        fi
      fi
    fi
    sleep 0.5
  done

  echo "timed out waiting for peer URL for profile $profile" >&2
  return 1
}

wait_for_bootstrap_peer_id() {
  local profile="$1"
  local endpoint="$2"
  local expected_peer_id="$3"
  local alias="${4:-}"
  local label="${5:-$profile -> $endpoint}"
  local output=""
  local peer_id=""
  local attempts=60
  local sleep_seconds=0.5
  local i

  if tor_enabled; then
    attempts=180
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if [[ -n "$alias" ]]; then
      output="$(pcli network bootstrap add "$endpoint" "$alias" --profile "$profile" --json 2>/dev/null || true)"
    else
      output="$(pcli network bootstrap add "$endpoint" --profile "$profile" --json 2>/dev/null || true)"
    fi
    if [[ -n "$output" ]]; then
      peer_id="$(printf '%s' "$output" | json_field data.peerId 2>/dev/null || true)"
      if [[ "$peer_id" == "$expected_peer_id" ]]; then
        printf '%s\n' "$output"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for peer bootstrap reachability: $label" >&2
  return 1
}

retry_pcli_json() {
  local label="$1"
  local attempts="$2"
  local sleep_seconds="$3"
  shift 3
  local output=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli "$@" 2>&1)"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "command failed after retries: $label" >&2
  printf '%s\n' "$output" >&2
  return 1
}

start_profile_daemon() {
  local profile="$1"
  local mode="$2"
  local peer_port="$3"
  local log_path="$4"
  local socks_port="${5:-}"
  local control_port="${6:-}"
  local cookie_path="${7:-}"

  if tor_enabled; then
    PARKER_TOR_SOCKS_ADDR="127.0.0.1:${socks_port}" \
    PARKER_TOR_CONTROL_ADDR="127.0.0.1:${control_port}" \
    PARKER_TOR_COOKIE_AUTH="$cookie_path" \
    pdaemon --profile "$profile" --mode "$mode" --peer-port "$peer_port" >"$log_path" 2>&1 &
  else
    pdaemon --profile "$profile" --mode "$mode" --peer-port "$peer_port" >"$log_path" 2>&1 &
  fi
  printf '%s\n' "$!"
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
  local i

  for ((i = 0; i < 80; i += 1)); do
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

force_cleanup_nigiri_docker() {
  local compose_file="$NIGIRI_DATADIR/docker-compose.yml"

  if [[ -f "$compose_file" ]]; then
    run_with_timeout 15 "$DOCKER_COMPOSE_BIN" -f "$compose_file" -p nigiri down -v --remove-orphans >/dev/null 2>&1 || true
  fi

  run_with_timeout 15 docker rm -f \
    ark \
    ark-wallet \
    bitcoin \
    chopsticks \
    cln \
    electrs \
    lnd \
    nigiri-nbxplorer-1 \
    nigiri-postgres-1 \
    tap >/dev/null 2>&1 || true
  run_with_timeout 15 docker network rm nigiri >/dev/null 2>&1 || true
}

cleanup_nigiri_data() {
  rm -rf "$NIGIRI_DATADIR"
}

stop_nigiri_stack() {
  local pid=""
  local i

  nigiri_cmd stop >/dev/null 2>&1 &
  pid=$!
  for ((i = 0; i < 50; i += 1)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  terminate_pid "$pid"
  wait "$pid" 2>/dev/null || true
  force_cleanup_nigiri_docker
  cleanup_nigiri_data
}

free_port() {
  pdevtool free-port
}

wait_for_http_json() {
  local url="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-1}"
  local body
  local i
  for ((i = 0; i < attempts; i += 1)); do
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
  local i
  for ((i = 0; i < attempts; i += 1)); do
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
  wait_for_tcp_port 127.0.0.1 3000 240 0.5 >/dev/null
  local address
  local funded=0
  local i
  address="$(nigiri_cmd arkd wallet address | tr -d '\r' | tail -n 1)"
  [[ -n "$address" ]]
  for ((i = 0; i < 30 && funded < 10; i += 1)); do
    if nigiri_cmd faucet "$address" >/dev/null 2>&1; then
      funded=$((funded + 1))
      continue
    fi
    sleep 1
  done
  if ((funded < 10)); then
    echo "timed out seeding Ark liquidity via nigiri faucet" >&2
    return 1
  fi
}

wait_for_ark_ready() {
  local attempts="${1:-120}"
  local body=""
  local signer_pubkey=""
  local forfeit_pubkey=""
  local i

  for ((i = 0; i < attempts; i += 1)); do
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

start_nigiri_stack() {
  local attempt

  for attempt in 1 2 3; do
    stop_nigiri_stack
    mkdir -p "$NIGIRI_DATADIR"
    : >"$BASE/nigiri-start.log"

    echo "Starting Nigiri (attempt ${attempt}/3)..."
    nigiri_cmd start --ark --ln --ci >"$BASE/nigiri-start.log" 2>&1 &
    NIGIRI_START_PID=$!

    if wait_for_http_json "http://127.0.0.1:7070/v1/info" 120 1 >/dev/null &&
      seed_ark_liquidity &&
      wait_for_ark_ready 120 >/dev/null; then
      terminate_pid "$NIGIRI_START_PID"
      wait "$NIGIRI_START_PID" 2>/dev/null || true
      NIGIRI_START_PID=""
      return 0
    fi

    echo "Nigiri startup attempt ${attempt} failed; retrying..." >&2
    terminate_pid "$NIGIRI_START_PID"
    wait "$NIGIRI_START_PID" 2>/dev/null || true
    NIGIRI_START_PID=""
    stop_nigiri_stack
    sleep 2
  done

  echo "Nigiri failed to become ready after 3 attempts" >&2
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
  local state_json=""
  local can_act=""
  local phase=""
  local attempts=60
  local i
  local args=(table action "$action" --table-id "$TABLE_ID" --profile "$actor" --json)
  if [[ -n "$amount" ]]; then
    args=(table action "$action" "$amount" --table-id "$TABLE_ID" --profile "$actor" --json)
  fi
  if tor_enabled; then
    attempts=180
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(pcli "${args[@]}" 2>&1)"; then
      return 0
    fi
    if [[ "$output" == *"cannot act while"* || "$output" == *"hand is still starting"* || "$output" == *"hand is not active"* ]]; then
      sleep 0.15
      continue
    fi
    if tor_enabled; then
      state_json="$(pcli table watch "$TABLE_ID" --profile "$actor" --json 2>/dev/null || true)"
      if [[ -n "$state_json" ]]; then
        can_act="$(printf '%s' "$state_json" | json_field data.local.canAct)"
        phase="$(printf '%s' "$state_json" | json_field data.publicState.phase)"
        if [[ "$can_act" != "true" || "$phase" == "settled" ]]; then
          return 0
        fi
      fi
      sleep 0.5
      continue
    fi
    printf '%s\n' "$output" >&2
    return 1
  done

  echo "timed out waiting to send action $action for $actor" >&2
  return 1
}

watch_table_state_with_retry() {
  local profile="${1:-alice}"
  local output=""
  local attempts=60
  local sleep_seconds=0.5
  local i

  if tor_enabled; then
    attempts=180
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(pcli table watch "$TABLE_ID" --profile "$profile" --json 2>&1)"; then
      printf '%s\n' "$output"
      return 0
    fi
    if tor_enabled; then
      sleep "$sleep_seconds"
      continue
    fi
    printf '%s\n' "$output" >&2
    return 1
  done

  echo "timed out waiting to watch table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_table_status() {
  local profile="$1"
  local desired_status="${2:-active}"
  local min_occupied="${3:-2}"
  local output=""
  local status=""
  local occupied=""
  local attempts=60
  local sleep_seconds=0.5
  local i

  if tor_enabled; then
    attempts=180
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(pcli table watch "$TABLE_ID" --profile "$profile" --json 2>/dev/null)"; then
      status="$(printf '%s' "$output" | json_field data.config.status)"
      occupied="$(printf '%s' "$output" | json_field data.config.occupiedSeats)"
      if [[ "$status" == "$desired_status" && "${occupied:-0}" -ge "$min_occupied" ]]; then
        printf '%s\n' "$output"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for table $TABLE_ID to reach status=$desired_status for $profile" >&2
  return 1
}

play_hand_automatically() {
  local state_json=""
  local hand_id=""
  local hand_number=""
  local initial_hand_number=""
  local phase=""
  local latest_snapshot_phase=""
  local latest_snapshot_hand_number=""
  local action_line=""
  local actor=""
  local action=""
  local amount=""
  local current_bet=""
  local pot_sats=""
  local turns=30
  local turn

  if tor_enabled; then
    turns=120
  fi

  for ((turn = 0; turn < turns; turn += 1)); do
    if tor_enabled; then
      state_json="$(watch_table_state_with_retry host)"
    else
      state_json="$(watch_table_state_with_retry alice)"
    fi
    hand_id="$(printf '%s' "$state_json" | json_field data.publicState.handId)"
    hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber)"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase)"
    latest_snapshot_phase="$(printf '%s' "$state_json" | json_field data.latestSnapshot.phase)"
    latest_snapshot_hand_number="$(printf '%s' "$state_json" | json_field data.latestSnapshot.handNumber)"
    if [[ -z "$hand_id" || -z "$phase" || "$phase" == "null" ]]; then
      sleep 0.25
      continue
    fi
    if [[ -z "$initial_hand_number" && -n "$hand_number" && "$hand_number" != "null" ]]; then
      initial_hand_number="$hand_number"
    fi
    if [[ "$phase" == "settled" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    if [[ -n "$initial_hand_number" && "$initial_hand_number" != "null" && "$latest_snapshot_phase" == "settled" && "$latest_snapshot_hand_number" == "$initial_hand_number" ]]; then
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
if tor_enabled; then
  HOST_TOR_SOCKS_PORT="${HOST_TOR_SOCKS_PORT:-$(free_port)}"
  HOST_TOR_CONTROL_PORT="${HOST_TOR_CONTROL_PORT:-$(free_port)}"
  WITNESS_TOR_SOCKS_PORT="${WITNESS_TOR_SOCKS_PORT:-$(free_port)}"
  WITNESS_TOR_CONTROL_PORT="${WITNESS_TOR_CONTROL_PORT:-$(free_port)}"
  ALICE_TOR_SOCKS_PORT="${ALICE_TOR_SOCKS_PORT:-$(free_port)}"
  ALICE_TOR_CONTROL_PORT="${ALICE_TOR_CONTROL_PORT:-$(free_port)}"
  BOB_TOR_SOCKS_PORT="${BOB_TOR_SOCKS_PORT:-$(free_port)}"
  BOB_TOR_CONTROL_PORT="${BOB_TOR_CONTROL_PORT:-$(free_port)}"
fi

rm -rf "$BASE" "$TOR_STATE_BASE"
mkdir -p "$BASE"/{daemons,profiles,runs,tor}
mkdir -p "$TOR_STATE_BASE"
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
if tor_enabled; then
  common_flags+=(
    --peer-host 0.0.0.0
    --use-tor
    --tor-target-host "$TOR_TARGET_HOST"
  )
fi

start_nigiri_stack

if tor_enabled; then
  echo "Starting Tor sidecars..."
  start_tor_sidecars
fi

echo "Starting public indexer on :${INDEXER_PORT}..."
HOST=127.0.0.1 PORT="$INDEXER_PORT" PARKER_NETWORK=regtest PARKER_DATADIR="$BASE/indexer" \
  "$ROOT_DIR/scripts/bin/parker-indexer" >"$BASE/indexer.log" 2>&1 &
INDEXER_PID=$!

sleep 2

echo "Starting daemons..."
HOST_DAEMON_PID="$(start_profile_daemon host host "$HOST_PORT" "$BASE/host.log" "$HOST_TOR_SOCKS_PORT" "$HOST_TOR_CONTROL_PORT" "$TOR_STATE_BASE/host/control_auth_cookie")"
WITNESS_DAEMON_PID="$(start_profile_daemon witness witness "$WITNESS_PORT" "$BASE/witness.log" "$WITNESS_TOR_SOCKS_PORT" "$WITNESS_TOR_CONTROL_PORT" "$TOR_STATE_BASE/witness/control_auth_cookie")"
ALICE_DAEMON_PID="$(start_profile_daemon alice player "$ALICE_PORT" "$BASE/alice.log" "$ALICE_TOR_SOCKS_PORT" "$ALICE_TOR_CONTROL_PORT" "$TOR_STATE_BASE/alice/control_auth_cookie")"
BOB_DAEMON_PID="$(start_profile_daemon bob player "$BOB_PORT" "$BASE/bob.log" "$BOB_TOR_SOCKS_PORT" "$BOB_TOR_CONTROL_PORT" "$TOR_STATE_BASE/bob/control_auth_cookie")"

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
HOST_PEER_ID="$(printf '%s' "$HOST_BOOT" | json_field data.transport.peer.peerId)"
if tor_enabled; then
  HOST_PEER_URL="$(wait_for_peer_url host true)"
  WITNESS_PEER_URL="$(wait_for_peer_url witness true)"
  ALICE_PEER_URL="$(wait_for_peer_url alice true)"
  BOB_PEER_URL="$(wait_for_peer_url bob true)"
  echo "Tor peer URLs:"
  printf '  host=%s\n  witness=%s\n  alice=%s\n  bob=%s\n' "$HOST_PEER_URL" "$WITNESS_PEER_URL" "$ALICE_PEER_URL" "$BOB_PEER_URL"
else
  WITNESS_PEER_URL="$(wait_for_peer_url witness false)"
fi
WITNESS_PEER_ID="$(printf '%s' "$WITNESS_BOOT" | json_field data.transport.peer.peerId)"
ALICE_PEER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.transport.peer.peerId)"
BOB_PEER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.transport.peer.peerId)"
ALICE_PLAYER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.transport.peer.walletPlayerId)"
BOB_PLAYER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.transport.peer.walletPlayerId)"

if tor_enabled; then
  echo "Waiting for Tor peer reachability..."
  wait_for_bootstrap_peer_id host "$WITNESS_PEER_URL" "$WITNESS_PEER_ID" witness "host -> witness" >/dev/null
  wait_for_bootstrap_peer_id host "$ALICE_PEER_URL" "$ALICE_PEER_ID" alice "host -> alice" >/dev/null
  wait_for_bootstrap_peer_id host "$BOB_PEER_URL" "$BOB_PEER_ID" bob "host -> bob" >/dev/null
  wait_for_bootstrap_peer_id alice "$HOST_PEER_URL" "$HOST_PEER_ID" host "alice -> host" >/dev/null
  wait_for_bootstrap_peer_id bob "$HOST_PEER_URL" "$HOST_PEER_ID" host "bob -> host" >/dev/null
fi

echo "Funding wallets..."
retry_pcli_json "alice faucet" 20 1 wallet faucet "$FAUCET_SATS" --profile alice --json >/dev/null
retry_pcli_json "alice onboard" 20 1 wallet onboard --profile alice --json >/dev/null
retry_pcli_json "bob faucet" 20 1 wallet faucet "$FAUCET_SATS" --profile bob --json >/dev/null
retry_pcli_json "bob onboard" 20 1 wallet onboard --profile bob --json >/dev/null

echo "Connecting host to witness..."
if tor_enabled; then
  retry_pcli_json "host bootstrap witness over Tor" 180 1 network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null
else
  pcli network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null
fi

echo "Creating table..."
CREATE_JSON="$(pcli table create --name auto-regtest-table --witness-peer-ids "$WITNESS_PEER_ID" --profile host --json)"

INVITE_CODE="$(printf '%s' "$CREATE_JSON" | json_field data.inviteCode)"
TABLE_ID="$(printf '%s' "$CREATE_JSON" | json_field data.table.tableId)"

echo "TABLE_ID=$TABLE_ID"

echo "Joining players..."
if tor_enabled; then
  retry_pcli_json "alice buy-in over Tor" 180 1 funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile alice --json >/dev/null
  retry_pcli_json "bob buy-in over Tor" 180 1 funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile bob --json >/dev/null
  echo "Waiting for players to observe the active table over Tor..."
  wait_for_table_status alice active 2 >/dev/null
  wait_for_table_status bob active 2 >/dev/null
else
  pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile alice --json >/dev/null
  pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile bob   --json >/dev/null
fi

echo "Playing one hand automatically..."
play_hand_automatically >/dev/null

echo "Final table state:"
watch_table_state_with_retry alice

echo "Cashing out..."
if tor_enabled; then
  retry_pcli_json "alice cashout over Tor" 60 1 funds cashout "$TABLE_ID" --profile alice --json
  retry_pcli_json "bob cashout over Tor" 60 1 funds cashout "$TABLE_ID" --profile bob --json
else
  pcli funds cashout "$TABLE_ID" --profile alice --json
  pcli funds cashout "$TABLE_ID" --profile bob   --json
fi

echo "Final wallet summaries:"
pcli wallet --profile alice --json
pcli wallet --profile bob   --json

echo "Done. Logs are under $BASE"
