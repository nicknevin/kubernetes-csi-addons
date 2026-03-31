#!/bin/bash
# Run the replication E2E test suite against an existing cluster.
#
# The suite lives under test/e2e/replication and implements Layer-1 VR scenarios
# (EnableVolumeReplication, GetVolumeReplicationInfo). Requires KUBECONFIG and
# cluster access. CRDs must be installed. CSI-Addons controller and a CSI driver
# with replication support (e.g. Ceph RBD) must be running.
#
# Usage: ./hack/run-replication-e2e.sh
#
# Environment variables (passed through to tests when set before make/script):
#   GINKGO_FOCUS        - Ginkgo focus expression to run only matching tests (default: run all).
#                          Examples: "L1-E-001", "EnableVolumeReplication", "GetVolumeReplicationInfo"
#   INSTALL_CRDS         - "true" to install CRDs before tests if missing (default: "false")
#   STORAGE_CLASS        - StorageClass name for PVCs (default: "rook-ceph-block"; not auto-detected).
#   CSI_PROVISIONER      - Must match CSIAddonsNode .spec.driver.name (default: "rook-ceph.rbd.csi.ceph.com"; not auto-detected).
#                          If state stays Unknown, run ./hack/diagnose-replication-vr.sh and set this.
#   REPLICATION_SECRET_NAME, REPLICATION_SECRET_NAMESPACE - If both set, use this existing secret
#                          for VolumeReplicationClass (e.g. rook-csi-rbd-provisioner in rook-ceph).
#   REPLICATION_POLL_TIMEOUT - Seconds to wait for Replicating=True (default 300). Increase if
#                          journal mode or second VR times out.
#   REPLICATION_TEST_TIMEOUT - Go test timeout for entire suite (default 30m). Increase if suite
#                          hits "test timed out after 10m0s" (e.g. 45m or 60m).
#   GINKGO_VERBOSE          - Set to "vv" for maximal verbosity (skipped/pending in output).
#                             Default: "v" (verbose) plus show-node-events and trace on failure.
#   IPTABLES_IMAGE          - Container image for iptables fault injection (default: alpine:3.19).
#                             Used by dual-cluster tests with network fault injection.
#
# Examples:
#   ./hack/run-replication-e2e.sh
#   GINKGO_FOCUS="L1-E-001" ./hack/run-replication-e2e.sh
#   REPLICATION_SECRET_NAME=rook-csi-rbd-provisioner REPLICATION_SECRET_NAMESPACE=rook-ceph ./hack/run-replication-e2e.sh
#
# Equivalent make target:
#   make test-replication-e2e
#   make test-replication-e2e GINKGO_FOCUS="L1-E-001"

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOGS_DIR="${REPO_ROOT}/Logs"
E2E_PKG="./test/e2e/replication/..."
CLEANUP_SCRIPT="${SCRIPT_DIR}/clean-replication-e2e-resources.sh"

mkdir -p "${LOGS_DIR}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
LOG_FILE="${LOGS_DIR}/replication-e2e_${TIMESTAMP}.log"
INSTALL_CRDS="${INSTALL_CRDS:-false}"
REPLICATION_TEST_TIMEOUT="${REPLICATION_TEST_TIMEOUT:-30m}"
GINKGO_VERBOSE="${GINKGO_VERBOSE:-v}"
EXIT_CODE=1
CLEANUP_ON_EXIT=0

# Run cleanup on exit (success, failure, or panic/timeout) so PVCs/VRs are not left behind.
run_cleanup_on_exit() {
	if [[ "$CLEANUP_ON_EXIT" == "1" ]] && [[ -x "$CLEANUP_SCRIPT" ]]; then
		echo ""
		echo "Cleaning up e2e resources (VRs, PVCs, test VRCs, namespaces)..."
		"$CLEANUP_SCRIPT" || true
	fi
}
trap 'run_cleanup_on_exit; exit ${EXIT_CODE:-1}' EXIT

echo "=========================================="
echo "Replication E2E Test Suite"
echo "=========================================="
echo ""

