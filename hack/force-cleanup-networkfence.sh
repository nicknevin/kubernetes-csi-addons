#!/bin/bash
# Force cleanup script for stuck NetworkFence resources with invalid CIDRs
#
# This script aggressively removes NetworkFence and NetworkFenceClass resources
# that may be stuck due to invalid CIDR data or other issues.
#
# Usage: ./hack/force-cleanup-networkfence.sh [--context <context>]

set -euo pipefail

# Configuration
CONTEXT="${1:-dr1}"
if [[ "$1" == "--context" ]]; then
	CONTEXT="$2"
fi

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

# Helper to run kubectl in a specific context
kubectl_ctx() {
	kubectl --context="$CONTEXT" "$@"
}

main() {
	echo
	log_info "Force Cleanup NetworkFence Resources"
	log_info "======================================"
	echo
	
	# Verify context exists
	if ! kubectl_ctx cluster-info &>/dev/null; then
		log_error "Cannot access cluster context: $CONTEXT"
		exit 1
	fi
	
	log_info "Working on context: $CONTEXT"
	
	# Check if NetworkFence CRD exists
	if ! kubectl_ctx get crd networkfences.csiaddons.openshift.io &>/dev/null; then
		log_warn "NetworkFence CRD not found in this cluster"
		exit 0
	fi
	
	# List current NetworkFences
	echo
	log_info "Current NetworkFences:"
	kubectl_ctx get networkfence -A 2>/dev/null || echo "  (none found)"
	
	# Force cleanup NetworkFences
	echo
	log_info "Force removing NetworkFences..."
	for nf in $(kubectl_ctx get networkfence -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo ""); do
		if [[ -z "$nf" ]]; then
			continue
		fi
		
		echo "  Processing NetworkFence: $nf"
		
		# Get CIDR info for diagnostics
		local cidrs
		cidrs=$(kubectl_ctx get networkfence "$nf" -o jsonpath='{.spec.cidrs}' 2>/dev/null || echo "unknown")
		echo "    CIDRs: $cidrs"
		
		# Step 1: Remove finalizers
		log_info "  Removing finalizers from $nf..."
		kubectl_ctx patch networkfence "$nf" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
		
		# Step 2: Delete with grace period 0 and force
		log_info "  Force deleting $nf..."
		kubectl_ctx delete networkfence "$nf" --ignore-not-found --grace-period=0 --force 2>/dev/null || true
		
		# Step 3: Verify deletion
		sleep 1
		if kubectl_ctx get networkfence "$nf" &>/dev/null; then
			log_error "  $nf still present after force delete, retrying..."
			kubectl_ctx patch networkfence "$nf" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
			kubectl_ctx delete networkfence "$nf" --grace-period=0 2>/dev/null || true
		else
			log_success "  $nf removed"
		fi
	done
	
	# Force cleanup NetworkFenceClasses
	echo
	log_info "Force removing NetworkFenceClasses..."
	if kubectl_ctx get crd networkfenceclasses.csiaddons.openshift.io &>/dev/null; then
		for nfc in $(kubectl_ctx get networkfenceclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo ""); do
			if [[ -z "$nfc" ]]; then
				continue
			fi
			
			echo "  Processing NetworkFenceClass: $nfc"
			
			# Step 1: Remove finalizers
			log_info "  Removing finalizers from $nfc..."
			kubectl_ctx patch networkfenceclass "$nfc" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
			
			# Step 2: Delete with grace period 0 and force
			log_info "  Force deleting $nfc..."
			kubectl_ctx delete networkfenceclass "$nfc" --ignore-not-found --grace-period=0 --force 2>/dev/null || true
			
			# Step 3: Verify deletion
			sleep 1
			if kubectl_ctx get networkfenceclass "$nfc" &>/dev/null; then
				log_error "  $nfc still present after force delete, retrying..."
				kubectl_ctx patch networkfenceclass "$nfc" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
				kubectl_ctx delete networkfenceclass "$nfc" --grace-period=0 2>/dev/null || true
			else
				log_success "  $nfc removed"
			fi
		done
	fi
	
	# Final verification
	echo
	log_info "Final verification:"
	local remaining
	remaining=$(kubectl_ctx get networkfence -o jsonpath='{.items | length}' 2>/dev/null || echo "0")
	if [[ "$remaining" -eq 0 ]]; then
		log_success "✓ All NetworkFences cleaned up!"
	else
		log_warn "⚠ $remaining NetworkFences still present"
	fi
	
	local remaining_nfc
	remaining_nfc=$(kubectl_ctx get networkfenceclass -o jsonpath='{.items | length}' 2>/dev/null || echo "0")
	if [[ "$remaining_nfc" -eq 0 ]]; then
		log_success "✓ All NetworkFenceClasses cleaned up!"
	else
		log_warn "⚠ $remaining_nfc NetworkFenceClasses still present"
	fi
	
	echo
	log_success "Force cleanup completed!"
	echo
}

main "$@"
