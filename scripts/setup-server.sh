#!/usr/bin/env bash
# Prepare this Mac to run the BeforeFire server. Run it once per Mac, and again
# to register more boards. It is safe to run more than once.
#
#   scripts/setup-server.sh                          create .env and the broker users
#   scripts/setup-server.sh DAPUR_A1B2C3 abcd2345    same, and register a board (serial and PoP from the sticker)
#   scripts/setup-server.sh DAPUR_A1B2C3 abcd2345 DAPUR_D4E5F6 wxyz6789   register several boards
#
# What it does:
#   1. Copies .env.example to .env when .env is missing.
#   2. Puts a new random MQTT_PASSWORD in .env when the value is empty or "change-me".
#      An existing password stays. Before it changes .env, it copies .env to .env.backup.
#   3. Creates the broker user "backend" with that password, or updates it.
#   4. Registers each board with its serial and PoP.
# Then start the server with scripts/start-server.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

readonly ENV_FILE=.env
readonly ENV_BACKUP=.env.backup
readonly PASSWD_FILE=deploy/mosquitto/config/passwd
readonly PLACEHOLDER_PASSWORD="change-me"

usage() {
  sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

env_value() {
  grep -E "^$1=" "$ENV_FILE" | tail -n 1 | cut -d= -f2- || true
}

# Replaces KEY=... in .env, or adds the line when the key is missing.
set_env_value() {
  local key="$1" value="$2" temporary
  temporary="$(mktemp "$ENV_FILE.XXXXXX")"
  if grep -q -E "^$key=" "$ENV_FILE"; then
    awk -v key="$key" -v value="$value" \
      'index($0, key "=") == 1 { print key "=" value; next } { print }' "$ENV_FILE" >"$temporary"
  else
    cat "$ENV_FILE" >"$temporary"
    printf '%s=%s\n' "$key" "$value" >>"$temporary"
  fi
  chmod 600 "$temporary"
  mv "$temporary" "$ENV_FILE"
}

main() {
  case "${1:-}" in -h|--help) usage ;; esac
  if [ $(( $# % 2 )) -ne 0 ]; then
    echo "Give each board as a pair: serial and PoP." >&2
    usage
  fi

  if ! docker info >/dev/null 2>&1; then
    echo "Docker is not running. Open Docker Desktop and run this script again." >&2
    exit 1
  fi

  # 1. .env
  local env_created=false
  if [ -f "$ENV_FILE" ]; then
    echo "• .env exists."
  else
    cp .env.example "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    env_created=true
    echo "• Created .env from .env.example."
  fi

  # 2. Random password for the backend user
  local password password_changed=false
  password="$(env_value MQTT_PASSWORD)"
  if [ -z "$password" ] || [ "$password" = "$PLACEHOLDER_PASSWORD" ]; then
    # Hexadecimal only, so the value needs no quotes in .env.
    password="$(openssl rand -hex 24)"
    if [ "$env_created" = false ]; then
      cp -p "$ENV_FILE" "$ENV_BACKUP"
      echo "• Copied the old .env to $ENV_BACKUP."
    fi
    set_env_value MQTT_PASSWORD "$password"
    password_changed=true
    echo "• Put a new random MQTT_PASSWORD in .env."
  else
    echo "• MQTT_PASSWORD in .env is already set. It stays."
  fi

  # 3. Broker user "backend"
  if [ ! -f "$PASSWD_FILE" ]; then
    scripts/mqtt-user.sh init
  elif [ "$password_changed" = true ]; then
    scripts/mqtt-user.sh backend
  else
    echo "• The broker already has the backend user."
  fi

  # 4. Boards
  while [ $# -gt 0 ]; do
    scripts/mqtt-user.sh add "$(echo "$1" | tr '[:lower:]' '[:upper:]')" "$2"
    shift 2
  done

  echo
  echo "Done. Start the server with: scripts/start-server.sh --build"
  if [ "$password_changed" = true ] && [ -n "$(docker compose ps -q --status running backend 2>/dev/null)" ]; then
    echo "The backend already runs with the old password. scripts/start-server.sh restarts it with the new one."
  fi
}

main "$@"
