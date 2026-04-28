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
	"os"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FaultInjectionHandler provides a unified, test-friendly API for network fault injection.
// It abstracts the complexity of fencing operations, connectivity validation, cleanup,
// and discovery behind simple method calls, allowing tests to focus on their test logic rather than
// provider-specific implementation details.
//
// The handler internally routes operations based on the injector type (iptables, networkfence, none)
// and handles all resource lifecycle management including validation and discovery.
//
// Tests call DiscoverFenceTargets() which encapsulates all discovery logic:
// - Injector type detection (iptables needs port-aware discovery, networkfence uses node IPs)
// - Full-DR mode detection (single-cluster vs peer cluster discovery)
// - Fallback chains (FENCE_CIDRS → services → auto-discovery → none)
type FaultInjectionHandler interface {
	// ApplyFence applies fault injection to the specified targets and VALIDATES it's active.
	// This internally:
	//   1. Establishes connectivity baseline (iptables) or initializes CRD (networkfence)
	//   2. Applies the fault injection rules/resources
	//   3. Waits and validates that fault injection is actually blocking/active
	//   4. Allows storage layer time to detect the network state
	//
	// For iptables:
	//   - Deploys DaemonSet with iptables rules
	//   - Validates connectivity is blocked (timeout: 120s, poll: 2s)
	//   - Allows 5s for iptables rules to settle
	//
	// For networkfence:
	//   - Creates NetworkFenceClass and NetworkFence CRDs
	//   - Validates Status.Result == Succeeded (timeout: 60s)
	//   - Allows 5s for storage to detect fence
	//
	// For none (no-op):
	//   - Returns immediately without doing anything
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//   - targets: List of target CIDRs/IPs to fence (discovered by test via replication/helpers functions)
	//
	// Returns:
	//   - error: Error if fence application or validation fails
	ApplyFence(ctx context.Context, targets []string) error

	// RemoveFence removes fault injection from the targets and VALIDATES connectivity is restored.
	// This internally:
	//   1. Removes the fault injection rules/resources
	//   2. Waits and validates that fault injection is actually removed
	//   3. Allows storage layer time to detect and recover from the network state
	//
	// For iptables:
	//   - Removes iptables REJECT rules for all fenced CIDRs
	//   - Validates connectivity is restored (timeout: 120s, poll: 2s)
	//   - Allows 5s for iptables rules to be removed
	//
	// For networkfence:
	//   - Updates NetworkFence spec.fenceState = Unfenced
	//   - Validates Status.Result == Succeeded and fenceState == Unfenced (timeout: 60s)
	//   - Allows 5s for storage to detect unfence
	//
	// For none (no-op):
	//   - Returns immediately without doing anything
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//
	// Returns:
	//   - error: Error if unfence operation or validation fails
	RemoveFence(ctx context.Context) error

	// IsSupported checks if fault injection is supported in the current cluster environment.
	// Returns (false, reason) if the required capabilities are not available, allowing tests
	// to skip gracefully with a meaningful message.
	//
	// Returns:
	//   - supported: true if fault injection can proceed, false if cluster lacks required capabilities
	//   - reason: Human-readable explanation if not supported (e.g., "NetworkFence requires CRDs")
	IsSupported(ctx context.Context) (bool, string)

	// Cleanup performs unified cleanup of all resources created by the handler.
	// This removes DaemonSets (iptables), CRDs (networkfence), and other temporary resources,
	// ensuring the cluster is clean after test execution.
	//
	// For iptables:
	//   - Removes all active iptables REJECT rules
	//   - Deletes DaemonSet and ConfigMaps
	//   - Performs surgical cleanup to not affect other cluster rules
	//
	// For networkfence:
	//   - Deletes NetworkFence CRs (triggers unfencing)
	//   - Deletes NetworkFenceClass CRs
	//
	// For none (no-op):
	//   - Returns immediately without doing anything
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//
	// Returns:
	//   - error: Error if cleanup fails (should not block test cleanup, logged for investigation)
	Cleanup(ctx context.Context) error

	// DiscoverFenceTargets discovers fence targets using injector-aware logic.
	// For iptables in full-DR: uses GetFenceCIDRsForFaultInjectionPeer (peer cluster backends)
	// For iptables single-cluster: uses GetFenceCIDRsForFaultInjection (local backends)
	// For networkfence: uses GetFenceCIDRs (node IPs from CSIAddonsNode status)
	// Falls back through FENCE_CIDRS env → service backends → auto-discovery → none
	// Returns empty list if nothing found (tests should skip).
	//
	// Note: This method requires the handler to be initialized with access to discover targets.
	// Handlers will use callback functions passed during handler construction, or call discovery
	// functions from the replication test helpers directly (via dynamic import/interface).
	DiscoverFenceTargets(ctx context.Context) []string

	// DiscoverFenceTargetsForClient discovers fence targets for a specific client.
	// This method accepts a client parameter to discover targets for that client (rather than
	// automatically choosing based on full-DR mode detection or internal state).
	// Useful when you need to discover targets for the peer cluster, local cluster, or a specific instance.
	//
	// For iptables: Discovers backend service IPs/CIDRs for the given client
	// For networkfence: Discovers node IPs for the given client
	// Falls back through FENCE_CIDRS env → service backends/node IPs → auto-discovery → none
	// Returns empty list if nothing found.
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//   - client: Kubernetes client for the cluster to discover targets from
	//
	// Returns:
	//   - []string: List of target CIDRs/IPs for the given client's cluster
	DiscoverFenceTargetsForClient(ctx context.Context, client client.Client) []string
}

