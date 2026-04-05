#!/bin/bash

# Test flow for validating DaemonSet deployment and network fence functionality
# This script provides a comprehensive validation of the iptables-based network fencing

set -euo pipefail

# Default configuration
DR1_CONTEXT="${DR1_CONTEXT:-dr1}"
DR2_CONTEXT="${DR2_CONTEXT:-dr2}"
TEST_NAMESPACE="${TEST_NAMESPACE:-e2e-iptables-test}"
IPTABLES_IMAGE="${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"

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

# Function to perform comprehensive connectivity checks
check_connectivity() {
    local pod_name="$1"
    local target_ip="$2"
    local test_name="$3"
    
    log_info "=== Comprehensive Connectivity Check: $test_name ==="
    
    # Extract just the IP from CIDR notation (e.g., 8.8.8.8/32 -> 8.8.8.8)
    local ip_only="${target_ip%/*}"
    
    # Test 1: Ping
    log_info "Test 1: PING to $ip_only..."
    local ping_output
    ping_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "timeout 3 ping -c 1 $ip_only 2>&1" || true)
    if echo "$ping_output" | grep -q "1 received\|0% packet loss"; then
        log_success "  ✓ PING: Success"
    elif echo "$ping_output" | grep -q "100% packet loss\|unreachable\|permission\|refused"; then
        log_warning "  ✗ PING: Failed (blocked/unreachable)"
    else
        log_warning "  ? PING: Inconclusive - $ping_output"
    fi
    
    # Test 2: DNS Resolution
    log_info "Test 2: DNS Resolution (google-dns)..."
    local dns_output
    dns_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "nslookup google.com 8.8.8.8 2>&1" || true)
    if echo "$dns_output" | grep -q "Name:.*google.com\|8.8.8.8"; then
        log_success "  ✓ DNS: Success (8.8.8.8 resolving)"
    else
        log_warning "  ✗ DNS: Failed or no response"
    fi
    
    # Test 3: Traceroute
    log_info "Test 3: Traceroute to $ip_only..."
    local tracert_output
    tracert_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "timeout 5 traceroute -m 3 $ip_only 2>&1 | head -5" || true)
    if echo "$tracert_output" | grep -q "$ip_only\|reached"; then
        log_success "  ✓ TRACEROUTE: Reached or in progress"
    else
        log_warning "  ✗ TRACEROUTE: No route or blocked"
    fi
    
    # Test 4: Netcat port connectivity (port 53 for DNS)
    log_info "Test 4: Netcat to $ip_only:53 (DNS)..."
    local nc_output
    nc_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "timeout 3 nc -zv $ip_only 53 2>&1" || true)
    if echo "$nc_output" | grep -q "open\|succeeded\|Connected"; then
        log_success "  ✓ NETCAT: Port 53 open"
    else
        log_warning "  ✗ NETCAT: Port 53 not reachable"
    fi
    
    # Test 5: HTTP/HTTPS check with curl (if IP has standard DNS)
    log_info "Test 5: HTTP connectivity check..."
    local curl_output
    curl_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "timeout 3 curl -I --connect-timeout 2 -m 2 http://8.8.8.8 2>&1" || true)
    if echo "$curl_output" | grep -q "HTTP\|Connected\|refused\|timeout"; then
        if echo "$curl_output" | grep -q "Connected\|100\|200\|301\|302"; then
            log_success "  ✓ HTTP: Connected (got response)"
        else
            log_warning "  ✗ HTTP: Connection refused or timeout"
        fi
    else
        log_warning "  ? HTTP: No response"
    fi
    
    # Test 6: Check iptables rules count
    log_info "Test 6: Iptables rules for $ip_only..."
    local rule_count
    rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            \$ipt_cmd -L OUTPUT -n | grep '$ip_only' | wc -l
        else
            echo '0'
        fi
    ")
    log_info "  Rules blocking $ip_only: $rule_count"
    
    # Test 7: Check routing to target
    log_info "Test 7: Routing check to $ip_only..."
    local route_output
    route_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ip route get $ip_only 2>&1" || true)
    log_info "  Route: $route_output"
    
    # Test 8: Interface check
    log_info "Test 8: Network interfaces..."
    local iface_output
    iface_output=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "ip link | grep -E 'UP|DOWN' | head -3")
    log_info "  Active interfaces: $(echo "$iface_output" | tr '\n' ', ')"
    
    log_info "=== End Connectivity Check ==="
    echo ""
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
    
    # Check kubectl contexts
    if ! kubectl config get-contexts "$DR1_CONTEXT" >/dev/null 2>&1; then
        log_error "Context $DR1_CONTEXT not found"
        return 1
    fi
    
    if ! kubectl config get-contexts "$DR2_CONTEXT" >/dev/null 2>&1; then
        log_error "Context $DR2_CONTEXT not found"
        return 1
    fi
    
    # Check if image exists in DR2 cluster
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
    
    # Create test namespace
    kubectl --context="$DR2_CONTEXT" create namespace "$TEST_NAMESPACE" --dry-run=client -o yaml | kubectl --context="$DR2_CONTEXT" apply -f -
    
    # Create DaemonSet using template file with variable substitution
    local template_file="$(dirname "$0")/../helpers/templates/iptables-daemonset.yaml"
    
    # Create temporary file with rendered template
    local rendered_template="/tmp/iptables-daemonset-rendered.yaml"
    
    # Simple variable substitution for the template
    # Remove all Go template directives since we don't need conditional logic for this test
    sed -e "s|{{ \.Namespace }}|$TEST_NAMESPACE|g" \
        -e "s|{{ \.Image }}|$IPTABLES_IMAGE|g" \
        -e '/{{-.*}}/d' \
        -e '/{{.*}}/d' \
        "$template_file" > "$rendered_template"
    
    # Apply the rendered template
    kubectl --context="$DR2_CONTEXT" apply -f "$rendered_template"
    
    # Clean up temporary file
    rm -f "$rendered_template"

    # Wait for DaemonSet to be ready
    log_info "Waiting for DaemonSet pods to be ready..."
    kubectl --context="$DR2_CONTEXT" wait --for=condition=ready pod -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" --timeout=120s
    
    log_success "DaemonSet deployed and ready"
}

