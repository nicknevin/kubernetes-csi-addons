#!/bin/bash

# Emergency cleanup script for CSI-Addons iptables fence rules
# This script can be run manually to clean up leftover fence rules after test failures

set -euo pipefail

# Default configuration
CONTEXT="${1:-dr2}"
NAMESPACE_PATTERN="${2:-e2e-*}"

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

# Function to clean up a specific namespace
cleanup_namespace() {
    local context="$1"
    local namespace="$2"
    
    log_info "Cleaning up namespace: $namespace"
    
    # Look for DaemonSets with iptables manager
    local daemonsets
    daemonsets=$(kubectl --context="$context" get daemonsets -n "$namespace" -l app=csi-addons-iptables-manager -o name 2>/dev/null | head -10)
    
    if [[ -z "$daemonsets" ]]; then
        log_info "No iptables DaemonSets found in namespace $namespace"
        return 0
    fi
    
    # For each DaemonSet, clean up fence rules on its pods
    for ds in $daemonsets; do
        local ds_name=${ds#daemonset.apps/}
        log_info "Processing DaemonSet: $ds_name"
        
        # Get pods for this DaemonSet
        local pods
        pods=$(kubectl --context="$context" get pods -n "$namespace" -l app=csi-addons-iptables-manager -o name 2>/dev/null)
        
        for pod in $pods; do
            local pod_name=${pod#pod/}
            log_info "Cleaning up pod: $pod_name"
            
            # Execute emergency cleanup in the pod
            if kubectl --context="$context" exec "$pod_name" -n "$namespace" -- sh -c "
                echo '[$(date)] Emergency cleanup starting...'
                
                # Auto-detect iptables command
                IPT_CMD=''
                for cmd in iptables-legacy iptables-nft iptables; do
                    if command -v \$cmd >/dev/null 2>&1; then
                        if \$cmd -L OUTPUT -n >/dev/null 2>&1; then
                            IPT_CMD=\"\$cmd\"
                            echo '[$(date)] Using iptables command:' \$IPT_CMD
                            break
                        fi
                    fi
                done
                
                if [ -z \"\$IPT_CMD\" ]; then
                    echo '[$(date)] No working iptables command found'
                    exit 1
                fi
                
                # Count existing REJECT rules
                reject_count=\$(\$IPT_CMD -S OUTPUT | grep -c \"\-j REJECT\" || true)
                echo \"[$(date)] Found \$reject_count REJECT rules to clean up\"
                
                if [ \"\$reject_count\" -gt 0 ]; then
                    # Remove all REJECT rules from OUTPUT chain
                    \$IPT_CMD -S OUTPUT | grep \"\-j REJECT\" | sed 's/^-A/-D/' | while read rule; do
                        echo \"[$(date)] Removing rule: \$rule\"
                        \$IPT_CMD \$rule 2>/dev/null || true
                    done
                    
                    # Verify cleanup
                    remaining=\$(\$IPT_CMD -S OUTPUT | grep -c \"\-j REJECT\" || true)
                    echo \"[$(date)] Cleanup completed. Remaining REJECT rules: \$remaining\"
                else
                    echo '[$(date)] No REJECT rules found to clean up'
                fi
            " 2>/dev/null; then
                log_success "Successfully cleaned up pod $pod_name"
            else
                log_warning "Failed to clean up pod $pod_name (pod may not be running)"
            fi
        done
    done
    
    # Optional: Clean up the DaemonSet and namespace resources
    log_info "Cleaning up DaemonSet resources in namespace $namespace"
    kubectl --context="$context" delete daemonsets -n "$namespace" -l app=csi-addons-iptables-manager --ignore-not-found=true
    kubectl --context="$context" delete jobs -n "$namespace" -l app=csi-addons-iptables-exec --ignore-not-found=true
    kubectl --context="$context" delete jobs -n "$namespace" -l app=csi-addons-iptables-cleanup --ignore-not-found=true
    
    log_success "Completed cleanup for namespace $namespace"
}

# Main execution
main() {
    log_info "Starting emergency cleanup for CSI-Addons iptables fence rules..."
    log_info "Context: $CONTEXT, Namespace pattern: $NAMESPACE_PATTERN"
    
    # Validate context exists
    if ! kubectl config get-contexts "$CONTEXT" >/dev/null 2>&1; then
        log_error "Context $CONTEXT not found"
        exit 1
    fi
    
    # Find namespaces matching the pattern
    local namespaces
    if [[ "$NAMESPACE_PATTERN" == "e2e-*" ]]; then
        namespaces=$(kubectl --context="$CONTEXT" get namespaces -o name 2>/dev/null | grep "namespace/e2e-" | sed 's/namespace\///' || true)
    else
        namespaces=$(kubectl --context="$CONTEXT" get namespace "$NAMESPACE_PATTERN" -o name 2>/dev/null | sed 's/namespace\///' || true)
    fi
    
    if [[ -z "$namespaces" ]]; then
        log_info "No namespaces found matching pattern: $NAMESPACE_PATTERN"
        exit 0
    fi
    
    log_info "Found namespaces: $(echo $namespaces | tr '\n' ' ')"
    
    # Clean up each namespace
    for ns in $namespaces; do
        cleanup_namespace "$CONTEXT" "$ns"
    done
    
    log_success "✅ Emergency cleanup completed successfully!"
    log_info "All CSI-Addons iptables fence rules have been cleaned up."
}

# Print help
if [[ "${1:-}" == "--help" ]] || [[ "${1:-}" == "-h" ]]; then
    cat << 'EOF'
CSI-Addons Emergency Cleanup Script

This script cleans up leftover iptables fence rules from failed E2E tests.

Usage:
  ./emergency-cleanup-iptables.sh [CONTEXT] [NAMESPACE_PATTERN]

Arguments:
  CONTEXT           Kubernetes context to use (default: dr2)
  NAMESPACE_PATTERN Namespace pattern to clean (default: e2e-*)

Examples:
  # Clean up all e2e namespaces in dr2 context
  ./emergency-cleanup-iptables.sh dr2 "e2e-*"
  
  # Clean up specific namespace in dr1 context  
  ./emergency-cleanup-iptables.sh dr1 e2e-iptables-test
  
  # Clean up using default settings
  ./emergency-cleanup-iptables.sh

EOF
    exit 0
fi

# Run main function
main "$@"