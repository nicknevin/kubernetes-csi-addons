#!/bin/bash
set -euo pipefail

if [ $# -ne 1 ]; then
	echo "Usage: $0 <target_ip_or_cidr>"
	exit 1
fi

target="$1"
ipt_cmd=$(detect-iptables)

if [ -z "$ipt_cmd" ]; then
	echo "[$(date)] ERROR: No working iptables command found"
	exit 1
fi

echo "[$(date)] Unfencing $target using $ipt_cmd"
$ipt_cmd -D OUTPUT -d "$target" -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || true
echo "[$(date)] Unfenced: $target"
