#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

NIGIRI_BIN="$ROOT_DIR/scripts/bin/nigiri"
DOCKER_COMPOSE_BIN="$ROOT_DIR/scripts/bin/docker-compose"
PARKER_CLI_BIN="$ROOT_DIR/scripts/bin/parker-cli"
PARKER_CONTROLLER_BIN="$ROOT_DIR/scripts/bin/parker-controller"
PARKER_DEVTOOL_BIN="$ROOT_DIR/scripts/bin/parker-devtool"
PARKER_INDEXER_BIN="$ROOT_DIR/scripts/bin/parker-indexer"

LOCAL_STATE_DIR="${LOCAL_STATE_DIR:-$ROOT_DIR/.tmp/local-regtest}"
RUNTIME_ENV="$LOCAL_STATE_DIR/runtime.env"
PID_DIR="$LOCAL_STATE_DIR/pids"
LOG_DIR="$LOCAL_STATE_DIR/logs"
WORK_DIR="$LOCAL_STATE_DIR/work"

DEFAULT_INDEXER_PORT=3020
DEFAULT_CONTROLLER_PORT=3030
DEFAULT_WITNESS_PORT=7061
DEFAULT_ALICE_PORT=7062
DEFAULT_BOB_PORT=7063

FAUCET_SATS="${FAUCET_SATS:-100000}"

