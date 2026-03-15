#!/bin/bash
# Run CSI-Addons replication tests against a Rook-Ceph cluster.
#
# Requires KUBECONFIG and cluster access. Uses USE_EXISTING_CLUSTER=true so
# replication controller integration tests run against the real cluster.
# CRDs must be installed (e.g. deploy/controller/crds.yaml).
#
# Usage: ./hack/run-ceph-replication-addon.sh
#
# Environment variables:
#   DRY_RUN          - "true" = list tests and exit, "preview" = list then run (default),
#                      "false" = run tests directly
#   GENERATE_REPORTS - "true" to generate JUnit and JSON reports (default: "true")
#   FOCUSED_REPORTS  - "true" to create focused JUnit reports for replication tests (default: "true")
#   KEEP_FULL_REPORT - "true" to keep full JUnit report alongside focused (default: "false")
#   REPORT_DIR       - Directory for reports (default: "${REPO_ROOT}/Reports")
#   COVERAGE_DIR     - Directory for coverage files (default: "${REPO_ROOT}")
#   INSTALL_CRDS     - "true" to install CRDs before tests if missing (default: "false")
#   VERBOSE          - Verbosity (default: "1")
#
# Examples:
#   DRY_RUN=false FOCUSED_REPORTS=true ./hack/run-ceph-replication-addon.sh
#   INSTALL_CRDS=true ./hack/run-ceph-replication-addon.sh
#
# Equivalent make target (no script features):
#   make test-replication-cluster

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOGS_DIR="${REPO_ROOT}/Logs"
REPORTS_DIR="${REPORT_DIR:-${REPO_ROOT}/Reports}"
COVERAGE_DIR="${COVERAGE_DIR:-${REPO_ROOT}}"

mkdir -p "${LOGS_DIR}"
mkdir -p "${REPORTS_DIR}"

DRY_RUN="${DRY_RUN:-preview}"
GENERATE_REPORTS="${GENERATE_REPORTS:-true}"
FOCUSED_REPORTS="${FOCUSED_REPORTS:-true}"
KEEP_FULL_REPORT="${KEEP_FULL_REPORT:-false}"
INSTALL_CRDS="${INSTALL_CRDS:-false}"

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
LOG_FILE="${LOGS_DIR}/ceph-replication-addon_${TIMESTAMP}.log"

echo "=========================================="
echo "CSI-Addons Replication Test Runner"
echo "=========================================="
echo ""

# Step 1: Verify cluster access
echo "[1/5] Verifying cluster access..."

if [[ -z "${KUBECONFIG:-}" ]]; then
	if [[ -f "${HOME}/.kube/config" ]]; then
		export KUBECONFIG="${HOME}/.kube/config"
		echo "Using KUBECONFIG: ${KUBECONFIG}"
	else
		echo "WARNING: KUBECONFIG not set and ${HOME}/.kube/config not found"
		echo "Please set KUBECONFIG or ensure kubeconfig is in default location"
	fi
else
	echo "Using KUBECONFIG: ${KUBECONFIG}"
fi

if ! kubectl cluster-info &>/dev/null; then
	echo "ERROR: Cannot access Kubernetes cluster."
	echo "  1. Set KUBECONFIG correctly"
	echo "  2. Ensure kubectl can access the cluster: kubectl cluster-info"
	exit 1
fi
echo "✓ Cluster accessible"

# Step 2: Detect Ceph CSI driver (Rook-Ceph or standard)
echo "[2/5] Detecting Ceph CSI drivers..."
DRIVER_NAME=""
if kubectl get csidriver rook-ceph.rbd.csi.ceph.com &>/dev/null; then
	DRIVER_NAME="rook-ceph.rbd.csi.ceph.com"
	echo "✓ Found Rook Ceph RBD driver: ${DRIVER_NAME}"
elif kubectl get csidriver rbd.csi.ceph.com &>/dev/null; then
	DRIVER_NAME="rbd.csi.ceph.com"
	echo "✓ Found Ceph RBD driver: ${DRIVER_NAME}"
