#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL_STATE_DIR="${LOCAL_STATE_DIR:-$ROOT_DIR/.tmp/local-regtest}"
PID_DIR="$LOCAL_STATE_DIR/pids"
DAEMON_STATE_DIR="$LOCAL_STATE_DIR/work/daemons"

declare -a MATCHED_PIDS=()
declare -a MATCHED_COMMANDS=()

usage() {
  cat <<'EOF'
usage: kill-floating-parker-processes.sh [--dry-run]

Find Parker daemon, controller, and indexer processes launched from this
workspace and terminate them. Also removes stale local pid and socket metadata
from the default local regtest state directory.
EOF
}

matches_workspace_process() {
  local command="$1"

  case "$command" in
    "$ROOT_DIR/.tmp/parker-bin/parker-daemon-go"*) return 0 ;;
    "$ROOT_DIR/.tmp/parker-bin/parker-controller-go"*) return 0 ;;
    "$ROOT_DIR/.tmp/parker-bin/parker-indexer-go"*) return 0 ;;
    "$ROOT_DIR/scripts/bin/parker-daemon"*) return 0 ;;
    "$ROOT_DIR/scripts/bin/parker-controller"*) return 0 ;;
    "$ROOT_DIR/scripts/bin/parker-indexer"*) return 0 ;;
    "/usr/bin/env bash $ROOT_DIR/scripts/bin/parker-daemon"*) return 0 ;;
    "/usr/bin/env bash $ROOT_DIR/scripts/bin/parker-controller"*) return 0 ;;
    "/usr/bin/env bash $ROOT_DIR/scripts/bin/parker-indexer"*) return 0 ;;
    "/bin/bash $ROOT_DIR/scripts/bin/parker-daemon"*) return 0 ;;
    "/bin/bash $ROOT_DIR/scripts/bin/parker-controller"*) return 0 ;;
    "/bin/bash $ROOT_DIR/scripts/bin/parker-indexer"*) return 0 ;;
  esac

  return 1
}

collect_matches() {
  local ps_output=""
  local line=""
  local pid=""
  local command=""

  ps_output="$(ps -axo pid=,command=)"

  while IFS= read -r line; do
    [[ "$line" =~ ^[[:space:]]*([0-9]+)[[:space:]]+(.*)$ ]] || continue
    pid="${BASH_REMATCH[1]}"
    command="${BASH_REMATCH[2]}"

    [[ "$pid" == "$$" ]] && continue
    matches_workspace_process "$command" || continue

    MATCHED_PIDS+=("$pid")
    MATCHED_COMMANDS+=("$command")
  done <<<"$ps_output"
}

print_matches() {
  local i

  for ((i = 0; i < ${#MATCHED_PIDS[@]}; i += 1)); do
    printf '%s %s\n' "${MATCHED_PIDS[$i]}" "${MATCHED_COMMANDS[$i]}"
  done
}

cleanup_local_state() {
  if [[ -d "$PID_DIR" ]]; then
    find "$PID_DIR" -maxdepth 1 -type f -name '*.pid' -delete
  fi

  if [[ -d "$DAEMON_STATE_DIR" ]]; then
    find "$DAEMON_STATE_DIR" -maxdepth 1 -type f \( -name '*.json' -o -name '*.sock' \) -delete
  fi
}

send_term() {
  local pid=""

  for pid in "${MATCHED_PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
}

send_kill_to_survivors() {
  local pid=""

  for pid in "${MATCHED_PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
}

wait_for_exit() {
  local attempts="${1:-20}"
  local sleep_seconds="${2:-0.1}"
  local i=""
  local pid=""

  for ((i = 0; i < attempts; i += 1)); do
    for pid in "${MATCHED_PIDS[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        sleep "$sleep_seconds"
        continue 2
      fi
    done
    return 0
  done

  return 1
}

assert_no_survivors() {
  local pid=""
  local failed=0

  for pid in "${MATCHED_PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      printf 'failed to terminate pid %s\n' "$pid" >&2
      failed=1
    fi
  done

  return "$failed"
}

dry_run=0
if [[ $# -gt 1 ]]; then
  usage >&2
  exit 1
fi

if [[ $# -eq 1 ]]; then
  case "$1" in
    --dry-run) dry_run=1 ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 1
      ;;
  esac
fi

collect_matches

if [[ ${#MATCHED_PIDS[@]} -eq 0 ]]; then
  cleanup_local_state
  echo "No floating Parker daemon/controller/indexer processes found for $ROOT_DIR."
  exit 0
fi

echo "Found ${#MATCHED_PIDS[@]} floating Parker process(es):"
print_matches

if [[ "$dry_run" == "1" ]]; then
  exit 0
fi

send_term
wait_for_exit 20 0.1 || true
send_kill_to_survivors
wait_for_exit 20 0.1 || true
cleanup_local_state
assert_no_survivors

echo "Killed ${#MATCHED_PIDS[@]} floating Parker process(es)."
