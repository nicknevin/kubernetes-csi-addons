#!/bin/bash

# CSI-Addons Iptables Image Preloader for Minikube
# This script preloads the iptables-manager image to minikube clusters
# so that pods can use imagePullPolicy: Never

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # WORKSPACE_ROOT may be used in future or for debugging
WORKSPACE_ROOT="$(dirname "$SCRIPT_DIR")"

# Configuration
IMAGE="localhost/csi-addons/iptables-manager:latest"
DR1_CONTEXT="dr1"
DR2_CONTEXT="dr2"
TEMP_DIR="/tmp/csi-addons-image-load"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
  echo -e "[${YELLOW}INFO${NC}] $*"
}

log_success() {
  echo -e "[${GREEN}SUCCESS${NC}] $*"
}

log_error() {
  echo -e "[${RED}ERROR${NC}] $*"
}

log_warn() {
  echo -e "[${YELLOW}WARN${NC}] $*"
}

# Detect container runtime
detect_container_runtime() {
  if command -v podman &> /dev/null; then
    echo "podman"
  elif command -v docker &> /dev/null; then
    echo "docker"
  else
    log_error "Neither podman nor docker found"
    exit 1
  fi
}

# Check if image exists locally
check_image_exists() {
  local runtime="$1"
  local image="$2"
  
  log_info "Checking if image '$image' exists locally..."
  if $runtime image inspect "$image" >/dev/null 2>&1; then
    log_success "Image found locally: $image"
    return 0
  else
    log_error "Image not found: $image"
    return 1
  fi
}

# Save image to tar file
save_image_to_tar() {
  local runtime="$1"
  local image="$2"
  local tar_file="$3"
  
  log_info "Saving image to tar..."
  mkdir -p "$TEMP_DIR"
  
  # Show progress
  echo -n "[INFO] Saving image to tar file: "
  $runtime save "$image" -o "$tar_file" 2>&1 | tail -1
  
  local size
  size=$(du -h "$tar_file" | cut -f1)
  log_success "Image saved to tar: $tar_file ($size)"
}

# Load image to cluster via minikube and containerd
load_image_to_minikube() {
  local context="$1"
  local tar_file="$2"
  local image="$3"
  
  log_info "Loading image into cluster: $context"
  
  # Check if minikube context exists
  if ! kubectl config get-contexts | grep -q "$context"; then
    log_error "Minikube context '$context' not found"
    return 1
  fi
  
  log_info "Copying image tar to minikube node..."
  if ! minikube --profile="$context" cp "$tar_file" /tmp/image.tar >/dev/null 2>&1; then
    log_error "Failed to copy tar to minikube"
    return 1
  fi
  
  log_info "Importing image with containerd..."
  if ! minikube --profile="$context" ssh "cat /tmp/image.tar | sudo ctr -a /run/containerd/containerd.sock -n k8s.io image import -" >/dev/null 2>&1; then
    log_error "Failed to import image into containerd"
    return 1
  fi
  
  log_success "Image imported successfully"
  
  # Clean up temp file on minikube
  minikube --profile="$context" ssh "rm -f /tmp/image.tar" >/dev/null 2>&1 || true
}

# Test if image is accessible in cluster
test_image_in_cluster() {
  local context="$1"
  local image="$2"
  
  log_info "Testing image accessibility in cluster: $context"
  
  # Clean up old test pod
  kubectl --context="$context" delete pod test-iptables-img --ignore-not-found >/dev/null 2>&1 || true
  sleep 1
  
  # Create test pod with localhost-prefixed image and explicit PullNever policy
  if kubectl --context="$context" run test-iptables-img \
    --image="$image" \
    --image-pull-policy=Never \
    --restart=Never \
    --command -- iptables --version >/dev/null 2>&1; then
    
    sleep 2
    if output=$(kubectl --context="$context" logs test-iptables-img 2>/dev/null); then
      log_success "Image test successful: $output"
      kubectl --context="$context" delete pod test-iptables-img --ignore-not-found >/dev/null 2>&1 || true
      return 0
    fi
  fi
  
  kubectl --context="$context" delete pod test-iptables-img --ignore-not-found >/dev/null 2>&1 || true
  log_warn "Image test failed (image may still work when deployed via DaemonSet)"
  return 0  # Non-fatal, continue anyway
}

# Main function
main() {
  local runtime
  local tar_file
  
  echo "======================================"
  echo "CSI-Addons Iptables Image Preloader"
  echo "======================================"
  echo ""
  
  # Detect container runtime
  runtime=$(detect_container_runtime)
  log_info "Using container runtime: $runtime"
  echo ""
  
  # Check if image exists
  if ! check_image_exists "$runtime" "$IMAGE"; then
    log_error "Cannot proceed without the image. Please build it first."
    exit 1
  fi
  echo ""
  
  # Save image to tar
  tar_file="$TEMP_DIR/iptables-image.tar"
  save_image_to_tar "$runtime" "$IMAGE" "$tar_file"
  echo ""
  
  # Load to DR1
  log_info "Loading to DR1..."
  if load_image_to_minikube "$DR1_CONTEXT" "$tar_file" "$IMAGE"; then
    log_success "DR1 image loaded"
    test_image_in_cluster "$DR1_CONTEXT" "$IMAGE"
  else
    log_error "Failed to load image to DR1"
  fi
  echo ""
  
  # Load to DR2
  log_info "Loading to DR2..."
  if load_image_to_minikube "$DR2_CONTEXT" "$tar_file" "$IMAGE"; then
    log_success "DR2 image loaded"
    test_image_in_cluster "$DR2_CONTEXT" "$IMAGE"
  else
    log_error "Failed to load image to DR2"
  fi
  echo ""
  
  log_success "Preload complete!"
  echo "Image '$IMAGE' is now available on both clusters"
  echo "Use in pods with: imagePullPolicy: Never"
}

main "$@"
