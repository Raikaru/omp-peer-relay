#!/usr/bin/env bash
# One-time: put the relay behind tailcat on HOST (default fates-vps).
# Needs tailcat installed on HOST. Prints the stable tailcat address that
# clients forward to. Allowed clients are managed with `tailcat-allow`.
set -euo pipefail
HOST="${1:-fates-vps}"
cd "$(dirname "$0")"
scp -q omp-tailcat.service tailcat-allow.sh "$HOST:/tmp/"
ssh "$HOST" 'bash -s' <<'EOF'
set -euo pipefail
id omp-tailcat >/dev/null 2>&1 || useradd --system --home-dir /var/lib/omp-tailcat --shell /usr/sbin/nologin omp-tailcat
install -d -m 0700 -o omp-tailcat -g omp-tailcat /var/lib/omp-tailcat
install -m 0644 /tmp/omp-tailcat.service /etc/systemd/system/omp-tailcat.service
install -m 0700 /tmp/tailcat-allow.sh /usr/local/sbin/tailcat-allow
rm -f /tmp/omp-tailcat.service /tmp/tailcat-allow.sh
if [ ! -f /var/lib/omp-tailcat/server.private.json ]; then
  runuser -u omp-tailcat -- env HOME=/var/lib/omp-tailcat tailcat genkey --key=/var/lib/omp-tailcat/server.private.json --fixed-region >/dev/null
fi
[ -f /etc/omp-tailcat.env ] || { umask 077; printf 'TAILCAT_ALLOW=none\n' > /etc/omp-tailcat.env; }
systemctl daemon-reload
systemctl enable omp-tailcat >/dev/null 2>&1
systemctl restart omp-tailcat
sleep 3
journalctl -u omp-tailcat -n 20 --no-pager -o cat | grep -o 'tc[A-Za-z0-9_-]\{20,\}' | tail -1
EOF