command="${1:-}"
if [[ $# -gt 0 ]]; then
  shift
fi

usage() {
  cat <<'EOF'
usage: local-stack.sh <command>

commands:
  local-up
  local-down
  deps-up
  deps-down
  start-indexer
  stop-indexer
  start-controller
  stop-controller
  start-daemon <witness|alice|bob>
  stop-daemon <witness|alice|bob>
  fund <alice|bob>

Set HOST_PROFILE=alice or HOST_PROFILE=bob to choose which player runs in host mode (default: alice).
EOF
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
    echo "nigiri must be available on PATH to manage the local stack." >&2
    exit 1
  }
  command -v curl >/dev/null 2>&1 || {
    echo "curl must be available on PATH to manage the local stack." >&2
    exit 1
  }
  if ! command -v go >/dev/null 2>&1 && [[ ! -x /opt/homebrew/bin/go ]] && [[ ! -x "$HOME/.gvm/gos/go1.24.0/bin/go" ]]; then
    echo "go must be available on PATH to build parker binaries." >&2
    exit 1
  fi
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

pdevtool() {
  "$PARKER_DEVTOOL_BIN" "$@"
}

json_field() {
  pdevtool json-field "$1"
}

free_port() {
  pdevtool free-port
}

port_is_in_use() {
  local port="$1"

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi
  return 1
}

choose_port() {
  local preferred="$1"
  if port_is_in_use "$preferred"; then
    free_port
    return 0
  fi
  printf '%s\n' "$preferred"
}

write_runtime_env() {
  local indexer_port="$1"
  local controller_port="$2"
  local witness_port="$3"
  local alice_port="$4"
  local bob_port="$5"
  local nigiri_datadir="$6"
  local selected_host_profile="$7"

  mkdir -p "$LOCAL_STATE_DIR" "$PID_DIR" "$LOG_DIR" "$WORK_DIR"

  cat >"$RUNTIME_ENV" <<EOF
export ROOT_DIR=$(printf '%q' "$ROOT_DIR")
export LOCAL_STATE_DIR=$(printf '%q' "$LOCAL_STATE_DIR")
export LOCAL_PID_DIR=$(printf '%q' "$PID_DIR")
export LOCAL_LOG_DIR=$(printf '%q' "$LOG_DIR")
export LOCAL_WORK_DIR=$(printf '%q' "$WORK_DIR")
export PARKER_NETWORK=regtest
export PARKER_ARK_SERVER_URL=http://127.0.0.1:7070
export PARKER_BOLTZ_URL=http://127.0.0.1:9069
export PARKER_DATADIR=$(printf '%q' "$WORK_DIR/data")
export PARKER_DAEMON_DIR=$(printf '%q' "$WORK_DIR/daemons")
export PARKER_PROFILE_DIR=$(printf '%q' "$WORK_DIR/profiles")
export PARKER_RUN_DIR=$(printf '%q' "$WORK_DIR/runs")
export PARKER_NIGIRI_DATADIR=$(printf '%q' "$nigiri_datadir")
export LOCAL_INDEXER_DATADIR=$(printf '%q' "$WORK_DIR/indexer")
export LOCAL_CONTROLLER_LOG=$(printf '%q' "$LOG_DIR/controller.log")
export LOCAL_INDEXER_LOG=$(printf '%q' "$LOG_DIR/indexer.log")
export PARKER_INDEXER_URL=http://127.0.0.1:${indexer_port}
export LOCAL_HOST_PROFILE=$(printf '%q' "$selected_host_profile")
export INDEXER_PORT=${indexer_port}
export CONTROLLER_PORT=${controller_port}
export WITNESS_PORT=${witness_port}
export ALICE_PORT=${alice_port}
export BOB_PORT=${bob_port}
EOF
}

legacy_local_nigiri_datadir() {
  printf '%s\n' "$LOCAL_STATE_DIR/nigiri"
}

default_local_nigiri_datadir() {
  local local_state_key
  local_state_key="$(printf '%s' "$LOCAL_STATE_DIR" | tr '/:' '__')"
  printf '%s\n' "${NIGIRI_DATADIR:-$HOME/Library/Application Support/Nigiri/parker-local/${local_state_key}}"
}

initialize_runtime_env() {
  local nigiri_datadir
  local indexer_port
  local controller_port
  local witness_port
  local alice_port
  local bob_port
  local selected_host_profile

  nigiri_datadir="$(default_local_nigiri_datadir)"
  indexer_port="$(choose_port "$DEFAULT_INDEXER_PORT")"
  controller_port="$(choose_port "$DEFAULT_CONTROLLER_PORT")"
  witness_port="$(choose_port "$DEFAULT_WITNESS_PORT")"
  alice_port="$(choose_port "$DEFAULT_ALICE_PORT")"
  bob_port="$(choose_port "$DEFAULT_BOB_PORT")"
  selected_host_profile="$(host_profile)"

  write_runtime_env "$indexer_port" "$controller_port" "$witness_port" "$alice_port" "$bob_port" "$nigiri_datadir" "$selected_host_profile"
}

load_runtime_env() {
  local desired_nigiri_datadir=""
  local legacy_nigiri_datadir=""
  local desired_host_profile=""

  if [[ ! -f "$RUNTIME_ENV" ]]; then
    initialize_runtime_env
  fi

  # shellcheck disable=SC1090
  source "$RUNTIME_ENV"

  desired_nigiri_datadir="$(default_local_nigiri_datadir)"
  legacy_nigiri_datadir="$(legacy_local_nigiri_datadir)"

  if [[ -z "${NIGIRI_DATADIR:-}" ]] &&
    [[ "${PARKER_NIGIRI_DATADIR:-}" == "$legacy_nigiri_datadir" ]] &&
    [[ "$desired_nigiri_datadir" != "$legacy_nigiri_datadir" ]]; then
    write_runtime_env \
      "$INDEXER_PORT" \
      "$CONTROLLER_PORT" \
      "$WITNESS_PORT" \
      "$ALICE_PORT" \
      "$BOB_PORT" \
      "$desired_nigiri_datadir" \
      "$(host_profile)"

    # shellcheck disable=SC1090
    source "$RUNTIME_ENV"
  fi

  mkdir -p \
    "$LOCAL_STATE_DIR" \
    "$LOCAL_PID_DIR" \
    "$LOCAL_LOG_DIR" \
    "$LOCAL_WORK_DIR" \
    "$PARKER_DATADIR" \
    "$PARKER_DAEMON_DIR" \
    "$PARKER_PROFILE_DIR" \
    "$PARKER_RUN_DIR" \
    "$LOCAL_INDEXER_DATADIR" \
    "$PARKER_NIGIRI_DATADIR"

  common_flags=(
    --network regtest
    --indexer-url "$PARKER_INDEXER_URL"
    --ark-server-url "$PARKER_ARK_SERVER_URL"
    --boltz-url "$PARKER_BOLTZ_URL"
    --datadir "$PARKER_DATADIR"
    --nigiri-datadir "$PARKER_NIGIRI_DATADIR"
    --daemon-dir "$PARKER_DAEMON_DIR"
    --profile-dir "$PARKER_PROFILE_DIR"
    --run-dir "$PARKER_RUN_DIR"
  )

  desired_host_profile="$(host_profile)"
  if [[ -z "${LOCAL_HOST_PROFILE:-}" || "${LOCAL_HOST_PROFILE}" != "$desired_host_profile" ]]; then
    write_runtime_env \
      "$INDEXER_PORT" \
      "$CONTROLLER_PORT" \
      "$WITNESS_PORT" \
      "$ALICE_PORT" \
      "$BOB_PORT" \
      "$PARKER_NIGIRI_DATADIR" \
      "$desired_host_profile"

    # shellcheck disable=SC1090
    source "$RUNTIME_ENV"
  fi
}

launch_detached() {
  local log_path="$1"
  shift

  mkdir -p "$(dirname "$log_path")"
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
}

pid_file_for() {
  local name="$1"
  printf '%s/%s.pid\n' "$LOCAL_PID_DIR" "$name"
}

read_pid_file() {
  local path="$1"
  if [[ -f "$path" ]]; then
    tr -d '[:space:]' <"$path"
  fi
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

wait_for_http_ok() {
  local url="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-0.5}"
  local body=""
  local i

  for ((i = 0; i < attempts; i += 1)); do
    if body="$(curl -fsS "$url" 2>/dev/null)" && [[ "$body" == *'"ok":true'* ]]; then
      return 0
    fi
    sleep "$sleep_seconds"
  done
  return 1
}

nigiri_cmd() {
  "$NIGIRI_BIN" --datadir "$PARKER_NIGIRI_DATADIR" "$@"
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
  local i

  for ((i = 0; i < attempts; i += 1)); do
    if body="$(curl -fsS http://127.0.0.1:7070/v1/info 2>/dev/null)" && [[ -n "$body" ]]; then
      signer_pubkey="$(printf '%s' "$body" | json_field signerPubkey)"
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

force_cleanup_nigiri_docker() {
  local compose_file="$PARKER_NIGIRI_DATADIR/docker-compose.yml"

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
  rm -rf "$PARKER_NIGIRI_DATADIR"
}

prepare_nigiri_data_dirs() {
  mkdir -p \
    "$PARKER_NIGIRI_DATADIR/volumes/bitcoin" \
    "$PARKER_NIGIRI_DATADIR/volumes/elements" \
    "$PARKER_NIGIRI_DATADIR/volumes/postgres" \
    "$PARKER_NIGIRI_DATADIR/volumes/tapd" \
    "$PARKER_NIGIRI_DATADIR/volumes/ark/wallet" \
    "$PARKER_NIGIRI_DATADIR/volumes/ark/data" \
    "$PARKER_NIGIRI_DATADIR/volumes/lnd" \
    "$PARKER_NIGIRI_DATADIR/volumes/nbxplorer" \
    "$PARKER_NIGIRI_DATADIR/volumes/lightningd"

  chmod -R 0777 "$PARKER_NIGIRI_DATADIR/volumes" 2>/dev/null || true
}

cleanup_local_runtime_state() {
  load_runtime_env
  rm -rf \
    "$LOCAL_PID_DIR" \
    "$LOCAL_WORK_DIR" \
    "$LOCAL_STATE_DIR/nigiri"
  mkdir -p "$LOCAL_STATE_DIR" "$LOCAL_LOG_DIR"
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

start_nigiri_stack() {
  local attempt

  for attempt in 1 2 3; do
    stop_nigiri_stack
    mkdir -p "$PARKER_NIGIRI_DATADIR"
    prepare_nigiri_data_dirs
    : >"$LOCAL_LOG_DIR/nigiri-start.log"

    echo "Starting Nigiri (attempt ${attempt}/3)..."
    nigiri_cmd start --ark --ln --ci >"$LOCAL_LOG_DIR/nigiri-start.log" 2>&1 &
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

ensure_deps_up() {
  load_runtime_env

  if wait_for_http_json "http://127.0.0.1:7070/v1/info" 2 0.5 >/dev/null &&
    wait_for_ark_wallet 2 >/dev/null &&
    wait_for_ark_ready 2 >/dev/null; then
    echo "Dependencies already running."
    return 0
  fi

  start_nigiri_stack
  echo "Dependencies are ready."
}

profile_mode() {
  local profile="$1"
  local selected_host_profile=""

  selected_host_profile="$(host_profile)"

  case "$profile" in
    witness) printf 'witness\n' ;;
    alice|bob)
      if [[ "$profile" == "$selected_host_profile" ]]; then
        printf 'host\n'
        return 0
      fi
      printf 'player\n'
      ;;
    *)
      echo "unknown profile $profile" >&2
      exit 1
      ;;
  esac
}

host_profile() {
  local profile="${HOST_PROFILE:-${LOCAL_HOST_PROFILE:-alice}}"

  case "$profile" in
    alice|bob) printf '%s\n' "$profile" ;;
    *)
      echo "HOST_PROFILE must be alice or bob (received: $profile)." >&2
      exit 1
      ;;
  esac
}

ensure_daemon_profile_running() {
  local profile="$1"
  local running_mode=""
  local desired_mode=""

  load_runtime_env
  start_controller
  desired_mode="$(profile_mode "$profile")"

  if daemon_is_reachable "$profile"; then
    running_mode="$(profile_daemon_mode "$profile")"
    if [[ "$running_mode" == "$desired_mode" ]]; then
      echo "$profile daemon already running in $running_mode mode."
      return 0
    fi

    echo "Restarting $profile daemon in $desired_mode mode."
  fi

  start_daemon_profile "$profile"
}

profile_port() {
  case "$1" in
    witness) printf '%s\n' "$WITNESS_PORT" ;;
    alice) printf '%s\n' "$ALICE_PORT" ;;
    bob) printf '%s\n' "$BOB_PORT" ;;
    *)
      echo "unknown profile $1" >&2
      exit 1
      ;;
  esac
}

