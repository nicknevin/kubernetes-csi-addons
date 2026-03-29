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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FaultInjectorType represents the type of fault injection mechanism
type FaultInjectorType string

const (
	// FaultInjectorNetworkFence uses NetworkFence CRDs (existing implementation)
	FaultInjectorNetworkFence FaultInjectorType = "networkfence"

	// FaultInjectorIptables uses iptables-based network blocking (new implementation)
	FaultInjectorIptables FaultInjectorType = "iptables"

	// FaultInjectorNone disables fault injection for compatibility testing
	FaultInjectorNone FaultInjectorType = "none"

	// EnvFaultInjector is the environment variable to select fault injection type
	EnvFaultInjector = "E2E_FAULT_INJECTOR"
)

// PeerFenceProvider defines the interface for network fault injection mechanisms.
// This interface abstracts different backends (NetworkFence CRDs, iptables, etc.)
// to provide a unified API for peer network isolation in E2E tests.
type PeerFenceProvider interface {
	// FenceIP blocks network connectivity to the specified CIDR or IP address.
	// This simulates network partition scenarios for disaster recovery testing.
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//   - targetCIDR: IP address or CIDR block to fence (e.g., "192.168.1.10/32", "10.0.0.0/24")
	//   - params: Optional parameters specific to the backend implementation
	//
	// Returns error if fencing operation fails.
	FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error

	// UnfenceIP restores network connectivity to the specified CIDR or IP address.
	// This removes the network partition to allow recovery testing.
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//   - targetCIDR: IP address or CIDR block to unfence (must match FenceIP call)
	//   - params: Optional parameters specific to the backend implementation
	//
	// Returns error if unfencing operation fails.
	UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error

	// VerifyConnectivity checks if network connectivity to the target is available.
	// This verifies the current fence state and validates fence/unfence operations.
	//
	// Parameters:
	//   - ctx: Context for the operation with timeout/cancellation
	//   - targetCIDR: IP address or CIDR block to test connectivity
	//   - expectedFenced: true if the target should be fenced (unreachable)
	//
	// Returns:
	//   - bool: true if connectivity matches expected state
	//   - error: error during connectivity verification (nil if successful)
	VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool) (bool, error)

	// IsSupported returns true if this fault injection mechanism is available in the cluster.
	// This allows tests to skip gracefully when the required capabilities are not available.
	IsSupported(ctx context.Context) bool

	// Cleanup performs any necessary cleanup operations for the provider.
	// This should remove any persistent state, DaemonSets, or resources created by the provider.
	Cleanup(ctx context.Context) error

	// GetProviderType returns the type of this fault injection provider.
	GetProviderType() FaultInjectorType
}

// FaultInjectionConfig holds configuration for fault injection providers.
type FaultInjectionConfig struct {
	// Type specifies which fault injection mechanism to use
	Type FaultInjectorType

	// Client is the Kubernetes client for cluster operations
	Client client.Client

	// Namespace for provider resources (DaemonSets, etc.)
	Namespace string

	// ProviderParams contains provider-specific configuration
	ProviderParams map[string]string
}

// NewFaultInjectionProvider creates a new fault injection provider based on configuration.
// The provider type is determined by the E2E_FAULT_INJECTOR environment variable,
// with fallback to NetworkFence for backward compatibility.
//
// Environment variable values:
//   - "networkfence": Uses NetworkFence CRDs (requires CSI-Addons controller)
//   - "iptables": Uses iptables-based network blocking (requires privileged DaemonSets)
//   - "none": Disables fault injection (tests will skip network partition scenarios)
//
// Parameters:
//   - config: Configuration including Kubernetes client and optional parameters
//
// Returns:
//   - PeerFenceProvider: The initialized provider
//   - error: Error if provider creation fails
func NewFaultInjectionProvider(config FaultInjectionConfig) (PeerFenceProvider, error) {
	// Determine provider type from environment or config
	providerType := config.Type
	if providerType == "" {
		envType := strings.ToLower(strings.TrimSpace(os.Getenv(EnvFaultInjector)))
		switch envType {
		case string(FaultInjectorNetworkFence):
			providerType = FaultInjectorNetworkFence
		case string(FaultInjectorIptables):
			providerType = FaultInjectorIptables
		case string(FaultInjectorNone):
			providerType = FaultInjectorNone
		case "":
			// Default to NetworkFence for backward compatibility
			providerType = FaultInjectorNetworkFence
		default:
			return nil, fmt.Errorf("unsupported fault injector type: %s (supported: networkfence, iptables, none)", envType)
		}
	}

	// Create provider based on type
	switch providerType {
	case FaultInjectorNetworkFence:
		return NewNetworkFenceFaultProvider(config)
	case FaultInjectorIptables:
		return NewIptablesFaultProvider(config)
	case FaultInjectorNone:
		return NewNoOpFaultProvider(config)
	default:
		return nil, fmt.Errorf("unknown fault injector type: %s", providerType)
	}
}

// GetFaultInjectorTypeFromEnv returns the fault injector type from environment variables.
// Used for logging and test skip decisions.
func GetFaultInjectorTypeFromEnv() FaultInjectorType {
	envType := strings.ToLower(strings.TrimSpace(os.Getenv(EnvFaultInjector)))
	switch envType {
	case string(FaultInjectorNetworkFence):
		return FaultInjectorNetworkFence
	case string(FaultInjectorIptables):
		return FaultInjectorIptables
	case string(FaultInjectorNone):
		return FaultInjectorNone
	default:
		return FaultInjectorNetworkFence // Default for backward compatibility
	}
}

// NoOpFaultProvider implements PeerFenceProvider but performs no actual fault injection.
// This is used when fault injection is disabled (E2E_FAULT_INJECTOR=none) to allow
// tests to run without network partition scenarios.
type NoOpFaultProvider struct {
	config FaultInjectionConfig
}

// NewNoOpFaultProvider creates a new no-op fault injection provider.
func NewNoOpFaultProvider(config FaultInjectionConfig) (PeerFenceProvider, error) {
	return &NoOpFaultProvider{config: config}, nil
}

func (p *NoOpFaultProvider) FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	// No-op: pretend fencing succeeded
	return nil
}

func (p *NoOpFaultProvider) UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	// No-op: pretend unfencing succeeded
	return nil
}

func (p *NoOpFaultProvider) VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool) (bool, error) {
	// No-op: always report connectivity as expected (no actual fencing occurred)
	return true, nil
}

func (p *NoOpFaultProvider) IsSupported(ctx context.Context) bool {
	// No-op provider is always "supported"
	return true
}

func (p *NoOpFaultProvider) Cleanup(ctx context.Context) error {
	// No-op: nothing to clean up
	return nil
}

func (p *NoOpFaultProvider) GetProviderType() FaultInjectorType {
	return FaultInjectorNone
}

// Common timeout values for fault injection operations
const (
	DefaultFenceTimeout   = 30 * time.Second
	DefaultUnfenceTimeout = 30 * time.Second
	DefaultVerifyTimeout  = 10 * time.Second
	DefaultCleanupTimeout = 60 * time.Second
)
