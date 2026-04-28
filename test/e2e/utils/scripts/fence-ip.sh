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

echo "[$(date)] Fencing $target using $ipt_cmd"

# Check if rule already exists, if not add it
if ! $ipt_cmd -C OUTPUT -d "$target" -j REJECT --reject-with icmp-host-unreachable 2>/dev/null; then
	$ipt_cmd -I OUTPUT -d "$target" -j REJECT --reject-with icmp-host-unreachable
	echo "[$(date)] Added fence rule for: $target"
else
	echo "[$(date)] Fence rule already exists for: $target"
fi

echo "[$(date)] Fenced: $target"