else
	echo "WARNING: No Ceph RBD CSI driver found. Continuing anyway (tests may skip driver-specific checks)."
	echo "Available drivers:"
	kubectl get csidrivers 2>/dev/null || true
fi

# Step 3: StorageClass (informational)
echo "[3/5] Detecting StorageClass..."
STORAGE_CLASS=""
if kubectl get storageclass rook-ceph-block &>/dev/null; then
	STORAGE_CLASS="rook-ceph-block"
elif kubectl get storageclass csi-rbd-sc &>/dev/null; then
	STORAGE_CLASS="csi-rbd-sc"
elif [[ -n "${DRIVER_NAME}" ]]; then
	STORAGE_CLASS=$(kubectl get storageclass -o jsonpath="{.items[?(@.provisioner==\"${DRIVER_NAME}\")].metadata.name}" 2>/dev/null | awk '{print $1}' || echo "")
fi
if [[ -n "${STORAGE_CLASS}" ]]; then
	echo "✓ Using StorageClass: ${STORAGE_CLASS}"
else
	echo "WARNING: No RBD StorageClass detected. List: $(kubectl get storageclass -o name 2>/dev/null | tr '\n' ' ' || true)"
fi

# Step 4: CRDs for replication (VolumeReplication, VolumeReplicationClass, etc.)
echo "[4/5] Checking replication CRDs..."
CRDS_YAML="${REPO_ROOT}/deploy/controller/crds.yaml"
if ! kubectl get crd volumereplications.replication.storage.openshift.io &>/dev/null; then
	if [[ "${INSTALL_CRDS}" == "true" ]]; then
		if [[ -f "${CRDS_YAML}" ]]; then
			echo "Installing CRDs from ${CRDS_YAML}..."
			kubectl apply -f "${CRDS_YAML}"
			echo "✓ CRDs installed"
		else
			echo "ERROR: deploy/controller/crds.yaml not found. Run 'make manifests' first."
			exit 1
		fi
	else
		echo "WARNING: VolumeReplication CRD not found. Controller tests may fail."
		echo "Install CRDs: kubectl apply -f deploy/controller/crds.yaml"
		echo "Or run with INSTALL_CRDS=true"
	fi
else
	echo "✓ Replication CRDs present"
fi

# Step 5: Build and run tests
echo "[5/5] Building and running replication tests..."
echo ""

cd "${REPO_ROOT}"

# Ensure envtest is available for when USE_EXISTING_CLUSTER=false (other suites)
VERBOSE="${VERBOSE:-1}"
make envtest 2>/dev/null || true

# Dry-run: list tests
if [[ "${DRY_RUN}" == "true" ]] || [[ "${DRY_RUN}" == "preview" ]]; then
	DRY_RUN_ONLY=false
	[[ "${DRY_RUN}" == "true" ]] && DRY_RUN_ONLY=true
	if [[ "${DRY_RUN_ONLY}" == "true" ]]; then
		echo "=========================================="
		echo "Dry-Run Only: Listing tests"
		echo "=========================================="
	else
		echo "=========================================="
		echo "Dry-Run Preview: Tests that will run"
		echo "=========================================="
	fi
	echo ""
	echo "Packages:"
	echo "  - ./internal/controller/replication.storage/..."
	echo "  - ./internal/client/... (volume replication client tests)"
	echo ""
	# Show package contents (no dry-run execution per project policy)
	set +e
	USE_EXISTING_CLUSTER=true go test ./internal/controller/replication.storage/... -list ".*" 2>&1 | head -60 || true
	set -e
	echo ""
	[[ "${DRY_RUN_ONLY}" == "true" ]] && exit 0
fi

# Export for controller-runtime envtest and for Ginkgo (no ANSI in logs)
export USE_EXISTING_CLUSTER=true
export GINKGO_NO_COLOR=TRUE

# Test packages: replication controller (against cluster) + client (unit)
TEST_PACKAGES="./internal/controller/replication.storage/... ./internal/client/..."

