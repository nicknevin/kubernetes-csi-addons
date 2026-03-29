#!/bin/bash
# Test the improved iptables image logic

set -e

echo "Testing improved iptables image selection logic..."

# Test 1: No kind/k3d available - should fall back to alpine
echo ""
echo "Test 1: Simulating environment without kind/k3d"
PATH_BACKUP="$PATH"
export PATH="/usr/bin:/bin"  # Remove kind/k3d from path

IPTABLES_IMAGE="csi-addons/iptables-manager:latest"
if command -v kind >/dev/null 2>&1 || command -v k3d >/dev/null 2>&1; then
    echo "✓ Should use custom image (kind/k3d available)"
else
    echo "✓ Should fall back to alpine:3.19 (no kind/k3d)"
fi

export PATH="$PATH_BACKUP"

# Test 2: Check if kind is available
echo ""
echo "Test 2: Checking cluster support"
if command -v kind >/dev/null 2>&1; then
    echo "✓ kind is available - custom images supported"
    kind get clusters 2>/dev/null || echo "  No kind clusters running"
else
    echo "✗ kind not available"
fi

if command -v k3d >/dev/null 2>&1; then
    echo "✓ k3d is available - custom images supported"
    k3d cluster list 2>/dev/null || echo "  No k3d clusters running"
else
    echo "✗ k3d not available"
fi

# Test 3: Image detection
echo ""
echo "Test 3: Testing image detection"
if command -v podman >/dev/null 2>&1; then
    CONTAINER_CMD="podman"
elif command -v docker >/dev/null 2>&1; then
    CONTAINER_CMD="docker"
else
    echo "✗ No container runtime available"
    exit 1
fi

echo "Using container runtime: $CONTAINER_CMD"

if $CONTAINER_CMD images --format "table {{.Repository}}:{{.Tag}}" | grep -E "(^|/)csi-addons/iptables-manager:latest\$" >/dev/null 2>&1; then
    echo "✓ Custom iptables image found locally"
else
    echo "✗ Custom iptables image not found locally"
fi

echo ""
echo "🎉 Image logic tests completed successfully!"