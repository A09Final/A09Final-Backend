#!/usr/bin/env bash
# Stop the BeforeFire server that scripts/start-server.sh --background started.
#
#   scripts/stop-server.sh               remove the name beforefire-server.local and stop the containers
#   scripts/stop-server.sh --keep-docker remove the name only; the containers keep running
#
# A start-server.sh that runs in a Terminal window (without --background)
# stops with Ctrl+C in that window.
set -euo pipefail
cd "$(dirname "$0")/.."

# Must match start-server.sh.
readonly PID_FILE=.server-name.pid
readonly STOP_WAIT_SECONDS=5

usage() {
  sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

main() {
  local stop_docker=true
  while [ $# -gt 0 ]; do
    case "$1" in
      --keep-docker) stop_docker=false ;;
      -h|--help) usage ;;
      *) echo "Unknown option: $1" >&2; usage ;;
    esac
    shift
  done

  local pid=""
  [ -f "$PID_FILE" ] && pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    # TERM lets start-server.sh remove the name before it ends.
    kill -TERM "$pid"
    local waited=0
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$STOP_WAIT_SECONDS" ]; do
      sleep 1
      waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
      pkill -f "dns-sd -P BeforeFire" 2>/dev/null || true
    fi
    rm -f "$PID_FILE"
    echo "Removed the name beforefire-server.local (PID $pid)."
  else
    rm -f "$PID_FILE"
    echo "The name does not run in the background. A start-server.sh in a Terminal window stops with Ctrl+C."
  fi

  if [ "$stop_docker" = true ]; then
    if docker info >/dev/null 2>&1; then
      docker compose down
    else
      echo "Docker is not running, so the containers are already stopped."
    fi
  elif [ -n "$(docker compose ps -q 2>/dev/null || true)" ]; then
    echo "The containers keep running. Stop them with: docker compose down"
  fi
}

main "$@"
