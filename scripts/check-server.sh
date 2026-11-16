#!/usr/bin/env bash
# Check the BeforeFire server the same way the app and the boards reach it:
# through the name beforefire-server.local. Run it on any Mac on the same Wi-Fi.
#
#   scripts/check-server.sh                 check the name, Bonjour, HTTP, MQTT, and the device list
#   scripts/check-server.sh DAPUR_A1B2C3    also check that this board is online
#
# Each line shows OK or FAIL. The exit code is the number of failed checks.
set -uo pipefail
cd "$(dirname "$0")/.."

readonly LOCAL_NAME="${BEFOREFIRE_LOCAL_NAME:-beforefire-server}"
readonly HOST="$LOCAL_NAME.local"
readonly WAIT_SECONDS=3

failures=0

env_value() {
  local value=""
  [ -f .env ] && value="$(grep -E "^$1=" .env | tail -n 1 | cut -d= -f2- | tr -d '[:space:]' || true)"
  echo "${value:-$2}"
}

pass() { printf '  OK    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n        %s\n' "$1" "$2"; failures=$((failures + 1)); }

# Runs a dns-sd query for a few seconds and prints its output.
dns_sd_for_a_while() {
  local output pid
  output="$(mktemp -t beforefire-check)"
  dns-sd "$@" >"$output" 2>&1 &
  pid=$!
  sleep "$WAIT_SECONDS"
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  cat "$output"
  rm -f "$output"
}

main() {
  case "${1:-}" in -h|--help) sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;; esac
  local serial
  serial="$(echo "${1:-}" | tr '[:lower:]' '[:upper:]')"
  local http_port mqtt_port
  http_port="$(env_value HTTP_PORT 8080)"
  mqtt_port="$(env_value MQTT_PORT 1883)"

  echo "Checking $HOST (HTTP $http_port, MQTT $mqtt_port)"

  local addresses
  addresses="$(dns_sd_for_a_while -G v4 "$HOST" | awk '$2 == "Add" { print $6 }' | sort -u)"
  if [ -z "$addresses" ]; then
    fail "Name $HOST" "No device answers. Run scripts/start-server.sh on the server Mac, on this Wi-Fi."
    echo
    echo "$failures check(s) failed."
    exit "$failures"
  fi
  if [ "$(echo "$addresses" | wc -l | tr -d ' ')" -gt 1 ]; then
    fail "Name $HOST" "More than one address answers ($(echo "$addresses" | tr '\n' ' ')). Stop the server on one Mac."
  else
    pass "Name $HOST -> $addresses"
  fi

  if dns_sd_for_a_while -B _beforefire._tcp | grep -q "_beforefire._tcp"; then
    pass "Bonjour service _beforefire._tcp"
  else
    fail "Bonjour service _beforefire._tcp" "Not found. The name may come from another program than start-server.sh."
  fi

  local health
  health="$(curl -s -m 5 "http://$HOST:$http_port/healthz" || true)"
  if [ "$health" = '{"mqttConnected":true}' ]; then
    pass "HTTP  http://$HOST:$http_port/healthz -> $health"
  elif [ -n "$health" ]; then
    fail "HTTP  http://$HOST:$http_port/healthz -> $health" "The backend runs but is not connected to the broker. See: docker compose logs backend"
  else
    fail "HTTP  http://$HOST:$http_port/healthz" "No answer. Check Docker on the server Mac: docker compose ps"
  fi

  if nc -z -G 3 "$HOST" "$mqtt_port" >/dev/null 2>&1; then
    pass "MQTT  $HOST:$mqtt_port is open"
  else
    fail "MQTT  $HOST:$mqtt_port" "The port is closed. Check the mosquitto container on the server Mac."
  fi

  local devices code
  devices="$(curl -s -m 5 -w '\n%{http_code}' "http://$HOST:$http_port/v1/devices" || true)"
  code="$(echo "$devices" | tail -n 1)"
  devices="$(echo "$devices" | sed '$d')"
  if [ "$code" = "200" ]; then
    pass "Device list -> $devices"
  elif [ "$code" = "404" ]; then
    fail "Device list" "404: the backend image is old. Run scripts/start-server.sh --build on the server Mac."
  else
    fail "Device list" "No answer (HTTP $code)."
  fi

  if [ -n "$serial" ]; then
    if echo "$devices" | grep -q "\"deviceId\":\"$serial\",\"connection\":\"online\""; then
      pass "Board $serial is online"
    elif echo "$devices" | grep -q "\"deviceId\":\"$serial\""; then
      fail "Board $serial is offline" "Check Wi-Fi of the board and type @M in the Serial Monitor."
    else
      fail "Board $serial is unknown" "The board never connected. Register it: scripts/setup-server.sh $serial <PoP>"
    fi
  fi

  echo
  if [ "$failures" -eq 0 ]; then
    echo "All checks passed."
  else
    echo "$failures check(s) failed."
  fi
  exit "$failures"
}

main "$@"
