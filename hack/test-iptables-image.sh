#!/bin/bash
# Quick test of the iptables image building logic

set -e

echo "Testing iptables image building logic..."

# Detect container command (same logic as Makefile)
if command -v podman > /dev/null 2>&1; then
    CONTAINER_CMD="podman"
elif command -v docker > /dev/null 2>&1; then
    CONTAINER_CMD="docker"
else
    echo "❌ Neither podman nor docker found"
    exit 1
fi

echo "Using container command: $CONTAINER_CMD"

# Check if custom image exists
IPTABLES_IMAGE="csi-addons/iptables-manager:latest"
echo "Checking for image: $IPTABLES_IMAGE"

if ! $CONTAINER_CMD images --format "table {{.Repository}}:{{.Tag}}" | grep -E "(^|/)csi-addons/iptables-manager:latest\$" >/dev/null 2>&1; then
	echo "❌ Custom iptables image not found"
	exit 1
else
	echo "✅ Custom iptables image found"
fi

# Test the image functionality
echo "Testing image functionality..."
if $CONTAINER_CMD run --rm "$IPTABLES_IMAGE" sh -c "iptables --version && which iptables ping nc nslookup" >/dev/null 2>&1; then
	echo "✅ Image functionality test passed"
else
	echo "❌ Image functionality test failed"
	exit 1
fi

echo "🎉 All tests passed! The custom iptables image is ready."