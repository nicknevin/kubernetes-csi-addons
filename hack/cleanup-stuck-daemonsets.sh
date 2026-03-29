#!/bin/bash
# Cleanup script for stuck image puller DaemonSets

echo "Cleaning up stuck image puller DaemonSets..."

# List of contexts to clean
CONTEXTS=("${DR1_CONTEXT:-dr1}" "${DR2_CONTEXT:-dr2}")

for context in "${CONTEXTS[@]}"; do
    echo "Checking context: $context"
    
    # Check if context is accessible
    if ! kubectl --context="$context" get nodes >/dev/null 2>&1; then
        echo "  Skipping inaccessible context: $context"
        continue
    fi
    
    # Find and delete image puller DaemonSets
    puller_ds=$(kubectl --context="$context" -n kube-system get daemonsets -o name 2>/dev/null | grep "image-puller" || true)
    
    if [[ -n "$puller_ds" ]]; then
        echo "  Found stuck DaemonSets in $context: $puller_ds"
        echo "$puller_ds" | xargs -r kubectl --context="$context" -n kube-system delete
        echo "  ✓ Cleaned up DaemonSets in $context"
    else
        echo "  ✓ No stuck DaemonSets found in $context"
    fi
done

echo "Cleanup completed."