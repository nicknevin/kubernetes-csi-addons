#!/bin/bash
# Consolidated iptables image preloader for CSI-Addons E2E testing
# 
# This script combines the functionality of both preload-iptables-simple.sh 
# and preload-iptables-image.sh, with the pod-based approach as primary
# and kind/k3d detection as fallback strategies.
#
# Environment variables:
#   DR1_CONTEXT      - Kubernetes context for DR1 cluster (default: dr1)
#   DR2_CONTEXT      - Kubernetes context for DR2 cluster (default: dr2)
#   IPTABLES_IMAGE   - Image to preload (default: csi-addons/iptables-manager:latest)
#   VERIFY_ONLY      - If set to 'true', only verify image availability without loading

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEMP_DIR="${TMPDIR:-/tmp}/csi-addons-image-load"

# Default values
DR1_CONTEXT="${DR1_CONTEXT:-dr1}"
DR2_CONTEXT="${DR2_CONTEXT:-dr2}"
IPTABLES_IMAGE="${IPTABLES_IMAGE:-csi-addons/iptables-manager:latest}"
VERIFY_ONLY="${VERIFY_ONLY:-false}"

# Colors for output
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

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Detect container runtime
detect_container_runtime() {
    if command -v podman >/dev/null 2>&1; then
        echo "podman"
    elif command -v docker >/dev/null 2>&1; then
        echo "docker"
    else
        log_error "Neither podman nor docker found"
        exit 1
    fi
}

CONTAINER_CMD=$(detect_container_runtime)
log_info "Using container runtime: $CONTAINER_CMD"

# Ensure temp directory exists
mkdir -p "$TEMP_DIR"

# Check if image exists locally
check_image_locally() {
    local image="$1"
    
    log_info "Checking if image '$image' exists locally..."
    
    if $CONTAINER_CMD images --format "table {{.Repository}}:{{.Tag}}" 2>/dev/null | grep -q "^${image}$"; then
        log_success "Image found locally: $image"
        return 0
    fi
    
    log_warn "Image not found locally: $image"
    return 1
}

# Save image to tar file
save_image_to_tar() {
    local image="$1"
    local tar_file="$2"
    
    log_info "Saving image to tar file: $tar_file"
    
    if $CONTAINER_CMD save "$image" -o "$tar_file" 2>/dev/null; then
        log_success "Image saved to tar: $tar_file ($(du -h "$tar_file" | cut -f1))"
        return 0
    else
        log_error "Failed to save image to tar"
        return 1
    fi
}

# Test if image is accessible in cluster
test_image_in_cluster() {
    local context="$1"
    local image="$2"
    
    local test_pod="test-img-$(date +%s)"
    local clean_image="${image#localhost/}"
    
    log_info "Testing image accessibility in cluster: $context"
    
    if kubectl --context="$context" run "$test_pod" \
        --image="$clean_image" \
        --restart=Never \
        --rm=true \
        --timeout=30s \
        --command \
        -- sh -c "iptables --version && echo 'Image works!'" >/dev/null 2>&1; then
        
        log_success "Image test passed in cluster: $context"
        return 0
    else
        log_warn "Image test failed in cluster: $context"
        return 1
    fi
}

# STRATEGY 1: Pod-based image loading (PRIMARY APPROACH)
# Uses ConfigMap to transfer image tar and deploys a job to load it
load_image_via_pod() {
    local context="$1"
    local image="$2"
    local tar_file="$3"
    
    log_info "Using pod-based image loading (primary strategy) for context: $context"
    
    # Verify cluster accessibility
    if ! kubectl --context="$context" cluster-info >/dev/null 2>&1; then
        log_error "Cannot access cluster context: $context"
        return 1
    fi
    
    local clean_image="${image#localhost/}"
    local config_map_name="iptables-image-data-$(date +%s)"
    local job_name="iptables-image-loader-$(date +%s)"
    
    # Base64 encode the tar file for ConfigMap
    log_info "Encoding image tar for ConfigMap..."
    local encoded_data
    encoded_data=$(base64 -w 0 < "$tar_file")
    local encoded_size=$(echo -n "$encoded_data" | wc -c)
    log_info "Encoded size: $(( encoded_size / 1024 / 1024 ))MB"
    
    # Create ConfigMap with encoded image
    log_info "Creating ConfigMap with image data..."
    kubectl --context="$context" create configmap "$config_map_name" \
        --from-literal=image.tar.b64="$encoded_data" \
        >/dev/null 2>&1
    
    # Create and run job to decode and load image
    log_info "Creating job to load image..."
    kubectl --context="$context" apply -f - >/dev/null 2>&1 <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $job_name
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: image-loader
        image: busybox:1.35
        command:
        - sh
        - -c
        - |
          echo 'Decoding and loading image...'
          base64 -d /data/image.tar.b64 > /tmp/image.tar
          if command -v ctr >/dev/null 2>&1; then
            ctr -a /run/containerd/containerd.sock -n k8s.io image import /tmp/image.tar
            ctr -a /run/containerd/containerd.sock -n k8s.io image tag $image $clean_image || true
          elif command -v docker >/dev/null 2>&1; then
            docker load < /tmp/image.tar
          else
            echo 'No container runtime found'
            exit 1
          fi
          echo 'Image loaded successfully'
        securityContext:
          privileged: true
        volumeMounts:
        - name: data
          mountPath: /data
        - name: containerd
          mountPath: /run/containerd
        - name: docker
          mountPath: /var/run/docker.sock
      volumes:
      - name: data
        configMap:
          name: $config_map_name
      - name: containerd
        hostPath:
          path: /run/containerd
      - name: docker
        hostPath:
          path: /var/run/docker.sock
EOF
    
    # Wait for job completion
    log_info "Waiting for image loading job to complete..."
    if kubectl --context="$context" wait --for=condition=complete --timeout=120s job/"$job_name" >/dev/null 2>&1; then
        log_success "Image loading job completed successfully"
        
        # Cleanup
        kubectl --context="$context" delete job "$job_name" configmap "$config_map_name" >/dev/null 2>&1 || true
        return 0
    else
        log_error "Image loading job failed or timed out"
        
        # Show job logs for debugging
        log_info "Job logs:"
        kubectl --context="$context" logs job/"$job_name" 2>/dev/null || echo "No logs available"
        
        # Cleanup
        kubectl --context="$context" delete job "$job_name" configmap "$config_map_name" >/dev/null 2>&1 || true
        return 1
    fi
}

