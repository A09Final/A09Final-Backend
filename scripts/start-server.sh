#!/usr/bin/env bash
# Start the BeforeFire server and give it the fixed name beforefire-server.local.
#
#   scripts/start-server.sh              start Docker and keep the name active (stop with Ctrl+C)
#   scripts/start-server.sh --build      same, but rebuild the backend image first
#   scripts/start-server.sh --name-only  keep the name active for containers that already run
#
# The app and the sensor firmware connect to beforefire-server.local, so the
# server can run on any Mac and on any Wi-Fi without new addresses in the app
# or on the board. The name works only while this script runs. When the Mac
# gets a new IP address (for example on another Wi-Fi), the script moves the
# name to the new address. Ctrl+C removes the name; the containers keep running
# until "docker compose down".
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

registration_pid=""
registered_ip=""
log_file=""

usage() {
  sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
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
  echo "$LOCAL_NAME.local -> $ip"
  echo "  App:   http://$LOCAL_NAME.local:$HTTP_PORT"
  echo "  Board: mqtt://$LOCAL_NAME.local:$MQTT_PORT"
}

cleanup() {
  stop_registration
  [ -n "$log_file" ] && rm -f "$log_file"
  echo
  echo "Removed the name $LOCAL_NAME.local. The containers still run; stop them with: docker compose down"
}

main() {
  local start_docker=true compose_args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --build) compose_args+=(--build) ;;
      --name-only) start_docker=false ;;
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

  local ip
  ip="$(current_ip)"
  if [ -z "$ip" ]; then
    echo "The Mac has no Wi-Fi or Ethernet address. Connect to the network that the iPhone and the board use." >&2
    exit 1
  fi

  # Another Mac that runs this script on the same network already owns the name.
  local owners
  owners="$(lookup_name "$LOCAL_NAME" | grep -v -x -F "$ip" || true)"
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

  log_file="$(mktemp -t beforefire-name)"
  trap cleanup EXIT
  trap 'exit 0' INT TERM

  register_name "$ip"
  echo "Keep this window open. Press Ctrl+C to remove the name."

  while true; do
    # "wait" returns at once on Ctrl+C, so the name goes away without delay.
    sleep "$CHECK_INTERVAL_SECONDS" &
    wait $! || true
    ip="$(current_ip)"
    if [ -z "$ip" ]; then
      if [ -n "$registered_ip" ]; then
        echo "The Mac left the network. Waiting for a new address..."
        stop_registration
      fi
      continue
    fi
    if [ -z "$registered_ip" ]; then
      echo "The Mac is back on a network."
    elif [ "$ip" != "$registered_ip" ]; then
      echo "The address of the Mac changed from $registered_ip."
    elif ! kill -0 "$registration_pid" 2>/dev/null; then
      echo "The name stopped. Publishing it again:"
      sed 's/^/  dns-sd: /' "$log_file" | tail -n 3
    else
      continue
    fi
    stop_registration
    register_name "$ip"
  done
}

main "$@"
