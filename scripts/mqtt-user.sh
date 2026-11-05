#!/usr/bin/env bash
# Manage Mosquitto users for BeforeFire.
#
#   scripts/mqtt-user.sh init                      create the passwd file with the backend user (MQTT_PASSWORD in .env)
#   scripts/mqtt-user.sh add DAPUR_A1B2C3 PASSWORD add or change a device (prototype: PASSWORD is the PoP from the sticker)
#   scripts/mqtt-user.sh add DAPUR_A1B2C3          add a device with a new random password and print it
#   scripts/mqtt-user.sh remove DAPUR_A1B2C3       remove a device; it cannot connect again
#
# The commands run inside the mosquitto image, so Mosquitto does not need to be
# installed on the Mac. A running broker reads the changes after SIGHUP.
set -euo pipefail
cd "$(dirname "$0")/.."

readonly PASSWD_FILE=/mosquitto/config/passwd
readonly ACL_FILE=/mosquitto/config/acl
readonly DEVICE_ID_PATTERN='^DAPUR_(ALM_)?[0-9A-F]{6}$'

usage() {
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

# Runs a shell command in a one-time mosquitto container. NEW_USER and
# NEW_PASSWORD pass through the environment, so no quoting problems occur.
run_in_mosquitto() {
  docker compose run --rm --no-deps -T \
    -e NEW_USER="${NEW_USER:-}" -e NEW_PASSWORD="${NEW_PASSWORD:-}" \
    --entrypoint sh mosquitto -c "$1"
}

# Mosquitto 2 warns (and later versions refuse) when these files are readable
# by other users or are not owned by the mosquitto user.
# Some Docker file shares do not allow chown. Mosquitto then only prints a warning.
fix_permissions() {
  run_in_mosquitto "chown mosquitto:mosquitto $PASSWD_FILE $ACL_FILE && chmod 0700 $PASSWD_FILE && chmod 0600 $ACL_FILE" \
    || echo "Warning: cannot change the owner of the passwd and acl files. Mosquitto still works."
}

reload_broker() {
  if [ -n "$(docker compose ps -q --status running mosquitto)" ]; then
    docker compose kill -s SIGHUP mosquitto >/dev/null
    echo "Mosquitto reloaded the users."
  else
    echo "Mosquitto is not running. It reads the users when it starts."
  fi
}

check_device_id() {
  if ! [[ "$1" =~ $DEVICE_ID_PATTERN ]]; then
    echo "Device ID '$1' is not valid. Use the serial from the sticker, for example DAPUR_A1B2C3." >&2
    exit 1
  fi
}

command_name="${1:-}"
case "$command_name" in
  init)
    if [ -f deploy/mosquitto/config/passwd ]; then
      echo "deploy/mosquitto/config/passwd exists. Remove it first to make a new file." >&2
      exit 1
    fi
    [ -f .env ] || { echo "Create .env from .env.example first." >&2; exit 1; }
    NEW_USER="$(grep -E '^MQTT_USERNAME=' .env | cut -d= -f2- || true)"
    NEW_USER="${NEW_USER:-backend}"
    NEW_PASSWORD="$(grep -E '^MQTT_PASSWORD=' .env | cut -d= -f2-)"
    if [ -z "$NEW_PASSWORD" ] || [ "$NEW_PASSWORD" = "change-me" ]; then
      echo "Set MQTT_PASSWORD in .env to a long random value first." >&2
      exit 1
    fi
    run_in_mosquitto "mosquitto_passwd -c -b $PASSWD_FILE \"\$NEW_USER\" \"\$NEW_PASSWORD\""
    fix_permissions
    echo "Created the passwd file with user '$NEW_USER'."
    reload_broker
    ;;
  add)
    NEW_USER="${2:-}"
    [ -n "$NEW_USER" ] || usage
    check_device_id "$NEW_USER"
    NEW_PASSWORD="${3:-}"
    if [ -z "$NEW_PASSWORD" ]; then
      NEW_PASSWORD="$(openssl rand -base64 24)"
      echo "New password for $NEW_USER (write it to the NVS of the board):"
      echo "  $NEW_PASSWORD"
    fi
    [ -f deploy/mosquitto/config/passwd ] || { echo "Run 'scripts/mqtt-user.sh init' first." >&2; exit 1; }
    run_in_mosquitto "mosquitto_passwd -b $PASSWD_FILE \"\$NEW_USER\" \"\$NEW_PASSWORD\""
    fix_permissions
    echo "Device $NEW_USER can connect."
    reload_broker
    ;;
  remove)
    NEW_USER="${2:-}"
    [ -n "$NEW_USER" ] || usage
    check_device_id "$NEW_USER"
    run_in_mosquitto "mosquitto_passwd -D $PASSWD_FILE \"\$NEW_USER\""
    fix_permissions
    echo "Device $NEW_USER removed."
    reload_broker
    ;;
  *)
    usage
    ;;
esac