REPORT_PREFIX=""
REPORT_DIR_ABS=""
if [[ "${GENERATE_REPORTS}" == "true" ]]; then
	REPORT_PREFIX="ceph-replication-addon-${TIMESTAMP}"
	REPORT_DIR_ABS=$(cd "$(dirname "${REPORTS_DIR}")" && pwd)/$(basename "${REPORTS_DIR}")
	mkdir -p "${REPORT_DIR_ABS}"
fi

# replication.storage/... includes: (1) package controller (Ginkgo) and (2) subpackage replication (no Ginkgo).
# We must not pass -ginkgo.* to the combined run or the replication subpackage fails. So run in three steps:
# 1) Controller package only (Ginkgo) - with optional report flags; 2) replication subpackage; 3) client.
# GINKGO_NO_COLOR=TRUE (set above) disables ANSI in Ginkgo output.
REPLICATION_CONTROLLER_PKG="./internal/controller/replication.storage"
REPLICATION_SUBPKG="./internal/controller/replication.storage/replication/..."
# Only the client package has tests; internal/client/fake has no test files.
CLIENT_PKG="./internal/client"

echo "=========================================="
echo "Running Tests"
echo "=========================================="
echo "USE_EXISTING_CLUSTER: ${USE_EXISTING_CLUSTER}"
echo "Driver: ${DRIVER_NAME:-none}"
echo "StorageClass: ${STORAGE_CLASS:-none}"
echo "Reports: ${GENERATE_REPORTS}"
echo "Log: ${LOG_FILE}"
echo ""
echo "Starting at $(date)..."
echo ""

# Clean test and build caches so tests run fresh (no cached results).
cd "${REPO_ROOT}"
go clean -testcache
go clean -cache
echo "Cache cleared. Running tests..."
echo ""

# Coverage: only enable for replication packages. Client packages (client + client/fake) trigger
# "go: no such tool covdata" in some Go installations when -coverprofile is used, so we skip coverage there.
COVER_CONTROLLER="${COVERAGE_DIR}/cover_replication_controller.out"
COVER_SUBPKG="${COVERAGE_DIR}/cover_replication_subpkg.out"

# 1) Replication controller package (Ginkgo suite only) - safe to pass ginkgo flags and reports
REPLICATION_CMD="go test -v ${REPLICATION_CONTROLLER_PKG} -coverprofile=${COVER_CONTROLLER}"
if [[ "${GENERATE_REPORTS}" == "true" ]]; then
	REPLICATION_CMD="${REPLICATION_CMD} -ginkgo.junit-report=${REPORT_DIR_ABS}/${REPORT_PREFIX}-junit.xml"
	REPLICATION_CMD="${REPLICATION_CMD} -ginkgo.json-report=${REPORT_DIR_ABS}/${REPORT_PREFIX}-report.json"
fi
eval "${REPLICATION_CMD}" 2>&1 | tee "${LOG_FILE}"
EXIT_CODE=${PIPESTATUS[0]}

# 2) Replication subpackage (single package, safe to use -coverprofile)
if [[ ${EXIT_CODE} -eq 0 ]]; then
	eval "go test -v ${REPLICATION_SUBPKG} -coverprofile=${COVER_SUBPKG}" 2>&1 | tee -a "${LOG_FILE}"
	EXIT_CODE=${PIPESTATUS[0]}
fi

# 3) Client packages: run without -coverprofile to avoid "go: no such tool covdata"
if [[ ${EXIT_CODE} -eq 0 ]]; then
	eval "go test -v ${CLIENT_PKG}" 2>&1 | tee -a "${LOG_FILE}"
	EXIT_CODE=${PIPESTATUS[0]}
fi

echo ""
echo "=========================================="
echo "Test Execution Complete"
echo "=========================================="
echo "Exit Code: ${EXIT_CODE}"
echo "Log: ${LOG_FILE}"
echo "Completed at $(date)"
if [[ -f "${COVER_CONTROLLER}" ]] || [[ -f "${COVER_SUBPKG}" ]]; then
	echo ""
	echo "Coverage files:"
	[[ -f "${COVER_CONTROLLER}" ]] && echo "  ${COVER_CONTROLLER}" && go tool cover -func="${COVER_CONTROLLER}" | tail -1
	[[ -f "${COVER_SUBPKG}" ]]    && echo "  ${COVER_SUBPKG}"    && go tool cover -func="${COVER_SUBPKG}" | tail -1
