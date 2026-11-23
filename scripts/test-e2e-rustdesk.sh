#!/usr/bin/env bash
# End-to-end verification of GoDesk Plane against the real official RustDesk desktop client.
set -euo pipefail

HTTP_URL="${HTTP_URL:-http://127.0.0.1:21114}"
RD_HOST="${RD_HOST:-127.0.0.1}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-godesk-admin}"
DISPLAY_NUM="${DISPLAY:-:99}"

if ! command -v rustdesk >/dev/null 2>&1; then
  echo "rustdesk binary not found in PATH" >&2
  exit 1
fi

# Wait for GoDesk Plane HTTP API to be healthy
for i in $(seq 1 30); do
  if curl -sf "${HTTP_URL}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done

TOKEN=$(curl -sS -X POST "${HTTP_URL}/admin/api/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASSWORD}\"}" | jq -r .token)

if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
  echo "Failed to authenticate against ${HTTP_URL}/admin/api/login" >&2
  exit 1
fi

SETTINGS=$(curl -sS "${HTTP_URL}/admin/api/settings" -H "Authorization: Bearer $TOKEN")
PUBKEY=$(echo "$SETTINGS" | jq -r .public_key)
echo "Server Public Key: $PUBKEY"

# Force relay temporarily for local loopback 2-client relay verification
curl -sS -X PUT "${HTTP_URL}/admin/api/settings" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"public_host\":\"${RD_HOST}\",\"require_key\":true,\"always_use_relay\":true,\"relay_servers\":[\"${RD_HOST}\"]}" >/dev/null

# Configure Host RustDesk client (current user HOME)
mkdir -p "${HOME}/.config/rustdesk"
cat > "${HOME}/.config/rustdesk/RustDesk2.toml" <<EOF
rendezvous_server = "${RD_HOST}:21116"
nat_type = 1
serial = 0

[options]
custom-rendezvous-server = "${RD_HOST}"
relay-server = "${RD_HOST}"
api-server = "${HTTP_URL}"
key = "${PUBKEY}"
EOF

# Configure Caller RustDesk client (isolated HOME)
CALLER_HOME="/tmp/godesk-e2e-caller"
rm -rf "$CALLER_HOME"
mkdir -p "${CALLER_HOME}/.config/rustdesk"
cat > "${CALLER_HOME}/.config/rustdesk/RustDesk2.toml" <<EOF
rendezvous_server = "${RD_HOST}:21116"
nat_type = 1
serial = 0

[options]
custom-rendezvous-server = "${RD_HOST}"
relay-server = "${RD_HOST}"
api-server = "${HTTP_URL}"
key = "${PUBKEY}"
EOF

HOST_PID=""
cleanup() {
  [ -n "$HOST_PID" ] && kill "$HOST_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "Starting real RustDesk host client..."
DISPLAY="$DISPLAY_NUM" rustdesk > /tmp/godesk-e2e-host.log 2>&1 &
HOST_PID=$!

# Wait for peer registration & sysinfo upload
HOST_ID=""
for i in $(seq 1 20); do
  PEERS_JSON=$(curl -sS "${HTTP_URL}/admin/api/peers" -H "Authorization: Bearer $TOKEN")
  HOST_ID=$(echo "$PEERS_JSON" | jq -r '.peers[] | select(.online == true and .hostname != "") | .id' | head -n 1)
  if [ -n "$HOST_ID" ]; then
    break
  fi
  sleep 0.5
done

if [ -z "$HOST_ID" ]; then
  echo "FAIL: RustDesk host did not register & upload sysinfo" >&2
  curl -sS "${HTTP_URL}/admin/api/peers" -H "Authorization: Bearer $TOKEN" | jq .
  exit 1
fi

echo "PASS: Real RustDesk peer registered online: ID=$HOST_ID"
curl -sS "${HTTP_URL}/admin/api/peers" -H "Authorization: Bearer $TOKEN" | jq '.peers[] | {id, hostname, version, os, online}'

echo "Initiating real RustDesk caller connection -> $HOST_ID..."
HOME="$CALLER_HOME" DISPLAY="$DISPLAY_NUM" timeout 7 rustdesk --connect "$HOST_ID" > /tmp/godesk-e2e-caller.log 2>&1 || true

sleep 1.5
kill "$HOST_PID" 2>/dev/null || true
HOST_PID=""
sleep 1

SESSIONS=$(curl -sS "${HTTP_URL}/admin/api/sessions" -H "Authorization: Bearer $TOKEN")
RELAY_COUNT=$(echo "$SESSIONS" | jq '[.sessions[] | select(.is_relay == true and (.bytes_sent + .bytes_recv) > 0)] | length')
CONN_AUDIT=$(curl -sS "${HTTP_URL}/admin/api/audit?category=connection" -H "Authorization: Bearer $TOKEN" | jq '.total')
RELAY_AUDIT=$(curl -sS "${HTTP_URL}/admin/api/audit?category=relay" -H "Authorization: Bearer $TOKEN" | jq '.total')

echo "Sessions:"
echo "$SESSIONS" | jq '.sessions'
echo "Connection audit events: $CONN_AUDIT | Relay audit events: $RELAY_AUDIT | Active/completed relay sessions with bytes: $RELAY_COUNT"

if [ "$RELAY_COUNT" -lt 1 ] || [ "$CONN_AUDIT" -lt 1 ] || [ "$RELAY_AUDIT" -lt 1 ]; then
  echo "FAIL: Expected relay session with transferred bytes and connection/relay audit logs" >&2
  exit 1
fi

echo "ALL REAL RUSTDESK E2E CHECKS PASSED!"
