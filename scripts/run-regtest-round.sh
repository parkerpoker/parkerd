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
SETUP_ONLY="${SETUP_ONLY:-false}"
KEEP_FAILED_RUN="${KEEP_FAILED_RUN:-false}"
ROUND_SCENARIO="${ROUND_SCENARIO:-standard-4d}"
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
ROUND_SCENARIO_STANDARD="standard-4d"
ROUND_SCENARIO_HOST_PLAYER="host-player-2d"

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

setting_enabled() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes) return 0 ;;
    *) return 1 ;;
  esac
}

tor_enabled() {
  setting_enabled "$USE_TOR"
}

setup_only_enabled() {
  setting_enabled "$SETUP_ONLY"
}

keep_failed_run_enabled() {
  setting_enabled "$KEEP_FAILED_RUN"
}

host_player_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_HOST_PLAYER" ]]
}

validate_round_scenario() {
  case "$ROUND_SCENARIO" in
    "$ROUND_SCENARIO_STANDARD"|"$ROUND_SCENARIO_HOST_PLAYER") ;;
    *)
      echo "unsupported ROUND_SCENARIO=$ROUND_SCENARIO" >&2
      exit 1
      ;;
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
  local exit_status=$?
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
  if keep_failed_run_enabled && [[ "$exit_status" -ne 0 ]]; then
    echo "Preserving failed run state under $BASE" >&2
    echo "Preserving failed Nigiri datadir under $NIGIRI_DATADIR" >&2
    return
  fi
  rm -rf "$TOR_STATE_BASE"
}
trap cleanup EXIT INT TERM HUP

ensure_toolchains
validate_round_scenario

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

launch_background() {
  local log_path="$1"
  shift

  if setup_only_enabled; then
    LAUNCHED_PID="$(
      LC_ALL=C LANG=C LC_CTYPE=C /usr/bin/perl -MPOSIX=setsid -e '
        use strict;
        use warnings;

        my $log_path = shift @ARGV;
        my $pid = fork();
        die "fork failed\n" unless defined $pid;

        if ($pid) {
          print "$pid\n";
          exit 0;
        }

        die "setsid failed\n" unless defined setsid();
        open STDIN, "<", "/dev/null" or die "open stdin failed: $!\n";
        open STDOUT, ">>", $log_path or die "open stdout failed: $!\n";
        open STDERR, ">&STDOUT" or die "redirect stderr failed: $!\n";
        exec @ARGV or die "exec failed: $!\n";
      ' "$log_path" "$@"
    )"
  else
    "$@" >"$log_path" 2>&1 &
    LAUNCHED_PID="$!"
  fi
}

