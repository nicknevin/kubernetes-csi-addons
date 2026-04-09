#!/bin/bash
# Print the Events section from `kubectl describe daemonset` for the iptables
# fault-injection manager on DR1 and DR2 kube contexts (full-DR replication E2E).
#
# Usage: ./hack/show-e2e-iptables-fence-events.sh
#
# Environment (same defaults as test/e2e/helpers/iptables_provider.go and preload scripts):
#   E2E_IPTABLES_DAEMONSET_NAMESPACE - DaemonSet namespace (default: csi-addons-system)
#   E2E_IPTABLES_DAEMONSET_NAME      - DaemonSet name (default: csi-addons-iptables-manager)
#   DR1_CONTEXT                      - Kube context for DR1 (default: dr1)
#   DR2_CONTEXT                      - Kube context for DR2 (default: dr2)
#
# Typically invoked via: make show-e2e-iptables-fence-events

set -euo pipefail

NS="${E2E_IPTABLES_DAEMONSET_NAMESPACE:-csi-addons-system}"
DS="${E2E_IPTABLES_DAEMONSET_NAME:-csi-addons-iptables-manager}"
CTX_DR1="${DR1_CONTEXT:-dr1}"
CTX_DR2="${DR2_CONTEXT:-dr2}"

show_ds_events() {
	local label="$1"
	local ctx="$2"
	echo "================================================================================"
	echo "### ${label} — Events from: kubectl --context ${ctx} describe daemonset -n ${NS} ${DS}"
	echo "================================================================================"
	local out rc
	out="$(kubectl --context "${ctx}" describe daemonset -n "${NS}" "${DS}" 2>&1)" && rc=0 || rc=$?
	if [ "${rc}" -ne 0 ]; then
		echo "(kubectl describe failed for context ${ctx}, exit ${rc})"
		echo "${out}"
	else
		echo "${out}" | sed -n '/^Events:/,$p'
	fi
	echo ""
}

echo "=== E2E iptables fence visibility (namespace=${NS}, DaemonSet=${DS}) ==="
echo "Contexts: DR1=${CTX_DR1}  DR2=${CTX_DR2}"
echo ""

show_ds_events DR1 "${CTX_DR1}"
show_ds_events DR2 "${CTX_DR2}"
