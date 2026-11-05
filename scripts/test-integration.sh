#!/usr/bin/env bash
# Run the integration test against a local Mosquitto that uses the real ACL file.
# Needs mosquitto and mosquitto_passwd on this computer (for example: brew install mosquitto).
set -euo pipefail
cd "$(dirname "$0")/.."

readonly PORT=18830
readonly BACKEND_PASSWORD=integration-backend-password
work_dir="$(mktemp -d)"
trap 'kill "${broker_pid:-}" 2>/dev/null || true; rm -rf "$work_dir"' EXIT

touch "$work_dir/passwd"
chmod 0700 "$work_dir/passwd"
mosquitto_passwd -b "$work_dir/passwd" backend "$BACKEND_PASSWORD"
mosquitto_passwd -b "$work_dir/passwd" DAPUR_A1B2C3 abcd2345
mosquitto_passwd -b "$work_dir/passwd" DAPUR_B2C3D4 efgh6789

cp deploy/mosquitto/config/acl "$work_dir/acl"
# Mosquitto started by root changes to the "mosquitto" user, which must read the files.
if [ "$(id -u)" = 0 ] && id mosquitto >/dev/null 2>&1; then
  chown -R mosquitto "$work_dir"
fi

cat > "$work_dir/mosquitto.conf" <<CONF
per_listener_settings false
allow_anonymous false
password_file $work_dir/passwd
acl_file $work_dir/acl
listener $PORT 127.0.0.1
CONF

mosquitto -c "$work_dir/mosquitto.conf" > "$work_dir/mosquitto.log" 2>&1 &
broker_pid=$!
sleep 1
kill -0 "$broker_pid" 2>/dev/null || { cat "$work_dir/mosquitto.log"; exit 1; }

BEFOREFIRE_TEST_MQTT_ADDRESS="127.0.0.1:$PORT" \
BEFOREFIRE_TEST_BACKEND_PASSWORD="$BACKEND_PASSWORD" \
  go test -tags integration -count=1 -race -v ./internal/app
