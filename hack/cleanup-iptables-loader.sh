#!/bin/bash
# Cleanup script to remove old iptables image loaders from clusters
#
# Removes old DaemonSets and ConfigMaps that may be left over from previous preload attempts

set -euo pipefail

# Configuration
DR1_CONTEXT="${DR1_CONTEXT:-dr1}"
DR2_CONTEXT="${DR2_CONTEXT:-dr2}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Cleanup function for a specific context
cleanup_context() {
    local context="$1"
    
    log_info "Cleaning up old loaders in context: $context"
    
    # Check if context is accessible
    if ! kubectl --context="$context" cluster-info >/dev/null 2>&1; then
        log_warn "Cannot access cluster context: $context"
        return 0
    fi
    
    # Delete old DaemonSets
    log_info "Deleting old iptables-image-loader DaemonSets..."
    kubectl --context="$context" delete daemonset iptables-image-loader -n kube-system --ignore-not-found=true 2>/dev/null
    
    # Wait for DaemonSet to be deleted
    sleep 2
    
    # Delete old ConfigMaps with iptables image tar
    log_info "Deleting old iptables-image-tar ConfigMaps..."
    kubectl --context="$context" delete configmap -n kube-system -l 'configmap-type=iptables-image' --ignore-not-found=true 2>/dev/null
    
    # Also delete any ConfigMaps that start with iptables-image-tar
    local cms=$(kubectl --context="$context" get configmap -n kube-system -o jsonpath='{.items[?(@.metadata.name=~"^iptables-image-tar")].metadata.name}' 2>/dev/null || true)
    if [[ -n "$cms" ]]; then
        log_info "Found old ConfigMaps: $cms"
        echo "$cms" | xargs -I {} kubectl --context="$context" delete configmap {} -n kube-system 2>/dev/null
    fi
    
    log_success "Cleanup completed for $context"
}

# Main
main() {
    echo
    log_info "CSI-Addons Iptables Loader Cleanup"
    log_info "==================================="
    echo
    
    # Cleanup both contexts
    cleanup_context "$DR1_CONTEXT"
    echo
    cleanup_context "$DR2_CONTEXT"
    
    echo
    log_success "✓ Cleanup completed!"
    echo
}

main "$@"
