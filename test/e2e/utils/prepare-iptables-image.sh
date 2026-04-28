#!/bin/bash
# Tag and verify the locally-built iptables image
#
# This script:
# 1. Builds the image with make -f test/e2e/utils/Makefile.iptables build-iptables-image
# 2. Tags it with the correct names (with and without localhost/)
# 3. Verifies it's available for use
#
# Usage: ./prepare-iptables-image.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[✓]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[!]${NC} $1"; }
log_error() { echo -e "${RED}[✗]${NC} $1"; }

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

echo
log_info "Building iptables image..."
log_info "============================="

# Build the image
if make -C "$REPO_ROOT" -f test/e2e/utils/Makefile.iptables build-iptables-image >/dev/null 2>&1; then
	log_success "Image built successfully"
else
	log_error "Failed to build image"
	exit 1
fi

echo
log_info "Tagging image with multiple names..."
log_info "Running: $CONTAINER_CMD tag localhost/csi-addons/iptables-manager:latest csi-addons/iptables-manager:latest"

# Tag the image with both localhost/ and without
# This allows it to be found by either name
if $CONTAINER_CMD tag localhost/csi-addons/iptables-manager:latest csi-addons/iptables-manager:latest; then
	log_success "Image tagged successfully"
else
	log_error "Failed to tag image"
	exit 1
fi

echo
log_info "Verifying image..."

# Verify both tags work
echo
log_info "Testing image accessibility with different tags:"

# Test with csi-addons prefix (what preload-images.sh needs)
if $CONTAINER_CMD run --rm csi-addons/iptables-manager:latest iptables --version >/dev/null 2>&1; then
	log_success "✓ csi-addons/iptables-manager:latest - ACCESSIBLE"
else
	log_error "✗ csi-addons/iptables-manager:latest - NOT ACCESSIBLE"
	exit 1
fi

# Test with localhost prefix (original)
if $CONTAINER_CMD run --rm localhost/csi-addons/iptables-manager:latest iptables --version >/dev/null 2>&1; then
	log_success "✓ localhost/csi-addons/iptables-manager:latest - ACCESSIBLE"
else
	log_warn "! localhost/csi-addons/iptables-manager:latest - NOT ACCESSIBLE"
fi

echo
log_info "Image details:"
if $CONTAINER_CMD inspect csi-addons/iptables-manager:latest >/dev/null 2>&1; then
	$CONTAINER_CMD inspect csi-addons/iptables-manager:latest --format="Size: {{.Size}} bytes"
	$CONTAINER_CMD inspect csi-addons/iptables-manager:latest --format="Created: {{.Created}}"
else
	log_error "Image 'csi-addons/iptables-manager:latest' not found"
	exit 1
fi

echo
log_success "✓ Image is ready for manual upload to clusters"
log_info ""
log_info "Next steps:"
log_info "1. Verify the tagging solved the issue by checking both image names exist:"
log_info "   podman images | grep iptables"
log_info "2. Manually test image in DR1: kubectl --context=dr1 run test --image=csi-addons/iptables-manager:latest --rm -it -- iptables --version"
log_info "3. Manually test image in DR2: kubectl --context=dr2 run test --image=csi-addons/iptables-manager:latest --rm -it -- iptables --version"
log_info "4. Use consolidated preloader: $REPO_ROOT/test/e2e/utils/preload-iptables-image.sh"
log_info ""
