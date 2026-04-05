#!/bin/bash

# Test inter-node fencing: Fence connectivity from one cluster to another
# This tests fencing the actual IP of a remote Kubernetes node

set -euo pipefail

# Configuration
SOURCE_CONTEXT="${SOURCE_CONTEXT:-dr1}"
TARGET_CONTEXT="${TARGET_CONTEXT:-dr2}"
TEST_NAMESPACE="${TEST_NAMESPACE:-e2e-inter-node-fence}"
IPTABLES_IMAGE="${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

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

# Get the target node IP from remote cluster
get_target_node_ip() {
    local context="$1"
    kubectl --context="$context" get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'
}

# Cleanup
cleanup() {
    log_info "Cleaning up test resources..."
    kubectl --context="$SOURCE_CONTEXT" delete namespace "$TEST_NAMESPACE" --ignore-not-found=true 2>/dev/null || true
}

# Don't trap until the end to avoid premature cleanup
trap_cleanup_on_exit() {
    trap cleanup EXIT
}

# Simplified connectivity test that logs each step
quick_connectivity_check() {
    local pod_name="$1"
    local target_ip="$2"
    local test_name="$3"
    
    log_info "=== Connectivity Check: $test_name ==="
    
    local ip_only="${target_ip%/*}"
    
    # Test 1: PING
    log_info "Test 1: PING to $ip_only..."
    if kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- timeout 3 ping -c 1 "$ip_only" >/dev/null 2>&1; then
        log_success "  ✓ PING: Working"
    else
        log_warning "  ✗ PING: Blocked or failed"
    fi
    
    # Test 2: Check iptables rules (most reliable)
    log_info "Test 2: Iptables rules for $ip_only..."
    local rule_count
    rule_count=$(kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            \$ipt_cmd -L OUTPUT -n | grep '$ip_only' | wc -l
        else
            echo '0'
        fi
    ")
    if [[ "$rule_count" -gt 0 ]]; then
        log_success "  ✓ IPTABLES: $rule_count blocking rule(s)"
    else
        log_info "  ℹ IPTABLES: No blocking rules ($rule_count)"
    fi
    
    # Test 3: Routing
    log_info "Test 3: IP route to $ip_only..."
    if kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- ip route get "$ip_only" 2>&1 | grep -q "via\|dev"; then
        log_success "  ✓ ROUTE: Path exists"
    else
        log_warning "  ✗ ROUTE: No route"
    fi
    
    # Test 4: Traceroute
    log_info "Test 4: Traceroute to $ip_only..."
    if kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- timeout 5 traceroute -m 3 "$ip_only" 2>&1 | head -3 | grep -q "$ip_only\|reached"; then
        log_success "  ✓ TRACEROUTE: Has route"
    else
        log_warning "  ✗ TRACEROUTE: No route detected"
    fi
    
    log_info "=== End Check ==="
    echo ""
}

# Main
main() {
    log_info "=== Inter-Node Fencing Test ==="
    log_info "Source cluster: $SOURCE_CONTEXT"
    log_info "Target cluster: $TARGET_CONTEXT"
    log_info "Test namespace: $TEST_NAMESPACE"
    echo ""
    
    # Get target node IP
    log_info "Discovering target node IP in $TARGET_CONTEXT..."
    local target_node_ip
    target_node_ip=$(get_target_node_ip "$TARGET_CONTEXT")
    
    if [[ -z "$target_node_ip" ]]; then
        log_error "Could not determine target node IP"
        return 1
    fi
    
    log_success "Target node IP: $target_node_ip"
    echo ""
    
    # Create namespace
    log_info "Creating test namespace in $SOURCE_CONTEXT..."
    kubectl --context="$SOURCE_CONTEXT" create namespace "$TEST_NAMESPACE" --dry-run=client -o yaml | kubectl --context="$SOURCE_CONTEXT" apply -f -
    
    # Deploy DaemonSet
    log_info "Deploying iptables DaemonSet to $SOURCE_CONTEXT..."
    local template_file="$(dirname "$0")/../helpers/templates/iptables-daemonset.yaml"
    local rendered_template="/tmp/iptables-daemonset-inter-node.yaml"
    
    sed -e "s|{{ \.Namespace }}|$TEST_NAMESPACE|g" \
        -e "s|{{ \.Image }}|$IPTABLES_IMAGE|g" \
        -e '/{{-.*}}/d' \
        -e '/{{.*}}/d' \
        "$template_file" > "$rendered_template"
    
    kubectl --context="$SOURCE_CONTEXT" apply -f "$rendered_template"
    rm -f "$rendered_template"
    
    log_info "Waiting for DaemonSet pods to be ready..."
    kubectl --context="$SOURCE_CONTEXT" wait --for=condition=ready pod -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" --timeout=120s
    
    log_success "DaemonSet ready"
    echo ""
    
    # Get pod name
    local pod_name
    pod_name=$(kubectl --context="$SOURCE_CONTEXT" get pods -l app=csi-addons-iptables-manager -n "$TEST_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
    log_info "Using pod: $pod_name"
    echo ""
    
    # STEP 1: Test before fencing
    log_info "STEP 1: Testing connectivity BEFORE fencing..."
    quick_connectivity_check "$pod_name" "$target_node_ip/32" "BEFORE FENCING ($target_node_ip)"
    
    # STEP 2: Apply fence
    log_info "STEP 2: Applying fence rule..."
    kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- fence-ip "$target_node_ip/32"
    log_success "Fence rule applied"
    echo ""
    
    # STEP 3: Test after fencing
    log_info "STEP 3: Testing connectivity AFTER fencing..."
    quick_connectivity_check "$pod_name" "$target_node_ip/32" "AFTER FENCING ($target_node_ip)"
    
    # Display rules
    log_info "Iptables OUTPUT rules (after fencing):"
    kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- sh -c "
        ipt_cmd=\$(detect-iptables)
        if [ -n \"\$ipt_cmd\" ]; then
            echo 'All blocking rules:'
            \$ipt_cmd -L OUTPUT -n | grep -E 'DROP|REJECT' || echo 'No blocking rules'
            echo ''
            echo 'Rules for target $target_node_ip:'
            \$ipt_cmd -L OUTPUT -n -v | grep '$target_node_ip' || echo 'No rules for this IP'
        fi
    "
    echo ""
    
    # STEP 4: Remove fence
    log_info "STEP 4: Removing fence rule..."
    kubectl --context="$SOURCE_CONTEXT" exec "$pod_name" -n "$TEST_NAMESPACE" -- unfence-ip "$target_node_ip/32"
    log_success "Fence rule removed"
    echo ""
    
    # STEP 5: Test after unfencing
    log_info "STEP 5: Testing connectivity AFTER unfencing..."
    quick_connectivity_check "$pod_name" "$target_node_ip/32" "AFTER UNFENCING ($target_node_ip)"
    
    log_success "✅ Inter-Node fencing test completed successfully!"
    
    # NOW set up cleanup trap to run at script exit
    trap_cleanup_on_exit
}

main "$@"
