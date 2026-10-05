#!/usr/bin/env bash
# Linux peer-bus client over tailcat. Works for owners and guests alike.
#
#   setup-linux-client.sh key
#       Print this machine's tailcat client key (generated on first use).
#       Send it to the relay admin, who runs `tailcat-allow add <name> <key>`.
#
#   setup-linux-client.sh install <machine> <tailcat-address>   (relay token on stdin)
#       Write ~/.omp/peer-bus.json and a systemd user service that forwards
#       127.0.0.1:7480 to the relay. <machine> must match the token's name.
#
# Requires tailcat on PATH (or ~/.local/bin) and the extension enabled in omp
# (see README: clone this repo, list extension/peer-bus under `extensions:`).
set -euo pipefail
TAILCAT=$(command -v tailcat || echo "$HOME/.local/bin/tailcat")
[ -x "$TAILCAT" ] || { echo "tailcat not found; install it from https://github.com/tailscale/tailcat/releases" >&2; exit 1; }

case "${1:-}" in
key)
	KEYFILE="$HOME/.config/tailcat/keys/client-default.private.json"
	[ -f "$KEYFILE" ] || "$TAILCAT" genkey --client --key=client-default >/dev/null 2>&1
	grep -o '"ServerPublic": *"nodekey:[0-9a-f]*"' "$KEYFILE" | grep -o 'nodekey:[0-9a-f]*'
	;;
install)
	[ $# -eq 3 ] || { echo "usage: $0 install <machine> <tailcat-address>  (token on stdin)" >&2; exit 2; }
	MACHINE=$2 ADDRESS=$3
	read -r TOKEN
	[ -n "$TOKEN" ] || { echo "no token on stdin" >&2; exit 2; }
	mkdir -p ~/.omp
	(
		umask 077
		printf '{\n  "url": "ws://127.0.0.1:7480/ws",\n  "token": "%s",\n  "machine": "%s"\n}\n' "$TOKEN" "$MACHINE" > ~/.omp/peer-bus.json
	)
	mkdir -p ~/.config/systemd/user
	cat > ~/.config/systemd/user/omp-relay-tunnel.service <<UNIT
[Unit]
Description=tailcat forward to omp-peer-relay
After=network-online.target

[Service]
ExecStart=$TAILCAT forward $ADDRESS 7480
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
	systemctl --user daemon-reload
	systemctl --user enable omp-relay-tunnel.service >/dev/null 2>&1
	systemctl --user restart omp-relay-tunnel.service
	for _ in $(seq 20); do curl -fsS http://127.0.0.1:7480/healthz 2>/dev/null && exit 0; sleep 1; done
	echo "relay not reachable yet; check: journalctl --user -u omp-relay-tunnel" >&2
	exit 1
	;;
*)
	sed -n '2,13p' "$0" >&2
	exit 2
	;;
esac