echo "[1/5] Verifying cluster access..."
if [[ -z "${KUBECONFIG:-}" ]]; then
	if [[ -f "${HOME}/.kube/config" ]]; then
		export KUBECONFIG="${HOME}/.kube/config"
		echo "Using KUBECONFIG: ${KUBECONFIG}"
	else
		echo "ERROR: KUBECONFIG not set and ${HOME}/.kube/config not found"
		exit 1
	fi
else
	echo "Using KUBECONFIG: ${KUBECONFIG}"
fi

if ! kubectl cluster-info &>/dev/null; then
	echo "ERROR: Cannot access cluster. Set KUBECONFIG and ensure kubectl works."
	exit 1
fi
CLEANUP_ON_EXIT=1

echo "Current context: $(kubectl config current-context 2>/dev/null || echo 'none')"
echo ""

echo "[2/5] Test env (pass-through to tests):"
echo "  REPLICATION_TEST_TIMEOUT=${REPLICATION_TEST_TIMEOUT:-30m} (go test -timeout)"
echo "  REPLICATION_POLL_TIMEOUT=${REPLICATION_POLL_TIMEOUT:-<default 300>}"
echo "  REPLICATION_SECRET_NAME=${REPLICATION_SECRET_NAME:-<unset, create per-ns secret>}"
echo "  REPLICATION_SECRET_NAMESPACE=${REPLICATION_SECRET_NAMESPACE:-<unset>}"
echo "  STORAGE_CLASS=${STORAGE_CLASS:-rook-ceph-block}"
echo "  CSI_PROVISIONER=${CSI_PROVISIONER:-rook-ceph.rbd.csi.ceph.com}"
echo "  DR1_CONTEXT=${DR1_CONTEXT:-<unset>}"
echo "  DR2_CONTEXT=${DR2_CONTEXT:-<unset>}"
echo "  IPTABLES_IMAGE=${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"
echo "  GINKGO_FOCUS=${GINKGO_FOCUS:-<all>}"
echo ""

echo "[3/5] Checking VolumeReplication CRD..."
if ! kubectl get crd volumereplications.replication.storage.openshift.io &>/dev/null; then
	if [[ "${INSTALL_CRDS}" == "true" ]]; then
		echo "Installing CRDs from deploy/controller/crds.yaml..."
		kubectl apply -f "${REPO_ROOT}/deploy/controller/crds.yaml"
	else
		echo "ERROR: VolumeReplication CRD not found. Install with: kubectl apply -f deploy/controller/crds.yaml"
		echo "       Or run with INSTALL_CRDS=true"
		exit 1
	fi
else
	echo "VolumeReplication CRD present"
fi
echo ""

echo "[3.5/6] Preparing iptables image for fault injection..."

