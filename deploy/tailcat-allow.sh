#!/bin/sh
# Manage which tailcat client keys may reach the relay (run on the relay host as root).
#   tailcat-allow list
#   tailcat-allow add <name> nodekey:<hex>
#   tailcat-allow remove <name>
# /etc/omp-tailcat.keys holds "<name> <nodekey>" lines; the service reads the
# derived comma list from /etc/omp-tailcat.env and is restarted on change.
set -eu
KEYS=/etc/omp-tailcat.keys
touch "$KEYS"; chmod 600 "$KEYS"
apply() {
	allow=$(awk 'NF==2 {print $2}' "$KEYS" | paste -sd, -)
	[ -n "$allow" ] || allow=none
	umask 077
	printf 'TAILCAT_ALLOW=%s\n' "$allow" > /etc/omp-tailcat.env
	systemctl restart omp-tailcat
}
case "${1:-}" in
list) cat "$KEYS" ;;
add)
	[ $# -eq 3 ] || { echo "usage: tailcat-allow add <name> nodekey:<hex>" >&2; exit 2; }
	echo "$3" | grep -Eq '^nodekey:[0-9a-f]{64}$' || { echo "not a nodekey: $3" >&2; exit 2; }
	grep -v "^$2 " "$KEYS" > "$KEYS.tmp" || true
	echo "$2 $3" >> "$KEYS.tmp"; mv "$KEYS.tmp" "$KEYS"; apply ;;
remove)
	[ $# -eq 2 ] || { echo "usage: tailcat-allow remove <name>" >&2; exit 2; }
	grep -v "^$2 " "$KEYS" > "$KEYS.tmp" || true
	mv "$KEYS.tmp" "$KEYS"; apply ;;
*) echo "usage: tailcat-allow list | add <name> nodekey:<hex> | remove <name>" >&2; exit 2 ;;
esac
