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
#   STORAGE_CLASS        - StorageClass name for PVCs (default: auto-detect or "rook-ceph-block")
#   CSI_PROVISIONER      - Must match CSIAddonsNode .spec.driver.name (default: "rook-ceph.rbd.csi.ceph.com").
#                          If state stays Unknown, run ./hack/diagnose-replication-vr.sh and set this.
#   REPLICATION_SECRET_NAME, REPLICATION_SECRET_NAMESPACE - If both set, use this existing secret
#                          for VolumeReplicationClass (e.g. rook-csi-rbd-provisioner in rook-ceph).
#   REPLICATION_POLL_TIMEOUT - Seconds to wait for Replicating=True (default 300). Increase if
#                          journal mode or second VR times out.
#   REPLICATION_TEST_TIMEOUT - Go test timeout for entire suite (default 30m). Increase if suite
#                          hits "test timed out after 10m0s" (e.g. 45m or 60m).
#   GINKGO_VERBOSE          - Set to "vv" for maximal verbosity (skipped/pending in output).
#                             Default: "v" (verbose) plus show-node-events and trace on failure.
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
echo "  STORAGE_CLASS=${STORAGE_CLASS:-<auto>}"
echo "  CSI_PROVISIONER=${CSI_PROVISIONER:-<auto>}"
echo "  DR1_CONTEXT=${DR1_CONTEXT:-<unset>}"
echo "  DR2_CONTEXT=${DR2_CONTEXT:-<unset>}"
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

echo "[4/5] Running replication E2E tests (timeout ${REPLICATION_TEST_TIMEOUT}, output tee'd to ${LOG_FILE})..."
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

echo "[4/5] Ginkgo options: ${GINKGO_EXTRA} ${GINKGO_FOCUS_FLAG[*]} ${GINKGO_SKIP_FLAG[*]}"
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
echo "[5/5] Done. Log file: ${LOG_FILE}"
echo "      (Detailed summary and per-spec results are included in the log above.)"
exit "${EXIT_CODE}"