# Simple function to load image to cluster
load_image_to_cluster() {
	local context="$1"
	local image="$2"
	
	echo "  [DEBUG] Attempting to load image '$image' to context '$context'"
	
	# Test if image is accessible first
	local test_pod="image-test-$(date +%s)"
	echo "  [DEBUG] Testing image accessibility in context '$context'"
	if kubectl --context="$context" run "$test_pod" --image="$image" --restart=Never --rm --timeout=30s --command -- echo "ok" >/dev/null 2>&1; then
		echo "Image $image already accessible in $context"
		return 0
	fi
	echo "  [DEBUG] Image not yet accessible in context, attempting to load..."
	
	# Try to load for kind clusters
	if command -v kind >/dev/null 2>&1; then
		local cluster_name="$context"
		[[ "$context" =~ kind- ]] && cluster_name="$(echo "$context" | sed 's/kind-//')"
		
		if kind get clusters 2>/dev/null | grep -q "^${cluster_name}$"; then
			echo "Loading via kind to cluster: $cluster_name"
			kind load docker-image "$image" --name="$cluster_name" && return 0
		fi
	fi
	
	# Try to load for k3d clusters
	if command -v k3d >/dev/null 2>&1; then
		local cluster_name="$context"
		[[ "$context" =~ k3d- ]] && cluster_name="$(echo "$context" | sed 's/k3d-//')"
		
		if k3d cluster list 2>/dev/null | grep -q "$cluster_name"; then
			echo "Loading via k3d to cluster: $cluster_name"
			k3d image import "$image" --cluster="$cluster_name" && return 0
		fi
	fi
	
	# Try to load for minikube clusters
	if command -v minikube >/dev/null 2>&1; then
		local cluster_name="$context"
		# Extract cluster name from context (remove minikube- prefix if present)
		[[ "$context" =~ minikube- ]] && cluster_name="$(echo "$context" | sed 's/minikube-//')"
		
		echo "  [DEBUG] Checking for minikube profile: '$cluster_name'"
		if minikube profile list 2>/dev/null | grep -q "^${cluster_name}"; then
			echo "Loading via minikube to cluster: $cluster_name"
			# Detect container runtime (podman/docker) for save command
			if command -v podman >/dev/null 2>&1; then
				CONTAINER_CMD="podman"
			elif command -v docker >/dev/null 2>&1; then
				CONTAINER_CMD="docker"
			else
				echo "  [DEBUG] Neither podman nor docker found, cannot load image"
				return 1
			fi
			echo "  [DEBUG] Running: $CONTAINER_CMD save '$image' | minikube image load --profile='$cluster_name' -"
			$CONTAINER_CMD save "$image" | minikube image load --profile="$cluster_name" - && return 0
		else
			echo "  [DEBUG] Minikube profile '$cluster_name' not found in profile list"
		fi
	fi
	
	echo "Cannot load $image to $context (not kind/k3d/minikube or image not in registry)"
	return 1
}

prepare_iptables_image() {
	local iptables_image="${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"
	
	# Use pre-built iptables image with all tools included
	# No fallback to alpine - the custom image has everything needed
	echo "  Using pre-built iptables image: $iptables_image"
	
	# Only attempt to load image to clusters if DR contexts are set (dual-cluster testing)
	if [[ -n "${DR1_CONTEXT:-}" && -n "${DR2_CONTEXT:-}" ]]; then
		echo "  Detected dual-cluster setup (DR1_CONTEXT=${DR1_CONTEXT}, DR2_CONTEXT=${DR2_CONTEXT})"
		
		# Detect container command
		if command -v podman > /dev/null 2>&1; then
			CONTAINER_CMD="podman"
		elif command -v docker > /dev/null 2>&1; then
			CONTAINER_CMD="docker"
		else
			echo "  WARNING: Neither podman nor docker found, skipping pre-cluster image loading"
			echo "  (Image should already be available in clusters)"
		fi
		
		# Normalize image name - remove localhost/ prefix if present (podman may add it)
		iptables_image="${iptables_image#localhost/}"
		echo "  Using normalized image: $iptables_image"
		
		# Build custom iptables image if needed
		if [[ "$iptables_image" == "csi-addons/iptables-manager:latest" ]]; then
			echo "  Checking if custom iptables image needs to be built..."
			if ! $CONTAINER_CMD images --format "table {{.Repository}}:{{.Tag}}" | grep -E "(^|/)csi-addons/iptables-manager:latest\$" >/dev/null 2>&1; then
				echo "  Building custom iptables image..."
				if [[ -f "${REPO_ROOT}/build/Containerfile.iptables" ]]; then
					if $CONTAINER_CMD build -t "csi-addons/iptables-manager:latest" -f "${REPO_ROOT}/build/Containerfile.iptables" "${REPO_ROOT}/build/" >/dev/null 2>&1; then
						echo "  ✓ Successfully built custom iptables image"
					else
						echo "  WARNING: Failed to build custom iptables image, will attempt to use existing"
					fi
				else
					echo "  WARNING: Containerfile.iptables not found, will attempt to use existing image"
				fi
			else
				echo "  ✓ Custom iptables image already exists locally"
			fi
		fi
		
		echo "  Attempting to load image $iptables_image to DR clusters..."
		
		# Load to DR1 cluster (non-fatal if fails - image may already be there)
		echo "    Loading to DR1 cluster ($DR1_CONTEXT)..."
		if load_image_to_cluster "$DR1_CONTEXT" "$iptables_image"; then
			echo "    ✓ Successfully loaded to DR1"
		else
			echo "    ℹ Image not pre-loaded to DR1 (will pull from registry if available)"
		fi
		
		# Load to DR2 cluster (non-fatal if fails - image may already be there)
		echo "    Loading to DR2 cluster ($DR2_CONTEXT)..."
		if load_image_to_cluster "$DR2_CONTEXT" "$iptables_image"; then
			echo "    ✓ Successfully loaded to DR2"
		else
			echo "    ℹ Image not pre-loaded to DR2 (will pull from registry if available)"
		fi
		
		echo "  ✓ Ready to use pre-built iptables image in clusters"
	else
		echo "  Single-cluster mode: skipping pre-cluster image load"
	fi
	
	# Always export the pre-built image (no alpine fallback)
	export E2E_IPTABLES_IMAGE="$iptables_image"
}