fi
echo ""

# Focused JUnit report (replication-only)
create_focused_junit_report() {
	local full_junit_file="$1"
	local focused_junit_file="$2"
	if [[ ! -f "${full_junit_file}" ]]; then
		echo "WARNING: JUnit report not found: ${full_junit_file}"
		return 1
	fi
	local temp_testcases
	temp_testcases=$(mktemp)
	# Match replication-related testcase names
	sed -n '/<testcase.*name="[^"]*[Rr]eplication[^"]*"/,/<\/testcase>/p' "${full_junit_file}" > "${temp_testcases}" || true
	local total failed
	total=$(grep -c '<testcase' "${temp_testcases}" 2>/dev/null || echo "0")
	failed=$(grep -c 'status="failed"' "${temp_testcases}" 2>/dev/null || echo "0")
	local total_time
	total_time=$(grep '<testsuites' "${full_junit_file}" | sed -n 's/.*time="\([^"]*\)".*/\1/p' || echo "0")
	cat > "${focused_junit_file}" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="${total}" failures="${failed}" time="${total_time}">
    <testsuite name="CSI-Addons Replication" tests="${total}" failures="${failed}" time="${total_time}" timestamp="$(date -Iseconds)">
EOF
	cat "${temp_testcases}" >> "${focused_junit_file}"
	echo "    </testsuite>" >> "${focused_junit_file}"
	echo "</testsuites>" >> "${focused_junit_file}"
	rm -f "${temp_testcases}"
	echo "✓ Focused JUnit report: ${focused_junit_file}"
	return 0
}

ln -sf "$(basename "${LOG_FILE}")" "${LOGS_DIR}/latest.log" 2>/dev/null || true

if [[ "${GENERATE_REPORTS}" == "true" ]]; then
	echo "=========================================="
	echo "Reports"
	echo "=========================================="
	FULL_JUNIT="${REPORT_DIR_ABS}/${REPORT_PREFIX}-junit.xml"
	if [[ -f "${FULL_JUNIT}" ]] && [[ "${FOCUSED_REPORTS}" == "true" ]]; then
		FOCUSED_JUNIT="${REPORT_DIR_ABS}/${REPORT_PREFIX}-focused-junit.xml"
		if create_focused_junit_report "${FULL_JUNIT}" "${FOCUSED_JUNIT}"; then
			if [[ "${KEEP_FULL_REPORT}" == "true" ]]; then
				mv "${FULL_JUNIT}" "${REPORT_DIR_ABS}/${REPORT_PREFIX}-full-junit.xml"
				mv "${FOCUSED_JUNIT}" "${FULL_JUNIT}"
			else
				mv "${FOCUSED_JUNIT}" "${FULL_JUNIT}"
			fi
		fi
	fi
	if [[ -f "${FULL_JUNIT}" ]]; then
		echo "JUnit: ${REPORT_DIR_ABS}/${REPORT_PREFIX}-junit.xml"
		ln -sf "${REPORT_PREFIX}-junit.xml" "${REPORT_DIR_ABS}/latest-junit.xml" 2>/dev/null || true
	fi
	if [[ -f "${REPORT_DIR_ABS}/${REPORT_PREFIX}-report.json" ]]; then
		echo "JSON: ${REPORT_DIR_ABS}/${REPORT_PREFIX}-report.json"
		ln -sf "${REPORT_PREFIX}-report.json" "${REPORT_DIR_ABS}/latest-report.json" 2>/dev/null || true
	fi
fi

if [[ ${EXIT_CODE} -eq 0 ]]; then
	echo "✓ Tests completed successfully"
else
	echo "✗ Tests failed. See log: ${LOG_FILE}"
fi
exit ${EXIT_CODE}