# STRATEGY 2: Minikube-specific loading
load_image_via_minikube() {
    local context="$1"
    local image="$2"
    local tar_file="$3"
    
    if ! command -v minikube >/dev/null 2>&1; then
        return 1
    fi
    
    log_info "Using minikube-based image loading for context: $context"
    
    # Copy tar to minikube and import
    if minikube --profile="$context" cp "$tar_file" /tmp/image.tar >/dev/null 2>&1; then
        local clean_image="${image#localhost/}"
        if minikube --profile="$context" ssh "cat /tmp/image.tar | sudo ctr -a /run/containerd/containerd.sock -n k8s.io image import - && sudo ctr -a /run/containerd/containerd.sock -n k8s.io image tag $image $clean_image && sudo rm -f /tmp/image.tar" >/dev/null 2>&1; then
            log_success "Image imported via minikube"
            return 0
        fi
    fi
    
    return 1
}

# STRATEGY 3: Kind cluster loading
load_image_via_kind() {
    local context="$1"
    local image="$2"
    
    if ! command -v kind >/dev/null 2>&1; then
        return 1
    fi
    
    # Extract cluster name from context
    local cluster_name="$context"
    [[ "$context" =~ ^kind- ]] && cluster_name="${context#kind-}"
    
    # Check if cluster exists
    if ! kind get clusters 2>/dev/null | grep -q "^${cluster_name}$"; then
        return 1
    fi
    
    log_info "Using kind image loading for cluster: $cluster_name"
    if kind load docker-image "$image" --name="$cluster_name" >/dev/null 2>&1; then
        log_success "Image loaded via kind: $cluster_name"
        return 0
    fi
    
    return 1
}

# STRATEGY 4: K3d cluster loading
load_image_via_k3d() {
    local context="$1"
    local image="$2"
    
    if ! command -v k3d >/dev/null 2>&1; then
        return 1
    fi
    
    # Extract cluster name from context
    local cluster_name="$context"
    [[ "$context" =~ ^k3d- ]] && cluster_name="${context#k3d-}"
    
    # Check if cluster exists
    if ! k3d cluster list 2>/dev/null | grep -q "$cluster_name"; then
        return 1
    fi
    
    log_info "Using k3d image import for cluster: $cluster_name"
    if k3d image import "$image" --cluster="$cluster_name" >/dev/null 2>&1; then
        log_success "Image loaded via k3d: $cluster_name"
        return 0
    fi
    
    return 1
}

# Load image to a specific cluster using multiple strategies
load_image_to_cluster() {
    local context="$1"
    local image="$2"
    
    log_info "Loading image to cluster context: $context"
    
    # First, verify cluster accessibility
    if ! kubectl --context="$context" cluster-info >/dev/null 2>&1; then
        log_error "Cannot access cluster context: $context"
        return 1
    fi
    
    # Check if image is already available
    if test_image_in_cluster "$context" "$image"; then
        log_success "Image already available in cluster: $context"
        return 0
    fi
    
    # Prepare tar file for pod-based loading
    local tar_file="$TEMP_DIR/iptables-image-$context.tar"
    if ! save_image_to_tar "$image" "$tar_file"; then
        log_error "Failed to save image to tar file"
        return 1
    fi
    
    # Try different loading strategies in order of preference
    
    # Strategy 1: Pod-based loading (most reliable, works with any k8s cluster)
    if load_image_via_pod "$context" "$image" "$tar_file"; then
        return 0
    fi
    
    # Strategy 2: Minikube-specific loading
    if load_image_via_minikube "$context" "$image" "$tar_file"; then
        return 0
    fi
    
    # Strategy 3: Kind cluster loading
    if load_image_via_kind "$context" "$image"; then
        return 0
    fi
    
    # Strategy 4: K3d cluster loading  
    if load_image_via_k3d "$context" "$image"; then
        return 0
    fi
    
    log_error "All image loading strategies failed for context: $context"
    return 1
}

