#!/bin/bash

# Validates iptables-based fencing using a real in-cluster target by default:
# discovers a Service or Pod IP (kube-dns / CoreDNS / kubernetes API), proves reachability,
# fences it, asserts iptables REJECT + datapath blocked, then unfences and asserts restore.

set -euo pipefail

# Default configuration
DR1_CONTEXT="${DR1_CONTEXT:-dr1}"
DR2_CONTEXT="${DR2_CONTEXT:-dr2}"
TEST_NAMESPACE="${TEST_NAMESPACE:-e2e-iptables-test}"
IPTABLES_IMAGE="${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"
# If empty, the script auto-discovers a reachable in-cluster IP (preferred).
# Override manually: VALIDATE_FENCE_TARGET=10.96.0.10/32
VALIDATE_FENCE_TARGET="${VALIDATE_FENCE_TARGET:-}"

# Last measure_probes() result (0 = probe worked, 1 = failed) — overwritten each call
BASELINE_DIG_OK=1
BASELINE_PING_OK=1
BASELINE_HTTPS_OK=1
# Snapshot taken once after "BEFORE FENCING" — used for strict block/restore asserts (must not be overwritten)
PRE_FENCE_DIG_OK=1
PRE_FENCE_PING_OK=1
PRE_FENCE_HTTPS_OK=1

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
	echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
	echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
	echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
	echo -e "${RED}[ERROR]${NC} $1"
}

# --- Probe helpers (run inside iptables DaemonSet pod via kubectl exec) ---
#
# kubectl exec forwards the *remote* command's exit code but also prints a line like
#   "command terminated with exit code N"
# to *kubectl's* stderr whenever N≠0. That is normal for failed probes and is confusing in logs.
# Probe kubectl invocations therefore use 2>/dev/null on kubectl only; remote exit codes are unchanged.
#
# Typical remote exit codes: ping non-zero = no reply; dig non-zero = query failed;
# curl 7 = failed to connect, 28 = operation timeout (see curl(1)).

# measure_probes pod ip → sets BASELINE_*_OK globals; prints summary
measure_probes() {
	local pod_name="$1"
	local ip="$2"
	local dig_out ping_rc dig_rc curl_rc

	BASELINE_DIG_OK=1
	BASELINE_PING_OK=1
	BASELINE_HTTPS_OK=1

	set +e
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ping -c 1 -W 2 $ip >/dev/null 2>&1" 2>/dev/null
	ping_rc=$?

	dig_out=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "dig @$ip +time=2 +tries=1 +short kubernetes.default.svc.cluster.local 2>&1" 2>/dev/null)
	dig_rc=$?
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "curl -k -s -o /dev/null --connect-timeout 2 --max-time 4 https://${ip}:443" 2>/dev/null
	curl_rc=$?
	set -e

	[[ "$ping_rc" -eq 0 ]] && BASELINE_PING_OK=0
	if [[ "$dig_rc" -eq 0 ]] && echo "$dig_out" | grep -v '^;' | grep -q '[[:alnum:].-]'; then
		BASELINE_DIG_OK=0
	fi
	[[ "$curl_rc" -eq 0 ]] && BASELINE_HTTPS_OK=0

	log_info "Probe summary for $ip: PING ok=$([[ $BASELINE_PING_OK -eq 0 ]] && echo yes || echo no), DIG(ok)=$([[ $BASELINE_DIG_OK -eq 0 ]] && echo yes || echo no), HTTPS:443 ok=$([[ $BASELINE_HTTPS_OK -eq 0 ]] && echo yes || echo no)"
}

# Returns 0 if at least one probe reaches the IP (suitable fence target for this test)
any_probe_succeeds() {
	local pod_name="$1"
	local ip="$2"
	measure_probes "$pod_name" "$ip"
	[[ $BASELINE_PING_OK -eq 0 || $BASELINE_DIG_OK -eq 0 || $BASELINE_HTTPS_OK -eq 0 ]]
}