// NewFaultInjectionHandler creates a new FaultInjectionHandler based on the E2E_FAULT_INJECTOR
// environment variable. Injector type is auto-detected from the environment, eliminating the need
// to pass it as a parameter.
//
// Environment variable values:
//   - "iptables": Uses iptables-based network blocking (default if unset)
//   - "networkfence": Uses NetworkFence CRDs (requires CSI-Addons controller)
//   - "none": Disables fault injection (handler methods become no-ops, IsSupported returns false)
//
// Parameters:
//   - ctx: Context for initialization
//   - config: FaultInjectionConfig with Kubernetes client and provider parameters
//
// Returns:
//   - FaultInjectionHandler: The initialized handler
//   - error: Error if handler creation fails
func NewFaultInjectionHandler(ctx context.Context, config FaultInjectionConfig) (FaultInjectionHandler, error) {
	// Determine injector type from environment
	injectorType := strings.ToLower(strings.TrimSpace(os.Getenv(EnvFaultInjector)))
	if injectorType == "" {
		injectorType = string(FaultInjectorIptables) // Default to iptables
	}

	switch injectorType {
	case string(FaultInjectorIptables):
		return NewIptablesHandler(ctx, config)
	case string(FaultInjectorNetworkFence):
		return NewNetworkFenceHandler(ctx, config)
	case string(FaultInjectorNone):
		return NewNoOpHandler(ctx, config)
	default:
		return nil, fmt.Errorf("unsupported fault injector type: %s (supported: iptables, networkfence, none)", injectorType)
	}
}

// NoOpHandler is a handler that performs no fault injection operations.
// Used when E2E_FAULT_INJECTOR=none, allowing tests to run without network partition scenarios.
type NoOpHandler struct {
	config FaultInjectionConfig
}

// NewNoOpHandler creates a new no-op handler.
func NewNoOpHandler(ctx context.Context, config FaultInjectionConfig) (FaultInjectionHandler, error) {
	return &NoOpHandler{config: config}, nil
}

// ApplyFence for no-op handler is a no-op; returns success immediately.
func (h *NoOpHandler) ApplyFence(ctx context.Context, targets []string) error {
	return nil
}

// RemoveFence for no-op handler is a no-op; returns success immediately.
func (h *NoOpHandler) RemoveFence(ctx context.Context) error {
	return nil
}

// IsSupported for no-op handler returns false with a message to skip the test.
func (h *NoOpHandler) IsSupported(ctx context.Context) (bool, string) {
	return false, "fault injection disabled (E2E_FAULT_INJECTOR=none)"
}

// Cleanup for no-op handler is a no-op; returns success immediately.
func (h *NoOpHandler) Cleanup(ctx context.Context) error {
	return nil
}

// DiscoverFenceTargets for no-op handler is a no-op; returns empty list (causing test to skip).
func (h *NoOpHandler) DiscoverFenceTargets(ctx context.Context) []string {
	return nil
}

// DiscoverFenceTargetsForClient for no-op handler is a no-op; returns empty list (causing test to skip).
func (h *NoOpHandler) DiscoverFenceTargetsForClient(ctx context.Context, client client.Client) []string {
	return nil
}
