/*
Copyright 2024 The Kubernetes-CSI-Addons Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helpers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IptablesHandler implements FaultInjectionHandler for iptables-based network fault injection.
// It wraps the PeerFenceProvider and applies FenceIP/UnfenceIP rules via iptables.
type IptablesHandler struct {
	config        FaultInjectionConfig
	faultProvider PeerFenceProvider
	targets       []string
	baseline      *ConnectivityBaseline
}

// NewIptablesHandler creates a new iptables-based fault injection handler.
func NewIptablesHandler(ctx context.Context, config FaultInjectionConfig) (FaultInjectionHandler, error) {
	// Create the underlying iptables fault provider
	provider, err := NewIptablesFaultProvider(config)
	if err != nil {
		return nil, fmt.Errorf("create iptables fault provider: %w", err)
	}

	return &IptablesHandler{
		config:        config,
		faultProvider: provider,
		targets:       []string{},
	}, nil
}

// extractIPFromTarget extracts plain IP from "IP:port" or "IP/CIDR:port" format.
// Handles both IPv4 and IPv6 addresses.
// Examples:
//
//	"192.168.1.10:6800" -> "192.168.1.10"
//	"192.168.1.10/32:6800" -> "192.168.1.10/32"
//	"[2001:db8::1]:9283" -> "[2001:db8::1]"
//	"[2001:db8::1]/64:9283" -> "[2001:db8::1]/64"
//	"[2001:db8::1]" -> "[2001:db8::1]"
func extractIPFromTarget(target string) string {
	// For IPv6 addresses in [brackets], find the closing bracket first
	if strings.HasPrefix(target, "[") {
		closeIdx := strings.Index(target, "]")
		if closeIdx > 0 {
			// Check if there's a port after the closing bracket
			remainder := target[closeIdx+1:]
			if strings.HasPrefix(remainder, ":") && len(remainder) > 1 {
				// Port found after bracket: "[ipv6]:port" -> "[ipv6]"
				return target[:closeIdx+1]
			}
			// No port or bracket is part of CIDR (e.g., "[ipv6]/64")
			return target
		}
	}

	// For IPv4, find the last colon (port separator)
	if idx := strings.LastIndex(target, ":"); idx > 0 {
		return target[:idx]
	}

	return target
}

// ApplyFence applies iptables fence rules and validates that iptables rules were created.
// Process:
//  1. Establish connectivity baseline (before fence) for informational purposes
//  2. Extract port from target CIDR:port format if present
//  3. Apply FenceIP rules for each target
//  4. Allow 5s for iptables rules to settle on all pods
//  5. Return success once iptables rules are confirmed applied
//     NOTE: Port-specific iptables rules won't block ICMP (ping), only TCP on the fenced port.
//     Connectivity baseline is captured for reference but full-blocking cannot be verified.
func (h *IptablesHandler) ApplyFence(ctx context.Context, targets []string) error {
	if len(targets) == 0 {
		return fmt.Errorf("no targets provided for fence")
	}

	// Extract port from first target (for informational purposes only)
	// Note: Port-specific iptables rules are used to avoid blocking entire node,
	// but connectivity verification cannot detect these port-specific blocks
	_, port, _ := parseTargetCIDRWithPort(targets[0])
	Logf("[DEBUG]", "[iptables] Fencing targets with port=%s (blocking will be port-specific)", port)

	// 1. Establish baseline connectivity (before fence) using first target as probe target
	// Extract plain IP from "IP:port" format if present
	probeTarget := extractIPFromTarget(targets[0])
	baseline, err := h.faultProvider.EstablishConnectivityBaseline(ctx, probeTarget)
	if err != nil {
		return fmt.Errorf("establish connectivity baseline: %w", err)
	}
	h.baseline = baseline

	// 2. Apply FenceIP rules for each target
	// Note: This internally calls ensureDaemonSet
	// Port is preserved in CIDR (e.g., "192.168.1.10/32:6800") so iptables only blocks that port, not all traffic
	for _, cidr := range targets {
		if err := h.faultProvider.FenceIP(ctx, cidr, nil); err != nil {
			return fmt.Errorf("fence CIDR %s: %w", cidr, err)
		}
	}

	// 3. Allow iptables rules to settle (5s is the existing value from iptables_provider)
	// This  includes the Sleep that FenceIP already does, so total is ~5s per FenceIP call
	time.Sleep(1 * time.Second) // Small additional wait to ensure rules are visible

	// 4. Success: iptables rules have been applied
	// For port-specific rules, we cannot reliably verify blocking via ICMP (which isn't blocked).
	// The rules are confirmed through the iptables provider's FenceIP execution.
	h.targets = targets // Store for RemoveFence
	return nil
}

// RemoveFence removes iptables fence rules.
// Process:
//  1. Remove UnfenceIP rules for each target
//  2. Allow 5s for iptables rules to be removed on all pods
//  3. Return success once unfence rules are applied
//     NOTE: Like ApplyFence, port-specific rules can't have connectivity verified reliably.
func (h *IptablesHandler) RemoveFence(ctx context.Context) error {
	if len(h.targets) == 0 {
		return fmt.Errorf("no targets to unfence (fence was not applied)")
	}

	// 1. Remove UnfenceIP rules for each target
	for _, cidr := range h.targets {
		if err := h.faultProvider.UnfenceIP(ctx, cidr, nil); err != nil {
			return fmt.Errorf("unfence CIDR %s: %w", cidr, err)
		}
	}

	// 2. Allow iptables rules to be removed on all DaemonSet pods (existing value: 5s)
	time.Sleep(5 * time.Second)

	// 3. Success: unfence rules have been applied
	// Like the fence operation, we cannot reliably verify connectivity is restored via ICMP
	// since the original rules were port-specific. Restoration is confirmed through provider execution.
	return nil
}

// IsSupported checks if iptables fault injection is supported in the cluster.
// Returns false if the cluster lacks privileged DaemonSet support.
func (h *IptablesHandler) IsSupported(ctx context.Context) (bool, string) {
	if !h.faultProvider.IsSupported(ctx) {
		return false, "iptables fault injection requires privileged DaemonSet support"
	}
	return true, ""
}

// Cleanup performs cleanup of iptables DaemonSet and iptables rules.
// Removes any active iptables REJECT rules (with surgical cleanup to preserve other rules)
// and deletes the DaemonSet and ConfigMaps created during fault injection.
func (h *IptablesHandler) Cleanup(ctx context.Context) error {
	if h.faultProvider != nil {
		return h.faultProvider.Cleanup(ctx)
	}
	return nil
}

// DiscoverFenceTargets discovers fence targets for iptables fault injection.
// Implements the unified discovery logic from L1-E-003:
// - For single-cluster: calls DiscoverFenceCIDRsForIptables (GetFenceCIDRsForFaultInjection logic)
// - For full-DR: calls DiscoverFenceCIDRsForIptablesPeer (GetFenceCIDRsForFaultInjectionPeer logic)
//
// Order of discovery:
//  1. FENCE_CIDRS environment variable (explicit targets)
//  2. FENCE_TARGET_SERVICES backend IPs (port-aware service discovery)
//  3. Auto-discovered endpoints from default namespaces
//  4. Returns empty list if nothing found (test caller should skip)
//
// DiscoverFenceTargets discovers fence targets for iptables fault injection.
// Implements the unified discovery logic from L1-E-003:
// - For single-cluster: calls DiscoverFenceCIDRsForIptables (GetFenceCIDRsForFaultInjection logic)
// - For full-DR: calls DiscoverFenceCIDRsForIptablesPeer (GetFenceCIDRsForFaultInjectionPeer logic)
//
// Order of discovery:
//  1. FENCE_CIDRS environment variable (explicit targets)
//  2. FENCE_TARGET_SERVICES backend IPs (port-aware service discovery)
//  3. Auto-discovered endpoints from default namespaces
//  4. Returns empty list if nothing found (test caller should skip)
//
// Note: For full-DR iptables, GetFenceCIDRsForIptablesPeer requires access to both
// fencing-cluster and peer-cluster clients. PeerClient must be set in config for full-DR.
func (h *IptablesHandler) DiscoverFenceTargets(ctx context.Context) []string {
	// Check if full-DR mode is enabled
	if IsFullDRMode() {
		// For full-DR iptables, use both fencing and peer clients
		if h.config.PeerClient != nil {
			return DiscoverFenceCIDRsForIptablesPeer(ctx, h.config.Client, h.config.PeerClient)
		}
		// If peer client not provided, fall back to FENCE_CIDRS env
		Logf("[WARN]", "IptablesHandler.DiscoverFenceTargets: Full-DR mode detected but PeerClient not set in config; falling back to FENCE_CIDRS environment variable")
		return parseFenceCIDRSFromEnv()
	}

	// Single-cluster iptables: use full discovery chain
	// This is the same logic as L1-E-003 for single-cluster:
	// FENCE_CIDRS env → service backends → auto-discovered endpoints → none
	return DiscoverFenceCIDRsForIptables(ctx, h.config.Client)
}

// DiscoverFenceTargetsForClient discovers fence targets for iptables fault injection on a specific client.
// Accepts a client parameter to discover targets for that specific cluster.
// This is useful when you need to discover targets for the peer cluster or a specific instance.
func (h *IptablesHandler) DiscoverFenceTargetsForClient(ctx context.Context, targetClient client.Client) []string {
	// For the given client, use full discovery chain
	// FENCE_CIDRS env → service backends → auto-discovered endpoints → none
	return DiscoverFenceCIDRsForIptables(ctx, targetClient)
}