# Emit candidate IPs (one per line): kube-dns / CoreDNS Service ClusterIPs, CoreDNS pod IP, kubernetes Service ClusterIP
collect_candidate_ips() {
	local ctx="$DR2_CONTEXT" ip
	# Common DNS service names
	for svc in kube-dns kube-dns-upstream rke2-coredns-rke2-coredns; do
		ip=$(kubectl --context="$ctx" get svc -n kube-system "$svc" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
		if [[ -n "${ip:-}" && "$ip" != "None" ]]; then
			echo "$ip"
		fi
	done
	# Label selector (k8s-app=kube-dns)
	ip=$(kubectl --context="$ctx" get svc -n kube-system -l k8s-app=kube-dns -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null || true)
	if [[ -n "${ip:-}" && "$ip" != "None" ]]; then
		echo "$ip"
	fi
	# CoreDNS pod IP (direct pod-to-pod style target)
	ip=$(kubectl --context="$ctx" get pods -n kube-system -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
	if [[ -n "${ip:-}" ]]; then
		echo "$ip"
	fi
	# kubernetes.default ClusterIP (HTTPS probe)
	ip=$(kubectl --context="$ctx" get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
	if [[ -n "${ip:-}" ]]; then
		echo "$ip"
	fi
	# OpenShift DNS
	if kubectl --context="$ctx" get ns openshift-dns &>/dev/null; then
		ip=$(kubectl --context="$ctx" get svc -n openshift-dns dns-default -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
		if [[ -n "${ip:-}" && "$ip" != "None" ]]; then
			echo "$ip"
		fi
	fi
}

# discover_fence_target pod_name → sets global test_target (CIDR), ip_only; or exits 1
discover_fence_target() {
	local pod_name="$1"
	local ip cand test_target

	if [[ -n "${VALIDATE_FENCE_TARGET:-}" ]]; then
		test_target="$VALIDATE_FENCE_TARGET"
		if [[ "$test_target" != */* ]]; then
			test_target="${test_target}/32"
		fi
		ip="${test_target%/*}"
		log_info "Using VALIDATE_FENCE_TARGET: $test_target"
		if ! any_probe_succeeds "$pod_name" "$ip"; then
			log_error "Override IP $ip is not reachable from the test pod (ping/dig/https). Pick another VALIDATE_FENCE_TARGET."
			return 1
		fi
		DISCOVERED_TARGET_CIDR="$test_target"
		DISCOVERED_IP="$ip"
		return 0
	fi

	log_info "Auto-discovering an in-cluster IP reachable from the DaemonSet pod (Service/Pod IP)..."
	while read -r cand; do
		[[ -z "$cand" ]] && continue
		log_info "  Trying candidate $cand ..."
		if any_probe_succeeds "$pod_name" "$cand"; then
			DISCOVERED_TARGET_CIDR="${cand}/32"
			DISCOVERED_IP="$cand"
			log_success "  Selected fence target ${DISCOVERED_TARGET_CIDR} (internal Service/Pod–style IP with working path from the node)"
			return 0
		fi
	done < <(collect_candidate_ips | sort -u)

	log_error "Could not find any reachable in-cluster IP from candidates (kube-dns Service, CoreDNS Pod, kubernetes Service)."
	log_info "Set VALIDATE_FENCE_TARGET to a /32 your nodes can reach (e.g. cluster DNS ClusterIP)."
	return 1
}

check_connectivity() {
	local pod_name="$1"
	local target_ip="$2"
	local test_name="$3"

	log_info "=== Connectivity: $test_name ==="

	local ip_only="${target_ip%/*}"
	measure_probes "$pod_name" "$ip_only"

	log_info "Test: ip route get $ip_only (informational)"
	local route_output
	route_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ip route get $ip_only 2>&1" || true)
	log_info "  Route: $route_output"

	log_info "Test: iptables OUTPUT rules mentioning $ip_only"
	local rule_count
	rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
	    ipt_cmd=\$(detect-iptables)
	    if [ -n \"\$ipt_cmd\" ]; then
	        \$ipt_cmd -L OUTPUT -n | grep '$ip_only' | wc -l
	    else
	        echo '0'
	    fi
	")
	log_info "  Rule lines: $rule_count"
	log_info "=== End section ==="
	echo ""
}

# Assert that probes that worked at baseline are blocked after fence
assert_datapath_blocked() {
	local pod_name="$1"
	local ip="$2"

	log_info "Strict check: datapath to $ip should be blocked for probes that worked BEFORE fence..."
	local fail=0

	if [[ "$PRE_FENCE_DIG_OK" -eq 0 ]]; then
		local dig_out dig_rc
		set +e
		dig_out=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "dig @$ip +time=2 +tries=1 +short kubernetes.default.svc.cluster.local 2>&1" 2>/dev/null)
		dig_rc=$?
		set -e
		if [[ "$dig_rc" -eq 0 ]] && echo "$dig_out" | grep -v '^;' | grep -q '[[:alnum:].-]'; then
			log_error "DIG still returned an answer after fence (expected block to $ip)"
			fail=1
		else
			log_success "DIG to $ip blocked or unreachable as expected (was working before fence)"
		fi
	fi

	if [[ "$PRE_FENCE_PING_OK" -eq 0 ]]; then
		local ping_rc
		set +e
		kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ping -c 1 -W 2 $ip >/dev/null 2>&1" 2>/dev/null
		ping_rc=$?
		set -e
		if [[ "$ping_rc" -eq 0 ]]; then
			log_error "PING still succeeded after fence (expected block to $ip)"
			fail=1
		else
			log_success "PING to $ip blocked as expected (was working before fence)"
		fi
	fi

	if [[ "$PRE_FENCE_HTTPS_OK" -eq 0 ]]; then
		local curl_rc
		set +e
		kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "curl -k -s -o /dev/null --connect-timeout 2 --max-time 4 https://${ip}:443" 2>/dev/null
		curl_rc=$?
		set -e
		if [[ "$curl_rc" -eq 0 ]]; then
			log_error "HTTPS to $ip:443 still connected after fence (expected block)"
			fail=1
		else
			log_success "HTTPS to $ip:443 blocked as expected (was working before fence)"
		fi
	fi

	if [[ "$PRE_FENCE_DIG_OK" -ne 0 && "$PRE_FENCE_PING_OK" -ne 0 && "$PRE_FENCE_HTTPS_OK" -ne 0 ]]; then
		log_warning "No pre-fence probe succeeded; cannot assert datapath block (iptables rule still validated above)"
	fi

	return "$fail"
}

# Assert probes restored after unfence
assert_datapath_restored() {
	local pod_name="$1"
	local ip="$2"

	log_info "Strict check: probes that worked BEFORE fence should work again after unfence..."
	local fail=0

	if [[ "$PRE_FENCE_DIG_OK" -eq 0 ]]; then
		local dig_out dig_rc
		set +e
		dig_out=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "dig @$ip +time=2 +tries=1 +short kubernetes.default.svc.cluster.local 2>&1" 2>/dev/null)
		dig_rc=$?
		set -e
		if [[ "$dig_rc" -ne 0 ]] || ! echo "$dig_out" | grep -v '^;' | grep -q '[[:alnum:].-]'; then
			log_error "DIG did not recover after unfence"
			fail=1
		else
			log_success "DIG to $ip restored"
		fi
	fi

	if [[ "$PRE_FENCE_PING_OK" -eq 0 ]]; then
		local ping_rc
		set +e
		kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ping -c 1 -W 2 $ip >/dev/null 2>&1" 2>/dev/null
		ping_rc=$?
		set -e
		if [[ "$ping_rc" -ne 0 ]]; then
			log_error "PING did not recover after unfence"
			fail=1
		else
			log_success "PING to $ip restored"
		fi
	fi

	if [[ "$PRE_FENCE_HTTPS_OK" -eq 0 ]]; then
		local curl_rc
		set +e
		kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "curl -k -s -o /dev/null --connect-timeout 2 --max-time 4 https://${ip}:443" 2>/dev/null
		curl_rc=$?
		set -e
		if [[ "$curl_rc" -ne 0 ]]; then
			log_error "HTTPS to $ip:443 did not recover after unfence"
			fail=1
		else
			log_success "HTTPS to $ip:443 restored"
		fi
	fi

	return "$fail"
}

# Cleanup function
cleanup() {
	log_info "Cleaning up test resources..."
	kubectl --context="$DR2_CONTEXT" delete namespace "$TEST_NAMESPACE" --ignore-not-found=true || true
	kubectl --context="$DR2_CONTEXT" delete job -l app=iptables-test --ignore-not-found=true || true
}

# Trap for cleanup on exit
trap cleanup EXIT

# Step 1: Validate prerequisites
validate_prerequisites() {
	log_info "Validating prerequisites..."

	if ! kubectl config get-contexts "$DR1_CONTEXT" >/dev/null 2>&1; then
		log_error "Context $DR1_CONTEXT not found"
		return 1
	fi

	if ! kubectl config get-contexts "$DR2_CONTEXT" >/dev/null 2>&1; then
		log_error "Context $DR2_CONTEXT not found"
		return 1
	fi

	if ! kubectl --context="$DR2_CONTEXT" run image-test --image="$IPTABLES_IMAGE" --image-pull-policy=Never --restart=Never --rm -i --timeout=30s --command -- echo "Image available" >/dev/null 2>&1; then
		log_error "Image $IPTABLES_IMAGE not available in $DR2_CONTEXT cluster"
		log_info "Please run: make -f test/e2e/utils/Makefile.iptables load-images-to-clusters"
		return 1
	fi

	log_success "Prerequisites validated"
}

# Step 2: Deploy iptables DaemonSet
deploy_daemonset() {
	log_info "Deploying iptables DaemonSet to $DR2_CONTEXT..."

	kubectl --context="$DR2_CONTEXT" create namespace "$TEST_NAMESPACE" --dry-run=client -o yaml | kubectl --context="$DR2_CONTEXT" apply -f -

	local template_file
	template_file="$(dirname "$0")/../helpers/templates/iptables-daemonset.yaml"
	local rendered_template="/tmp/iptables-daemonset-rendered.yaml"

	sed -e "s|{{ \.Namespace }}|$TEST_NAMESPACE|g" \
		-e "s|{{ \.Image }}|$IPTABLES_IMAGE|g" \
		-e '/{{-.*}}/d' \
		-e '/{{.*}}/d' \
		"$template_file" >"$rendered_template"

	kubectl --context="$DR2_CONTEXT" apply -f "$rendered_template"
	rm -f "$rendered_template"

	log_info "Waiting for DaemonSet pods to be ready..."
	kubectl --context="$DR2_CONTEXT" wait --for=condition=ready pod -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" --timeout=120s

	log_success "DaemonSet deployed and ready"
}

# Step 3: Test network fencing functionality
test_network_fencing() {
	log_info "Testing network fencing functionality (internal Service/Pod IP)..."

	local pod_name
	pod_name=$(kubectl --context="$DR2_CONTEXT" get pods -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')

	if [[ -z "$pod_name" ]]; then
		log_error "No DaemonSet pod found"
		return 1
	fi

	log_info "Using DaemonSet pod: $pod_name"

	DISCOVERED_TARGET_CIDR=""
	DISCOVERED_IP=""
	if ! discover_fence_target "$pod_name"; then
		return 1
	fi

	local test_target="$DISCOVERED_TARGET_CIDR"
	local ip_only="$DISCOVERED_IP"

	log_info "Baseline connectivity to fence target $test_target (internal IP)..."
	check_connectivity "$pod_name" "$test_target" "BEFORE FENCING"

	# measure_probes() overwrites BASELINE_* on every check_connectivity; strict asserts need the pre-fence snapshot only.
	PRE_FENCE_DIG_OK=$BASELINE_DIG_OK
	PRE_FENCE_PING_OK=$BASELINE_PING_OK
	PRE_FENCE_HTTPS_OK=$BASELINE_HTTPS_OK
	log_info "Saved pre-fence probe snapshot for strict asserts (0=worked): DIG=$PRE_FENCE_DIG_OK PING=$PRE_FENCE_PING_OK HTTPS=$PRE_FENCE_HTTPS_OK"

	log_info "Applying fence rule for $test_target ..."
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- fence-ip "$test_target"

	log_info "Iptables rules after fencing (target=$test_target):"
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
	    ipt_cmd=\$(detect-iptables)
	    if [ -n \"\$ipt_cmd\" ]; then
	        echo '=== OUTPUT (after fence) ==='
	        \$ipt_cmd -L OUTPUT -n -v | head -25
	        echo ''
	        \$ipt_cmd -L OUTPUT -n -v | grep '${ip_only}' || echo '(no line for ${ip_only})'
	    else
	        echo 'iptables not detected'
	    fi
	"

	local rule_count
	rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
	    ipt_cmd=\$(detect-iptables)
	    if [ -n \"\$ipt_cmd\" ]; then
	        \$ipt_cmd -L OUTPUT -n | grep '${ip_only}' | wc -l
	    else
	        echo '0'
	    fi
	")

	if [[ "$rule_count" -gt 0 ]]; then
		log_success "Fence rule present in iptables ($rule_count line(s))"
	else
		log_error "Fence rule not found in iptables"
		return 1
	fi

	check_connectivity "$pod_name" "$test_target" "AFTER FENCING"

	if ! assert_datapath_blocked "$pod_name" "$ip_only"; then
		log_error "Datapath block assertion failed"
		return 1
	fi

	log_info "Removing fence rule for $test_target ..."
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- unfence-ip "$test_target"

	log_info "Iptables after unfence:"
	kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
	    ipt_cmd=\$(detect-iptables)
	    if [ -n \"\$ipt_cmd\" ]; then
	        \$ipt_cmd -L OUTPUT -n | grep '${ip_only}' || echo 'No rules for ${ip_only}'
	    fi
	"

	rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
	    ipt_cmd=\$(detect-iptables)
	    if [ -n \"\$ipt_cmd\" ]; then
	        \$ipt_cmd -L OUTPUT -n | grep '${ip_only}' | wc -l
	    else
	        echo '0'
	    fi
	")

	if [[ "$rule_count" -eq 0 ]]; then
		log_success "Fence rule removed from iptables"
	else
		log_warning "Unexpected: still $rule_count rule line(s) for $ip_only"
	fi

	check_connectivity "$pod_name" "$test_target" "AFTER UNFENCING"

	if ! assert_datapath_restored "$pod_name" "$ip_only"; then
		log_error "Datapath restore assertion failed"
		return 1
	fi

	log_success "Network fencing test completed (internal IP + iptables + datapath)"
}

# Step 4: Validate DaemonSet logs and health
validate_daemonset_health() {
	log_info "Validating DaemonSet health and logs..."

	local desired_pods ready_pods
	desired_pods=$(kubectl --context="$DR2_CONTEXT" get daemonset csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.status.desiredNumberScheduled}')
	ready_pods=$(kubectl --context="$DR2_CONTEXT" get daemonset csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.status.numberReady}')

	log_info "DaemonSet status: $ready_pods/$desired_pods pods ready"

	if [[ "$ready_pods" -eq "$desired_pods" ]] && [[ "$desired_pods" -gt 0 ]]; then
		log_success "All DaemonSet pods are ready"
	else
		log_error "DaemonSet not fully ready"
		return 1
	fi

	log_info "Recent DaemonSet pod logs:"
	local pod_name
	pod_name=$(kubectl --context="$DR2_CONTEXT" get pods -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')

	kubectl --context="$DR2_CONTEXT" logs "$pod_name" -n "$TEST_NAMESPACE" --tail=20 || true

	log_success "DaemonSet health validation completed"
}

# Main execution
main() {
	log_info "Starting iptables network fence validation flow..."
	log_info "Configuration: DR2_CONTEXT=$DR2_CONTEXT, TEST_NAMESPACE=$TEST_NAMESPACE, IPTABLES_IMAGE=$IPTABLES_IMAGE, VALIDATE_FENCE_TARGET=${VALIDATE_FENCE_TARGET:-<auto: in-cluster Service/Pod IP>}"
	log_info "Note: kubectl's 'command terminated with exit code N' is suppressed for probe execs (expected failures). Remote exit codes still apply; see probe helper comment block at top of script."

	validate_prerequisites
	deploy_daemonset
	test_network_fencing
	validate_daemonset_health

	log_success "✅ All iptables network fence validation tests passed!"
	log_info "Fencing validated against a reachable internal IP, iptables OUTPUT REJECT, and datapath block/restore."
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