profile_nickname() {
  case "$1" in
    witness) printf 'Witness\n' ;;
    alice) printf 'Alice\n' ;;
    bob) printf 'Bob\n' ;;
    *)
      echo "unknown profile $1" >&2
      exit 1
      ;;
  esac
}

profile_metadata_path() {
  printf '%s/%s.json\n' "$PARKER_DAEMON_DIR" "$1"
}

profile_socket_path() {
  printf '%s/%s.sock\n' "$PARKER_DAEMON_DIR" "$1"
}

profile_pid_from_metadata() {
  local metadata_path
  metadata_path="$(profile_metadata_path "$1")"
  if [[ -f "$metadata_path" ]]; then
    json_field pid <"$metadata_path" 2>/dev/null || true
  fi
}

profile_cli() {
  local profile="$1"
  shift
  local peer_port
  peer_port="$(profile_port "$profile")"

  run_with_timeout 30 "$PARKER_CLI_BIN" "$@" "${common_flags[@]}" --profile "$profile" --peer-port "$peer_port"
}

retry_profile_json() {
  local profile="$1"
  local label="$2"
  local attempts="$3"
  local sleep_seconds="$4"
  shift 4
  local output=""
  local attempt

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(profile_cli "$profile" "$@" 2>&1)"; then
      printf '%s\n' "$output"
      return 0
    fi
    if wait_for_vtxo_ban_expiry "$output"; then
      continue
    fi
    sleep "$sleep_seconds"
  done

  echo "command failed after retries: $label" >&2
  printf '%s\n' "$output" >&2
  return 1
}

