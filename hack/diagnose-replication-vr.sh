#!/bin/bash
# Diagnose why a VolumeReplication stays in state Unknown.
#
# Prints controller logs (replication-related lines), CSIAddonsNode driver names,
# and VolumeReplicationClass provisioners so you can see connection or driver-name
# mismatches and the actual controller error.
#
# Usage: ./hack/diagnose-replication-vr.sh [namespace [vr-name]]
#   If namespace (and optionally vr-name) are given, only log lines mentioning that VR are shown.
#   Example: ./hack/diagnose-replication-vr.sh e2e-replication-b8c5f92a vr-snapshot
#
# Requires KUBECONFIG and cluster access. Controller is expected in csi-addons-system.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
NAMESPACE="${1:-}"
VR_NAME="${2:-}"
CONTROLLER_NS="${CONTROLLER_NAMESPACE:-csi-addons-system}"
CONTROLLER_LABEL="${CONTROLLER_LABEL:-app.kubernetes.io/name=csi-addons}"

if [[ -z "${KUBECONFIG:-}" ]] && [[ -f "${HOME}/.kube/config" ]]; then
	export KUBECONFIG="${HOME}/.kube/config"
fi

if ! kubectl cluster-info &>/dev/null; then
	echo "ERROR: Cannot access cluster."
	exit 1
fi

echo "=========================================="
echo "Diagnose VolumeReplication (state Unknown)"
echo "=========================================="
echo "Context: $(kubectl config current-context 2>/dev/null || echo 'none')"
echo ""

# 1) VolumeReplicationClass provisioners (controller looks up by this name)
echo "--- VolumeReplicationClass provisioners (controller uses these to find CSIAddonsNode) ---"
if kubectl get crd volumereplicationclasses.replication.storage.openshift.io &>/dev/null; then
	kubectl get volumereplicationclass -o custom-columns="NAME:.metadata.name,PROVISIONER:.spec.provisioner" 2>/dev/null || true
else
	echo "VRC CRD not found."
fi
echo ""

# 2) CSIAddonsNode driver names (controller must find a node whose driver name matches VRC provisioner)
echo "--- CSIAddonsNode driver names (must match VRC provisioner) ---"
if kubectl get crd csiaddonsnodes.csiaddons.openshift.io &>/dev/null; then
	kubectl get csiaddonsnode -A -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name,DRIVER:.spec.driver.name,STATE:.status.state" 2>/dev/null || true
elif kubectl get crd csiaddonsnodes.csiaddons.openshift.io 2>/dev/null; then
	kubectl get csiaddonsnode -A -o wide 2>/dev/null || true
else
	echo "CSIAddonsNode CRD not found. Controller needs CSIAddonsNode to connect to the sidecar."
fi
echo ""

# 3) Controller logs (replication-related errors)
echo "--- Controller logs (replication / VolumeReplication / enable / promote) ---"
LOG_FILTER="replication|VolumeReplication|failed to enable|failed to promote|markVolumeAsPrimary|enableReplication|CSIAddonsNode|does not support VolumeReplication"
if [[ -n "$NAMESPACE" ]]; then
	LOG_FILTER="${LOG_FILTER}|${NAMESPACE}"
	[[ -n "$VR_NAME" ]] && LOG_FILTER="${LOG_FILTER}|${VR_NAME}"
fi

if kubectl get ns "$CONTROLLER_NS" &>/dev/null; then
	# Prefer deployment
	if kubectl get deployment -n "$CONTROLLER_NS" -l "$CONTROLLER_LABEL" -o name 2>/dev/null | head -1 | grep -q .; then
		POD=$(kubectl get pods -n "$CONTROLLER_NS" -l "$CONTROLLER_LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	else
		POD=$(kubectl get pods -n "$CONTROLLER_NS" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	fi
	if [[ -n "$POD" ]]; then
		kubectl logs -n "$CONTROLLER_NS" "$POD" --tail=200 2>/dev/null | grep -iE "$LOG_FILTER" || echo "(no matching log lines)"
	else
		echo "No controller pod found in $CONTROLLER_NS."
	fi
else
	echo "Namespace $CONTROLLER_NS not found. Set CONTROLLER_NAMESPACE if the controller runs elsewhere."
fi
echo ""

echo "--- Tip: VRC provisioner must exactly match CSIAddonsNode .spec.driver.name (e.g. rook-ceph.rbd.csi.ceph.com). If they differ, controller cannot find a connection and state stays Unknown. ---"
