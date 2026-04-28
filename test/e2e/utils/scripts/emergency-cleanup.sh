#!/bin/bash
set -euo pipefail

ipt_cmd=$(detect-iptables)

if [ -z "$ipt_cmd" ]; then
	echo "[$(date)] ERROR: No working iptables command found"
	exit 1
fi

echo "[$(date)] Emergency cleanup: removing all REJECT rules using $ipt_cmd"
$ipt_cmd -S OUTPUT | grep "\-j REJECT" | sed 's/^-A/-D/' | while IFS= read -r rule; do
	echo "[$(date)] Removing rule: $rule"
	$ipt_cmd "$rule" 2>/dev/null || true
done
echo "[$(date)] Emergency cleanup completed"
