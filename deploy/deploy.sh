#!/usr/bin/env bash
# Build the relay for linux/amd64 and install/upgrade it on HOST (default fates-vps).
# First run creates the service user, /etc/omp-peer-relay.env
# (RELAY_LISTEN=127.0.0.1:7480) and the `omp-relay-admin` wrapper for issuing
# machine tokens. Every run swaps the binary and (re)installs the nightly
# database snapshot timer (14 kept in /var/lib/omp-peer-relay/backups).
set -euo pipefail
HOST="${1:-fates-vps}"
cd "$(dirname "$0")/../relay"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" -o ../bin/omp-peer-relay-linux-amd64 .
scp -q ../bin/omp-peer-relay-linux-amd64 ../deploy/omp-peer-relay.service ../deploy/omp-peer-relay-backup.service ../deploy/omp-peer-relay-backup.timer "$HOST:/tmp/"

ssh "$HOST" 'bash -s' <<'EOF'
set -euo pipefail
id omp-relay >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin omp-relay
install -d -m 0755 /opt/omp-peer-relay
install -m 0755 /tmp/omp-peer-relay-linux-amd64 /opt/omp-peer-relay/omp-peer-relay
for unit in omp-peer-relay.service omp-peer-relay-backup.service omp-peer-relay-backup.timer; do
  install -m 0644 "/tmp/$unit" "/etc/systemd/system/$unit"
  rm -f "/tmp/$unit"
done
rm -f /tmp/omp-peer-relay-linux-amd64
if [ ! -f /etc/omp-peer-relay.env ] || grep -q '^RELAY_TOKEN=' /etc/omp-peer-relay.env; then
  umask 077
  printf 'RELAY_LISTEN=127.0.0.1:7480\n' > /etc/omp-peer-relay.env
fi
cat > /usr/local/sbin/omp-relay-admin <<'WRAP'
#!/bin/sh
# Manage machine tokens: omp-relay-admin add <name> -role owner|guest (-room <url>... | -all-rooms) | list | rooms | rotate | revoke
# Runs as the service user so SQLite side files keep the right owner.
exec runuser -u omp-relay -- /opt/omp-peer-relay/omp-peer-relay principal "$@" -db /var/lib/omp-peer-relay/relay.db
WRAP
chmod 0700 /usr/local/sbin/omp-relay-admin
systemctl daemon-reload
systemctl enable omp-peer-relay >/dev/null 2>&1
systemctl enable --now omp-peer-relay-backup.timer >/dev/null 2>&1
systemctl restart omp-peer-relay
sleep 1
systemctl is-active omp-peer-relay
curl -fsS http://127.0.0.1:7480/healthz
EOF
