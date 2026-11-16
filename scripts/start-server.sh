#!/usr/bin/env bash
# Start the BeforeFire server and give it the fixed name beforefire-server.local.
#
#   scripts/start-server.sh               start Docker and keep the name active (stop with Ctrl+C)
#   scripts/start-server.sh --build       same, but rebuild the backend image first
#   scripts/start-server.sh --background  same, but keep the name active in the background and return
#                                         to the prompt (stop with scripts/stop-server.sh)
#   scripts/start-server.sh --name-only   keep the name active for containers that already run
#   The options can be combined, for example: scripts/start-server.sh --build --background
#
# The app and the sensor firmware connect to beforefire-server.local, so the
# server can run on any Mac and on any Wi-Fi without new addresses in the app
# or on the board. The name works only while this script runs. When the Mac
# gets a new IP address (for example on another Wi-Fi), the script moves the
# name to the new address. Ctrl+C removes the name; the containers keep running
# until "docker compose down". In the background, the name stays active after
# the Terminal window closes, until scripts/stop-server.sh or a restart of the Mac.
#
# Only one Mac on a Wi-Fi network can use the name. Some school and office
# networks block .local names. On such a network, type the IP address of the
# Mac in the app and on the board (@M) instead.
set -euo pipefail
cd "$(dirname "$0")/.."

# Must match SensorBackendConfiguration.defaultBackendAddress in the app and
# BACKEND_MQTT_URI in the firmware (Sensor/KirimBackend.cpp).
readonly LOCAL_NAME="${BEFOREFIRE_LOCAL_NAME:-beforefire-server}"
readonly SERVICE_TYPE="_beforefire._tcp"
readonly CHECK_INTERVAL_SECONDS=5
readonly LOOKUP_SECONDS=3
# Used by --background and scripts/stop-server.sh. Both files are in .gitignore.
readonly PID_FILE=.server-name.pid
readonly BACKGROUND_LOG=.server-name.log

registration_pid=""
registered_ip=""
log_file=""
detached=false

usage() {
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

# In the background, every line gets the time, so the log shows when the address changed.
say() {
  if [ "$detached" = true ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
  else
    echo "$*"
  fi
}

# Prints the PID of the background name keeper when it runs.
background_pid() {
  local pid
  [ -f "$PID_FILE" ] || return 0
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    echo "$pid"
  else
    rm -f "$PID_FILE"
  fi
}

# Reads one value from .env, or prints the default. .env does not use quotes.
env_value() {
  local key="$1" fallback="$2" value=""
  if [ -f .env ]; then
    value="$(grep -E "^${key}=" .env | tail -n 1 | cut -d= -f2- | tr -d '[:space:]' || true)"
  fi
  echo "${value:-$fallback}"
}

# Prints the IPv4 address that other devices on the Wi-Fi or LAN can reach.
# A VPN can own the default route, so the script then uses the first Wi-Fi or
# Ethernet interface with an address.
current_ip() {
  local interface ip
  interface="$(route -n get default 2>/dev/null | awk '/interface:/ { print $2 }')"
  case "$interface" in
    en*|bridge*)
      ip="$(ipconfig getifaddr "$interface" 2>/dev/null || true)"
      if [ -n "$ip" ]; then
        echo "$ip"
        return
      fi
      ;;
  esac
  for interface in $(ifconfig -l); do
    case "$interface" in
      en*)
        ip="$(ipconfig getifaddr "$interface" 2>/dev/null || true)"
        if [ -n "$ip" ]; then
          echo "$ip"
          return
        fi
        ;;
    esac
  done
}

# Prints the IPv4 addresses that answer for NAME.local on the network.
lookup_name() {
  local output pid
  output="$(mktemp -t beforefire-lookup)"
  dns-sd -G v4 "$1.local" >"$output" 2>&1 &
  pid=$!
  sleep "$LOOKUP_SECONDS"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  # Columns: time, Add/Rmv, flags, interface, hostname, address, TTL.
  awk '$2 == "Add" { print $6 }' "$output" | sort -u
  rm -f "$output"
}

stop_registration() {
  if [ -n "$registration_pid" ]; then
    kill "$registration_pid" 2>/dev/null || true
    wait "$registration_pid" 2>/dev/null || true
  fi
  registration_pid=""
  registered_ip=""
}

register_name() {
  local ip="$1"
  # -P publishes the service and the address record NAME.local -> IP.
  # The TXT values tell a Bonjour client both ports.
  dns-sd -P "BeforeFire" "$SERVICE_TYPE" local "$HTTP_PORT" "$LOCAL_NAME.local" "$ip" \
    "http=$HTTP_PORT" "mqtt=$MQTT_PORT" >"$log_file" 2>&1 &
  registration_pid=$!
  registered_ip="$ip"
  say "$LOCAL_NAME.local -> $ip"
  if [ "$detached" = false ]; then
    echo "  App:   http://$LOCAL_NAME.local:$HTTP_PORT"
    echo "  Board: mqtt://$LOCAL_NAME.local:$MQTT_PORT"
  fi
}

cleanup() {
  stop_registration
  [ -n "$log_file" ] && rm -f "$log_file"
  if [ "$detached" = true ]; then
    [ "$(cat "$PID_FILE" 2>/dev/null || true)" = "$$" ] && rm -f "$PID_FILE"
    say "Removed the name $LOCAL_NAME.local."
  else
    echo
    echo "Removed the name $LOCAL_NAME.local. The containers still run; stop them with: docker compose down"
  fi
}

