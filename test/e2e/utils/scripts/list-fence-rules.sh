#!/bin/bash
set -euo pipefail

ipt_cmd=$(detect-iptables)

if [ -z "$ipt_cmd" ]; then
    echo "[$(date)] ERROR: No working iptables command found"
    exit 1
fi

echo "[$(date)] Current fence rules using $ipt_cmd:"
$ipt_cmd -L OUTPUT -n --line-numbers | grep REJECT || echo "No fence rules found"