latest_vtxo_ban_epoch() {
  LC_ALL=C LANG=C LC_CTYPE=C /usr/bin/perl -MTime::Piece -e '
    use strict;
    use warnings;

    my $input = do { local $/; <STDIN> // q{} };
    my $max = 0;

    while ($input =~ /(20\d{2}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z?)/g) {
      my $value = $1;
      $value =~ s/Z$//;
      my $tp = eval { Time::Piece->strptime($value, "%Y-%m-%dT%H:%M:%S") };
      next unless $tp;
      my $epoch = $tp->epoch;
      $max = $epoch if $epoch > $max;
    }

    print $max if $max > 0;
  '
}

wait_for_vtxo_ban_expiry() {
  local output="$1"
  local latest_epoch=""
  local now_epoch=0
  local sleep_seconds=0

  [[ "$output" == *"VTXO_BANNED"* ]] || return 1

  latest_epoch="$(printf '%s' "$output" | latest_vtxo_ban_epoch)"
  [[ -n "$latest_epoch" ]] || return 1

  now_epoch="$(date -u +%s)"
  sleep_seconds=$((latest_epoch - now_epoch + 2))
  if ((sleep_seconds > 0)); then
    echo "Ark temporarily banned the onboarding script; waiting ${sleep_seconds}s before retrying..." >&2
    sleep "$sleep_seconds"
  fi

  return 0
}

daemon_is_reachable() {
  local profile="$1"
  local status=""
  local reachable=""

  if ! status="$(profile_cli "$profile" daemon status --json 2>/dev/null || true)"; then
    return 1
  fi
  [[ -n "$status" ]] || return 1
  reachable="$(printf '%s' "$status" | json_field data.reachable 2>/dev/null || true)"
  [[ "$reachable" == "true" ]]
}

profile_daemon_mode() {
  local profile="$1"
  local status=""

  status="$(profile_cli "$profile" daemon status --json 2>/dev/null || true)"
  [[ -n "$status" ]] || return 1
  printf '%s' "$status" | json_field data.metadata.mode 2>/dev/null || true
}

wait_for_daemon_reachable() {
  local profile="$1"
  local status
  local reachable
  local i

  for ((i = 0; i < 80; i += 1)); do
    if status="$(profile_cli "$profile" daemon status --json 2>/dev/null || true)" && [[ -n "$status" ]]; then
      reachable="$(printf '%s' "$status" | json_field data.reachable 2>/dev/null || true)"
      if [[ "$reachable" == "true" ]]; then
        return 0
      fi
    fi
    sleep 0.25
  done

  echo "timed out waiting for daemon $profile to become reachable" >&2
  return 1
}

start_daemon_profile() {
  local profile="$1"
  local mode
  local running_mode=""

  load_runtime_env
  start_controller
  mode="$(profile_mode "$profile")"

  if daemon_is_reachable "$profile"; then
    running_mode="$(profile_daemon_mode "$profile")"
    if [[ "$running_mode" == "$mode" ]]; then
      echo "$profile daemon already running in $mode mode."
      return 0
    fi

    echo "Restarting $profile daemon in $mode mode."
    stop_daemon_profile "$profile"
  fi

  profile_cli "$profile" daemon start --mode "$mode" --json >/dev/null
  wait_for_daemon_reachable "$profile"
  echo "$profile daemon is running in $mode mode on parker://127.0.0.1:$(profile_port "$profile")."
}

stop_daemon_profile() {
  local profile="$1"
  local pid=""

  load_runtime_env
  pid="$(profile_pid_from_metadata "$profile")"

  if daemon_is_reachable "$profile"; then
    profile_cli "$profile" daemon stop --json >/dev/null 2>&1 || true
  fi

  if [[ -n "$pid" ]]; then
    terminate_pid "$pid"
  fi

  rm -f "$(profile_metadata_path "$profile")" "$(profile_socket_path "$profile")"
  echo "$profile daemon stopped."
}

start_indexer() {
  local pid_file=""
  local existing_pid=""

  load_runtime_env
  pid_file="$(pid_file_for indexer)"
  existing_pid="$(read_pid_file "$pid_file")"

  if wait_for_http_ok "http://127.0.0.1:${INDEXER_PORT}/health" 2 0.25; then
    echo "Indexer already running on http://127.0.0.1:${INDEXER_PORT}."
    return 0
  fi

  if [[ -n "$existing_pid" ]]; then
    terminate_pid "$existing_pid"
    rm -f "$pid_file"
  fi

  launch_detached "$LOCAL_INDEXER_LOG" \
    env \
    HOST=127.0.0.1 \
    PORT="$INDEXER_PORT" \
    PARKER_NETWORK=regtest \
    PARKER_DATADIR="$LOCAL_INDEXER_DATADIR" \
    "$PARKER_INDEXER_BIN"
  printf '%s\n' "$LAUNCHED_PID" >"$pid_file"

  wait_for_http_ok "http://127.0.0.1:${INDEXER_PORT}/health" 120 0.5
  echo "Indexer is running on http://127.0.0.1:${INDEXER_PORT}."
}

stop_indexer() {
  local pid_file=""
  local pid=""

  load_runtime_env
  pid_file="$(pid_file_for indexer)"
  pid="$(read_pid_file "$pid_file")"

  if [[ -n "$pid" ]]; then
    terminate_pid "$pid"
  fi
  rm -f "$pid_file"
  echo "Indexer stopped."
}

start_controller() {
  local pid_file=""
  local existing_pid=""

  load_runtime_env
  pid_file="$(pid_file_for controller)"
  existing_pid="$(read_pid_file "$pid_file")"

  if wait_for_http_ok "http://127.0.0.1:${CONTROLLER_PORT}/health" 2 0.25; then
    echo "Controller already running on http://127.0.0.1:${CONTROLLER_PORT}."
    return 0
  fi

  if [[ -n "$existing_pid" ]]; then
    terminate_pid "$existing_pid"
    rm -f "$pid_file"
  fi

  launch_detached "$LOCAL_CONTROLLER_LOG" \
    env \
    PARKER_NETWORK=regtest \
    PARKER_ARK_SERVER_URL="$PARKER_ARK_SERVER_URL" \
    PARKER_BOLTZ_URL="$PARKER_BOLTZ_URL" \
    PARKER_INDEXER_URL="$PARKER_INDEXER_URL" \
    PARKER_DATADIR="$PARKER_DATADIR" \
    PARKER_DAEMON_DIR="$PARKER_DAEMON_DIR" \
    PARKER_PROFILE_DIR="$PARKER_PROFILE_DIR" \
    PARKER_RUN_DIR="$PARKER_RUN_DIR" \
    PARKER_NIGIRI_DATADIR="$PARKER_NIGIRI_DATADIR" \
    PARKER_CONTROLLER_HOST=127.0.0.1 \
    PARKER_CONTROLLER_PORT="$CONTROLLER_PORT" \
    "$PARKER_CONTROLLER_BIN"
  printf '%s\n' "$LAUNCHED_PID" >"$pid_file"

  wait_for_http_ok "http://127.0.0.1:${CONTROLLER_PORT}/health" 120 0.5
  echo "Controller is running on http://127.0.0.1:${CONTROLLER_PORT}."
}

stop_controller() {
  local pid_file=""
  local pid=""

  load_runtime_env
  pid_file="$(pid_file_for controller)"
  pid="$(read_pid_file "$pid_file")"

  if [[ -n "$pid" ]]; then
    terminate_pid "$pid"
  fi
  rm -f "$pid_file"
  echo "Controller stopped."
}

fund_profile() {
  local profile="$1"
  local nickname

  load_runtime_env
  nickname="$(profile_nickname "$profile")"

  ensure_deps_up
  ensure_daemon_profile_running "$profile"
  echo "Bootstrapping $profile wallet..."
  retry_profile_json "$profile" "$profile bootstrap" 20 1 bootstrap "$nickname" --json >/dev/null
  echo "Requesting faucet funds for $profile..."
  retry_profile_json "$profile" "$profile faucet" 20 1 wallet faucet "$FAUCET_SATS" --json >/dev/null
  echo "Onboarding $profile wallet..."
  retry_profile_json "$profile" "$profile onboard" 20 1 wallet onboard --json >/dev/null
  echo "Funded $profile wallet."
  profile_cli "$profile" wallet --json
}

ensure_profile_funded() {
  local profile="$1"
  local wallet_json=""
  local total_sats=""

  ensure_daemon_profile_running "$profile"
  wallet_json="$(profile_cli "$profile" wallet --json 2>/dev/null || true)"
  total_sats="$(printf '%s' "$wallet_json" | json_field data.totalSats 2>/dev/null || true)"
  if [[ "$total_sats" =~ ^[0-9]+$ ]] && (( total_sats > 0 )); then
    echo "$profile wallet already funded with $total_sats sats."
    return 0
  fi

  fund_profile "$profile"
}

print_local_summary() {
  local selected_host_profile=""

  load_runtime_env
  selected_host_profile="$(host_profile)"
  cat <<EOF
Local stack is ready.
RUNTIME_ENV=$RUNTIME_ENV
INDEXER_URL=http://127.0.0.1:${INDEXER_PORT}
CONTROLLER_URL=http://127.0.0.1:${CONTROLLER_PORT}
HOST_PROFILE=${selected_host_profile}
WITNESS_PORT=${WITNESS_PORT}
ALICE_PORT=${ALICE_PORT}
BOB_PORT=${BOB_PORT}
EOF
}

local_up() {
  local selected_host_profile=""

  ensure_deps_up
  start_indexer
  start_controller
  selected_host_profile="$(host_profile)"
  start_daemon_profile "$selected_host_profile"
  start_daemon_profile witness
  if [[ "$selected_host_profile" != "alice" ]]; then
    start_daemon_profile alice
  fi
  if [[ "$selected_host_profile" != "bob" ]]; then
    start_daemon_profile bob
  fi
  ensure_profile_funded alice
  ensure_profile_funded bob
  print_local_summary
}

local_down() {
  load_runtime_env
  stop_daemon_profile bob || true
  stop_daemon_profile alice || true
  stop_daemon_profile witness || true
  stop_controller || true
  stop_indexer || true
  stop_nigiri_stack || true
  cleanup_local_runtime_state
  echo "Local stack stopped."
}

ensure_toolchains

case "$command" in
  local-up)
    local_up
    ;;
  local-down)
    local_down
    ;;
  deps-up)
    ensure_deps_up
    ;;
  deps-down)
    load_runtime_env
    stop_nigiri_stack
    echo "Dependencies stopped."
    ;;
  start-indexer)
    start_indexer
    ;;
  stop-indexer)
    stop_indexer
    ;;
  start-controller)
    start_controller
    ;;
  stop-controller)
    stop_controller
    ;;
  start-daemon)
    [[ $# -eq 1 ]] || {
      echo "start-daemon requires one profile." >&2
      exit 1
    }
    start_daemon_profile "$1"
    ;;
  stop-daemon)
    [[ $# -eq 1 ]] || {
      echo "stop-daemon requires one profile." >&2
      exit 1
    }
    stop_daemon_profile "$1"
    ;;
  fund)
    [[ $# -eq 1 ]] || {
      echo "fund requires one profile." >&2
      exit 1
    }
    case "$1" in
      alice|bob) ;;
      *)
        echo "fund only supports alice or bob." >&2
        exit 1
        ;;
    esac
    fund_profile "$1"
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac
