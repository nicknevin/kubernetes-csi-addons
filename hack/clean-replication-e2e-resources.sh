#!/bin/bash
# Clean up leftover resources from replication E2E test runs.
#
# Deletes in order: VolumeReplications (after removing finalizers), PVCs (after
# removing finalizers), VolumeSnapshots (if CRD exists), e2e-replication-* namespaces,
# and test-created VolumeReplicationClasses. Run this before a fresh E2E run if
# previous runs left resources stuck (e.g. Terminating).
#
# Usage: ./hack/clean-replication-e2e-resources.sh [--dry-run] [--contexts <context1,context2,...>]
#
# Environment variables:
#   KUBECONFIG - Same as kubectl (default: $HOME/.kube/config)
#
# Examples:
#   ./hack/clean-replication-e2e-resources.sh
#   ./hack/clean-replication-e2e-resources.sh --dry-run
#   ./hack/clean-replication-e2e-resources.sh --contexts DR1,DR2

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DRY_RUN=false
CONTEXTS=()

for arg in "$@"; do
	case "$arg" in
		--dry-run) DRY_RUN=true ;;
		--contexts) 
			shift
			IFS=',' read -ra CONTEXTS <<< "$1"
			;;
		*) echo "Unknown option: $arg"; echo "Usage: $0 [--dry-run] [--contexts <context1,context2,...>]"; exit 1 ;;
	esac
done

# If no contexts specified, auto-detect DR1 and DR2
if [[ ${#CONTEXTS[@]} -eq 0 ]]; then
	mapfile -t AVAILABLE_CONTEXTS < <(kubectl config get-contexts -o name 2>/dev/null || echo "")
	for ctx in "${AVAILABLE_CONTEXTS[@]}"; do
		if [[ "$ctx" == *"DR1"* ]] || [[ "$ctx" == *"DR2"* ]] || [[ "$ctx" == *"dr1"* ]] || [[ "$ctx" == *"dr2"* ]]; then
			CONTEXTS+=("$ctx")
		fi
	done
	# If no DR contexts found, use current context
	if [[ ${#CONTEXTS[@]} -eq 0 ]]; then
		CURRENT_CONTEXT=$(kubectl config current-context 2>/dev/null || echo "")
		if [[ -n "$CURRENT_CONTEXT" ]]; then
			CONTEXTS=("$CURRENT_CONTEXT")
		fi
	fi
fi

if [[ -z "${KUBECONFIG:-}" ]] && [[ -f "${HOME}/.kube/config" ]]; then
	export KUBECONFIG="${HOME}/.kube/config"
fi

if ! kubectl cluster-info &>/dev/null; then
	echo "ERROR: Cannot access cluster. Set KUBECONFIG and ensure kubectl works."
	exit 1
fi

if [[ ${#CONTEXTS[@]} -eq 0 ]]; then
	echo "ERROR: No Kubernetes contexts found to clean."
	exit 1
fi


# Helper to run kubectl in a specific context
kubectl_ctx() {
	local ctx="$1"
	shift
	kubectl --context="$ctx" "$@"
}

# Remove finalizer from a VR so it can be deleted
remove_vr_finalizer() {
	local ctx="$1"
	local ns="$2"
	local name="$3"
	if [[ "$DRY_RUN" == "true" ]]; then
		echo "  [dry-run] would patch VR $ns/$name to remove finalizer"
		return 0
	fi
	kubectl_ctx "$ctx" patch vr -n "$ns" "$name" --type=json -p='[{"op": "replace", "path": "/metadata/finalizers", "value": []}]' 2>/dev/null || true
}

# Remove finalizer from a PVC so it can be deleted
remove_pvc_finalizer() {
	local ctx="$1"
	local ns="$2"
	local name="$3"
	if [[ "$DRY_RUN" == "true" ]]; then
		echo "  [dry-run] would patch PVC $ns/$name to remove finalizer"
		return 0
	fi
	kubectl_ctx "$ctx" patch pvc -n "$ns" "$name" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
}

echo "=========================================="
echo "Clean replication E2E resources"
echo "=========================================="
[[ "$DRY_RUN" == "true" ]] && echo "(dry-run: no changes will be made)"
echo "Cleaning ${#CONTEXTS[@]} context(s): ${CONTEXTS[*]}"
echo ""

for CURRENT_CTX in "${CONTEXTS[@]}"; do
	echo "Processing context: $CURRENT_CTX"
	echo "=========================================="
	
	# 1) Namespaces matching e2e-replication-*
	mapfile -t NAMESPACES < <(kubectl_ctx "$CURRENT_CTX" get namespaces -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -E '^e2e-replication-' || true)
	if [[ ${#NAMESPACES[@]} -eq 0 ]]; then
		echo "No e2e-replication-* namespaces found."
	else
		echo "Found ${#NAMESPACES[@]} e2e-replication namespace(s): ${NAMESPACES[*]}"
		for ns in "${NAMESPACES[@]}"; do
			echo "  Cleaning namespace: $ns"
			# VolumeReplications: remove finalizers then delete (skip if CRD not present)
			if kubectl_ctx "$CURRENT_CTX" get crd volumereplications.replication.storage.openshift.io &>/dev/null; then
				for n in $(kubectl_ctx "$CURRENT_CTX" get vr -n "$ns" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
					remove_vr_finalizer "$CURRENT_CTX" "$ns" "$n"
					[[ "$DRY_RUN" != "true" ]] && kubectl_ctx "$CURRENT_CTX" delete vr -n "$ns" "$n" --ignore-not-found --timeout=15s 2>/dev/null || true
				done
			fi

			# PVCs: remove finalizers then delete
			for pvc in $(kubectl_ctx "$CURRENT_CTX" get pvc -n "$ns" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
				remove_pvc_finalizer "$CURRENT_CTX" "$ns" "$pvc"
				[[ "$DRY_RUN" != "true" ]] && kubectl_ctx "$CURRENT_CTX" delete pvc -n "$ns" "$pvc" --ignore-not-found --timeout=15s 2>/dev/null || true
			done

			# VolumeSnapshots (if CRD exists)
			if kubectl_ctx "$CURRENT_CTX" get crd volumesnapshots.snapshot.storage.k8s.io &>/dev/null; then
				for vs in $(kubectl_ctx "$CURRENT_CTX" get volumesnapshot -n "$ns" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
					if [[ "$DRY_RUN" == "true" ]]; then
						echo "  [dry-run] would delete VolumeSnapshot $ns/$vs"
					else
						kubectl_ctx "$CURRENT_CTX" delete volumesnapshot -n "$ns" "$vs" --ignore-not-found --timeout=10s 2>/dev/null || true
					fi
				done
			fi

			# Delete namespace
			if [[ "$DRY_RUN" == "true" ]]; then
				echo "  [dry-run] would delete namespace $ns"
			else
				kubectl_ctx "$CURRENT_CTX" delete namespace "$ns" --ignore-not-found --timeout=60s 2>/dev/null || true
			fi
		done
	fi

	# 2) VolumeReplicationClasses created by tests (name prefix matches)
	if kubectl_ctx "$CURRENT_CTX" get crd volumereplicationclasses.replication.storage.openshift.io &>/dev/null; then
		echo ""
		echo "Cleaning test VolumeReplicationClasses..."
		for vrc in $(kubectl_ctx "$CURRENT_CTX" get volumereplicationclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
			if [[ "$vrc" == vrc-snapshot-* ]] || [[ "$vrc" == vrc-journal-* ]] || [[ "$vrc" == vrc-idem-* ]] || [[ "$vrc" == vrc-info-* ]] || [[ "$vrc" == vrc-nonexist-* ]] || [[ "$vrc" == vrc-fence-* ]]; then
				if [[ "$DRY_RUN" == "true" ]]; then
					echo "  [dry-run] would delete VRC $vrc"
				else
					kubectl_ctx "$CURRENT_CTX" delete volumereplicationclass "$vrc" --ignore-not-found --timeout=10s 2>/dev/null || true
					echo "  Deleted VRC $vrc"
				fi
			fi
		done
	fi

	# 3) NetworkFence and NetworkFenceClass created by L1-E-003 and L1-PROM/DEM-003/004 tests
	remove_networkfence_finalizer() {
		local name="$1"
		if [[ "$DRY_RUN" == "true" ]]; then
			echo "  [dry-run] would remove finalizer from NetworkFence $name"
			return 0
		fi
		kubectl_ctx "$CURRENT_CTX" patch networkfence "$name" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
	}

	remove_networkfenceclass_finalizer() {
		local name="$1"
		if [[ "$DRY_RUN" == "true" ]]; then
			echo "  [dry-run] would remove finalizer from NetworkFenceClass $name"
			return 0
		fi
		kubectl_ctx "$CURRENT_CTX" patch networkfenceclass "$name" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
	}

	# Unfence a NetworkFence by setting fenceState to Unfenced
	unfence_networkfence() {
		local name="$1"
		if [[ "$DRY_RUN" == "true" ]]; then
			echo "  [dry-run] would unfence NetworkFence $name"
			return 0
		fi
		kubectl_ctx "$CURRENT_CTX" patch networkfence "$name" -p '{"spec":{"fenceState":"Unfenced"}}' --type=merge 2>/dev/null || true
	}

	# Wait for NetworkFence to report Succeeded result (after unfencing)
	wait_for_networkfence_unfence_success() {
		local name="$1"
		local timeout=30
		local waited=0
		while [[ $waited -lt $timeout ]]; do
			local result=$(kubectl_ctx "$CURRENT_CTX" get networkfence "$name" -o jsonpath='{.status.result}' 2>/dev/null || echo "")
			if [[ "$result" == "Succeeded" ]]; then
				return 0
			fi
			sleep 2
			waited=$((waited + 2))
		done
		return 1
	}

	if kubectl_ctx "$CURRENT_CTX" get crd networkfences.csiaddons.openshift.io &>/dev/null; then
		echo ""
		echo "Cleaning test NetworkFences..."
		for nf in $(kubectl_ctx "$CURRENT_CTX" get networkfence -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
			# Match test patterns: nf-fence-*, nf-e2e-replication-*, nf-prom-*-e2e-replication-*, nf-dem-*-e2e-replication-*
			if [[ "$nf" == nf-fence-* ]] || [[ "$nf" == nf-e2e-replication-* ]] || [[ "$nf" == nf-prom-*-e2e-replication-* ]] || [[ "$nf" == nf-dem-*-e2e-replication-* ]]; then
				if [[ "$DRY_RUN" == "true" ]]; then
					echo "  [dry-run] would unfence and delete NetworkFence $nf"
				else
					# Unfence first (set fenceState to Unfenced) before deletion
					echo "  Unfencing NetworkFence $nf..."
					unfence_networkfence "$nf"
					if ! wait_for_networkfence_unfence_success "$nf"; then
						echo "  Warning: unfence did not complete successfully for $nf, proceeding with deletion"
					fi
					
					# Delete the NetworkFence
					kubectl_ctx "$CURRENT_CTX" delete networkfence "$nf" --ignore-not-found --timeout=30s 2>/dev/null || true
					# Remove finalizer if still present (e.g. stuck in Terminating)
					if kubectl_ctx "$CURRENT_CTX" get networkfence "$nf" &>/dev/null; then
						echo "  NetworkFence $nf stuck, removing finalizer..."
						remove_networkfence_finalizer "$nf"
						kubectl_ctx "$CURRENT_CTX" delete networkfence "$nf" --ignore-not-found --timeout=15s 2>/dev/null || true
					fi
					echo "  Deleted NetworkFence $nf"
				fi
			fi
		done
	fi
	if kubectl_ctx "$CURRENT_CTX" get crd networkfenceclasses.csiaddons.openshift.io &>/dev/null; then
		echo "Cleaning test NetworkFenceClasses..."
		for nfc in $(kubectl_ctx "$CURRENT_CTX" get networkfenceclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
			# Match test patterns: nfc-fence-*, nfc-e2e-replication-*, nfc-prom-*-e2e-replication-*, nfc-dem-*-e2e-replication-*
			if [[ "$nfc" == nfc-fence-* ]] || [[ "$nfc" == nfc-e2e-replication-* ]] || [[ "$nfc" == nfc-prom-*-e2e-replication-* ]] || [[ "$nfc" == nfc-dem-*-e2e-replication-* ]]; then
				if [[ "$DRY_RUN" == "true" ]]; then
					echo "  [dry-run] would delete NetworkFenceClass $nfc"
				else
					echo "  Deleting NetworkFenceClass $nfc..."
					# Delete the NetworkFenceClass
					kubectl_ctx "$CURRENT_CTX" delete networkfenceclass "$nfc" --ignore-not-found --timeout=30s 2>/dev/null || true
					# Remove finalizer if still present (e.g. stuck in Terminating)
					if kubectl_ctx "$CURRENT_CTX" get networkfenceclass "$nfc" &>/dev/null; then
						echo "  NetworkFenceClass $nfc stuck, removing finalizer..."
						remove_networkfenceclass_finalizer "$nfc"
						kubectl_ctx "$CURRENT_CTX" delete networkfenceclass "$nfc" --ignore-not-found --timeout=15s 2>/dev/null || true
					fi
					echo "  Deleted NetworkFenceClass $nfc"
				fi
			fi
		done
	fi
	
	echo ""
done

echo "Done."
exit 0