# Starts this script again without a terminal. It keeps the name active after
# the Terminal window closes. nohup ignores the hang-up signal of the window.
start_in_background() {
  local ip="$1" pid
  nohup "$0" --detached >>"$BACKGROUND_LOG" 2>&1 </dev/null &
  pid=$!
  echo "$pid" >"$PID_FILE"
  sleep 1
  if ! kill -0 "$pid" 2>/dev/null; then
    rm -f "$PID_FILE"
    echo "The name did not start. See $BACKGROUND_LOG." >&2
    tail -n 5 "$BACKGROUND_LOG" >&2
    exit 1
  fi
  echo "$LOCAL_NAME.local -> $ip"
  echo "  App:   http://$LOCAL_NAME.local:$HTTP_PORT"
  echo "  Board: mqtt://$LOCAL_NAME.local:$MQTT_PORT"
  echo "The name runs in the background (PID $pid). The Terminal is free; you can close it."
  echo "  Check: scripts/check-server.sh"
  echo "  Log:   tail -f $BACKGROUND_LOG"
  echo "  Stop:  scripts/stop-server.sh"
}

main() {
  local start_docker=true background=false compose_args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --build) compose_args+=(--build) ;;
      --name-only) start_docker=false ;;
      --background) background=true ;;
      # Only for start_in_background: the name keeper without a terminal.
      --detached) detached=true; start_docker=false ;;
      -h|--help) usage ;;
      *) echo "Unknown option: $1" >&2; usage ;;
    esac
    shift
  done

  if ! command -v dns-sd >/dev/null 2>&1; then
    echo "dns-sd is missing. This script runs only on macOS." >&2
    exit 1
  fi

  HTTP_PORT="$(env_value HTTP_PORT 8080)"
  MQTT_PORT="$(env_value MQTT_PORT 1883)"

  # This Mac already keeps the name in the background. A second copy would publish it twice.
  local running_pid=""
  [ "$detached" = false ] && running_pid="$(background_pid)"
  if [ -n "$running_pid" ] && { [ "$background" = false ] || [ "$start_docker" = false ]; }; then
    echo "The name already runs in the background on this Mac (PID $running_pid)." >&2
    echo "Stop it first with: scripts/stop-server.sh --keep-docker" >&2
    exit 1
  fi

  local ip
  ip="$(current_ip)"
  if [ -z "$ip" ] && [ "$detached" = false ]; then
    echo "The Mac has no Wi-Fi or Ethernet address. Connect to the network that the iPhone and the board use." >&2
    exit 1
  fi

  # Another Mac that runs this script on the same network already owns the name.
  # The background keeper skips it, because the first copy already checked.
  local owners=""
  if [ "$detached" = false ] && [ -z "$running_pid" ]; then
    owners="$(lookup_name "$LOCAL_NAME" | grep -v -x -F "$ip" || true)"
  fi
  if [ -n "$owners" ]; then
    echo "Another device already uses $LOCAL_NAME.local ($(echo "$owners" | tr '\n' ' ' | sed 's/ $//'))." >&2
    echo "Stop the server on that Mac first. Only one BeforeFire server can run on a network." >&2
    exit 1
  fi

  if [ "$start_docker" = true ]; then
    if ! docker info >/dev/null 2>&1; then
      echo "Docker is not running. Open Docker Desktop and run this script again." >&2
      exit 1
    fi
    local password
    password="$(env_value MQTT_PASSWORD "")"
    if [ ! -f .env ] || [ -z "$password" ] || [ "$password" = "change-me" ] \
        || [ ! -f deploy/mosquitto/config/passwd ]; then
      echo "This Mac is not set up yet. Run first: scripts/setup-server.sh <Serial> <PoP>" >&2
      exit 1
    fi
    docker compose up -d ${compose_args[@]+"${compose_args[@]}"}
  fi

  if [ "$background" = true ]; then
    if [ -n "$running_pid" ]; then
      echo "The name $LOCAL_NAME.local already runs in the background (PID $running_pid). The containers are up to date."
    else
      start_in_background "$ip"
    fi
    exit 0
  fi

  log_file="$(mktemp -t beforefire-name)"
  trap cleanup EXIT
  trap 'exit 0' INT TERM

  if [ -n "$ip" ]; then
    register_name "$ip"
  fi
  if [ "$detached" = true ]; then
    say "Keeping the name in the background (PID $$)."
  else
    echo "Keep this window open. Press Ctrl+C to remove the name."
  fi

  while true; do
    # "wait" returns at once on Ctrl+C, so the name goes away without delay.
    sleep "$CHECK_INTERVAL_SECONDS" &
    wait $! || true
    ip="$(current_ip)"
    if [ -z "$ip" ]; then
      if [ -n "$registered_ip" ]; then
        say "The Mac left the network. Waiting for a new address..."
        stop_registration
      fi
      continue
    fi
    if [ -z "$registered_ip" ]; then
      say "The Mac is on a network."
    elif [ "$ip" != "$registered_ip" ]; then
      say "The address of the Mac changed from $registered_ip."
    elif ! kill -0 "$registration_pid" 2>/dev/null; then
      say "The name stopped. Publishing it again:"
      sed 's/^/  dns-sd: /' "$log_file" | tail -n 3
    else
      continue
    fi
    stop_registration
    register_name "$ip"
  done
}

main "$@"
