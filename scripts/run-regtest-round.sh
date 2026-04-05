#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
NIGIRI_BIN="$ROOT_DIR/scripts/bin/nigiri"
DOCKER_COMPOSE_BIN="$ROOT_DIR/scripts/bin/docker-compose"
TOP_LEVEL_BASHPID="${BASHPID:-$$}"

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
HAND_SETTLE_TIMEOUT_SECONDS="${HAND_SETTLE_TIMEOUT_SECONDS:-}"
CASHOUT_TIMEOUT_SECONDS="${CASHOUT_TIMEOUT_SECONDS:-}"
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
ROUND_SCENARIO_TIMEOUT_RECOVERY="recovery-timeout-2d"
ROUND_SCENARIO_ABORTED_HAND="aborted-hand-2d"
ROUND_SCENARIO_ALL_IN="all-in-side-pot-2d"
ROUND_SCENARIO_TURN_CHALLENGE="turn-challenge-2d"
ROUND_SCENARIO_EMERGENCY_EXIT="emergency-exit-2d"
ROUND_SCENARIO_MULTI_HAND="multi-hand-2d"
ROUND_SCENARIO_CHALLENGE_ESCAPE="challenge-escape-2d"
ROUND_SCENARIO_RECOVERY_SHOWDOWN="recovery-showdown-2d"
ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE="cashout-after-challenge-2d"

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

timing_metrics_enabled() {
  setting_enabled "${PARKER_TIMING_METRICS:-}"
}

host_player_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_HOST_PLAYER" ]]
}

recovery_timeout_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_TIMEOUT_RECOVERY" ]]
}

aborted_hand_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_ABORTED_HAND" ]]
}

all_in_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_ALL_IN" ]]
}

turn_challenge_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_TURN_CHALLENGE" ]]
}

emergency_exit_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_EMERGENCY_EXIT" ]]
}

multi_hand_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_MULTI_HAND" ]]
}

challenge_escape_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_CHALLENGE_ESCAPE" ]]
}

recovery_showdown_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_RECOVERY_SHOWDOWN" ]]
}

cashout_after_challenge_scenario_enabled() {
  [[ "$ROUND_SCENARIO" == "$ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE" ]]
}

chain_challenge_scenario_enabled() {
  turn_challenge_scenario_enabled ||
    challenge_escape_scenario_enabled ||
    cashout_after_challenge_scenario_enabled
}

scenario_skips_cashout() {
  recovery_timeout_scenario_enabled || recovery_showdown_scenario_enabled || turn_challenge_scenario_enabled || challenge_escape_scenario_enabled
}

scenario_cashout_skip_reason() {
  case "$ROUND_SCENARIO" in
    "$ROUND_SCENARIO_TIMEOUT_RECOVERY") printf '%s\n' "the recovery scenario intentionally leaves the Ark server offline." ;;
    "$ROUND_SCENARIO_RECOVERY_SHOWDOWN") printf '%s\n' "the recovery scenario intentionally leaves the Ark server offline." ;;
    "$ROUND_SCENARIO_TURN_CHALLENGE") printf '%s\n' "this scenario verifies challenge resolution without requiring a post-resolution cash-out." ;;
    "$ROUND_SCENARIO_CHALLENGE_ESCAPE") printf '%s\n' "this scenario verifies the CSV escape path, which resolves custody on-chain instead of through a follow-on Ark cash-out." ;;
    *) printf '%s\n' "this scenario does not require a cash out." ;;
  esac
}

round_scenario_description() {
  case "$1" in
    "$ROUND_SCENARIO_STANDARD") printf '%s\n' "baseline 2-player direct-timeout round with Ark settlement and cash-out" ;;
    "$ROUND_SCENARIO_HOST_PLAYER") printf '%s\n' "host also occupies a seat in the 2-player direct-timeout round" ;;
    "$ROUND_SCENARIO_TIMEOUT_RECOVERY") printf '%s\n' "direct-timeout hand that finalizes from recovery or challenge proof material after Ark goes offline" ;;
    "$ROUND_SCENARIO_ABORTED_HAND") printf '%s\n' "forces a hand abort, preserves custody, then proves the table can continue into a fresh hand" ;;
    "$ROUND_SCENARIO_ALL_IN") printf '%s\n' "forces an explicit all-in hand and verifies showdown settlement and clean cash-out in the 2-seat protocol" ;;
    "$ROUND_SCENARIO_TURN_CHALLENGE") printf '%s\n' "uses chain-challenge timeout mode, opens an on-chain turn challenge, then resolves a concrete option bundle" ;;
    "$ROUND_SCENARIO_EMERGENCY_EXIT") printf '%s\n' "executes an emergency exit after live table activity and verifies unilateral exit safety" ;;
    "$ROUND_SCENARIO_MULTI_HAND") printf '%s\n' "plays multiple hands at one table to exercise carry-forward, blind rotation, and stack continuity" ;;
    "$ROUND_SCENARIO_CHALLENGE_ESCAPE") printf '%s\n' "opens a turn challenge, advances CSV maturity, and resolves through the escape path" ;;
    "$ROUND_SCENARIO_RECOVERY_SHOWDOWN") printf '%s\n' "lets a hand reach showdown, takes Ark offline before payout settlement, and finalizes through recovery" ;;
    "$ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE") printf '%s\n' "resolves a disputed turn through chain-challenge timeout and cashes out immediately afterward" ;;
    *) return 1 ;;
  esac
}

validate_round_scenario() {
  case "$ROUND_SCENARIO" in
    "$ROUND_SCENARIO_STANDARD"|\
    "$ROUND_SCENARIO_HOST_PLAYER"|\
    "$ROUND_SCENARIO_TIMEOUT_RECOVERY"|\
    "$ROUND_SCENARIO_ABORTED_HAND"|\
    "$ROUND_SCENARIO_ALL_IN"|\
    "$ROUND_SCENARIO_TURN_CHALLENGE"|\
    "$ROUND_SCENARIO_EMERGENCY_EXIT"|\
    "$ROUND_SCENARIO_MULTI_HAND"|\
    "$ROUND_SCENARIO_CHALLENGE_ESCAPE"|\
    "$ROUND_SCENARIO_RECOVERY_SHOWDOWN"|\
    "$ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE") ;;
    *)
      echo "unsupported ROUND_SCENARIO=$ROUND_SCENARIO" >&2
      echo "supported scenarios:" >&2
      local scenario=""
      for scenario in \
        "$ROUND_SCENARIO_STANDARD" \
        "$ROUND_SCENARIO_HOST_PLAYER" \
        "$ROUND_SCENARIO_TIMEOUT_RECOVERY" \
        "$ROUND_SCENARIO_ABORTED_HAND" \
        "$ROUND_SCENARIO_ALL_IN" \
        "$ROUND_SCENARIO_TURN_CHALLENGE" \
        "$ROUND_SCENARIO_EMERGENCY_EXIT" \
        "$ROUND_SCENARIO_MULTI_HAND" \
        "$ROUND_SCENARIO_CHALLENGE_ESCAPE" \
        "$ROUND_SCENARIO_RECOVERY_SHOWDOWN" \
        "$ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE"; do
        printf '  %s: %s\n' "$scenario" "$(round_scenario_description "$scenario")" >&2
      done
      exit 1
      ;;
  esac
}