start_profile_daemon() {
  local profile="$1"
  local mode="$2"
  local peer_port="$3"
  local log_path="$4"
  local socks_port="${5:-}"
  local control_port="${6:-}"
  local cookie_path="${7:-}"
  local command=(
    "$ROOT_DIR/scripts/bin/parker-daemon"
    "${common_flags[@]}"
    --profile "$profile"
    --mode "$mode"
    --peer-port "$peer_port"
  )

  if tor_enabled; then
    launch_background "$log_path" \
      env \
      PARKER_TOR_SOCKS_ADDR="127.0.0.1:${socks_port}" \
      PARKER_TOR_CONTROL_ADDR="127.0.0.1:${control_port}" \
      PARKER_TOR_COOKIE_AUTH="$cookie_path" \
      "${command[@]}"
    return 0
  fi

  launch_background "$log_path" "${command[@]}"
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

prepare_nigiri_data_dirs() {
  mkdir -p \
    "$NIGIRI_DATADIR/volumes/bitcoin" \
    "$NIGIRI_DATADIR/volumes/elements" \
    "$NIGIRI_DATADIR/volumes/postgres" \
    "$NIGIRI_DATADIR/volumes/tapd" \
    "$NIGIRI_DATADIR/volumes/ark/wallet" \
    "$NIGIRI_DATADIR/volumes/ark/data" \
    "$NIGIRI_DATADIR/volumes/lnd" \
    "$NIGIRI_DATADIR/volumes/nbxplorer" \
    "$NIGIRI_DATADIR/volumes/lightningd"

  chmod -R 0777 "$NIGIRI_DATADIR/volumes" 2>/dev/null || true
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
  if ! keep_failed_run_enabled; then
    cleanup_nigiri_data
  fi
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
    prepare_nigiri_data_dirs
    : >"$BASE/nigiri-start.log"

    echo "Starting Nigiri (attempt ${attempt}/3)..."
    nigiri_cmd start --ark --ln --ci >"$BASE/nigiri-start.log" 2>&1 &
    NIGIRI_START_PID=$!
    prepare_nigiri_data_dirs

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

prebuild_parker_go_binaries() {
  echo "Prebuilding Parker Go binaries..."
  PARKER_BUILD_ONLY=1 "$ROOT_DIR/scripts/bin/parker-daemon" >/dev/null
  PARKER_BUILD_ONLY=1 "$ROOT_DIR/scripts/bin/parker-cli" >/dev/null
}

select_table_action() {
  local args=(
    select-table-action
    --player "$PLAYER_ONE_PROFILE=$PLAYER_ONE_PLAYER_ID"
    --player "$PLAYER_TWO_PROFILE=$PLAYER_TWO_PLAYER_ID"
  )
  if host_player_scenario_enabled; then
    args+=(--avoid-showdown)
  fi
  pdevtool "${args[@]}"
}

fund_and_onboard_profile() {
  local profile="$1"
  retry_pcli_json "$profile faucet" 20 1 wallet faucet "$FAUCET_SATS" --profile "$profile" --json >/dev/null
  retry_pcli_json "$profile onboard" 20 1 wallet onboard --profile "$profile" --json >/dev/null
}

buy_in_profile() {
  local profile="$1"

  if tor_enabled; then
    retry_pcli_json "$profile buy-in over Tor" 180 1 funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile "$profile" --json >/dev/null
    return 0
  fi

  pcli funds buy-in "$INVITE_CODE" "$BUY_IN_SATS" --profile "$profile" --json >/dev/null
}

cashout_profile() {
  local profile="$1"

  if tor_enabled; then
    retry_pcli_json "$profile cashout over Tor" 60 1 funds cashout "$TABLE_ID" --profile "$profile" --json >/dev/null
    return 0
  fi

  pcli funds cashout "$TABLE_ID" --profile "$profile" --json >/dev/null
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

write_runtime_env() {
  local runtime_env_path="$BASE/runtime.env"

  {
    printf 'export ROUND_SCENARIO=%q\n' "$ROUND_SCENARIO"
    printf 'export ROOT_DIR=%q\n' "$ROOT_DIR"
    printf 'export BASE=%q\n' "$BASE"
    printf 'export USE_TOR=%q\n' "$USE_TOR"
    printf 'export PARKER_NETWORK=%q\n' "regtest"
    printf 'export PARKER_ARK_SERVER_URL=%q\n' "http://127.0.0.1:7070"
    printf 'export PARKER_BOLTZ_URL=%q\n' "http://127.0.0.1:9069"
    printf 'export PARKER_INDEXER_URL=%q\n' "http://127.0.0.1:${INDEXER_PORT}"
    printf 'export PARKER_DATADIR=%q\n' "$BASE/data"
    printf 'export PARKER_NIGIRI_DATADIR=%q\n' "$NIGIRI_DATADIR"
    printf 'export PARKER_DAEMON_DIR=%q\n' "$BASE/daemons"
    printf 'export PARKER_PROFILE_DIR=%q\n' "$BASE/profiles"
    printf 'export PARKER_RUN_DIR=%q\n' "$BASE/runs"
    printf 'export INDEXER_PORT=%q\n' "$INDEXER_PORT"
    printf 'export HOST_PORT=%q\n' "$HOST_PORT"
    printf 'export WITNESS_PORT=%q\n' "$WITNESS_PORT"
    printf 'export ALICE_PORT=%q\n' "$ALICE_PORT"
    printf 'export BOB_PORT=%q\n' "$BOB_PORT"
    printf 'export HOST_DAEMON_PID=%q\n' "${HOST_DAEMON_PID:-}"
    printf 'export WITNESS_DAEMON_PID=%q\n' "${WITNESS_DAEMON_PID:-}"
    printf 'export ALICE_DAEMON_PID=%q\n' "${ALICE_DAEMON_PID:-}"
    printf 'export BOB_DAEMON_PID=%q\n' "${BOB_DAEMON_PID:-}"
    printf 'export INDEXER_PID=%q\n' "${INDEXER_PID:-}"
    printf 'export HOST_PEER_ID=%q\n' "${HOST_PEER_ID:-}"
    printf 'export WITNESS_PEER_ID=%q\n' "${WITNESS_PEER_ID:-}"
    printf 'export ALICE_PEER_ID=%q\n' "${ALICE_PEER_ID:-}"
    printf 'export BOB_PEER_ID=%q\n' "${BOB_PEER_ID:-}"
    printf 'export HOST_PEER_URL=%q\n' "${HOST_PEER_URL:-}"
    printf 'export WITNESS_PEER_URL=%q\n' "${WITNESS_PEER_URL:-}"
    printf 'export ALICE_PEER_URL=%q\n' "${ALICE_PEER_URL:-}"
    printf 'export BOB_PEER_URL=%q\n' "${BOB_PEER_URL:-}"
    printf 'export HOST_PLAYER_ID=%q\n' "${HOST_PLAYER_ID:-}"
    printf 'export ALICE_PLAYER_ID=%q\n' "${ALICE_PLAYER_ID:-}"
    printf 'export BOB_PLAYER_ID=%q\n' "${BOB_PLAYER_ID:-}"
    printf 'export PLAYER_ONE_PROFILE=%q\n' "${PLAYER_ONE_PROFILE:-}"
    printf 'export PLAYER_ONE_PLAYER_ID=%q\n' "${PLAYER_ONE_PLAYER_ID:-}"
    printf 'export PLAYER_TWO_PROFILE=%q\n' "${PLAYER_TWO_PROFILE:-}"
    printf 'export PLAYER_TWO_PLAYER_ID=%q\n' "${PLAYER_TWO_PLAYER_ID:-}"
    printf 'export INVITE_CODE=%q\n' "${INVITE_CODE:-}"
    printf 'export TABLE_ID=%q\n' "${TABLE_ID:-}"
    printf 'export TOR_PROJECT=%q\n' "$TOR_PROJECT"
    printf 'export TOR_COMPOSE_FILE=%q\n' "$TOR_COMPOSE_FILE"
    printf 'export TOR_STATE_BASE=%q\n' "$TOR_STATE_BASE"
    printf 'export HOST_TOR_SOCKS_PORT=%q\n' "${HOST_TOR_SOCKS_PORT:-}"
    printf 'export HOST_TOR_CONTROL_PORT=%q\n' "${HOST_TOR_CONTROL_PORT:-}"
    printf 'export WITNESS_TOR_SOCKS_PORT=%q\n' "${WITNESS_TOR_SOCKS_PORT:-}"
    printf 'export WITNESS_TOR_CONTROL_PORT=%q\n' "${WITNESS_TOR_CONTROL_PORT:-}"
    printf 'export ALICE_TOR_SOCKS_PORT=%q\n' "${ALICE_TOR_SOCKS_PORT:-}"
    printf 'export ALICE_TOR_CONTROL_PORT=%q\n' "${ALICE_TOR_CONTROL_PORT:-}"
    printf 'export BOB_TOR_SOCKS_PORT=%q\n' "${BOB_TOR_SOCKS_PORT:-}"
    printf 'export BOB_TOR_CONTROL_PORT=%q\n' "${BOB_TOR_CONTROL_PORT:-}"
  } >"$runtime_env_path"
}

print_local_stack_summary() {
  local runtime_env_path="$BASE/runtime.env"

  echo "Local regtest stack is ready."
  printf 'ROUND_SCENARIO=%s\n' "$ROUND_SCENARIO"
  printf 'BASE=%s\n' "$BASE"
  printf 'RUNTIME_ENV=%s\n' "$runtime_env_path"
  printf 'INDEXER_URL=http://127.0.0.1:%s\n' "$INDEXER_PORT"
  printf 'HOST_PEER_URL=%s\n' "${HOST_PEER_URL:-}"
  printf 'WITNESS_PEER_URL=%s\n' "${WITNESS_PEER_URL:-}"
  printf 'ALICE_PEER_URL=%s\n' "${ALICE_PEER_URL:-}"
  printf 'BOB_PEER_URL=%s\n' "${BOB_PEER_URL:-}"
  printf 'INVITE_CODE=%s\n' "$INVITE_CODE"
  printf 'TABLE_ID=%s\n' "$TABLE_ID"
  printf 'LOG_DIR=%s\n' "$BASE"
  printf 'SOURCE_ENV=source %q\n' "$runtime_env_path"
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
  local max_wait_seconds=45
  local start_epoch=0
  local turn

  if tor_enabled; then
    max_wait_seconds=120
  fi

  start_epoch="$(date +%s)"
  for ((turn = 0; ; turn += 1)); do
    if (( "$(date +%s)" - start_epoch >= max_wait_seconds )); then
      break
    fi
    state_json="$(watch_table_state_with_retry "$WATCH_PROFILE")"
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

write_table_artifact() {
  local profile="$1"
  local path="$2"
  mkdir -p "$(dirname "$path")"
  watch_table_state_with_retry "$profile" >"$path"
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

PLAYER_ONE_PROFILE="alice"
PLAYER_ONE_PLAYER_ID=""
PLAYER_ONE_PEER_ID=""
PLAYER_ONE_PEER_URL=""
PLAYER_TWO_PROFILE="bob"
PLAYER_TWO_PLAYER_ID=""
PLAYER_TWO_PEER_ID=""
PLAYER_TWO_PEER_URL=""
WATCH_PROFILE="alice"

if host_player_scenario_enabled; then
  PLAYER_ONE_PROFILE="host"
  PLAYER_TWO_PROFILE="alice"
  WATCH_PROFILE="host"
fi
if tor_enabled; then
  WATCH_PROFILE="host"
fi

prebuild_parker_go_binaries
start_nigiri_stack

if tor_enabled; then
  echo "Starting Tor sidecars..."
  start_tor_sidecars
fi

echo "Starting public indexer on :${INDEXER_PORT}..."
launch_background "$BASE/indexer.log" \
  env \
  HOST=127.0.0.1 \
  PORT="$INDEXER_PORT" \
  PARKER_NETWORK=regtest \
  PARKER_DATADIR="$BASE/indexer" \
  "$ROOT_DIR/scripts/bin/parker-indexer"
INDEXER_PID="$LAUNCHED_PID"

sleep 2

echo "Starting daemons..."
start_profile_daemon host host "$HOST_PORT" "$BASE/host.log" "$HOST_TOR_SOCKS_PORT" "$HOST_TOR_CONTROL_PORT" "$TOR_STATE_BASE/host/control_auth_cookie"
HOST_DAEMON_PID="$LAUNCHED_PID"
if ! host_player_scenario_enabled; then
  start_profile_daemon witness witness "$WITNESS_PORT" "$BASE/witness.log" "$WITNESS_TOR_SOCKS_PORT" "$WITNESS_TOR_CONTROL_PORT" "$TOR_STATE_BASE/witness/control_auth_cookie"
  WITNESS_DAEMON_PID="$LAUNCHED_PID"
fi
start_profile_daemon alice player "$ALICE_PORT" "$BASE/alice.log" "$ALICE_TOR_SOCKS_PORT" "$ALICE_TOR_CONTROL_PORT" "$TOR_STATE_BASE/alice/control_auth_cookie"
ALICE_DAEMON_PID="$LAUNCHED_PID"
if ! host_player_scenario_enabled; then
  start_profile_daemon bob player "$BOB_PORT" "$BASE/bob.log" "$BOB_TOR_SOCKS_PORT" "$BOB_TOR_CONTROL_PORT" "$TOR_STATE_BASE/bob/control_auth_cookie"
  BOB_DAEMON_PID="$LAUNCHED_PID"
fi

sleep 2

assert_go_daemon_active host
assert_go_daemon_active alice
wait_for_daemon_reachable host
wait_for_daemon_reachable alice
if ! host_player_scenario_enabled; then
  assert_go_daemon_active witness
  assert_go_daemon_active bob
  wait_for_daemon_reachable witness
  wait_for_daemon_reachable bob
fi
echo "Verified Go daemon ownership via metadata PID, startup banner, and live socket reachability."

echo "Bootstrapping identities..."
HOST_BOOT="$(pcli bootstrap Host --profile host --json)"
ALICE_BOOT="$(pcli bootstrap Alice --profile alice --json)"
if ! host_player_scenario_enabled; then
  WITNESS_BOOT="$(pcli bootstrap Witness --profile witness --json)"
  BOB_BOOT="$(pcli bootstrap Bob --profile bob --json)"
fi
HOST_PEER_ID="$(printf '%s' "$HOST_BOOT" | json_field data.transport.peer.peerId)"
HOST_PLAYER_ID="$(printf '%s' "$HOST_BOOT" | json_field data.transport.peer.walletPlayerId)"
HOST_PEER_URL="$(wait_for_peer_url host "$(if tor_enabled; then printf 'true'; else printf 'false'; fi)")"
if tor_enabled; then
  ALICE_PEER_URL="$(wait_for_peer_url alice true)"
  if ! host_player_scenario_enabled; then
    WITNESS_PEER_URL="$(wait_for_peer_url witness true)"
    BOB_PEER_URL="$(wait_for_peer_url bob true)"
  fi
  echo "Tor peer URLs:"
  printf '  host=%s\n' "$HOST_PEER_URL"
  if ! host_player_scenario_enabled; then
    printf '  witness=%s\n' "$WITNESS_PEER_URL"
  fi
  printf '  alice=%s\n' "$ALICE_PEER_URL"
  if ! host_player_scenario_enabled; then
    printf '  bob=%s\n' "$BOB_PEER_URL"
  fi
else
  ALICE_PEER_URL="$(wait_for_peer_url alice false)"
  if ! host_player_scenario_enabled; then
    WITNESS_PEER_URL="$(wait_for_peer_url witness false)"
    BOB_PEER_URL="$(wait_for_peer_url bob false)"
  fi
fi
ALICE_PEER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.transport.peer.peerId)"
ALICE_PLAYER_ID="$(printf '%s' "$ALICE_BOOT" | json_field data.transport.peer.walletPlayerId)"
if ! host_player_scenario_enabled; then
  WITNESS_PEER_ID="$(printf '%s' "$WITNESS_BOOT" | json_field data.transport.peer.peerId)"
  BOB_PEER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.transport.peer.peerId)"
  BOB_PLAYER_ID="$(printf '%s' "$BOB_BOOT" | json_field data.transport.peer.walletPlayerId)"
fi

PLAYER_ONE_PLAYER_ID="$ALICE_PLAYER_ID"
PLAYER_ONE_PEER_ID="$ALICE_PEER_ID"
PLAYER_ONE_PEER_URL="$ALICE_PEER_URL"
PLAYER_TWO_PLAYER_ID="${BOB_PLAYER_ID:-}"
PLAYER_TWO_PEER_ID="${BOB_PEER_ID:-}"
PLAYER_TWO_PEER_URL="${BOB_PEER_URL:-}"
if host_player_scenario_enabled; then
  PLAYER_ONE_PLAYER_ID="$HOST_PLAYER_ID"
  PLAYER_ONE_PEER_ID="$HOST_PEER_ID"
  PLAYER_ONE_PEER_URL="$HOST_PEER_URL"
  PLAYER_TWO_PLAYER_ID="$ALICE_PLAYER_ID"
  PLAYER_TWO_PEER_ID="$ALICE_PEER_ID"
  PLAYER_TWO_PEER_URL="$ALICE_PEER_URL"
fi

if tor_enabled; then
  echo "Waiting for Tor peer reachability..."
  if host_player_scenario_enabled; then
    wait_for_bootstrap_peer_id host "$ALICE_PEER_URL" "$ALICE_PEER_ID" alice "host -> alice" >/dev/null
    wait_for_bootstrap_peer_id alice "$HOST_PEER_URL" "$HOST_PEER_ID" host "alice -> host" >/dev/null
  else
    wait_for_bootstrap_peer_id host "$WITNESS_PEER_URL" "$WITNESS_PEER_ID" witness "host -> witness" >/dev/null
    wait_for_bootstrap_peer_id host "$ALICE_PEER_URL" "$ALICE_PEER_ID" alice "host -> alice" >/dev/null
    wait_for_bootstrap_peer_id host "$BOB_PEER_URL" "$BOB_PEER_ID" bob "host -> bob" >/dev/null
    wait_for_bootstrap_peer_id alice "$HOST_PEER_URL" "$HOST_PEER_ID" host "alice -> host" >/dev/null
    wait_for_bootstrap_peer_id bob "$HOST_PEER_URL" "$HOST_PEER_ID" host "bob -> host" >/dev/null
  fi
fi

echo "Funding wallets..."
fund_and_onboard_profile "$PLAYER_ONE_PROFILE"
fund_and_onboard_profile "$PLAYER_TWO_PROFILE"

if ! host_player_scenario_enabled; then
  echo "Connecting host to witness..."
  if tor_enabled; then
    retry_pcli_json "host bootstrap witness over Tor" 180 1 network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null
  else
    pcli network bootstrap add "$WITNESS_PEER_URL" witness --profile host --json >/dev/null
  fi
fi

echo "Creating table..."
create_table_args=(table create --name auto-regtest-table --profile host --json)
if ! host_player_scenario_enabled; then
  create_table_args+=(--witness-peer-ids "$WITNESS_PEER_ID")
fi
CREATE_JSON="$(pcli "${create_table_args[@]}")"

INVITE_CODE="$(printf '%s' "$CREATE_JSON" | json_field data.inviteCode)"
TABLE_ID="$(printf '%s' "$CREATE_JSON" | json_field data.table.tableId)"

echo "TABLE_ID=$TABLE_ID"
echo "Waiting for host to observe the new table..."
watch_table_state_with_retry host >/dev/null

echo "Joining players..."
buy_in_profile "$PLAYER_ONE_PROFILE"
buy_in_profile "$PLAYER_TWO_PROFILE"
echo "Waiting for players to observe the active table..."
wait_for_table_status "$PLAYER_ONE_PROFILE" active 2 >/dev/null
wait_for_table_status "$PLAYER_TWO_PROFILE" active 2 >/dev/null
write_table_artifact host "$BASE/artifacts/table-active.json"

write_runtime_env

if setup_only_enabled; then
  trap - EXIT INT TERM HUP
  print_local_stack_summary
  exit 0
fi

echo "Playing one hand automatically..."
play_hand_automatically >/dev/null
write_table_artifact host "$BASE/artifacts/table-after-hand.json"

echo "Final table state:"
watch_table_state_with_retry "$WATCH_PROFILE"

echo "Cashing out..."
cashout_profile "$PLAYER_ONE_PROFILE"
cashout_profile "$PLAYER_TWO_PROFILE"
write_table_artifact host "$BASE/artifacts/table-after-cashout.json"

echo "Final wallet summaries:"
pcli wallet --profile "$PLAYER_ONE_PROFILE" --json
pcli wallet --profile "$PLAYER_TWO_PROFILE" --json

echo "Done. Logs are under $BASE"