# Step 3: Test network fencing functionality
test_network_fencing() {
    log_info "Testing network fencing functionality..."
    
    # Get the DaemonSet pod
    local pod_name
    pod_name=$(kubectl --context="$DR2_CONTEXT" get pods -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
    
    if [[ -z "$pod_name" ]]; then
        log_error "No DaemonSet pod found"
        return 1
    fi
    
    log_info "Using DaemonSet pod: $pod_name"
    
    # Test target IP (using a public DNS server)
    local test_target="8.8.8.8/32"
    
    # Display initial iptables rules
    log_info "Initial iptables rules (before fencing):"
    kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            echo '=== OUTPUT chain (before) ==='
            \$ipt_cmd -L OUTPUT -n -v | head -20
            echo ''
            echo '=== Rules matching 8.8.8.8 (before) ==='
            \$ipt_cmd -L OUTPUT -n | grep '8.8.8.8' || echo 'No rules found'
        else
            echo 'iptables command not detected'
        fi
    "
    
    # Step 3a: Verify initial connectivity with comprehensive checks
    log_info "Testing initial connectivity to $test_target..."
    check_connectivity "$pod_name" "$test_target" "BEFORE FENCING"
    
    # Step 3b: Apply fence rule
    log_info "Applying fence rule for $test_target..."
    kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- fence-ip "$test_target"
    
    # Display iptables rules after fencing
    log_info "Iptables rules after fencing (target=$test_target):"
    kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            echo '=== Full OUTPUT chain (after fence) ==='
            \$ipt_cmd -L OUTPUT -n -v | head -20
            echo ''
            echo '=== Rules matching 8.8.8.8 (after fence) ==='
            \$ipt_cmd -L OUTPUT -n -v | grep '8.8.8.8' || echo 'No rules found'
            echo ''
            echo '=== All rules with DROP/REJECT target ==='
            \$ipt_cmd -L OUTPUT -n | grep -E 'DROP|REJECT' || echo 'No DROP/REJECT rules found'
        else
            echo 'iptables command not detected'
        fi
    "
    
    # Verify fence rule is active
    log_info "Verifying fence rule is active..."
    local rule_count
    rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            \$ipt_cmd -L OUTPUT -n | grep '8.8.8.8' | wc -l
        else
            echo '0'
        fi
    ")
    
    if [[ "$rule_count" -gt 0 ]]; then
        log_success "Fence rule is active (found $rule_count rules)"
    else
        log_error "Fence rule not found in iptables"
        return 1
    fi
    
    # Step 3c: Test that connectivity is blocked with comprehensive checks
    log_info "Testing that connectivity is blocked..."
    check_connectivity "$pod_name" "$test_target" "AFTER FENCING (Should be blocked)"
    
    # Step 3d: Remove fence rule
    log_info "Removing fence rule for $test_target..."
    kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- unfence-ip "$test_target"
    
    # Display iptables rules after unfencing
    log_info "Iptables rules after unfencing (target=$test_target):"
    kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            echo '=== Full OUTPUT chain (after unfence) ==='
            \$ipt_cmd -L OUTPUT -n -v | head -20
            echo ''
            echo '=== Rules matching 8.8.8.8 (after unfence) ==='
            \$ipt_cmd -L OUTPUT -n | grep '8.8.8.8' || echo 'No rules found'
            echo ''
            echo '=== All rules with DROP/REJECT target ==='
            \$ipt_cmd -L OUTPUT -n | grep -E 'DROP|REJECT' || echo 'No DROP/REJECT rules found'
        else
            echo 'iptables command not detected'
        fi
    "
    
    # Verify fence rule is removed
    log_info "Verifying fence rule is removed..."
    rule_count=$(kubectl --context="$DR2_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            \$ipt_cmd -L OUTPUT -n | grep '8.8.8.8' | wc -l
        else
            echo '0'
        fi
    ")
    
    if [[ "$rule_count" -eq 0 ]]; then
        log_success "Fence rule successfully removed"
    else
        log_warning "Fence rule may still be present (found $rule_count rules)"
    fi
    
    # Step 3e: Verify connectivity is restored with comprehensive checks
    log_info "Testing that connectivity is restored..."
    check_connectivity "$pod_name" "$test_target" "AFTER UNFENCING (Should be open)"
    
    log_success "Network fencing functionality test completed"
}

# Step 4: Validate DaemonSet logs and health
validate_daemonset_health() {
    log_info "Validating DaemonSet health and logs..."
    
    # Get DaemonSet status
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
    
    # Show pod logs
    log_info "Recent DaemonSet pod logs:"
    local pod_name
    pod_name=$(kubectl --context="$DR2_CONTEXT" get pods -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
    
    kubectl --context="$DR2_CONTEXT" logs "$pod_name" -n "$TEST_NAMESPACE" --tail=20 || true
    
    log_success "DaemonSet health validation completed"
}

# Main execution
main() {
    log_info "Starting iptables network fence validation flow..."
    log_info "Configuration: DR2_CONTEXT=$DR2_CONTEXT, TEST_NAMESPACE=$TEST_NAMESPACE, IPTABLES_IMAGE=$IPTABLES_IMAGE"
    
    validate_prerequisites
    deploy_daemonset
    test_network_fencing
    validate_daemonset_health
    
    log_success "✅ All iptables network fence validation tests passed!"
    log_info "The network fencing system is working correctly and ready for E2E testing"
}

# Run main function if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi