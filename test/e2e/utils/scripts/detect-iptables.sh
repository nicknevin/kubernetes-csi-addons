#!/bin/bash
# Auto-detect the best iptables command for this environment
# Prefer iptables-legacy in environments with nf_tables compatibility issues

detect_iptables_cmd() {
	for iptables_variant in iptables-legacy iptables ip6tables; do
		if command -v "$iptables_variant" >/dev/null 2>&1; then
			# Test if we can actually use the command and check for compatibility issues
			local test_output
			test_output=$("$iptables_variant" -L OUTPUT -n 2>&1)
			local exit_code=$?

			# Check for nf_tables compatibility issues
			if echo "$test_output" | grep -q "Could not fetch rule set generation id"; then
				echo "[$(date)] WARNING: $iptables_variant has nf_tables compatibility issues, trying next..." >&2
				continue
			fi

			# Accept if it works or gives expected permission errors
			if [ $exit_code -eq 0 ] || echo "$test_output" | grep -q "Permission denied\|you must be root"; then
				echo "$iptables_variant"
				return 0
			fi
		fi
	done
	echo ""
	return 1
}

detect_iptables_cmd