resolve_nigiri_datadir() {
  local preferred="$1"
  local fallback_app_support="$HOME/Library/Application Support/Nigiri/parker-auto/$(printf '%s' "$BASE" | tr '/:' '__')"
  local fallback_home_nigiri="$HOME/.nigiri/parker-auto/$(printf '%s' "$BASE" | tr '/:' '__')"
  local candidate=""

  docker_bind_mount_is_writable() {
    local path="$1"
    mkdir -p "$path" 2>/dev/null || return 1
    docker run --rm --user 1000:1000 -v "$path:/probe" alpine:3.20 \
      sh -lc 'touch /probe/.docker-write-test && rm /probe/.docker-write-test' >/dev/null 2>&1
  }

  for candidate in "$preferred" "$fallback_app_support" "$fallback_home_nigiri"; do
    [[ -n "$candidate" ]] || continue
    if docker_bind_mount_is_writable "$candidate"; then
      if [[ "$candidate" != "$preferred" ]]; then
        printf 'Falling back to Docker-writable Nigiri datadir at %s\n' "$candidate" >&2
      fi
      printf '%s\n' "$candidate"
      return 0
    fi
  done

  echo "unable to find a Docker-writable Nigiri datadir (tried $preferred, $fallback_app_support, $fallback_home_nigiri)" >&2
  return 1
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
  if [[ "${BASHPID:-$$}" != "$TOP_LEVEL_BASHPID" ]]; then
    return 0
  fi
  local exit_status=$?
  set +e
  collect_run_daemon_pids() {
    local metadata metadata_json pid command_line
    for metadata in "$BASE"/daemons/*.json; do
      [[ -f "$metadata" ]] || continue
      metadata_json="$(cat "$metadata" 2>/dev/null || true)"
      [[ -n "$metadata_json" ]] || continue
      pid="$(json_field pid <<<"$metadata_json" 2>/dev/null || true)"
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
  if declare -F stop_nigiri_stack >/dev/null 2>&1; then
    stop_nigiri_stack || true
  fi
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
NIGIRI_DATADIR="$(resolve_nigiri_datadir "$NIGIRI_DATADIR")"
echo "ROUND_SCENARIO=$ROUND_SCENARIO ($(round_scenario_description "$ROUND_SCENARIO"))"

command_timeout_seconds() {
  if [[ -n "$PCLI_TIMEOUT_SECONDS" ]]; then
    printf '%s\n' "$PCLI_TIMEOUT_SECONDS"
    return 0
  fi
  if tor_enabled; then
    printf '90\n'
    return 0
  fi
  printf '90\n'
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

raw_table_watch_json() {
  local profile="${1:-host}"
  "$ROOT_DIR/scripts/bin/parker-cli" table "${common_flags[@]}" watch "$TABLE_ID" --profile "$profile" --json
}

pdevtool() {
  if [[ ! -t 0 ]]; then
    local input=""
    input="$(cat)"
    "$ROOT_DIR/scripts/bin/parker-devtool" "$@" <<<"$input"
    return
  fi

  "$ROOT_DIR/scripts/bin/parker-devtool" "$@"
}

json_field() {
  pdevtool json-field "$1"
}

table_has_settled_custody_checkpoint() {
  local state_json="$1"
  local phase=""
  local hand_id=""
  local hand_number=""
  local latest_custody_hash=""
  local latest_snapshot_phase=""
  local latest_snapshot_hand_number=""
  local pot_sats=""
  local current_bet_sats=""
  local acting_seat_index=""

  phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  hand_id="$(printf '%s' "$state_json" | json_field data.publicState.handId 2>/dev/null || true)"
  hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
  latest_custody_hash="$(printf '%s' "$state_json" | json_field data.latestCustodyState.stateHash 2>/dev/null || true)"
  latest_snapshot_phase="$(printf '%s' "$state_json" | json_field data.latestSnapshot.phase 2>/dev/null || true)"
  latest_snapshot_hand_number="$(printf '%s' "$state_json" | json_field data.latestSnapshot.handNumber 2>/dev/null || true)"
  pot_sats="$(printf '%s' "$state_json" | json_field data.publicState.potSats 2>/dev/null || true)"
  current_bet_sats="$(printf '%s' "$state_json" | json_field data.publicState.currentBetSats 2>/dev/null || true)"
  acting_seat_index="$(printf '%s' "$state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"
  if [[ -z "$phase" || "$phase" != "settled" ]]; then
    return 1
  fi
  if [[ -z "$hand_id" || "$hand_id" == "null" ]]; then
    return 1
  fi
  if [[ -z "$latest_custody_hash" || "$latest_custody_hash" == "null" ]]; then
    return 1
  fi
  if [[ "$state_json" != *"\"type\":\"HandResult\""* ]]; then
    if [[ "$latest_snapshot_phase" != "settled" ]]; then
      return 1
    fi
    if [[ -n "$hand_number" && "$hand_number" != "null" ]] && [[ "$latest_snapshot_hand_number" != "$hand_number" ]]; then
      return 1
    fi
    if [[ "${pot_sats:-}" != "0" || "${current_bet_sats:-}" != "0" ]]; then
      return 1
    fi
    if [[ -n "$acting_seat_index" && "$acting_seat_index" != "null" ]]; then
      return 1
    fi
    return 0
  fi
  if [[ "$state_json" != *"\"handId\":\"$hand_id\""* ]]; then
    return 1
  fi
  if [[ "$state_json" != *"\"latestCustodyStateHash\":\"$latest_custody_hash\""* ]]; then
    return 1
  fi
  return 0
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

configure_scenario_ark_delays() {
  local compose_file="$NIGIRI_DATADIR/docker-compose.yml"
  local exit_delay=""

  if recovery_timeout_scenario_enabled; then
    exit_delay="512"
  elif recovery_showdown_scenario_enabled; then
    exit_delay="512"
  elif challenge_escape_scenario_enabled; then
    exit_delay="512"
  fi
  if [[ -z "$exit_delay" ]]; then
    return 0
  fi
  wait_for_file "$compose_file" 120 0.25 >/dev/null
  SCENARIO_EXIT_DELAY="$exit_delay" /usr/bin/perl -0pi -e '
    use strict;
    use warnings;
    my $delay = $ENV{SCENARIO_EXIT_DELAY};
    s/ARKD_UNILATERAL_EXIT_DELAY: "[^"]+"/ARKD_UNILATERAL_EXIT_DELAY: "$delay"/g;
    if (/ARKD_UNILATERAL_EXIT_DELAY: "$delay"/ && !/ARKD_PUBLIC_UNILATERAL_EXIT_DELAY:/) {
      s/(ARKD_UNILATERAL_EXIT_DELAY: "$delay"\n)/$1      ARKD_PUBLIC_UNILATERAL_EXIT_DELAY: "$delay"\n/g;
    }
    s/ARKD_PUBLIC_UNILATERAL_EXIT_DELAY: "[^"]+"/ARKD_PUBLIC_UNILATERAL_EXIT_DELAY: "$delay"/g;
  ' "$compose_file"
  run_with_timeout 30 "$DOCKER_COMPOSE_BIN" -f "$compose_file" -p nigiri up -d --force-recreate ark >/dev/null
  wait_for_http_json "http://127.0.0.1:7070/v1/info" 120 1 >/dev/null
}

iso_epoch_seconds() {
  /usr/bin/perl -MTime::Piece -e '
    use strict;
    use warnings;
    my $value = shift @ARGV;
    my $epoch = Time::Piece->strptime($value, "%Y-%m-%dT%H:%M:%SZ")->epoch;
    print "$epoch\n";
  ' "$1"
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
    nigiri_cmd start --ark --ci >"$BASE/nigiri-start.log" 2>&1 &
    NIGIRI_START_PID=$!
    prepare_nigiri_data_dirs

    if wait_for_http_json "http://127.0.0.1:7070/v1/info" 120 1 >/dev/null &&
      configure_scenario_ark_delays &&
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
  args+=(--prefer-early-settlement)
  pdevtool "${args[@]}"
}

fund_and_onboard_profile() {
  local profile="$1"
  retry_pcli_json "$profile faucet" 20 1 wallet faucet "$FAUCET_SATS" --profile "$profile" --json >/dev/null
  retry_pcli_json "$profile onboard" 20 1 wallet onboard --profile "$profile" --json >/dev/null
}

wait_for_onchain_fee_reserve() {
  local address="$1"
  local attempts="${2:-120}"
  local utxos=""
  local count=""
  local i

  for ((i = 0; i < attempts; i += 1)); do
    utxos="$(curl -sf "http://127.0.0.1:3000/address/${address}/utxo" 2>/dev/null || true)"
    count="$(json_array_length "$utxos")"
    if [[ "$count" =~ ^[0-9]+$ ]] && (( count > 0 )); then
      return 0
    fi
    sleep 0.5
  done

  echo "timed out waiting for onchain fee reserve at $address" >&2
  return 1
}

fund_onchain_fee_reserve() {
  local profile="$1"
  local address=""
  local profile_state_path="$BASE/profiles/${profile}.json"

  retry_pcli_json "$profile wallet summary" 20 1 wallet summary --profile "$profile" --json >/dev/null
  if [[ ! -f "$profile_state_path" ]]; then
    echo "missing profile state for $profile while funding onchain reserve" >&2
    return 1
  fi

  address="$(jq -r '.cachedOnchainAddresses[0] // empty' "$profile_state_path" 2>/dev/null || true)"
  if [[ -z "$address" ]]; then
    echo "missing cached onchain address for $profile while funding onchain reserve" >&2
    return 1
  fi

  if ! nigiri_cmd faucet "$address" >/dev/null 2>&1; then
    echo "failed funding onchain fee reserve for $profile at $address" >&2
    return 1
  fi
  wait_for_onchain_fee_reserve "$address" >/dev/null
}

prepare_onchain_fee_reserve() {
  local profile="$1"

  fund_onchain_fee_reserve "$profile" || return 1
  retry_pcli_json "$profile wallet summary" 20 1 wallet summary --profile "$profile" --json >/dev/null
}

seed_recovery_fee_reserves() {
  local -a profiles=()
  local profile=""

  profiles+=(host)
  for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE"; do
    if [[ -n "$profile" && ! " ${profiles[*]} " =~ " ${profile} " ]]; then
      profiles+=("$profile")
    fi
  done

  echo "Funding onchain fee reserves for recovery broadcasters..." >&2
  for profile in "${profiles[@]}"; do
    fund_onchain_fee_reserve "$profile"
  done
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
  local cashout_timeout="$CASHOUT_TIMEOUT_SECONDS"

  if [[ -z "$cashout_timeout" ]]; then
    if tor_enabled; then
      cashout_timeout=90
    else
      cashout_timeout=60
    fi
  fi

  (
    PCLI_TIMEOUT_SECONDS="$cashout_timeout"
    if tor_enabled; then
      retry_pcli_json "$profile cashout over Tor" 60 1 funds cashout "$TABLE_ID" --profile "$profile" --json >/dev/null
      exit 0
    fi

    retry_pcli_json "$profile cashout" 40 0.5 funds cashout "$TABLE_ID" --profile "$profile" --json >/dev/null
  )
}

send_table_action_with_retry() {
  local actor="$1"
  local action="$2"
  local amount="${3:-}"
  local request_profile="$actor"
  local output=""
  local state_json=""
  local can_act=""
  local phase=""
  local attempts=60
  local i
  if [[ "$actor" != "host" ]] && ! host_player_scenario_enabled; then
    request_profile="host"
  fi
  local args=(table action "$action" --table-id "$TABLE_ID" --profile "$request_profile" --json)
  if [[ -n "$amount" ]]; then
    args=(table action "$action" "$amount" --table-id "$TABLE_ID" --profile "$request_profile" --json)
  fi
  if tor_enabled; then
    attempts=180
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(
      (
        PCLI_TIMEOUT_SECONDS=15
        pcli "${args[@]}"
      ) 2>&1
    )"; then
      return 0
    fi
    if [[ "$output" == *"cannot act while"* || "$output" == *"hand is still starting"* || "$output" == *"hand is not active"* ]]; then
      sleep 0.15
      continue
    fi
    if [[ "$output" == *"accepted history would roll back table events"* ]]; then
      sleep 0.25
      continue
    fi
    if [[ "$output" == *"missing forfeit tx"* ]]; then
      sleep 0.5
      continue
    fi
    if [[ "$output" == *"INVALID_INTENT_TIMERANGE"* || "$output" == *"proof of ownership expired"* ]]; then
      sleep 0.25
      continue
    fi
    if [[ "$output" == *"command timed out after"* ]]; then
      sleep 0.25
      continue
    fi
    if [[ "$output" == *"not enough intent confirmations received"* ]]; then
      sleep 0.5
      continue
    fi
    if tor_enabled; then
      state_json="$(pcli table watch "$TABLE_ID" --profile "$request_profile" --json 2>/dev/null || true)"
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

wait_for_action_progress() {
  local actor="$1"
  local previous_state_json="$2"
  shift 2
  local -a watch_profiles=("$@")
  local previous_custody_seq=""
  local previous_event_hash=""
  local previous_phase=""
  local previous_hand_id=""
  local previous_acting_seat=""
  local actor_state_json=""
  local actor_can_act=""
  local current_state_json=""
  local current_custody_seq=""
  local current_event_hash=""
  local current_phase=""
  local current_hand_id=""
  local current_acting_seat=""
  local attempts=80
  local sleep_seconds=0.25
  local i

  previous_custody_seq="$(printf '%s' "$previous_state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  previous_event_hash="$(printf '%s' "$previous_state_json" | json_field data.publicState.latestEventHash 2>/dev/null || true)"
  previous_phase="$(printf '%s' "$previous_state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  previous_hand_id="$(printf '%s' "$previous_state_json" | json_field data.publicState.handId 2>/dev/null || true)"
  previous_acting_seat="$(printf '%s' "$previous_state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"

  if [[ "${#watch_profiles[@]}" -eq 0 ]]; then
    watch_profiles=("$WATCH_PROFILE")
  fi

  if tor_enabled; then
    attempts=180
    sleep_seconds=0.5
  fi

  for ((i = 0; i < attempts; i += 1)); do
    current_state_json="$(freshest_table_state "${watch_profiles[@]}")"
    if table_has_settled_custody_checkpoint "$current_state_json"; then
      return 0
    fi
    current_custody_seq="$(printf '%s' "$current_state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
    current_event_hash="$(printf '%s' "$current_state_json" | json_field data.publicState.latestEventHash 2>/dev/null || true)"
    current_phase="$(printf '%s' "$current_state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    current_hand_id="$(printf '%s' "$current_state_json" | json_field data.publicState.handId 2>/dev/null || true)"
    current_acting_seat="$(printf '%s' "$current_state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"
    if [[ -n "$current_event_hash" && "$current_event_hash" != "$previous_event_hash" ]]; then
      return 0
    fi
    if [[ "$previous_custody_seq" =~ ^[0-9]+$ && "$current_custody_seq" =~ ^[0-9]+$ ]] && (( current_custody_seq > previous_custody_seq )); then
      return 0
    fi
    if [[ -n "$previous_phase" && "$current_phase" != "$previous_phase" ]]; then
      return 0
    fi
    if [[ -n "$previous_hand_id" && "$current_hand_id" != "$previous_hand_id" ]]; then
      return 0
    fi
    if [[ -n "$previous_acting_seat" && "$current_acting_seat" != "$previous_acting_seat" ]]; then
      return 0
    fi

    actor_state_json="$(watch_table_state_with_retry "$actor")"
    if table_has_settled_custody_checkpoint "$actor_state_json"; then
      return 0
    fi
    actor_can_act="$(printf '%s' "$actor_state_json" | json_field data.local.canAct 2>/dev/null || true)"
    if [[ "$actor_can_act" != "true" ]]; then
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for table action progress" >&2
  return 1
}

watch_table_state_with_retry() {
  local profile="${1:-alice}"
  local output=""
  local last_error=""
  local attempts=60
  local sleep_seconds=0.5
  local i

  if tor_enabled; then
    attempts=180
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(
      (
        raw_table_watch_json "$profile"
      ) 2>&1
    )"; then
      if [[ -z "$(printf '%s' "$output" | tr -d '[:space:]')" ]]; then
        last_error="empty table watch output"
        sleep "$sleep_seconds"
        continue
      fi
      printf '%s\n' "$output"
      return 0
    fi
    last_error="$output"
    sleep "$sleep_seconds"
  done

  if [[ -n "$last_error" ]]; then
    printf '%s\n' "$last_error" >&2
  fi
  echo "timed out waiting to watch table $TABLE_ID for $profile" >&2
  return 1
}

phase_supports_direct_action() {
  case "$1" in
    preflop | flop | turn | river) return 0 ;;
    *) return 1 ;;
  esac
}

round_watch_profiles() {
  local -a profiles=()
  local profile=""

  profiles+=("$WATCH_PROFILE")
  if ! host_player_scenario_enabled; then
    profiles+=(witness)
  fi
  for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE"; do
    [[ -n "$profile" ]] || continue
    if [[ ! " ${profiles[*]} " =~ " ${profile} " ]]; then
      profiles+=("$profile")
    fi
  done

  printf '%s\n' "${profiles[@]}"
}

turn_menu_action_cooldown_complete() {
  local state_json="$1"

  STATE_JSON="$state_json" python3 - <<'PY'
import datetime
import json
import os
import sys

raw = os.environ.get("STATE_JSON", "")
if not raw:
    sys.exit(1)
try:
    payload = json.loads(raw)
except json.JSONDecodeError:
    sys.exit(1)

data = payload.get("data") or {}
transitions = data.get("custodyTransitions") or []
if not transitions:
    sys.exit(0)
proof = (transitions[-1] or {}).get("proof") or {}
finalized_at = str(proof.get("finalizedAt") or "").strip()
if not finalized_at:
    sys.exit(0)
if finalized_at.endswith("Z"):
    finalized_at = finalized_at[:-1] + "+00:00"
try:
    finalized = datetime.datetime.fromisoformat(finalized_at)
except ValueError:
    sys.exit(0)
if finalized.tzinfo is None:
    finalized = finalized.replace(tzinfo=datetime.timezone.utc)
cooldown_seconds = 2.0
now = datetime.datetime.now(datetime.timezone.utc)
sys.exit(0 if (now - finalized).total_seconds() >= cooldown_seconds else 1)
PY
}

wait_for_actor_locally_actionable_state() {
  local actor="$1"
  local expected_state_json="$2"
  local attempts="${3:-80}"
  local sleep_seconds="${4:-0.15}"
  local state_json=""
  local expected_phase=""
  local expected_hand_id=""
  local expected_acting_seat=""
  local expected_custody_seq=""
  local expected_event_hash=""
  local phase=""
  local hand_id=""
  local acting_seat=""
  local custody_seq=""
  local event_hash=""
  local can_act=""
  local attempt

  expected_phase="$(printf '%s' "$expected_state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  expected_hand_id="$(printf '%s' "$expected_state_json" | json_field data.publicState.handId 2>/dev/null || true)"
  expected_acting_seat="$(printf '%s' "$expected_state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"
  expected_custody_seq="$(printf '%s' "$expected_state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  expected_event_hash="$(printf '%s' "$expected_state_json" | json_field data.publicState.latestEventHash 2>/dev/null || true)"

  if [[ -z "$actor" || -z "$expected_hand_id" || "$expected_hand_id" == "null" ]]; then
    return 1
  fi
  if [[ -z "$expected_phase" || "$expected_phase" == "null" ]] || ! phase_supports_direct_action "$expected_phase"; then
    return 1
  fi

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if ! state_json="$(watch_table_state_with_retry "$actor" 2>/dev/null)"; then
      sleep "$sleep_seconds"
      continue
    fi

    if table_has_settled_custody_checkpoint "$state_json"; then
      printf '%s\n' "$state_json"
      return 0
    fi

    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    hand_id="$(printf '%s' "$state_json" | json_field data.publicState.handId 2>/dev/null || true)"
    acting_seat="$(printf '%s' "$state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"
    custody_seq="$(printf '%s' "$state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
    event_hash="$(printf '%s' "$state_json" | json_field data.publicState.latestEventHash 2>/dev/null || true)"
    can_act="$(printf '%s' "$state_json" | json_field data.local.canAct 2>/dev/null || true)"

    if [[ "$hand_id" != "$expected_hand_id" ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    if [[ "$phase" != "$expected_phase" ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    if [[ -n "$expected_acting_seat" && "$expected_acting_seat" != "null" && "$acting_seat" != "$expected_acting_seat" ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    if [[ "$expected_custody_seq" =~ ^[0-9]+$ && "$custody_seq" =~ ^[0-9]+$ ]] && (( custody_seq < expected_custody_seq )); then
      sleep "$sleep_seconds"
      continue
    fi
    if [[ -n "$expected_event_hash" && "$expected_event_hash" != "null" && -n "$event_hash" && "$event_hash" != "$expected_event_hash" ]]; then
      return 1
    fi
    if [[ "$can_act" == "true" ]]; then
      if turn_menu_action_cooldown_complete "$state_json"; then
        printf '%s\n' "$state_json"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  return 1
}

freshest_table_state() {
  local -a profiles=("$@")
  local best_state_json=""
  local best_profile=""
  local best_epoch=-1
  local best_settled_checkpoint=0
  local best_custody_seq=-1
  local best_updated_at=""
  local best_event_count=-1
  local profile=""
  local state_json=""
  local epoch=""
  local settled_checkpoint=0
  local custody_seq=""
  local updated_at=""
  local event_count=""
  local events_json=""

  for profile in "${profiles[@]}"; do
    [[ -n "$profile" ]] || continue
    if ! state_json="$(watch_table_state_with_retry "$profile")"; then
      continue
    fi

    epoch="$(printf '%s' "$state_json" | json_field data.publicState.epoch 2>/dev/null || true)"
    custody_seq="$(printf '%s' "$state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
    updated_at="$(printf '%s' "$state_json" | json_field data.publicState.updatedAt 2>/dev/null || true)"
    events_json="$(printf '%s' "$state_json" | json_field data.events 2>/dev/null || true)"
    event_count="$(json_array_length "$events_json")"

    settled_checkpoint=0
    if table_has_settled_custody_checkpoint "$state_json"; then
      settled_checkpoint=1
    fi

    if [[ ! "$epoch" =~ ^[0-9]+$ ]]; then
      epoch=0
    fi
    if [[ ! "$custody_seq" =~ ^[0-9]+$ ]]; then
      custody_seq=-1
    fi
    if [[ -z "$updated_at" || "$updated_at" == "null" ]]; then
      updated_at=""
    fi
    if [[ ! "$event_count" =~ ^[0-9]+$ ]]; then
      event_count=0
    fi

    if (( epoch > best_epoch )) ||
      (( epoch == best_epoch && custody_seq > best_custody_seq )) ||
      [[ "$epoch" == "$best_epoch" && "$custody_seq" == "$best_custody_seq" && "$updated_at" > "$best_updated_at" ]] ||
      [[ "$epoch" == "$best_epoch" && "$custody_seq" == "$best_custody_seq" && "$updated_at" == "$best_updated_at" ]] && (( event_count > best_event_count )) ||
      [[ "$epoch" == "$best_epoch" && "$custody_seq" == "$best_custody_seq" && "$updated_at" == "$best_updated_at" ]] && (( event_count == best_event_count && settled_checkpoint > best_settled_checkpoint )); then
      best_state_json="$state_json"
      best_profile="$profile"
      best_epoch="$epoch"
      best_settled_checkpoint="$settled_checkpoint"
      best_custody_seq="$custody_seq"
      best_updated_at="$updated_at"
      best_event_count="$event_count"
    fi
  done

  if [[ -z "$best_state_json" ]]; then
    echo "timed out waiting to watch table $TABLE_ID across profiles: ${profiles[*]}" >&2
    return 1
  fi

  printf '%s\n' "$best_state_json"
}

json_array_length() {
  local raw="${1:-}"
  if [[ -z "$(printf '%s' "$raw" | tr -d '[:space:]')" ]]; then
    printf '0\n'
    return 0
  fi
  printf '%s' "$raw" | /usr/bin/perl -MJSON::PP -e '
    use strict;
    use warnings;

    local $/;
    my $raw = <STDIN>;
    my $decoded = eval { JSON::PP::decode_json($raw) };
    if ($@ || ref($decoded) ne "ARRAY") {
      print "0\n";
      exit 0;
    }
    print scalar(@{$decoded}), "\n";
  '
}

profile_player_id() {
  case "$1" in
    host) printf '%s\n' "${HOST_PLAYER_ID:-}" ;;
    alice) printf '%s\n' "${ALICE_PLAYER_ID:-}" ;;
    bob) printf '%s\n' "${BOB_PLAYER_ID:-}" ;;
    *) printf '\n' ;;
  esac
}

other_round_player_profile() {
  case "$1" in
    "$PLAYER_ONE_PROFILE") printf '%s\n' "$PLAYER_TWO_PROFILE" ;;
    "$PLAYER_TWO_PROFILE") printf '%s\n' "$PLAYER_ONE_PROFILE" ;;
    *) printf '\n' ;;
  esac
}

challenge_survivor_cashout_profile() {
  local challenge_artifact="$BASE/artifacts/table-after-challenge-open.json"
  local dead_player_id=""

  if [[ ! -f "$challenge_artifact" ]]; then
    return 1
  fi

  dead_player_id="$(json_field data.pendingTurnChallenge.timeoutResolution.deadPlayerIds.0 <"$challenge_artifact" 2>/dev/null || true)"
  if [[ -z "$dead_player_id" || "$dead_player_id" == "null" ]]; then
    dead_player_id="$(json_field data.pendingTurnChallenge.timeoutResolution.actingPlayerId <"$challenge_artifact" 2>/dev/null || true)"
  fi
  if [[ -z "$dead_player_id" || "$dead_player_id" == "null" ]]; then
    dead_player_id="$(json_field data.pendingTurnChallenge.actingPlayerId <"$challenge_artifact" 2>/dev/null || true)"
  fi

  case "$dead_player_id" in
    "$PLAYER_ONE_PLAYER_ID") printf '%s\n' "$PLAYER_TWO_PROFILE" ;;
    "$PLAYER_TWO_PLAYER_ID") printf '%s\n' "$PLAYER_ONE_PROFILE" ;;
    *) return 1 ;;
  esac
}

profile_peer_id() {
  case "$1" in
    host) printf '%s\n' "${HOST_PEER_ID:-}" ;;
    witness) printf '%s\n' "${WITNESS_PEER_ID:-}" ;;
    alice) printf '%s\n' "${ALICE_PEER_ID:-}" ;;
    bob) printf '%s\n' "${BOB_PEER_ID:-}" ;;
    *) printf '\n' ;;
  esac
}

seat_index_for_player() {
  local state_json="$1"
  local player_id="$2"
  local seat_player_id=""
  local seat_index=""
  local seat

  for seat in 0 1; do
    seat_player_id="$(printf '%s' "$state_json" | json_field "data.publicState.seatedPlayers.${seat}.playerId" 2>/dev/null || true)"
    if [[ "$seat_player_id" != "$player_id" ]]; then
      continue
    fi
    seat_index="$(printf '%s' "$state_json" | json_field "data.publicState.seatedPlayers.${seat}.seatIndex" 2>/dev/null || true)"
    if [[ -n "$seat_index" && "$seat_index" != "null" ]]; then
      printf '%s\n' "$seat_index"
      return 0
    fi
  done

  return 1
}

seat_status_for_player() {
  local state_json="$1"
  local player_id="$2"
  local seat_player_id=""
  local seat_status=""
  local seat

  for seat in 0 1; do
    seat_player_id="$(printf '%s' "$state_json" | json_field "data.publicState.seatedPlayers.${seat}.playerId" 2>/dev/null || true)"
    if [[ "$seat_player_id" != "$player_id" ]]; then
      continue
    fi
    seat_status="$(printf '%s' "$state_json" | json_field "data.publicState.seatedPlayers.${seat}.status" 2>/dev/null || true)"
    if [[ -n "$seat_status" && "$seat_status" != "null" ]]; then
      printf '%s\n' "$seat_status"
      return 0
    fi
  done

  return 1
}

wait_for_profile_cashout_visibility() {
  local observer_profile="$1"
  local target_profile="$2"
  local expected_status="${3:-completed}"
  local target_player_id=""
  local attempts=80
  local sleep_seconds=0.5
  local state_json=""
  local observed_status=""
  local i

  target_player_id="$(profile_player_id "$target_profile")"
  if [[ -z "$target_player_id" ]]; then
    echo "missing player id for cash-out profile $target_profile" >&2
    return 1
  fi
  if tor_enabled; then
    attempts=180
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    state_json="$(watch_table_state_with_retry "$observer_profile")"
    observed_status="$(seat_status_for_player "$state_json" "$target_player_id" 2>/dev/null || true)"
    if [[ "$observed_status" == "$expected_status" ]]; then
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for $observer_profile to observe $target_profile status=$expected_status" >&2
  return 1
}

acting_profile_for_state() {
  local state_json="$1"
  local acting_seat=""
  local profile=""
  local player_id=""
  local seat_index=""

  acting_seat="$(printf '%s' "$state_json" | json_field data.publicState.actingSeatIndex 2>/dev/null || true)"
  if [[ -z "$acting_seat" || "$acting_seat" == "null" ]]; then
    return 1
  fi

  for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE"; do
    player_id="$(profile_player_id "$profile")"
    [[ -n "$player_id" ]] || continue
    seat_index="$(seat_index_for_player "$state_json" "$player_id" 2>/dev/null || true)"
    if [[ "$seat_index" == "$acting_seat" ]]; then
      printf '%s\n' "$profile"
      return 0
    fi
  done

  return 1
}

profile_for_seat_index() {
  local state_json="$1"
  local target_seat_index="$2"
  local profile=""
  local player_id=""
  local seat_index=""

  if [[ -z "$target_seat_index" || "$target_seat_index" == "null" ]]; then
    return 1
  fi

  for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE"; do
    player_id="$(profile_player_id "$profile")"
    [[ -n "$player_id" ]] || continue
    seat_index="$(seat_index_for_player "$state_json" "$player_id" 2>/dev/null || true)"
    if [[ "$seat_index" == "$target_seat_index" ]]; then
      printf '%s\n' "$profile"
      return 0
    fi
  done

  return 1
}

challenger_profile_for_state() {
  local state_json="$1"
  acting_profile_for_state "$state_json"
}

latest_transition_index_from_state() {
  local state_json="$1"
  local custody_seq=""

  custody_seq="$(printf '%s' "$state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  if [[ -z "$custody_seq" || "$custody_seq" == "null" || ! "$custody_seq" =~ ^[0-9]+$ || "$custody_seq" -le 0 ]]; then
    return 1
  fi
  printf '%s\n' "$((custody_seq - 1))"
}

find_recovery_bundle_index() {
  local state_json="$1"
  local transition_index="$2"
  local expected_kind="$3"
  local bundles_json=""
  local bundle_count=0
  local bundle_index
  local bundle_kind=""

  bundles_json="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryBundles" 2>/dev/null || true)"
  bundle_count="$(json_array_length "$bundles_json")"
  for ((bundle_index = 0; bundle_index < bundle_count; bundle_index += 1)); do
    bundle_kind="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryBundles.${bundle_index}.kind" 2>/dev/null || true)"
    if [[ "$bundle_kind" == "$expected_kind" ]]; then
      printf '%s\n' "$bundle_index"
      return 0
    fi
  done

  return 1
}

wait_for_actionable_table_state() {
  local profile="${1:-host}"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local phase=""
  local first_option_type=""
  local local_can_act=""
  local local_option_id=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    if [[ "$phase" == "preflop" || "$phase" == "flop" || "$phase" == "turn" || "$phase" == "river" ]]; then
      first_option_type="$(printf '%s' "$state_json" | json_field data.pendingTurnMenu.options.0.action.type 2>/dev/null || true)"
      if [[ -z "$first_option_type" || "$first_option_type" == "null" ]]; then
        first_option_type="$(printf '%s' "$state_json" | json_field data.pendingTurnMenu.options.0.action.Type 2>/dev/null || true)"
      fi
      if acting_profile_for_state "$state_json" >/dev/null 2>&1 &&
        [[ -n "$first_option_type" && "$first_option_type" != "null" ]]; then
        if [[ "$profile" == "host" || "$profile" == "witness" ]]; then
          printf '%s\n' "$state_json"
          return 0
        fi
        local_can_act="$(printf '%s' "$state_json" | json_field data.local.canAct 2>/dev/null || true)"
        local_option_id="$(printf '%s' "$state_json" | json_field data.local.turnMenu.options.0.optionId 2>/dev/null || true)"
        if [[ "$local_can_act" == "true" && -n "$local_option_id" && "$local_option_id" != "null" ]]; then
          if turn_menu_action_cooldown_complete "$state_json"; then
            printf '%s\n' "$state_json"
            return 0
          fi
        fi
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for an actionable table state with a deterministic turn menu" >&2
  return 1
}

wait_for_latest_custody_seq_gt() {
  local profile="$1"
  local previous_seq="$2"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""
  local custody_seq=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    custody_seq="$(printf '%s' "$state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
    if [[ "$custody_seq" =~ ^[0-9]+$ ]] && (( custody_seq > previous_seq )); then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for custody sequence to advance beyond $previous_seq" >&2
  return 1
}

forced_aggression_action_for_state() {
  local state_json="$1"
  local actor_profile="$2"
  local actor_player_id=""
  local selected=""

  actor_player_id="$(profile_player_id "$actor_profile" 2>/dev/null || true)"
  [[ -n "$actor_player_id" ]] || return 1

  selected="$(STATE_JSON="$state_json" python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("STATE_JSON", "")
if not raw:
    sys.exit(1)
try:
    payload = json.loads(raw)
except json.JSONDecodeError:
    sys.exit(1)

menu = ((payload.get("data") or {}).get("local") or {}).get("turnMenu") or {}
if not (menu.get("options") or []):
    menu = ((payload.get("data") or {}).get("pendingTurnMenu") or {})
options = menu.get("options") or []
preferred = {"raise": 0, "bet": 1}

best = None
for option in options:
    action = option.get("action") or {}
    action_type = str(action.get("type") or action.get("Type") or "").strip().lower()
    if action_type not in preferred:
        continue
    rank = preferred[action_type]
    total = action.get("totalSats")
    if total is None:
        total = action.get("TotalSats")
    if action_type in {"raise", "bet"} and not isinstance(total, int):
        continue
    candidate = (rank, action_type, total)
    if best is None or candidate < best:
        best = candidate

if best is None:
    print("no aggressive deterministic menu option is available for the current turn", file=sys.stderr)
    sys.exit(1)

_, action_type, total = best
print(f"{action_type} {total}")
PY
)" || return 1
  [[ -n "$selected" ]] || return 1
  printf '%s\n' "$selected"
}

passive_action_for_state() {
  local state_json="$1"
  local selected=""

  selected="$(STATE_JSON="$state_json" python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("STATE_JSON", "")
if not raw:
    sys.exit(1)
payload = json.loads(raw)
menu = ((payload.get("data") or {}).get("local") or {}).get("turnMenu") or {}
if not (menu.get("options") or []):
    menu = ((payload.get("data") or {}).get("pendingTurnMenu") or {})
options = menu.get("options") or []
priority = {"check": 0, "call": 1, "fold": 2}
best = None
for option in options:
    action = option.get("action") or {}
    action_type = str(action.get("type") or action.get("Type") or "").strip().lower()
    if action_type not in priority:
        continue
    candidate = (priority[action_type], action_type)
    if best is None or candidate < best:
        best = candidate
if best is None:
    sys.exit(1)
_, action_type = best
print(action_type)
PY
)" || return 1
  [[ -n "$selected" ]] || return 1
  printf '%s\n' "$selected"
}

all_in_action_for_state() {
  local state_json="$1"
  local selected=""

  selected="$(STATE_JSON="$state_json" python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("STATE_JSON", "")
if not raw:
    sys.exit(1)
payload = json.loads(raw)
menu = ((payload.get("data") or {}).get("local") or {}).get("turnMenu") or {}
if not (menu.get("options") or []):
    menu = ((payload.get("data") or {}).get("pendingTurnMenu") or {})
options = menu.get("options") or []
best = None
for option in options:
    action = option.get("action") or {}
    action_type = str(action.get("type") or action.get("Type") or "").strip().lower()
    if action_type not in {"bet", "raise"}:
        continue
    total = action.get("totalSats")
    if total is None:
        total = action.get("TotalSats")
    if not isinstance(total, int):
        continue
    candidate = (total, action_type)
    if best is None or candidate > best:
        best = candidate
if best is None:
    sys.exit(1)
total, action_type = best
print(f"{action_type} {total}")
PY
)" || return 1
  [[ -n "$selected" ]] || return 1
  printf '%s\n' "$selected"
}

challenge_option_id_for_state() {
  local state_json="$1"
  local selected=""

  selected="$(STATE_JSON="$state_json" python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("STATE_JSON", "")
if not raw:
    sys.exit(1)
payload = json.loads(raw)
menu = ((payload.get("data") or {}).get("local") or {}).get("turnMenu") or {}
if not (menu.get("options") or []):
    menu = ((payload.get("data") or {}).get("pendingTurnMenu") or {})
options = menu.get("options") or []
priority = {"check": 0, "call": 1, "bet": 2, "raise": 3, "fold": 4}
best = None
for option in options:
    action = option.get("action") or {}
    action_type = str(action.get("type") or action.get("Type") or "").strip().lower()
    option_id = str(option.get("optionId") or "").strip()
    if action_type not in priority or not option_id:
        continue
    total = action.get("totalSats")
    if total is None:
        total = action.get("TotalSats")
    if not isinstance(total, int):
        total = -1
    candidate = (priority[action_type], -total, option_id)
    if best is None or candidate < best:
        best = candidate
if best is None:
    sys.exit(1)
print(best[2])
PY
)" || return 1
  [[ -n "$selected" ]] || return 1
  printf '%s\n' "$selected"
}

wait_for_public_phase() {
  local profile="$1"
  local desired_phase="$2"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""
  local phase=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    if [[ "$phase" == "$desired_phase" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for table $TABLE_ID to reach phase=$desired_phase for $profile" >&2
  return 1
}

wait_for_showdown_recovery_checkpoint() {
  local profile="$1"
  local expected_hand_number="${2:-}"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""
  local phase=""
  local hand_number=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    if recovery_showdown_scenario_enabled; then
      phase="$(printf '%s' "$state_json" | jq -r '.data.publicState.phase // empty' 2>/dev/null || true)"
      hand_number="$(printf '%s' "$state_json" | jq -r '.data.publicState.handNumber // empty' 2>/dev/null || true)"
    else
      phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
      hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    fi
    if [[ -n "$expected_hand_number" && "$expected_hand_number" =~ ^[0-9]+$ && "$hand_number" =~ ^[0-9]+$ && "$hand_number" != "$expected_hand_number" ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    if [[ "$phase" == "showdown-reveal" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for showdown recovery checkpoint on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_board_open_phase() {
  local profile="$1"
  local desired_phase="$2"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""
  local phase=""
  local board_open_count=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    if [[ "$phase" != "$desired_phase" ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    board_open_count="$(printf '%s' "$state_json" | jq -r --arg phase "$desired_phase" '[.data.activeHand.cards.transcript.records[]? | select(.kind == "board-open" and .phase == $phase)] | length' 2>/dev/null || true)"
    if [[ "$board_open_count" =~ ^[0-9]+$ ]] && (( board_open_count > 0 )); then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for board-open during phase=$desired_phase on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_transcript_record() {
  local profile="$1"
  local desired_phase="$2"
  local desired_kind="$3"
  local seat_profile="${4:-}"
  local expected_hand_number="${5:-}"
  local attempts="${6:-240}"
  local sleep_seconds="${7:-0.5}"
  local state_json=""
  local seat_filter=""
  local player_id=""
  local record_count=""
  local current_hand_number=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    current_hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    if [[ "$expected_hand_number" =~ ^[0-9]+$ && "$current_hand_number" =~ ^[0-9]+$ ]] && (( current_hand_number > expected_hand_number )); then
      return 1
    fi
    seat_filter=""
    if [[ -n "$seat_profile" ]]; then
      player_id="$(profile_player_id "$seat_profile" 2>/dev/null || true)"
      if [[ -z "$player_id" ]]; then
        sleep "$sleep_seconds"
        continue
      fi
      seat_filter="$(seat_index_for_player "$state_json" "$player_id" 2>/dev/null || true)"
      if [[ -z "$seat_filter" ]]; then
        sleep "$sleep_seconds"
        continue
      fi
    fi
    record_count="$(printf '%s' "$state_json" | jq -r --arg phase "$desired_phase" --arg kind "$desired_kind" --arg seat "$seat_filter" '[.data.activeHand.cards.transcript.records[]? | select(.kind == $kind and .phase == $phase and ($seat == "" or ((.seatIndex // "") | tostring) == $seat))] | length' 2>/dev/null || true)"
    if [[ "$record_count" =~ ^[0-9]+$ ]] && (( record_count > 0 )); then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for transcript record kind=$desired_kind phase=$desired_phase on table $TABLE_ID for $profile" >&2
  return 1
}

log_recovery_showdown_step() {
  local message="$1"
  local log_path="$BASE/recovery-showdown.log"
  local timestamp=""

  timestamp="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf '%s %s\n' "$timestamp" "$message" >>"$log_path"
  printf '%s\n' "$message" >&2
}

wait_for_event_type() {
  local profile="$1"
  local desired_type="$2"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    if [[ "$state_json" == *"\"type\":\"$desired_type\""* ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for event type $desired_type on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_event_type_any() {
  local desired_type="$1"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local profile=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    while IFS= read -r profile; do
      [[ -n "$profile" ]] || continue
      state_json="$(watch_table_state_with_retry "$profile" 1 "$sleep_seconds" 2>/dev/null || true)"
      if [[ -n "$state_json" && "$state_json" == *"\"type\":\"$desired_type\""* ]]; then
        printf '%s\n' "$state_json"
        return 0
      fi
    done < <(round_watch_profiles)
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for event type $desired_type on table $TABLE_ID across all replicas" >&2
  return 1
}

trigger_emergency_exit_with_retry() {
  local profile="$1"
  local attempts="${2:-20}"
  local sleep_seconds="${3:-1}"
  local output=""
  local state_json=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    output="$(pcli funds exit "$TABLE_ID" --profile "$profile" --json 2>&1 || true)"
    state_json="$(wait_for_event_type_any EmergencyExit 20 0.5 2>/dev/null || true)"
    if [[ -n "$state_json" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for emergency exit acceptance on table $TABLE_ID for $profile" >&2
  printf '%s\n' "$output" >&2
  return 1
}

abort_hand_with_retry() {
  local attempts="${1:-40}"
  local sleep_seconds="${2:-0.1}"
  local output=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli table abort-hand "$TABLE_ID" --profile host --json 2>/dev/null)"; then
      if [[ "$output" == *"\"type\":\"HandAbort\""* ]]; then
        printf '%s\n' "$output"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out aborting the active hand on table $TABLE_ID" >&2
  return 1
}

rotate_host_with_retry() {
  local profile="${1:-witness}"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-0.5}"
  local output=""
  local expected_host_peer_id=""
  local observed_host_peer_id=""

  expected_host_peer_id="$(profile_peer_id "$profile")"
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli table rotate-host "$TABLE_ID" --profile "$profile" --json 2>/dev/null)"; then
      observed_host_peer_id="$(printf '%s' "$output" | json_field data.currentHost.peer.peerId 2>/dev/null || true)"
      if [[ -z "$observed_host_peer_id" || "$observed_host_peer_id" == "null" ]]; then
        observed_host_peer_id="$(printf '%s' "$output" | json_field data.config.hostPeerId 2>/dev/null || true)"
      fi
      if [[ "$output" == *"\"type\":\"HostRotated\""* ]] || [[ -n "$expected_host_peer_id" && "$observed_host_peer_id" == "$expected_host_peer_id" ]]; then
        printf '%s\n' "$output"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out rotating host for table $TABLE_ID via $profile" >&2
  return 1
}

wait_for_transition_kind_present() {
  local profile="$1"
  local desired_kind="$2"
  local attempts="${3:-240}"
  local sleep_seconds="${4:-0.5}"
  local state_json=""
  local transition_index=""
  local kind=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    transition_index="$(latest_transition_index_from_state "$state_json" 2>/dev/null || true)"
    if [[ -n "$transition_index" ]]; then
      kind="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.kind" 2>/dev/null || true)"
      if [[ "$kind" == "$desired_kind" ]]; then
        printf '%s\n' "$state_json"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for custody transition kind $desired_kind on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_pending_turn_challenge() {
  local profile="${1:-host}"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local status=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    status="$(printf '%s' "$state_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)"
    if [[ "$status" == "open" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for an open pending turn challenge on table $TABLE_ID for $profile" >&2
  return 1
}

CHALLENGE_OBSERVED_PROFILE=""
CHALLENGE_OBSERVED_STATE_JSON=""

wait_for_pending_turn_challenge_any() {
  local attempts="${1:-240}"
  local sleep_seconds="${2:-0.5}"
  local state_json=""
  local status=""
  local profile=""

  CHALLENGE_OBSERVED_PROFILE=""
  CHALLENGE_OBSERVED_STATE_JSON=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE" host witness; do
      [[ -n "$profile" ]] || continue
      state_json="$(watch_table_state_with_retry "$profile")"
      status="$(printf '%s' "$state_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)"
      if [[ "$status" == "open" ]]; then
        CHALLENGE_OBSERVED_PROFILE="$profile"
        CHALLENGE_OBSERVED_STATE_JSON="$state_json"
        printf '%s\n' "$state_json"
        return 0
      fi
    done
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for an open pending turn challenge on table $TABLE_ID across local replicas" >&2
  return 1
}

wait_for_turn_menu_challenge_envelope() {
  local profile="${1:-host}"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local bundle_hash=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    bundle_hash="$(printf '%s' "$state_json" | json_field data.pendingTurnMenu.challengeEnvelope.openBundle.bundleHash 2>/dev/null || true)"
    if [[ -n "$bundle_hash" && "$bundle_hash" != "null" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for a pending turn menu challenge envelope on table $TABLE_ID for $profile" >&2
  return 1
}

CHALLENGE_READY_PROFILE=""
CHALLENGE_READY_STATE_JSON=""

wait_for_challenge_ready_turn_state() {
  local attempts="${1:-240}"
  local sleep_seconds="${2:-0.5}"
  local profile=""
  local state_json=""
  local phase=""
  local can_act=""
  local option_id=""
  local bundle_hash=""

  CHALLENGE_READY_PROFILE=""
  CHALLENGE_READY_STATE_JSON=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    for profile in "$PLAYER_ONE_PROFILE" "$PLAYER_TWO_PROFILE"; do
      [[ -n "$profile" ]] || continue
      if ! state_json="$(watch_table_state_with_retry "$profile" 1 "$sleep_seconds" 2>/dev/null)"; then
        continue
      fi
      phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
      if ! phase_supports_direct_action "$phase"; then
        continue
      fi
      can_act="$(printf '%s' "$state_json" | json_field data.local.canAct 2>/dev/null || true)"
      option_id="$(printf '%s' "$state_json" | json_field data.local.turnMenu.options.0.optionId 2>/dev/null || true)"
      bundle_hash="$(printf '%s' "$state_json" | json_field data.pendingTurnMenu.challengeEnvelope.openBundle.bundleHash 2>/dev/null || true)"
      if [[ "$can_act" != "true" || -z "$option_id" || "$option_id" == "null" || -z "$bundle_hash" || "$bundle_hash" == "null" ]]; then
        continue
      fi
      if ! turn_menu_action_cooldown_complete "$state_json"; then
        continue
      fi
      CHALLENGE_READY_PROFILE="$profile"
      CHALLENGE_READY_STATE_JSON="$state_json"
      printf '%s\n' "$state_json"
      return 0
    done
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for a locally actionable turn menu with challenge envelope" >&2
  return 1
}

prepare_challenge_ready_turn() {
  local _hand_attempts="${1:-3}"
  local hand_attempt=""
  local state_json=""
  local aborted_state_json=""
  local aborted_hand_number=""

  CHALLENGE_READY_PROFILE=""
  CHALLENGE_READY_STATE_JSON=""

  for ((hand_attempt = 1; hand_attempt <= _hand_attempts; hand_attempt += 1)); do
    if wait_for_challenge_ready_turn_state 120 0.5 >/dev/null 2>&1; then
      state_json="$CHALLENGE_READY_STATE_JSON"
      return 0
    fi

    if (( hand_attempt >= _hand_attempts )); then
      break
    fi

    aborted_state_json="$(wait_for_round_profiles_settled_abort_hand 20 0.5 2>/dev/null || true)"
    if [[ -z "$aborted_state_json" ]]; then
      break
    fi
    aborted_hand_number="$(printf '%s' "$aborted_state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    if [[ -z "$aborted_hand_number" || "$aborted_hand_number" == "null" ]]; then
      break
    fi

    echo "Observed a settled hand abort before the turn became challenge-ready; starting the next hand..." >&2
    start_next_hand_with_retry host "$aborted_hand_number" 120 0.5 >/dev/null || return 1
  done

  echo "timed out waiting for a challenge-ready turn menu on the current hand" >&2
  return 1
}

wait_for_turn_challenge_cleared() {
  local profile="${1:-host}"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local status=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    status="$(printf '%s' "$state_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)"
    if [[ -z "$status" || "$status" == "null" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for the pending turn challenge to clear on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_turn_challenge_escape_ready() {
  local profile="${1:-host}"
  local attempts="${2:-240}"
  local sleep_seconds="${3:-0.5}"
  local state_json=""
  local ready=""
  local escape_eligible_at=""
  local eligible_epoch=""
  local now_epoch=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    ready="$(printf '%s' "$state_json" | json_field data.local.turnChallengeChain.escapeReady 2>/dev/null || true)"
    if [[ "$ready" == "true" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    escape_eligible_at="$(printf '%s' "$state_json" | json_field data.pendingTurnChallenge.escapeEligibleAt 2>/dev/null || true)"
    if [[ -n "$escape_eligible_at" && "$escape_eligible_at" != "null" ]]; then
      eligible_epoch="$(iso_epoch_seconds "$escape_eligible_at" 2>/dev/null || true)"
      now_epoch="$(date -u +%s)"
      if [[ "$eligible_epoch" =~ ^[0-9]+$ && "$now_epoch" =~ ^[0-9]+$ ]] && (( now_epoch >= eligible_epoch )); then
        printf '%s\n' "$state_json"
        return 0
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for challenge escape readiness on table $TABLE_ID for $profile" >&2
  return 1
}

wait_for_recovered_showdown_transition() {
  local profile="${1:-host}"
  local source_transition_hash="${2:-}"
  local previous_seq="${3:-}"
  local attempts="${4:-360}"
  local sleep_seconds="${5:-1}"
  local state_json=""
  local transition_count=""
  local transition_index=""
  local custody_seq=""
  local kind=""
  local recovery_source_hash=""
  local recovery_witness=""
  local settlement_witness=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    transition_count="$(printf '%s' "$state_json" | jq -r '.data.custodyTransitions | length' 2>/dev/null || true)"
    if [[ ! "$transition_count" =~ ^[0-9]+$ ]] || (( transition_count == 0 )); then
      sleep "$sleep_seconds"
      continue
    fi
    for ((transition_index = transition_count - 1; transition_index >= 0; transition_index -= 1)); do
      kind="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.kind" 2>/dev/null || true)"
      [[ "$kind" == "showdown-payout" ]] || continue

      custody_seq="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.custodySeq" 2>/dev/null || true)"
      if [[ "$previous_seq" =~ ^[0-9]+$ && "$custody_seq" =~ ^[0-9]+$ ]] && (( custody_seq <= previous_seq )); then
        continue
      fi

      recovery_witness="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryWitness" 2>/dev/null || true)"
      [[ -n "$recovery_witness" && "$recovery_witness" != "null" ]] || continue

      settlement_witness="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.settlementWitness" 2>/dev/null || true)"
      [[ -z "$settlement_witness" || "$settlement_witness" == "null" ]] || continue

      recovery_source_hash="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryWitness.sourceTransitionHash" 2>/dev/null || true)"
      if [[ -n "$source_transition_hash" && "$source_transition_hash" != "null" && "$recovery_source_hash" != "$source_transition_hash" ]]; then
        continue
      fi

      printf '%s\n' "$state_json"
      return 0
    done
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for recovery-backed showdown payout on table $TABLE_ID for $profile" >&2
  return 1
}

cached_onchain_address_for_profile() {
  local profile="$1"
  local profile_state_path="$BASE/profiles/${profile}.json"

  if [[ ! -f "$profile_state_path" ]]; then
    return 1
  fi
  jq -r '.cachedOnchainAddresses[0] // empty' "$profile_state_path" 2>/dev/null
}

mine_regtest_block() {
  local profile="${1:-host}"
  local address=""

  address="$(cached_onchain_address_for_profile "$profile" 2>/dev/null || true)"
  if [[ -z "$address" ]]; then
    echo "missing cached onchain address for $profile while mining a regtest block" >&2
    return 1
  fi
  nigiri_cmd faucet "$address" >/dev/null 2>&1
}

restart_profile_daemon() {
  local profile="$1"

  case "$profile" in
    host)
      start_profile_daemon host host "$HOST_PORT" "$BASE/host.log" "$HOST_TOR_SOCKS_PORT" "$HOST_TOR_CONTROL_PORT" "$TOR_STATE_BASE/host/control_auth_cookie"
      HOST_DAEMON_PID="$LAUNCHED_PID"
      ;;
    witness)
      start_profile_daemon witness witness "$WITNESS_PORT" "$BASE/witness.log" "$WITNESS_TOR_SOCKS_PORT" "$WITNESS_TOR_CONTROL_PORT" "$TOR_STATE_BASE/witness/control_auth_cookie"
      WITNESS_DAEMON_PID="$LAUNCHED_PID"
      ;;
    alice)
      start_profile_daemon alice player "$ALICE_PORT" "$BASE/alice.log" "$ALICE_TOR_SOCKS_PORT" "$ALICE_TOR_CONTROL_PORT" "$TOR_STATE_BASE/alice/control_auth_cookie"
      ALICE_DAEMON_PID="$LAUNCHED_PID"
      ;;
    bob)
      start_profile_daemon bob player "$BOB_PORT" "$BASE/bob.log" "$BOB_TOR_SOCKS_PORT" "$BOB_TOR_CONTROL_PORT" "$TOR_STATE_BASE/bob/control_auth_cookie"
      BOB_DAEMON_PID="$LAUNCHED_PID"
      ;;
    *)
      echo "unsupported profile restart: $profile" >&2
      return 1
      ;;
  esac
  sleep 0.5
}

clear_profile_daemon_pid() {
  case "$1" in
    host) HOST_DAEMON_PID="" ;;
    witness) WITNESS_DAEMON_PID="" ;;
    alice) ALICE_DAEMON_PID="" ;;
    bob) BOB_DAEMON_PID="" ;;
  esac
}

profile_daemon_pid() {
  case "$1" in
    host) printf '%s\n' "${HOST_DAEMON_PID:-}" ;;
    witness) printf '%s\n' "${WITNESS_DAEMON_PID:-}" ;;
    alice) printf '%s\n' "${ALICE_DAEMON_PID:-}" ;;
    bob) printf '%s\n' "${BOB_DAEMON_PID:-}" ;;
    *) printf '\n' ;;
  esac
}

stop_profile_daemon() {
  local profile="$1"
  local pid=""

  pid="$(profile_daemon_pid "$profile")"
  if [[ -n "$pid" ]]; then
    terminate_pid "$pid"
  else
    pcli daemon stop --profile "$profile" >/dev/null 2>&1 || true
  fi
  clear_profile_daemon_pid "$profile"
}

kill_profile_daemon_immediately() {
  local profile="$1"
  local pid=""

  pid="$(profile_daemon_pid "$profile")"
  if [[ -n "$pid" ]]; then
    kill -9 "$pid" 2>/dev/null || true
  else
    pcli daemon stop --profile "$profile" >/dev/null 2>&1 || true
  fi
  clear_profile_daemon_pid "$profile"
}

stop_recovery_timeout_services() {
  if [[ -n "${1:-}" ]]; then
    stop_profile_daemon "$1"
  fi
  if [[ -n "${INDEXER_PID:-}" ]]; then
    terminate_pid "$INDEXER_PID"
    INDEXER_PID=""
  fi
  run_with_timeout 15 docker stop ark >/dev/null 2>&1 || true
}

stop_recovery_showdown_services() {
  if [[ -n "${1:-}" ]]; then
    kill_profile_daemon_immediately "$1"
  fi
  run_with_timeout 3 docker kill ark >/dev/null 2>&1 || run_with_timeout 15 docker stop ark >/dev/null 2>&1 || true
}

wait_for_timeout_recovery_transition() {
  local profile="$1"
  local source_transition_hash="$2"
  local previous_seq="$3"
  local attempts="${4:-360}"
  local sleep_seconds="${5:-1}"
  local state_json=""
  local custody_seq=""
  local transition_index=""
  local previous_transition_hash=""
  local kind=""
  local recovery_bundle_hash=""
  local recovery_source_hash=""
  local recovery_txid=""
  local settlement_witness=""
  local challenge_bundle_kind=""
  local challenge_bundle_hash=""
  local challenge_txid=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    state_json="$(watch_table_state_with_retry "$profile")"
    custody_seq="$(printf '%s' "$state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
    if [[ ! "$custody_seq" =~ ^[0-9]+$ ]] || (( custody_seq <= previous_seq )); then
      sleep "$sleep_seconds"
      continue
    fi
    transition_index="$(latest_transition_index_from_state "$state_json" 2>/dev/null || true)"
    [[ -n "$transition_index" ]] || {
      sleep "$sleep_seconds"
      continue
    }
    kind="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.kind" 2>/dev/null || true)"
    previous_transition_hash=""
    if [[ "$transition_index" =~ ^[0-9]+$ ]] && (( transition_index > 0 )); then
      previous_transition_hash="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.$((transition_index - 1)).proof.transitionHash" 2>/dev/null || true)"
    fi
    recovery_bundle_hash="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryWitness.bundleHash" 2>/dev/null || true)"
    recovery_source_hash="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryWitness.sourceTransitionHash" 2>/dev/null || true)"
    recovery_txid="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.recoveryWitness.recoveryTxid" 2>/dev/null || true)"
    challenge_bundle_kind="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.challengeBundle.kind" 2>/dev/null || true)"
    challenge_bundle_hash="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.challengeWitness.bundleHash" 2>/dev/null || true)"
    challenge_txid="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.challengeWitness.transactionId" 2>/dev/null || true)"
    settlement_witness="$(printf '%s' "$state_json" | json_field "data.custodyTransitions.${transition_index}.proof.settlementWitness" 2>/dev/null || true)"
    if [[ "$kind" == "timeout" &&
      -n "$recovery_bundle_hash" && "$recovery_bundle_hash" != "null" &&
      "$recovery_source_hash" == "$source_transition_hash" &&
      -n "$recovery_txid" && "$recovery_txid" != "null" &&
      -z "$settlement_witness" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    if [[ "$kind" == "timeout" &&
      "$challenge_bundle_kind" == "timeout" &&
      -n "$challenge_bundle_hash" && "$challenge_bundle_hash" != "null" &&
      -n "$challenge_txid" && "$challenge_txid" != "null" &&
      -z "$settlement_witness" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for timeout completion from $source_transition_hash" >&2
  return 1
}

run_timeout_recovery_scenario() {
  local state_json=""
  local actor_state_json=""
  local source_transition_state=""
  local initial_seq=0
  local latest_seq=0
  local source_index=""
  local source_kind=""
  local bundle_index=""
  local source_transition_hash=""
  local defaulting_profile=""
  local forced_selection=""
  local forced_action=""
  local forced_amount=""
  local earliest_execute_at=""
  local final_table_json=""
  local recovery_wait_seconds=360
  local earliest_epoch=""
  local now_epoch=""

  echo "Forcing deterministic timeout recovery scenario..." >&2
  state_json="$(wait_for_actionable_table_state host)" || return 1

  defaulting_profile="$(acting_profile_for_state "$state_json")" || return 1
  actor_state_json="$(wait_for_actionable_table_state "$defaulting_profile")" || return 1
  initial_seq="$(printf '%s' "$actor_state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  [[ "$initial_seq" =~ ^[0-9]+$ ]] || initial_seq=0
  forced_selection="$(forced_aggression_action_for_state "$actor_state_json" "$defaulting_profile")" || return 1
  read -r forced_action forced_amount <<<"$forced_selection"
  [[ -n "$forced_action" ]] || return 1
  printf 'Creating contested pot via %s %s %s...\n' "$defaulting_profile" "$forced_action" "$forced_amount" >&2
  send_table_action_with_retry "$defaulting_profile" "$forced_action" "$forced_amount"

  source_transition_state="$(wait_for_latest_custody_seq_gt host "$initial_seq")" || return 1
  latest_seq="$(printf '%s' "$source_transition_state" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  source_index="$(latest_transition_index_from_state "$source_transition_state")" || return 1
  source_kind="$(printf '%s' "$source_transition_state" | json_field "data.custodyTransitions.${source_index}.kind" 2>/dev/null || true)"
  source_transition_hash="$(printf '%s' "$source_transition_state" | json_field "data.custodyTransitions.${source_index}.proof.transitionHash" 2>/dev/null || true)"
  bundle_index="$(find_recovery_bundle_index "$source_transition_state" "$source_index" timeout 2>/dev/null || true)"
  if [[ -n "$bundle_index" ]]; then
    earliest_execute_at="$(printf '%s' "$source_transition_state" | json_field "data.custodyTransitions.${source_index}.proof.recoveryBundles.${bundle_index}.earliestExecuteAt" 2>/dev/null || true)"
  elif [[ "$source_kind" == "turn-challenge-open" ]]; then
    earliest_execute_at="$(printf '%s' "$source_transition_state" | json_field "data.pendingTurnChallenge.timeoutEligibleAt" 2>/dev/null || true)"
    if [[ -z "$earliest_execute_at" || "$earliest_execute_at" == "null" ]]; then
      echo "expected timeout eligibility metadata on the latest turn challenge open transition" >&2
      return 1
    fi
  else
    echo "expected either a stored timeout recovery bundle or a turn-challenge-open source transition" >&2
    return 1
  fi

  defaulting_profile="$(acting_profile_for_state "$source_transition_state")" || return 1
  if [[ -z "$defaulting_profile" ]]; then
    echo "expected a defaulting player after the forcing action" >&2
    return 1
  fi

  echo "Stopping defaulting player daemon and Ark/indexer services before timeout finalization completes..." >&2
  printf 'Defaulting profile=%s sourceTransition=%s earliestExecuteAt=%s\n' "$defaulting_profile" "$source_transition_hash" "$earliest_execute_at" >&2
  stop_recovery_timeout_services "$defaulting_profile"

  if [[ -n "$earliest_execute_at" ]]; then
    earliest_epoch="$(iso_epoch_seconds "$earliest_execute_at" 2>/dev/null || true)"
    now_epoch="$(date -u +%s)"
    if [[ "$earliest_epoch" =~ ^[0-9]+$ ]] && [[ "$now_epoch" =~ ^[0-9]+$ ]]; then
      recovery_wait_seconds="$((earliest_epoch - now_epoch + 300))"
      if (( recovery_wait_seconds < 360 )); then
        recovery_wait_seconds=360
      fi
    fi
  fi

  echo "Waiting for timeout completion after U..." >&2
  if ! final_table_json="$(wait_for_timeout_recovery_transition host "$source_transition_hash" "$latest_seq" "$recovery_wait_seconds")"; then
    return 1
  fi
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$final_table_json" >"$BASE/artifacts/table-after-hand.json"

  echo "Timeout completion confirmed." >&2
  printf '%s\n' "$final_table_json"
}

wait_for_table_status() {
  local profile="$1"
  local desired_status="${2:-active}"
  local min_occupied="${3:-2}"
  local output=""
  local status=""
  local occupied=""
  local hand_number=""
  local phase=""
  local attempts=120
  local sleep_seconds=0.5
  local i

  if tor_enabled; then
    attempts=240
    sleep_seconds=1
  fi

  for ((i = 0; i < attempts; i += 1)); do
    if output="$(
      (
        PCLI_TIMEOUT_SECONDS=10
        pcli table watch "$TABLE_ID" --profile "$profile" --json
      ) 2>/dev/null
    )"; then
      status="$(printf '%s' "$output" | json_field data.config.status)"
      occupied="$(printf '%s' "$output" | json_field data.config.occupiedSeats)"
      hand_number="$(printf '%s' "$output" | json_field data.publicState.handNumber 2>/dev/null || true)"
      phase="$(printf '%s' "$output" | json_field data.publicState.phase 2>/dev/null || true)"
      if [[ "$status" == "$desired_status" && "${occupied:-0}" -ge "$min_occupied" ]]; then
        printf '%s\n' "$output"
        return 0
      fi
      if [[ "$desired_status" == "active" && "${occupied:-0}" -ge "$min_occupied" ]]; then
        if [[ "$hand_number" =~ ^[1-9][0-9]*$ ]]; then
          printf '%s\n' "$output"
          return 0
        fi
        if [[ -n "$phase" && "$phase" != "null" ]]; then
          printf '%s\n' "$output"
          return 0
        fi
      fi
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for table $TABLE_ID to reach status=$desired_status for $profile" >&2
  return 1
}

wait_for_round_profiles_status() {
  local desired_status="${1:-active}"
  local min_occupied="${2:-2}"
  local profile=""

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    wait_for_table_status "$profile" "$desired_status" "$min_occupied" >/dev/null || return 1
  done < <(round_watch_profiles)
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

print_timing_summary() {
  if ! timing_metrics_enabled; then
    return 0
  fi
  echo "Timing summary:"
  perl "$ROOT_DIR/scripts/summarize-mesh-timing.pl" "$BASE" || true
}

play_hand_automatically() {
  local require_hand_number_gt="${1:-}"
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
  local max_wait_seconds=300
  local settled_grace_seconds=15
  local settled_grace_start_epoch=0
  local start_epoch=0
  local now_epoch=0
  local turn
  local -a watch_profiles=()
  local selection_state_json=""

  if tor_enabled; then
    max_wait_seconds=360
  fi
  if [[ -n "$HAND_SETTLE_TIMEOUT_SECONDS" ]]; then
    max_wait_seconds="$HAND_SETTLE_TIMEOUT_SECONDS"
  fi

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    watch_profiles+=("$profile")
  done < <(round_watch_profiles)

  start_epoch="$(date +%s)"
  for ((turn = 0; ; turn += 1)); do
    now_epoch="$(date +%s)"
    if (( now_epoch - start_epoch >= max_wait_seconds )); then
      if (( settled_grace_start_epoch == 0 || now_epoch - settled_grace_start_epoch >= settled_grace_seconds )); then
        break
      fi
    fi
    state_json="$(freshest_table_state "${watch_profiles[@]}")"
    hand_id="$(printf '%s' "$state_json" | json_field data.publicState.handId)"
    hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber)"
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase)"
    latest_snapshot_phase="$(printf '%s' "$state_json" | json_field data.latestSnapshot.phase)"
    latest_snapshot_hand_number="$(printf '%s' "$state_json" | json_field data.latestSnapshot.handNumber)"
    if [[ "$phase" == "settled" || "$latest_snapshot_phase" == "settled" ]]; then
      if (( settled_grace_start_epoch == 0 )); then
        settled_grace_start_epoch="$now_epoch"
      fi
    else
      settled_grace_start_epoch=0
    fi
    if [[ -z "$hand_id" || -z "$phase" || "$phase" == "null" ]]; then
      sleep 0.25
      continue
    fi
    if [[ -n "$require_hand_number_gt" && "$require_hand_number_gt" != "null" && "$hand_number" =~ ^[0-9]+$ ]] && (( hand_number <= require_hand_number_gt )); then
      sleep 0.25
      continue
    fi
    if [[ -z "$initial_hand_number" && -n "$hand_number" && "$hand_number" != "null" ]]; then
      initial_hand_number="$hand_number"
    fi
    if table_has_settled_custody_checkpoint "$state_json"; then
      printf '%s\n' "$state_json"
      return 0
    fi
    if [[ "$phase" == "settled" ]]; then
      sleep 0.25
      continue
    fi
    if [[ -n "$initial_hand_number" && "$initial_hand_number" != "null" && "$latest_snapshot_phase" == "settled" && "$latest_snapshot_hand_number" == "$initial_hand_number" ]]; then
      sleep 0.25
      continue
    fi

    action_line="$(printf '%s' "$state_json" | select_table_action)"
    if [[ -z "$action_line" || "$action_line" == "settled" ]]; then
      sleep 0.25
      continue
    fi

    actor=""
    action=""
    amount=""
    read -r actor action amount <<<"$action_line"
    if ! phase_supports_direct_action "$phase"; then
      printf '{"actor":"%s","payload":{"type":"%s"%s},"phase":"%s","mode":"protocol-progress"}\n' \
        "$actor" \
        "$action" \
        "$(if [[ -n "$amount" ]]; then printf ',"totalSats":%s' "$amount"; fi)" \
        "$phase" >&2
      if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
        sleep 0.25
        continue
      fi
      wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
      continue
    fi
    selection_state_json="$(wait_for_actor_locally_actionable_state "$actor" "$state_json" 120 0.15 2>/dev/null || true)"
    if [[ -z "$selection_state_json" ]]; then
      sleep 0.25
      continue
    fi
    action_line="$(printf '%s' "$selection_state_json" | select_table_action)"
    if [[ -z "$action_line" || "$action_line" == "settled" ]]; then
      sleep 0.25
      continue
    fi
    actor=""
    action=""
    amount=""
    read -r actor action amount <<<"$action_line"
    phase="$(printf '%s' "$selection_state_json" | json_field data.publicState.phase)"
    current_bet="$(printf '%s' "$selection_state_json" | json_field data.publicState.currentBetSats)"
    pot_sats="$(printf '%s' "$selection_state_json" | json_field data.publicState.potSats)"
    printf '{"actor":"%s","currentBetSats":%s,"payload":{"type":"%s"%s},"phase":"%s","potSats":%s}\n' \
      "$actor" \
      "${current_bet:-0}" \
      "$action" \
      "$(if [[ -n "$amount" ]]; then printf ',"totalSats":%s' "$amount"; fi)" \
      "$phase" \
      "${pot_sats:-0}" >&2
    if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
      sleep 0.25
      continue
    fi
    wait_for_action_progress "$actor" "$selection_state_json" "${watch_profiles[@]}"
  done

  echo "hand did not settle in time" >&2
  return 1
}

play_hand_passively_until_phase() {
  local desired_phase="$1"
  local require_hand_number_gt="${2:-}"
  local state_json=""
  local hand_number=""
  local phase=""
  local action_line=""
  local actor=""
  local action=""
  local amount=""
  local -a watch_profiles=()
  local max_wait_seconds=300
  local start_epoch=0
  local now_epoch=0

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    watch_profiles+=("$profile")
  done < <(round_watch_profiles)

  start_epoch="$(date +%s)"
  while true; do
    now_epoch="$(date +%s)"
    if (( now_epoch - start_epoch >= max_wait_seconds )); then
      break
    fi
    if recovery_showdown_scenario_enabled; then
      state_json="$(
        (
          raw_table_watch_json "$WATCH_PROFILE"
        ) 2>/dev/null
      )"
      if [[ -z "$state_json" ]]; then
        log_recovery_showdown_step "Direct host watch returned no state while searching for the showdown recovery target"
        sleep 0.25
        continue
      fi
    else
      state_json="$(freshest_table_state "${watch_profiles[@]}")"
    fi
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    if [[ -n "$require_hand_number_gt" && "$hand_number" =~ ^[0-9]+$ ]] && (( hand_number <= require_hand_number_gt )); then
      sleep 0.25
      continue
    fi
    if [[ "$phase" == "$desired_phase" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    if table_has_settled_custody_checkpoint "$state_json"; then
      echo "hand settled before reaching $desired_phase" >&2
      return 1
    fi
    action_line="$(printf '%s' "$state_json" | select_table_action)"
    if [[ -z "$action_line" || "$action_line" == "settled" ]]; then
      sleep 0.25
      continue
    fi
    read -r actor action amount <<<"$action_line"
    if ! phase_supports_direct_action "$phase"; then
      if [[ -z "$action" ]]; then
        sleep 0.25
        continue
      fi
      printf '{"actor":"%s","payload":{"type":"%s"%s},"phase":"%s","mode":"passive-protocol-progress"}\n' \
        "$actor" \
        "$action" \
        "$(if [[ -n "$amount" ]]; then printf ',"totalSats":%s' "$amount"; fi)" \
        "$phase" >&2
      if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
        sleep 0.25
        continue
      fi
      wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
      continue
    fi
    action="$(passive_action_for_state "$state_json" 2>/dev/null || true)"
    if [[ -z "$action" ]]; then
      sleep 0.25
      continue
    fi
    amount=""
    printf '{"actor":"%s","payload":{"type":"%s"},"phase":"%s","mode":"passive-progress"}\n' \
      "$actor" \
      "$action" \
      "$phase" >&2
    if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
      sleep 0.25
      continue
    fi
    wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
  done

  echo "hand did not reach phase $desired_phase in time" >&2
  return 1
}

play_hand_passively_until_actionable_selection() {
  local desired_phase="$1"
  local desired_actor="$2"
  local desired_action="$3"
  local require_hand_number_gt="${4:-}"
  local state_json=""
  local hand_number=""
  local phase=""
  local actor=""
  local target_actor=""
  local action=""
  local amount=""
  local -a watch_profiles=()
  local max_wait_seconds=300
  local start_epoch=0
  local now_epoch=0

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    watch_profiles+=("$profile")
  done < <(round_watch_profiles)

  start_epoch="$(date +%s)"
  while true; do
    now_epoch="$(date +%s)"
    if (( now_epoch - start_epoch >= max_wait_seconds )); then
      break
    fi
    if recovery_showdown_scenario_enabled; then
      state_json="$(
        (
          raw_table_watch_json "$WATCH_PROFILE"
        ) 2>/dev/null
      )"
      if [[ -z "$state_json" ]]; then
        log_recovery_showdown_step "Direct host watch returned no state while searching for the actionable showdown recovery target"
        sleep 0.25
        continue
      fi
    else
      if ! state_json="$(freshest_table_state "${watch_profiles[@]}")"; then
        sleep 0.25
        continue
      fi
    fi
    phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
    hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    if [[ -n "$require_hand_number_gt" && "$hand_number" =~ ^[0-9]+$ ]] && (( hand_number <= require_hand_number_gt )); then
      sleep 0.25
      continue
    fi
    if [[ "$phase" == "settled" ]]; then
      echo "hand ended before reaching actionable $desired_phase/$desired_actor/$desired_action" >&2
      return 1
    fi
    if table_has_settled_custody_checkpoint "$state_json"; then
      echo "hand settled before reaching actionable $desired_phase/$desired_actor/$desired_action" >&2
      return 1
    fi
    if recovery_showdown_scenario_enabled; then
      local selection_line=""
      local selector_state_path="$BASE/recovery-selector-state.json"
      printf '%s' "$state_json" >"$selector_state_path"
      if ! selection_line="$(PLAYER_ONE_PROFILE="$PLAYER_ONE_PROFILE" \
        PLAYER_ONE_PLAYER_ID="$PLAYER_ONE_PLAYER_ID" \
        PLAYER_TWO_PROFILE="$PLAYER_TWO_PROFILE" \
        PLAYER_TWO_PLAYER_ID="$PLAYER_TWO_PLAYER_ID" \
        STATE_PATH="$selector_state_path" \
        python3 - <<'PY' 2>>"$BASE/recovery-showdown.log"
import json
import os

with open(os.environ["STATE_PATH"], "r", encoding="utf-8") as fh:
    raw = fh.read()
payload = json.loads(raw) if raw else {}
data = payload.get("data") or {}
public = data.get("publicState") or {}
menu = (data.get("pendingTurnMenu") or {}).get("options") or []

phase = public.get("phase") or ""
hand = public.get("handNumber")
acting_seat = public.get("actingSeatIndex")
dealer_seat = public.get("dealerSeatIndex")

seat_to_profile = {}
for seated in public.get("seatedPlayers") or []:
    pid = seated.get("playerId")
    seat = seated.get("seatIndex")
    if pid == os.environ.get("PLAYER_ONE_PLAYER_ID"):
        seat_to_profile[seat] = os.environ.get("PLAYER_ONE_PROFILE", "")
    elif pid == os.environ.get("PLAYER_TWO_PLAYER_ID"):
        seat_to_profile[seat] = os.environ.get("PLAYER_TWO_PROFILE", "")

actor = seat_to_profile.get(acting_seat, "")
dealer_actor = seat_to_profile.get(dealer_seat, "")

best = None
priority = {"check": 0, "call": 1, "bet": 2, "raise": 2, "fold": 3}
for option in menu:
    action = option.get("action") or {}
    action_type = str(action.get("type") or action.get("Type") or "").strip().lower()
    if action_type not in priority:
        continue
    total = action.get("totalSats")
    if total is None:
        total = action.get("TotalSats")
    if not isinstance(total, int):
        total = 0
    candidate = (priority[action_type], total, action_type)
    if best is None or candidate < best:
        best = candidate

action_type = ""
amount = ""
if best is not None:
    _, total, action_type = best
    if total:
        amount = str(total)

print(json.dumps({
    "phase": str(phase),
    "hand": "" if hand is None else str(hand),
    "actor": actor,
    "action": action_type,
    "amount": amount,
    "dealer_actor": dealer_actor,
}))
PY
      )"; then
        log_recovery_showdown_step "Passive selector derivation failed for the current host state"
        sleep 0.25
        continue
      fi
      if [[ -z "$selection_line" ]]; then
        log_recovery_showdown_step "Passive selector produced no output for the current host state"
        sleep 0.25
        continue
      fi
      local dealer_actor=""
      phase="$(printf '%s' "$selection_line" | jq -r '.phase // empty' 2>/dev/null || true)"
      hand_number="$(printf '%s' "$selection_line" | jq -r '.hand // empty' 2>/dev/null || true)"
      actor="$(printf '%s' "$selection_line" | jq -r '.actor // empty' 2>/dev/null || true)"
      action="$(printf '%s' "$selection_line" | jq -r '.action // empty' 2>/dev/null || true)"
      amount="$(printf '%s' "$selection_line" | jq -r '.amount // empty' 2>/dev/null || true)"
      dealer_actor="$(printf '%s' "$selection_line" | jq -r '.dealer_actor // empty' 2>/dev/null || true)"
      if [[ -z "$action" ]]; then
        local state_snippet=""
        state_snippet="$(printf '%s' "$state_json" | tr '\n' ' ' | cut -c1-180)"
        log_recovery_showdown_step "Passive selector has no action for phase ${phase:-unknown} on hand ${hand_number:-unknown}; selection='${selection_line:-empty}' state='${state_snippet}'"
        sleep 0.25
        continue
      fi
      if [[ -z "$actor" || -z "$action" ]]; then
        log_recovery_showdown_step "Passive selector returned incomplete action for phase ${phase:-unknown} on hand ${hand_number:-unknown}: ${selection_line:-empty}"
        sleep 0.25
        continue
      fi
      target_actor="$desired_actor"
      if [[ "$target_actor" == "dealer" ]]; then
        target_actor="$dealer_actor"
        if [[ -z "$target_actor" ]]; then
          log_recovery_showdown_step "Passive selector could not map dealer seat for phase ${phase:-unknown} on hand ${hand_number:-unknown}"
          sleep 0.25
          continue
        fi
      fi
      log_recovery_showdown_step "Passive selector candidate phase=${phase:-unknown} hand=${hand_number:-unknown} actor=$actor action=$action${amount:+ amount=$amount}"
      if [[ "$phase" == "$desired_phase" && "$actor" == "$target_actor" && "$action" == "$desired_action" ]]; then
        printf '%s\n' "$state_json"
        return 0
      fi
      if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
        log_recovery_showdown_step "Passive selector action $action via $actor did not complete; retrying"
        sleep 0.25
        continue
      fi
      wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
      continue
    fi
    if ! phase_supports_direct_action "$phase"; then
      local action_line=""
      action_line="$(printf '%s' "$state_json" | select_table_action)"
      if [[ -z "$action_line" || "$action_line" == "settled" ]]; then
        sleep 0.25
        continue
      fi
      read -r actor action amount <<<"$action_line"
      if [[ -z "$action" ]]; then
        sleep 0.25
        continue
      fi
      printf '{"actor":"%s","payload":{"type":"%s"%s},"phase":"%s","mode":"passive-protocol-progress"}\n' \
        "$actor" \
        "$action" \
        "$(if [[ -n "$amount" ]]; then printf ',"totalSats":%s' "$amount"; fi)" \
        "$phase" >&2
      if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
        sleep 0.25
        continue
      fi
      wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
      continue
    fi
    actor="$(acting_profile_for_state "$state_json" 2>/dev/null || true)"
    if [[ -z "$actor" ]]; then
      if recovery_showdown_scenario_enabled; then
        log_recovery_showdown_step "Passive direct step has no actor for phase ${phase:-unknown} on hand ${hand_number:-unknown}"
      fi
      sleep 0.25
      continue
    fi
    action="$(passive_action_for_state "$state_json" 2>/dev/null || true)"
    if [[ -z "$action" ]]; then
      if recovery_showdown_scenario_enabled; then
        log_recovery_showdown_step "Passive direct step has no action for actor $actor in phase ${phase:-unknown} on hand ${hand_number:-unknown}"
      fi
      sleep 0.25
      continue
    fi
    amount=""
    if recovery_showdown_scenario_enabled; then
      log_recovery_showdown_step "Passive direct candidate phase=${phase:-unknown} hand=${hand_number:-unknown} actor=$actor action=$action"
    fi
    target_actor="$desired_actor"
    if [[ "$target_actor" == "dealer" ]]; then
      target_actor="$(profile_for_seat_index "$state_json" "$(printf '%s' "$state_json" | json_field data.publicState.dealerSeatIndex 2>/dev/null || true)" 2>/dev/null || true)"
      if [[ -z "$target_actor" ]]; then
        sleep 0.25
        continue
      fi
    fi
    if [[ "$phase" == "$desired_phase" && "$actor" == "$target_actor" && "$action" == "$desired_action" ]]; then
      printf '%s\n' "$state_json"
      return 0
    fi
    if recovery_showdown_scenario_enabled; then
      log_recovery_showdown_step "Submitting passive direct action $action via $actor during ${phase:-unknown} on hand ${hand_number:-unknown}"
    fi
    if ! send_table_action_with_retry "$actor" "$action" "$amount"; then
      if recovery_showdown_scenario_enabled; then
        log_recovery_showdown_step "Passive direct action $action via $actor did not complete; retrying"
      fi
      sleep 0.25
      continue
    fi
    wait_for_action_progress "$actor" "$state_json" "${watch_profiles[@]}"
  done

  echo "hand did not reach actionable $desired_phase/$desired_actor/$desired_action in time" >&2
  return 1
}

resolve_turn_challenge_with_retry() {
  local profile="$1"
  local option_id="$2"
  local attempts="${3:-120}"
  local sleep_seconds="${4:-0.5}"
  local output=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli table challenge-resolve "$option_id" "$TABLE_ID" --profile "$profile" --json 2>&1)"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out resolving turn challenge option $option_id on table $TABLE_ID for $profile" >&2
  printf '%s\n' "$output" >&2
  return 1
}

open_turn_challenge_with_retry() {
  local profile="$1"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-0.5}"
  local output=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli table challenge-open "$TABLE_ID" --profile "$profile" --json 2>&1)"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out opening turn challenge on table $TABLE_ID for $profile" >&2
  printf '%s\n' "$output" >&2
  return 1
}

table_state_has_started_next_hand() {
  local state_json="$1"
  local previous_hand_number="${2:-}"
  local phase=""
  local hand_number=""

  [[ -n "$state_json" ]] || return 1
  phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"

  if [[ -n "$previous_hand_number" && "$previous_hand_number" =~ ^[0-9]+$ && "$hand_number" =~ ^[0-9]+$ ]]; then
    if (( hand_number > previous_hand_number )) && [[ -n "$phase" && "$phase" != "null" && "$phase" != "settled" ]]; then
      return 0
    fi
    return 1
  fi

  [[ -n "$phase" && "$phase" != "null" && "$phase" != "settled" ]]
}

wait_for_round_profiles_started_next_hand() {
  local previous_hand_number="${1:-}"
  local attempts="${2:-120}"
  local sleep_seconds="${3:-0.5}"
  local profile=""
  local state_json=""
  local hand_number=""
  local target_hand_number=""
  local host_state_json=""
  local all_started=0

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    all_started=1
    target_hand_number=""
    host_state_json=""

    while IFS= read -r profile; do
      [[ -n "$profile" ]] || continue
      state_json="$(watch_table_state_with_retry "$profile" 2>/dev/null || true)"
      if ! table_state_has_started_next_hand "$state_json" "$previous_hand_number"; then
        all_started=0
        break
      fi
      hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
      if [[ -z "$target_hand_number" ]]; then
        target_hand_number="$hand_number"
      elif [[ "$hand_number" != "$target_hand_number" ]]; then
        all_started=0
        break
      fi
      if [[ "$profile" == "host" ]]; then
        host_state_json="$state_json"
      fi
    done < <(round_watch_profiles)

    if (( all_started )) && [[ -n "$host_state_json" ]]; then
      printf '%s\n' "$host_state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for round profiles to converge on the next hand for table $TABLE_ID" >&2
  return 1
}

start_next_hand_with_retry() {
  local profile="${1:-host}"
  local previous_hand_number="${2:-}"
  local attempts="${3:-20}"
  local sleep_seconds="${4:-0.5}"
  local output=""
  local state_json=""
  local current_state_json=""
  local observed_profile=""
  local observed_state_json=""
  local converged_state_json=""

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if output="$(pcli table start-next-hand "$TABLE_ID" --profile "$profile" --json 2>&1)"; then
      state_json="$output"
      if table_state_has_started_next_hand "$state_json" "$previous_hand_number"; then
        if converged_state_json="$(wait_for_round_profiles_started_next_hand "$previous_hand_number" 120 "$sleep_seconds" 2>/dev/null)"; then
          printf '%s\n' "$converged_state_json"
          return 0
        fi
      fi
    fi

    current_state_json="$(watch_table_state_with_retry "$profile" 2>/dev/null || true)"
    if table_state_has_started_next_hand "$current_state_json" "$previous_hand_number"; then
      if converged_state_json="$(wait_for_round_profiles_started_next_hand "$previous_hand_number" 120 "$sleep_seconds" 2>/dev/null)"; then
        printf '%s\n' "$converged_state_json"
        return 0
      fi
    fi

    while IFS= read -r observed_profile; do
      [[ -n "$observed_profile" ]] || continue
      [[ "$observed_profile" == "$profile" ]] && continue
      observed_state_json="$(watch_table_state_with_retry "$observed_profile" 1 "$sleep_seconds" 2>/dev/null || true)"
      if table_state_has_started_next_hand "$observed_state_json" "$previous_hand_number"; then
        if converged_state_json="$(wait_for_round_profiles_started_next_hand "$previous_hand_number" 120 "$sleep_seconds" 2>/dev/null)"; then
          printf '%s\n' "$converged_state_json"
          return 0
        fi
      fi
    done < <(round_watch_profiles)

    if [[ "$output" == *"duplicated input"* || "$output" == *"not enough intent confirmations received"* ]]; then
      sleep "$sleep_seconds"
      continue
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out starting the next hand on table $TABLE_ID for $profile" >&2
  printf '%s\n' "$output" >&2
  return 1
}

table_state_is_settled_abort() {
  local state_json="$1"
  local phase=""

  phase="$(printf '%s' "$state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  [[ "$phase" == "settled" && "$state_json" == *"\"type\":\"HandAbort\""* ]]
}

wait_for_round_profiles_settled_abort_hand() {
  local attempts="${1:-120}"
  local sleep_seconds="${2:-0.5}"
  local profile=""
  local state_json=""
  local hand_number=""
  local target_hand_number=""
  local host_state_json=""
  local all_aborted=0

  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    all_aborted=1
    target_hand_number=""
    host_state_json=""

    while IFS= read -r profile; do
      [[ -n "$profile" ]] || continue
      state_json="$(watch_table_state_with_retry "$profile" 2>/dev/null || true)"
      if ! table_state_is_settled_abort "$state_json"; then
        all_aborted=0
        break
      fi
      hand_number="$(printf '%s' "$state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
      if [[ -z "$hand_number" || "$hand_number" == "null" ]]; then
        all_aborted=0
        break
      fi
      if [[ -z "$target_hand_number" ]]; then
        target_hand_number="$hand_number"
      elif [[ "$hand_number" != "$target_hand_number" ]]; then
        all_aborted=0
        break
      fi
      if [[ "$profile" == "host" ]]; then
        host_state_json="$state_json"
      fi
    done < <(round_watch_profiles)

    if (( all_aborted )) && [[ -n "$host_state_json" ]]; then
      printf '%s\n' "$host_state_json"
      return 0
    fi
    sleep "$sleep_seconds"
  done

  echo "timed out waiting for round profiles to converge on a settled hand abort for table $TABLE_ID" >&2
  return 1
}

play_hand_until_result() {
  local require_hand_number_gt="${1:-}"
  local hand_attempts="${2:-4}"
  local table_json=""
  local hand_number=""
  local profile=""
  local -a watch_profiles=()

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    watch_profiles+=("$profile")
  done < <(round_watch_profiles)

  for ((hand_attempt = 1; hand_attempt <= hand_attempts; hand_attempt += 1)); do
    if table_json="$(play_hand_automatically "$require_hand_number_gt" 2>/dev/null)"; then
      if [[ "$table_json" == *"\"type\":\"HandResult\""* ]]; then
        printf '%s\n' "$table_json"
        return 0
      fi
      if table_state_is_settled_abort "$table_json"; then
        hand_number="$(printf '%s' "$table_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
        start_next_hand_with_retry host "$hand_number" 40 0.25 >/dev/null || return 1
        continue
      fi
    fi

    table_json="$(freshest_table_state "${watch_profiles[@]}")"
    if table_state_is_settled_abort "$table_json"; then
      hand_number="$(printf '%s' "$table_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
      start_next_hand_with_retry host "$hand_number" 40 0.25 >/dev/null || return 1
      continue
    fi
  done

  echo "timed out producing a settled hand result on table $TABLE_ID" >&2
  return 1
}

play_automatic_hands_until_net_transfer() {
  local require_hand_number_gt="${1:-}"
  local max_settled_hands="${2:-5}"
  local final_table_json=""
  local last_settled_hand_number="$require_hand_number_gt"

  for ((settled_hand = 1; settled_hand <= max_settled_hands; settled_hand += 1)); do
    final_table_json="$(play_hand_automatically "$last_settled_hand_number")" || return 1
    last_settled_hand_number="$(json_field data.publicState.handNumber <<<"$final_table_json")"
    if host_player_scenario_enabled || hand_has_net_transfer "$final_table_json"; then
      printf '%s\n' "$final_table_json"
      return 0
    fi
    printf 'Hand %s settled without net chip transfer; continuing to the next hand...\n' "${last_settled_hand_number:-unknown}" >&2
  done

  echo "round did not produce a net chip transfer within $max_settled_hands settled hands" >&2
  return 1
}

settle_hand_with_forced_fold() {
  local state_json=""
  local selection_state_json=""
  local actor=""
  local phase=""
  local current_bet=""
  local pot_sats=""
  local profile=""
  local -a watch_profiles=()

  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    watch_profiles+=("$profile")
  done < <(round_watch_profiles)

  state_json="$(wait_for_actionable_table_state host)" || return 1
  actor="$(acting_profile_for_state "$state_json")" || return 1
  selection_state_json="$(wait_for_actor_locally_actionable_state "$actor" "$state_json" 120 0.15)" || return 1
  phase="$(printf '%s' "$selection_state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  current_bet="$(printf '%s' "$selection_state_json" | json_field data.publicState.currentBetSats 2>/dev/null || true)"
  pot_sats="$(printf '%s' "$selection_state_json" | json_field data.publicState.potSats 2>/dev/null || true)"
  printf '{"actor":"%s","currentBetSats":%s,"payload":{"type":"fold"},"phase":"%s","potSats":%s}\n' \
    "$actor" \
    "${current_bet:-0}" \
    "$phase" \
    "${pot_sats:-0}" >&2
  send_table_action_with_retry "$actor" fold

  for ((attempt = 0; attempt < 240; attempt += 1)); do
    state_json="$(freshest_table_state "${watch_profiles[@]}")"
    if table_has_settled_custody_checkpoint "$state_json"; then
      printf '%s\n' "$state_json"
      return 0
    fi
    sleep 0.25
  done

  echo "timed out waiting for forced-fold hand settlement" >&2
  return 1
}

run_aborted_hand_scenario() {
  local aborted_table_json=""
  local final_table_json=""

  echo "Forcing aborted-hand scenario..." >&2
  echo "Forcing a no-blame hand abort via the host so the table can continue deterministically..." >&2
  aborted_table_json="$(abort_hand_with_retry 240 0.5)" || return 1
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$aborted_table_json" >"$BASE/artifacts/table-after-abort.json"

  echo "Starting a fresh post-abort hand..." >&2
  start_next_hand_with_retry host "" 120 0.5 >/dev/null
  echo "Playing a fresh post-abort hand to verify the table continues..." >&2
  final_table_json="$(play_hand_automatically)" || return 1
  printf '%s\n' "$final_table_json"
}

run_all_in_scenario() {
  local state_json=""
  local actor=""
  local actor_state_json=""
  local all_in_selection=""
  local all_in_action=""
  local all_in_amount=""
  local caller_state_json=""
  local caller=""
  local call_action=""
  local final_table_json=""

  echo "Forcing explicit all-in coverage..." >&2
  state_json="$(wait_for_actionable_table_state host)" || return 1
  actor="$(acting_profile_for_state "$state_json")" || return 1
  actor_state_json="$(wait_for_actionable_table_state "$actor")" || return 1
  all_in_selection="$(all_in_action_for_state "$actor_state_json")" || return 1
  read -r all_in_action all_in_amount <<<"$all_in_selection"
  printf 'Sending all-in line via %s %s %s...\n' "$actor" "$all_in_action" "$all_in_amount" >&2
  send_table_action_with_retry "$actor" "$all_in_action" "$all_in_amount"

  state_json="$(wait_for_actionable_table_state host)" || return 1
  caller="$(acting_profile_for_state "$state_json")" || return 1
  caller_state_json="$(wait_for_actionable_table_state "$caller")" || return 1
  call_action="$(passive_action_for_state "$caller_state_json")" || return 1
  printf 'Completing all-in with %s %s...\n' "$caller" "$call_action" >&2
  send_table_action_with_retry "$caller" "$call_action"

  write_table_artifact host "$BASE/artifacts/table-after-all-in.json"
  final_table_json="$(play_hand_automatically)" || return 1
  printf '%s\n' "$final_table_json"
}

run_turn_challenge_option_scenario() {
  local state_json=""
  local challenger=""
  local open_table_json=""
  local option_id=""
  local resolved_table_json=""
  local final_table_json=""

  echo "Forcing on-chain turn challenge option resolution..." >&2
  prepare_onchain_fee_reserve "$PLAYER_ONE_PROFILE" || return 1
  prepare_onchain_fee_reserve "$PLAYER_TWO_PROFILE" || return 1
  prepare_challenge_ready_turn 3 || return 1
  state_json="$CHALLENGE_READY_STATE_JSON"
  challenger="$CHALLENGE_READY_PROFILE"
  open_table_json="$(open_turn_challenge_with_retry "$challenger" 360 0.5)" || return 1
  if [[ "$(printf '%s' "$open_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "open" ]]; then
    open_table_json="$(wait_for_pending_turn_challenge_any 240 0.5)" || return 1
  fi
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$open_table_json" >"$BASE/artifacts/table-after-challenge-open.json"

  option_id="$(challenge_option_id_for_state "$open_table_json")" || return 1
  printf 'Resolving turn challenge with option %s via %s...\n' "$option_id" "$challenger" >&2
  resolved_table_json="$(resolve_turn_challenge_with_retry "$challenger" "$option_id" 120 0.5)" || return 1
  if [[ "$(printf '%s' "$resolved_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "" &&
        "$(printf '%s' "$resolved_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "null" ]]; then
    resolved_table_json="$(wait_for_turn_challenge_cleared "$challenger" 240 0.5)" || return 1
  fi
  printf '%s\n' "$resolved_table_json" >"$BASE/artifacts/table-after-challenge-resolution.json"
  printf '%s\n' "$resolved_table_json"
}

run_turn_challenge_timeout_cashout_scenario() {
  local state_json=""
  local challenger=""
  local open_table_json=""
  local resolved_table_json=""
  local final_table_json=""

  echo "Forcing challenged timeout resolution before cash-out..." >&2
  prepare_onchain_fee_reserve "$PLAYER_ONE_PROFILE" || return 1
  prepare_onchain_fee_reserve "$PLAYER_TWO_PROFILE" || return 1
  prepare_challenge_ready_turn 3 || return 1
  state_json="$CHALLENGE_READY_STATE_JSON"
  challenger="$CHALLENGE_READY_PROFILE"
  open_table_json="$(open_turn_challenge_with_retry "$challenger" 360 0.5)" || return 1
  if [[ "$(printf '%s' "$open_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "open" ]]; then
    open_table_json="$(wait_for_pending_turn_challenge_any 240 0.5)" || return 1
  fi
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$open_table_json" >"$BASE/artifacts/table-after-challenge-open.json"

  echo "Waiting for turn challenge timeout eligibility..." >&2
  resolved_table_json="$(resolve_turn_challenge_with_retry "$challenger" timeout 240 0.5)" || return 1
  if [[ "$(printf '%s' "$resolved_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "" &&
        "$(printf '%s' "$resolved_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "null" ]]; then
    resolved_table_json="$(wait_for_turn_challenge_cleared "$challenger" 240 0.5)" || return 1
  fi
  printf '%s\n' "$resolved_table_json" >"$BASE/artifacts/table-after-challenge-resolution.json"

  final_table_json="$(play_hand_automatically)" || return 1
  printf '%s\n' "$final_table_json"
}

run_turn_challenge_escape_scenario() {
  local state_json=""
  local challenger=""
  local open_table_json=""
  local ready_table_json=""
  local final_table_json=""
  local open_confirmed=""
  local eligible_height=""
  local chain_tip_height=""
  local escape_eligible_at=""
  local eligible_epoch=""
  local now_epoch=""
  local wait_seconds=""
  local remaining_blocks=""
  local batch_size=""
  local batch_index=""

  echo "Forcing turn challenge escape after CSV maturity..." >&2
  prepare_onchain_fee_reserve "$PLAYER_ONE_PROFILE" || return 1
  prepare_onchain_fee_reserve "$PLAYER_TWO_PROFILE" || return 1
  prepare_challenge_ready_turn 3 || return 1
  state_json="$CHALLENGE_READY_STATE_JSON"
  challenger="$CHALLENGE_READY_PROFILE"
  open_table_json="$(open_turn_challenge_with_retry "$challenger" 360 0.5)" || return 1
  if [[ "$(printf '%s' "$open_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "open" ]]; then
    open_table_json="$(wait_for_pending_turn_challenge_any 240 0.5)" || return 1
  fi
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$open_table_json" >"$BASE/artifacts/table-after-challenge-open.json"

  echo "Mining regtest blocks until challenge escape is eligible..." >&2
  for ((mined = 0; mined < 640; )); do
    if ready_table_json="$(wait_for_turn_challenge_escape_ready "$challenger" 4 0.5 2>/dev/null)"; then
      break
    fi
    state_json="$(watch_table_state_with_retry "$challenger")"
    escape_eligible_at="$(printf '%s' "$state_json" | json_field data.pendingTurnChallenge.escapeEligibleAt 2>/dev/null || true)"
    open_confirmed="$(printf '%s' "$state_json" | json_field data.local.turnChallengeChain.openConfirmed 2>/dev/null || true)"
    eligible_height="$(printf '%s' "$state_json" | json_field data.local.turnChallengeChain.escapeEligibleHeight 2>/dev/null || true)"
    chain_tip_height="$(printf '%s' "$state_json" | json_field data.local.turnChallengeChain.chainTipHeight 2>/dev/null || true)"
    if [[ -n "$escape_eligible_at" && "$escape_eligible_at" != "null" ]]; then
      eligible_epoch="$(iso_epoch_seconds "$escape_eligible_at" 2>/dev/null || true)"
      now_epoch="$(date -u +%s)"
      wait_seconds=1
      if [[ "$eligible_epoch" =~ ^[0-9]+$ && "$now_epoch" =~ ^[0-9]+$ ]]; then
        wait_seconds=$((eligible_epoch - now_epoch))
        if ((wait_seconds < 1)); then
          wait_seconds=1
        elif ((wait_seconds > 5)); then
          wait_seconds=5
        fi
      fi
      sleep "$wait_seconds"
      mined=$((mined + 1))
      continue
    fi
    batch_size=1
    if [[ "$open_confirmed" == "true" && "$eligible_height" =~ ^[0-9]+$ && "$chain_tip_height" =~ ^[0-9]+$ ]]; then
      remaining_blocks=$((eligible_height - chain_tip_height))
      if ((remaining_blocks > 1)); then
        batch_size=$remaining_blocks
        if ((batch_size > 32)); then
          batch_size=32
        fi
      fi
    fi
    for ((batch_index = 0; batch_index < batch_size; batch_index += 1)); do
      mine_regtest_block host || return 1
    done
    mined=$((mined + batch_size))
  done
  if [[ -z "$ready_table_json" ]]; then
    ready_table_json="$(wait_for_turn_challenge_escape_ready "$challenger" 240 0.5)" || return 1
  fi
  final_table_json="$(resolve_turn_challenge_with_retry "$challenger" escape 60 0.5)" || return 1
  if [[ "$(printf '%s' "$final_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "" &&
        "$(printf '%s' "$final_table_json" | json_field data.pendingTurnChallenge.status 2>/dev/null || true)" != "null" ]]; then
    final_table_json="$(wait_for_turn_challenge_cleared "$challenger" 240 0.5)" || return 1
  fi
  printf '%s\n' "$final_table_json" >"$BASE/artifacts/table-after-challenge-resolution.json"
  printf '%s\n' "$final_table_json"
}

run_multi_hand_scenario() {
  local first_hand_json=""
  local second_hand_json=""
  local first_hand_number=""

  echo "Playing multiple hands without cashing out..." >&2
  first_hand_json="$(play_hand_until_result "" 4)" || return 1
  first_hand_number="$(printf '%s' "$first_hand_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$first_hand_json" >"$BASE/artifacts/table-after-hand-1.json"

  start_next_hand_with_retry host "$first_hand_number" 40 0.25 >/dev/null || return 1
  second_hand_json="$(play_hand_until_result "$first_hand_number" 4)" || return 1
  printf '%s\n' "$second_hand_json"
}

run_recovery_showdown_scenario() {
  local final_river_actor=""
  local final_river_selection_json=""
  local retry_state_json=""
  local showdown_state_json=""
  local final_table_json=""
  local hand_number=""
  local completed_hand_number=""
  local candidate_hand_number=""
  local checkpoint_phase=""
  local source_transition_hash=""
  local source_transition_seq=""
  local pre_action_seq=0
  local source_index=""
  local bundle_index=""
  local earliest_execute_at=""
  local earliest_epoch=""
  local now_epoch=""
  local recovery_wait_seconds=900
  local recovery_wait_attempts=1800
  local final_action_log_path=""
  local final_action_pid=""
  local source_transition_state=""
  local attempt=0

  : >"$BASE/recovery-showdown.log"
  log_recovery_showdown_step "Driving the hand to showdown before forcing payout recovery..."
  for ((attempt = 0; attempt < 5; attempt += 1)); do
    log_recovery_showdown_step "Attempt $((attempt + 1)): searching for final river dealer check after hand ${completed_hand_number:-0}"
    final_river_selection_json="$(play_hand_passively_until_actionable_selection river dealer check "$completed_hand_number" 2>/dev/null || true)"
    if [[ -n "$final_river_selection_json" ]]; then
      candidate_hand_number="$(printf '%s' "$final_river_selection_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
      log_recovery_showdown_step "Found target final river dealer check on hand ${candidate_hand_number:-unknown}"
      final_river_actor="$(acting_profile_for_state "$final_river_selection_json" 2>/dev/null || true)"
      [[ -n "$final_river_actor" ]] || return 1
      if [[ "$final_river_actor" != "$PLAYER_ONE_PROFILE" ]]; then
        completed_hand_number="$candidate_hand_number"
        log_recovery_showdown_step "Hand ${completed_hand_number:-unknown} ends river with $final_river_actor acting last; retrying for a hand where $PLAYER_ONE_PROFILE can send the final river check after Bob is frozen"
        start_next_hand_with_retry host "$completed_hand_number" 120 0.25 >/dev/null || true
        final_river_selection_json=""
        continue
      fi
      pre_action_seq="$(printf '%s' "$final_river_selection_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
      if [[ ! "$pre_action_seq" =~ ^[0-9]+$ ]]; then
        pre_action_seq=0
      fi
      log_recovery_showdown_step "Sending final river check with actor $final_river_actor"
      final_action_log_path="$BASE/recovery-final-river-action.log"
      : >"$final_action_log_path"
      (
        send_table_action_with_retry "$final_river_actor" check
      ) >"$final_action_log_path" 2>&1 &
      final_action_pid=$!
      sleep 3
      log_recovery_showdown_step "Final river check is in flight; hard-stopping $PLAYER_TWO_PROFILE after the initial signing round"
      kill_profile_daemon_immediately "$PLAYER_TWO_PROFILE"
      if ! source_transition_state="$(wait_for_latest_custody_seq_gt host "$pre_action_seq" 80 0.25 2>/dev/null)"; then
        wait "$final_action_pid" 2>/dev/null || true
        final_action_pid=""
        cat "$final_action_log_path" >&2 || true
        return 1
      fi
      wait "$final_action_pid" 2>/dev/null || true
      final_action_pid=""
      log_recovery_showdown_step "Final river check accepted; waiting for the strict showdown-reveal checkpoint before killing Ark"
      hand_number="$candidate_hand_number"
      break
    fi
    retry_state_json="$(watch_table_state_with_retry host 2>/dev/null || true)"
    completed_hand_number="$(printf '%s' "$retry_state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
    if [[ ! "$completed_hand_number" =~ ^[0-9]+$ ]]; then
      echo "recovery showdown scenario could not determine the completed hand number after a failed passive attempt" >&2
      return 1
    fi
    log_recovery_showdown_step "Target not found; advancing to next hand after hand $completed_hand_number"
    start_next_hand_with_retry host "$completed_hand_number" 120 0.25 >/dev/null || return 1
  done
  showdown_state_json="$(wait_for_showdown_recovery_checkpoint host "$hand_number" 120 0.1 2>/dev/null || wait_for_showdown_recovery_checkpoint "$PLAYER_ONE_PROFILE" "$hand_number" 120 0.1)" || return 1
  hand_number="$(printf '%s' "$showdown_state_json" | json_field data.publicState.handNumber 2>/dev/null || true)"
  checkpoint_phase="$(printf '%s' "$showdown_state_json" | json_field data.publicState.phase 2>/dev/null || true)"
  source_transition_hash="$(printf '%s' "$showdown_state_json" | jq -r '.data.custodyTransitions[-1].proof.transitionHash // empty' 2>/dev/null || true)"
  source_transition_seq="$(printf '%s' "$showdown_state_json" | json_field data.latestCustodyState.custodySeq 2>/dev/null || true)"
  source_index="$(latest_transition_index_from_state "$showdown_state_json" 2>/dev/null || true)"
  if [[ -n "$source_index" ]]; then
    bundle_index="$(find_recovery_bundle_index "$showdown_state_json" "$source_index" showdown-payout 2>/dev/null || true)"
    if [[ -n "$bundle_index" ]]; then
      earliest_execute_at="$(printf '%s' "$showdown_state_json" | json_field "data.custodyTransitions.${source_index}.proof.recoveryBundles.${bundle_index}.earliestExecuteAt" 2>/dev/null || true)"
      earliest_epoch="$(iso_epoch_seconds "$earliest_execute_at" 2>/dev/null || true)"
      now_epoch="$(date -u +%s)"
      if [[ "$earliest_epoch" =~ ^[0-9]+$ && "$now_epoch" =~ ^[0-9]+$ ]]; then
        recovery_wait_seconds="$((earliest_epoch - now_epoch + 300))"
        if (( recovery_wait_seconds < 600 )); then
          recovery_wait_seconds=600
        fi
        recovery_wait_attempts="$((recovery_wait_seconds * 2))"
      fi
    fi
  fi
  log_recovery_showdown_step "Reached ${checkpoint_phase:-showdown-reveal} at hand $hand_number; killing Ark immediately before cooperative showdown payout can settle"
  stop_recovery_showdown_services ""
  log_recovery_showdown_step "Waiting for recovery-backed showdown payout from source seq=$source_transition_seq earliestExecuteAt=${earliest_execute_at:-unknown} timeout=${recovery_wait_seconds}s"
  final_table_json="$(wait_for_recovered_showdown_transition host "$source_transition_hash" "$source_transition_seq" "$recovery_wait_attempts" 0.5)" || return 1
  log_recovery_showdown_step "Observed recovery-backed showdown payout transition"
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$showdown_state_json" >"$BASE/artifacts/table-before-recovery-showdown.json"
  printf '%s\n' "$final_table_json"
}

run_emergency_exit_scenario() {
  local settled_table_json=""
  local final_table_json=""
  local exit_result_json=""
  local exit_status=""
  local exit_phase=""

  echo "Playing a live hand before emergency exit..." >&2
  settled_table_json="$(settle_hand_with_forced_fold)" || return 1
  mkdir -p "$BASE/artifacts"
  printf '%s\n' "$settled_table_json" >"$BASE/artifacts/table-before-emergency-exit.json"

  echo "Executing emergency exit..." >&2
  prepare_onchain_fee_reserve "$PLAYER_ONE_PROFILE" || return 1
  exit_result_json="$(pcli funds exit "$TABLE_ID" --profile "$PLAYER_ONE_PROFILE" --json 2>&1 || true)"
  final_table_json="$(wait_for_event_type_any EmergencyExit 20 0.5 2>/dev/null || true)"
  printf '%s\n' "$exit_result_json" >"$BASE/artifacts/emergency-exit-result.json"
  if [[ -z "$final_table_json" ]]; then
    exit_status="$(printf '%s' "$exit_result_json" | json_field data.status 2>/dev/null || true)"
    if [[ "$exit_status" != "pending-exit" && "$exit_status" != "exited" ]]; then
      echo "timed out waiting for emergency exit acceptance on table $TABLE_ID for $PLAYER_ONE_PROFILE" >&2
      printf '%s\n' "$exit_result_json" >&2
      return 1
    fi
    final_table_json="$(watch_table_state_with_retry host)" || return 1
  fi
  exit_phase="$(printf '%s' "$final_table_json" | json_field data.publicState.phase 2>/dev/null || true)"
  if [[ -n "$exit_phase" && "$exit_phase" != "null" && "$exit_phase" != "settled" ]]; then
    echo "Aborting the live hand after emergency exit so the pending exit can be accepted..." >&2
    abort_hand_with_retry 40 0.5 >/dev/null || return 1
    final_table_json="$(watch_table_state_with_retry host)" || return 1
  fi
  if [[ "$final_table_json" != *'"type":"EmergencyExit"'* ]]; then
    echo "Replaying the persisted emergency-exit request after the table is ready..." >&2
    final_table_json="$(trigger_emergency_exit_with_retry "$PLAYER_ONE_PROFILE" 30 1)" || return 1
  fi
  printf '%s\n' "$final_table_json" >"$BASE/artifacts/table-after-emergency-exit.json"
  wait_for_table_status "$PLAYER_TWO_PROFILE" seating 1 >/dev/null ||
    wait_for_table_status "$PLAYER_TWO_PROFILE" ready 1 >/dev/null ||
    return 1
  printf '%s\n' "$final_table_json"
}

hand_has_net_transfer() {
  local state_json="$1"
  local player_one_balance=""
  local player_two_balance=""

  player_one_balance="$(json_field "data.publicState.chipBalances.${PLAYER_ONE_PLAYER_ID}" <<<"$state_json" 2>/dev/null || true)"
  player_two_balance="$(json_field "data.publicState.chipBalances.${PLAYER_TWO_PLAYER_ID}" <<<"$state_json" 2>/dev/null || true)"
  if [[ -z "$player_one_balance" || "$player_one_balance" == "null" || -z "$player_two_balance" || "$player_two_balance" == "null" ]]; then
    return 1
  fi
  if [[ "$player_one_balance" != "$BUY_IN_SATS" || "$player_two_balance" != "$BUY_IN_SATS" ]]; then
    return 0
  fi
  return 1
}

write_table_artifact() {
  local profile="$1"
  local path="$2"
  local -a watch_profiles=("$profile")
  mkdir -p "$(dirname "$path")"
  if watch_table_state_with_retry "$profile" >"$path"; then
    return 0
  fi
  if [[ "$profile" == "$WATCH_PROFILE" ]]; then
    while IFS= read -r candidate; do
      [[ -n "$candidate" ]] || continue
      if [[ ! " ${watch_profiles[*]} " =~ " ${candidate} " ]]; then
        watch_profiles+=("$candidate")
      fi
    done < <(round_watch_profiles)
  fi
  freshest_table_state "${watch_profiles[@]}" >"$path"
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
mkdir -p "$BASE/daemons" "$BASE/profiles" "$BASE/runs" "$BASE/tor"
mkdir -p "$TOR_STATE_BASE"

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
WATCH_PROFILE="host"

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
if recovery_timeout_scenario_enabled; then
  seed_recovery_fee_reserves
fi

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
if chain_challenge_scenario_enabled; then
  create_table_args+=(--turn-timeout-mode chain-challenge --action-timeout-ms 4000)
  if challenge_escape_scenario_enabled; then
    create_table_args+=(--turn-challenge-window-ms 600000)
  else
    create_table_args+=(--turn-challenge-window-ms 4000)
  fi
else
  create_table_args+=(--turn-timeout-mode direct)
fi
if recovery_showdown_scenario_enabled; then
  create_table_args+=(--action-timeout-ms 30000)
  create_table_args+=(--hand-protocol-timeout-ms 30000)
  create_table_args+=(--next-hand-delay-ms 120000)
fi
if (multi_hand_scenario_enabled || ! scenario_skips_cashout) && ! recovery_showdown_scenario_enabled; then
  create_table_args+=(--next-hand-delay-ms 15000)
fi
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
if aborted_hand_scenario_enabled; then
  echo "Waiting for round participants to observe the ready table..."
  wait_for_round_profiles_status ready 2
  echo "Starting the initial hand..."
  start_next_hand_with_retry host "" 120 0.5 >/dev/null
elif multi_hand_scenario_enabled || ! scenario_skips_cashout || recovery_showdown_scenario_enabled; then
  echo "Waiting for round participants to observe the ready table..."
  wait_for_round_profiles_status ready 2
  echo "Starting the initial hand..."
  start_next_hand_with_retry host "" 120 0.5 >/dev/null
  echo "Waiting for round participants to observe the active table..."
  wait_for_round_profiles_status active 2
else
  echo "Waiting for players to observe the active table..."
  wait_for_table_status "$PLAYER_ONE_PROFILE" active 2 >/dev/null
  wait_for_table_status "$PLAYER_TWO_PROFILE" active 2 >/dev/null
fi
write_table_artifact host "$BASE/artifacts/table-active.json"

write_runtime_env

if setup_only_enabled; then
  trap - EXIT INT TERM HUP
  print_local_stack_summary
  exit 0
fi

case "$ROUND_SCENARIO" in
  "$ROUND_SCENARIO_TIMEOUT_RECOVERY")
    FINAL_TABLE_JSON="$(run_timeout_recovery_scenario)"
    ;;
  "$ROUND_SCENARIO_ABORTED_HAND")
    FINAL_TABLE_JSON="$(run_aborted_hand_scenario)"
    ;;
  "$ROUND_SCENARIO_ALL_IN")
    FINAL_TABLE_JSON="$(run_all_in_scenario)"
    ;;
  "$ROUND_SCENARIO_TURN_CHALLENGE")
    FINAL_TABLE_JSON="$(run_turn_challenge_option_scenario)"
    ;;
  "$ROUND_SCENARIO_EMERGENCY_EXIT")
    FINAL_TABLE_JSON="$(run_emergency_exit_scenario)"
    ;;
  "$ROUND_SCENARIO_MULTI_HAND")
    FINAL_TABLE_JSON="$(run_multi_hand_scenario)"
    ;;
  "$ROUND_SCENARIO_CHALLENGE_ESCAPE")
    FINAL_TABLE_JSON="$(run_turn_challenge_escape_scenario)"
    ;;
  "$ROUND_SCENARIO_RECOVERY_SHOWDOWN")
    FINAL_TABLE_JSON="$(run_recovery_showdown_scenario)"
    ;;
  "$ROUND_SCENARIO_CASHOUT_AFTER_CHALLENGE")
    FINAL_TABLE_JSON="$(run_turn_challenge_timeout_cashout_scenario)"
    ;;
  *)
    echo "Playing automatic hands until funds move..."
    MAX_SETTLED_HANDS=5
    if host_player_scenario_enabled; then
      MAX_SETTLED_HANDS=1
    fi
    FINAL_TABLE_JSON="$(play_automatic_hands_until_net_transfer "" "$MAX_SETTLED_HANDS")" || exit 1
    ;;
esac

if [[ -z "$FINAL_TABLE_JSON" ]]; then
  echo "scenario $ROUND_SCENARIO did not produce a final table artifact" >&2
  exit 1
fi
mkdir -p "$BASE/artifacts"
printf '%s\n' "$FINAL_TABLE_JSON" >"$BASE/artifacts/table-after-hand.json"

echo "Final table state:"
printf '%s\n' "$FINAL_TABLE_JSON"

if scenario_skips_cashout; then
  echo "Skipping cash out because $(scenario_cashout_skip_reason)"
  print_timing_summary
  echo "Done. Logs are under $BASE"
  exit 0
fi

if emergency_exit_scenario_enabled; then
  echo "Cashing out the non-exiting player after emergency exit..."
  cashout_profile "$PLAYER_TWO_PROFILE"
  write_table_artifact host "$BASE/artifacts/table-after-cashout.json"
  echo "Final wallet summaries:"
  pcli wallet --profile "$PLAYER_ONE_PROFILE" --json
  pcli wallet --profile "$PLAYER_TWO_PROFILE" --json
  print_timing_summary
  echo "Done. Logs are under $BASE"
  exit 0
fi

CASHOUT_FIRST_PROFILE="$PLAYER_ONE_PROFILE"
CASHOUT_SECOND_PROFILE="$PLAYER_TWO_PROFILE"
CURRENT_HOST_PEER_ID="$(printf '%s' "$FINAL_TABLE_JSON" | json_field data.currentHost.peer.peerId)"
if [[ -z "$CURRENT_HOST_PEER_ID" || "$CURRENT_HOST_PEER_ID" == "null" ]]; then
  CURRENT_HOST_PEER_ID="$(printf '%s' "$FINAL_TABLE_JSON" | json_field data.config.hostPeerId)"
fi
if [[ -n "$CURRENT_HOST_PEER_ID" ]]; then
  if [[ "$CURRENT_HOST_PEER_ID" == "${PLAYER_TWO_PEER_ID:-}" ]]; then
    CASHOUT_FIRST_PROFILE="$PLAYER_TWO_PROFILE"
    CASHOUT_SECOND_PROFILE="$PLAYER_ONE_PROFILE"
  elif [[ "$CURRENT_HOST_PEER_ID" == "${PLAYER_ONE_PEER_ID:-}" ]]; then
    CASHOUT_FIRST_PROFILE="$PLAYER_ONE_PROFILE"
    CASHOUT_SECOND_PROFILE="$PLAYER_TWO_PROFILE"
  fi
fi
if challenge_escape_scenario_enabled || cashout_after_challenge_scenario_enabled; then
  CHALLENGE_SURVIVOR_PROFILE="$(challenge_survivor_cashout_profile 2>/dev/null || true)"
  if [[ -n "$CHALLENGE_SURVIVOR_PROFILE" ]]; then
    CASHOUT_FIRST_PROFILE="$CHALLENGE_SURVIVOR_PROFILE"
    CASHOUT_SECOND_PROFILE="$(other_round_player_profile "$CHALLENGE_SURVIVOR_PROFILE")"
  fi
fi

echo "Cashing out..."
if ! cashout_profile "$CASHOUT_FIRST_PROFILE"; then
  if (challenge_escape_scenario_enabled || cashout_after_challenge_scenario_enabled) &&
    [[ -n "$CASHOUT_SECOND_PROFILE" && "$CASHOUT_SECOND_PROFILE" != "$CASHOUT_FIRST_PROFILE" ]]; then
    echo "Retrying cash out with the other player first after the initial attempt failed..." >&2
    CASHOUT_FIRST_PROFILE="$CASHOUT_SECOND_PROFILE"
    CASHOUT_SECOND_PROFILE="$(other_round_player_profile "$CASHOUT_FIRST_PROFILE")"
    cashout_profile "$CASHOUT_FIRST_PROFILE"
  else
    exit 1
  fi
fi
if [[ "$CASHOUT_SECOND_PROFILE" != "$CASHOUT_FIRST_PROFILE" ]]; then
  wait_for_profile_cashout_visibility "$CASHOUT_SECOND_PROFILE" "$CASHOUT_FIRST_PROFILE"
  cashout_profile "$CASHOUT_SECOND_PROFILE"
fi
write_table_artifact host "$BASE/artifacts/table-after-cashout.json"

echo "Final wallet summaries:"
pcli wallet --profile "$PLAYER_ONE_PROFILE" --json
pcli wallet --profile "$PLAYER_TWO_PROFILE" --json

print_timing_summary

echo "Done. Logs are under $BASE"