# Call the image preparation function
prepare_iptables_image
echo ""

echo "[4/6] Running replication E2E tests (timeout ${REPLICATION_TEST_TIMEOUT}, output tee'd to ${LOG_FILE})..."
echo "  Use REPLICATION_POLL_TIMEOUT=600 if Replicating=True times out."
echo "  Use REPLICATION_TEST_TIMEOUT=45m or 60m if suite hits test timeout."
echo ""

cd "${REPO_ROOT}"
# Disable Ginkgo color so log files and CI have plain text (no ANSI codes).
export GINKGO_NO_COLOR=TRUE

# Ginkgo flags for verbose logs: test case names, progress, and detailed summary (see suite_test.go ReportAfterSuite).
# GINKGO_VERBOSE defaults to "v" (verbose + trace + show-node-events). Set to "vv" for maximal verbosity.
if [[ "${GINKGO_VERBOSE}" == "vv" ]]; then
	GINKGO_EXTRA="--ginkgo.vv"
else
	GINKGO_EXTRA="--ginkgo.v --ginkgo.trace --ginkgo.show-node-events"
fi

# Pass focus/skip to Ginkgo when set (e.g. GINKGO_FOCUS="L1-E-001" to run only matching specs).
GINKGO_FOCUS_FLAG=()
if [[ -n "${GINKGO_FOCUS:-}" ]]; then
	GINKGO_FOCUS_FLAG=("--ginkgo.focus=${GINKGO_FOCUS}")
fi
GINKGO_SKIP_FLAG=()
if [[ -n "${GINKGO_SKIP:-}" ]]; then
	GINKGO_SKIP_FLAG=("--ginkgo.skip=${GINKGO_SKIP}")
fi

echo "[4/6] Ginkgo options: ${GINKGO_EXTRA} ${GINKGO_FOCUS_FLAG[*]} ${GINKGO_SKIP_FLAG[*]}"
echo ""

# Run tests with extended timeout (default 30m); cleanup runs on EXIT trap even on panic/timeout.
set +e
if command -v stdbuf &>/dev/null; then
	USE_EXISTING_CLUSTER=true stdbuf -oL go test -v -timeout "${REPLICATION_TEST_TIMEOUT}" "${E2E_PKG}" ${GINKGO_EXTRA} "${GINKGO_FOCUS_FLAG[@]}" "${GINKGO_SKIP_FLAG[@]}" 2>&1 | tee "${LOG_FILE}"
else
	USE_EXISTING_CLUSTER=true go test -v -timeout "${REPLICATION_TEST_TIMEOUT}" "${E2E_PKG}" ${GINKGO_EXTRA} "${GINKGO_FOCUS_FLAG[@]}" "${GINKGO_SKIP_FLAG[@]}" 2>&1 | tee "${LOG_FILE}"
fi
EXIT_CODE="${PIPESTATUS[0]}"
set -e

echo ""
echo "[6/6] Done. Log file: ${LOG_FILE}"
echo "      (Detailed summary and per-spec results are included in the log above.)"
exit "${EXIT_CODE}"