# Verify image availability in a cluster
verify_image_in_cluster() {
    local context="$1"
    local image="$2"
    
    log_info "Verifying image availability in cluster context: $context"
    
    # Check if cluster is accessible
    if ! kubectl --context="$context" cluster-info >/dev/null 2>&1; then
        log_error "Cannot access cluster context: $context"
        return 1
    fi
    
    # Get node count
    local node_count
    node_count=$(kubectl --context="$context" get nodes --no-headers 2>/dev/null | wc -l)
    log_info "Found $node_count nodes in cluster: $context"
    
    # Try to create a test pod
    local test_pod="verify-$(date +%s)"
    local clean_image="${image#localhost/}"
    
    if kubectl --context="$context" run "$test_pod" \
        --image="$clean_image" \
        --restart=Never \
        --rm \
        --timeout=30s \
        --command \
        -- sh -c "iptables --version && ip link show >/dev/null && echo 'Verification successful'" \
        >/dev/null 2>&1; then
        
        log_success "Image verification successful in cluster: $context"
        return 0
    else
        log_error "Image verification failed in cluster: $context"
        return 1
    fi
}

# Main function
main() {
    log_info "CSI-Addons Iptables Image Preloader (Consolidated)"
    log_info "================================================="
    log_info "Image: $IPTABLES_IMAGE"
    log_info "DR1 Context: $DR1_CONTEXT"
    log_info "DR2 Context: $DR2_CONTEXT"
    log_info "Verify Only: $VERIFY_ONLY"
    log_info "Temp Directory: $TEMP_DIR"
    echo
    
    # Check if image exists locally
    if ! check_image_locally "$IPTABLES_IMAGE"; then
        log_error "Image not found locally: $IPTABLES_IMAGE"
        log_info "Building image locally..."
        
        if [[ -f "$REPO_ROOT/test/e2e/utils/Makefile.iptables" ]]; then
            cd "$REPO_ROOT"
            if make -f test/e2e/utils/Makefile.iptables build-iptables-image >/dev/null 2>&1; then
                log_success "Image built successfully"
            else
                log_error "Failed to build image"
                exit 1
            fi
        else
            log_error "Makefile for iptables image not found at expected location"
            exit 1
        fi
    fi
    
    echo
    log_info "Processing DR clusters..."
    echo
    
    if [[ "$VERIFY_ONLY" == "true" ]]; then
        log_info "Verification mode: checking image availability only"
        
        log_info "Checking DR1 ($DR1_CONTEXT)..."
        if verify_image_in_cluster "$DR1_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR1 image verification passed"
        else
            log_warn "DR1 image verification failed"
        fi
        
        echo
        
        log_info "Checking DR2 ($DR2_CONTEXT)..."
        if verify_image_in_cluster "$DR2_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR2 image verification passed"
        else
            log_warn "DR2 image verification failed"
        fi
    else
        log_info "Loading mode: ensuring image is available in both clusters"
        
        log_info "Processing DR1 ($DR1_CONTEXT)..."
        if load_image_to_cluster "$DR1_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR1 image loading completed"
        else
            log_error "DR1 image loading failed"
            exit 1
        fi
        
        echo
        
        log_info "Processing DR2 ($DR2_CONTEXT)..."
        if load_image_to_cluster "$DR2_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR2 image loading completed"
        else
            log_error "DR2 image loading failed"
            exit 1
        fi
        
        echo
        
        log_info "Verifying images in both clusters..."
        
        if verify_image_in_cluster "$DR1_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR1 image verification passed"
        else
            log_error "DR1 image verification failed"
            exit 1
        fi
        
        if verify_image_in_cluster "$DR2_CONTEXT" "$IPTABLES_IMAGE"; then
            log_success "DR2 image verification passed"
        else
            log_error "DR2 image verification failed"
            exit 1
        fi
    fi
    
    echo
    log_success "Iptables image preloading completed successfully!"
    log_info "The iptables image is now ready for fencing tests."
    log_info "Clean up temp directory: rm -rf $TEMP_DIR"
    echo
    
    return 0
}

# Cleanup on exit
cleanup() {
    if [[ -d "$TEMP_DIR" ]]; then
        log_info "Cleaning up temporary files..."
        rm -rf "$TEMP_DIR"
    fi
}

trap cleanup EXIT

# Execute main function
main "$@"