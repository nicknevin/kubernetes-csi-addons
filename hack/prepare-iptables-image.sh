#!/bin/bash
# Prepare alpine:3.19 image for iptables fault injection in both DR clusters
#
# This script ensures that the alpine:3.19 image required for iptables fault injection
# is available in both DR1 and DR2 clusters. It checks if the image exists locally,
# pulls it if needed, and then loads it into both cluster contexts.
#
# Environment variables:
#   DR1_CONTEXT - Kubernetes context for DR1 cluster
#   DR2_CONTEXT - Kubernetes context for DR2 cluster
#   IPTABLES_IMAGE - Image to use (default: alpine:3.19)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IPTABLES_IMAGE="${IPTABLES_IMAGE:-alpine:3.19}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

check_required_vars() {
    if [[ -z "${DR1_CONTEXT:-}" ]]; then
        log_error "DR1_CONTEXT environment variable is required"
        exit 1
    fi
    
    if [[ -z "${DR2_CONTEXT:-}" ]]; then
        log_error "DR2_CONTEXT environment variable is required"
        exit 1
    fi
}

check_image_in_cluster() {
    local context="$1"
    local image="$2"
    
    log_info "Checking if $image exists in cluster context: $context"
    
    # Try to get nodes in the cluster
    if ! kubectl --context="$context" get nodes >/dev/null 2>&1; then
        log_error "Cannot access cluster context: $context"
        return 1
    fi
    
    # Check if any node has the image
    local nodes
    nodes=$(kubectl --context="$context" get nodes -o name | sed 's|node/||')
    
    for node in $nodes; do
        # Use crictl on the node to check if image exists (assuming containerd/crio)
        local has_image=false
        if kubectl --context="$context" debug "node/$node" -it=false --image=busybox -- \
           sh -c "chroot /host crictl images | grep -q '$image'" 2>/dev/null; then
            has_image=true
        elif kubectl --context="$context" debug "node/$node" -it=false --image=busybox -- \
           sh -c "chroot /host docker images | grep -q '$image'" 2>/dev/null; then
            has_image=true
        fi
        
        if $has_image; then
            log_info "Image $image found on node $node in context $context"
            return 0
        fi
    done
    
    log_warn "Image $image not found in any node of context $context"
    return 1
}

load_image_to_cluster() {
    local context="$1"
    local image="$2"
    
    log_info "Loading image $image to cluster context: $context"
    
    # For kind clusters
    if [[ "$context" =~ kind.* ]]; then
        if command -v kind >/dev/null 2>&1; then
            log_info "Detected kind cluster, using 'kind load docker-image'"
            kind load docker-image "$image" --name="$(echo "$context" | sed 's/kind-//')"
            return $?
        fi
    fi
    
    # For k3s/k3d clusters  
    if [[ "$context" =~ k3d.* ]]; then
        if command -v k3d >/dev/null 2>&1; then
            log_info "Detected k3d cluster, using 'k3d image import'"
            k3d image import "$image" --cluster="$(echo "$context" | sed 's/k3d-//')"
            return $?
        fi
    fi
    
    # Generic approach: create a DaemonSet that pulls the image
    log_info "Using DaemonSet approach to pull image on all nodes"
    
    local daemonset_name="image-puller-$(echo "$image" | tr '/:.' '-')"
    
    kubectl --context="$context" apply -f - <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${daemonset_name}
  namespace: kube-system
  labels:
    app: image-puller
spec:
  selector:
    matchLabels:
      app: image-puller
  template:
    metadata:
      labels:
        app: image-puller
    spec:
      containers:
      - name: image-puller
        image: ${image}
        command: ["/bin/sh", "-c", "echo 'Image pulled successfully'; sleep 30"]
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
      restartPolicy: Always
      terminationGracePeriodSeconds: 5
EOF

    # Wait for DaemonSet to be ready
    log_info "Waiting for DaemonSet $daemonset_name to pull image on all nodes..."
    kubectl --context="$context" -n kube-system rollout status daemonset/"$daemonset_name" --timeout=300s
    
    # Clean up the DaemonSet
    log_info "Cleaning up image puller DaemonSet"
    kubectl --context="$context" -n kube-system delete daemonset/"$daemonset_name" || true
    
    log_info "Image $image should now be available on all nodes in context $context"
    return 0
}

ensure_image_locally() {
    local image="$1"
    
    log_info "Checking if $image exists locally"
    
    if docker images --format "table {{.Repository}}:{{.Tag}}" | grep -q "^$image\$"; then
        log_info "Image $image found locally"
        return 0
    fi
    
    log_info "Pulling image $image locally"
    docker pull "$image"
    return $?
}

main() {
    log_info "Preparing iptables image: $IPTABLES_IMAGE"
    
    check_required_vars
    
    # Ensure image exists locally first
    if ! ensure_image_locally "$IPTABLES_IMAGE"; then
        log_error "Failed to ensure image $IPTABLES_IMAGE is available locally"
        exit 1
    fi
    
    # Check and load image in DR1 cluster
    if ! check_image_in_cluster "$DR1_CONTEXT" "$IPTABLES_IMAGE"; then
        log_info "Loading image to DR1 cluster..."
        if ! load_image_to_cluster "$DR1_CONTEXT" "$IPTABLES_IMAGE"; then
            log_error "Failed to load image to DR1 cluster context: $DR1_CONTEXT"
            exit 1
        fi
    else
        log_info "Image $IPTABLES_IMAGE already available in DR1 cluster"
    fi
    
    # Check and load image in DR2 cluster  
    if ! check_image_in_cluster "$DR2_CONTEXT" "$IPTABLES_IMAGE"; then
        log_info "Loading image to DR2 cluster..."
        if ! load_image_to_cluster "$DR2_CONTEXT" "$IPTABLES_IMAGE"; then
            log_error "Failed to load image to DR2 cluster context: $DR2_CONTEXT"
            exit 1
        fi
    else
        log_info "Image $IPTABLES_IMAGE already available in DR2 cluster"
    fi
    
    log_info "Image preparation completed successfully!"
    log_info "Image $IPTABLES_IMAGE is now available in both DR1 ($DR1_CONTEXT) and DR2 ($DR2_CONTEXT) clusters"
}

main "$@"