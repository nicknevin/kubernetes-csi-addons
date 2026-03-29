# Fault Injection Framework

This document describes the network fault injection framework implemented for E2E testing in the CSI-Addons project. The framework provides a vendor-agnostic approach to network fencing with multiple backend providers.

## Overview

The fault injection framework allows E2E tests to simulate network failures by blocking IP addresses or CIDR ranges. This is crucial for testing disaster recovery scenarios and replication behavior under network partition conditions.

## Architecture

The framework consists of several key components:

### 1. PeerFenceProvider Interface

The core interface that all fault injection backends must implement:

```go
type PeerFenceProvider interface {
    // FenceIP blocks network access to the specified CIDR
    FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error
    
    // UnfenceIP restores network access to the specified CIDR
    UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error
    
    // VerifyConnectivity tests connectivity to the target and verifies the expected state
    VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool) (bool, error)
    
    // IsSupported returns true if this fault injection mechanism is available in the cluster
    IsSupported(ctx context.Context) bool
    
    // GetProviderType returns the type of this provider
    GetProviderType() FaultInjectorType
    
    // Cleanup removes all resources created by this provider
    Cleanup(ctx context.Context) error
}
```

### 2. Provider Implementations

#### IptablesFaultProvider
- Uses privileged DaemonSets with NET_ADMIN capabilities
- Creates iptables rules to block traffic to specific CIDRs
- Manages rules via ConfigMaps that are mounted and monitored by DaemonSet pods
- Requires cluster support for privileged containers

#### NetworkFenceFaultProvider  
- Uses the existing NetworkFence CRDs (NetworkFence, NetworkFenceClass)
- Integrates with CSIAddonsNode capabilities detection
- Provides backward compatibility with existing NetworkFence functionality

#### NoOpFaultProvider
- Default fallback provider when no specific backend is configured
- Always reports success but performs no actual operations
- Useful for testing the framework itself or when fault injection is not needed

### 3. Factory Function

The `NewFaultInjectionProvider()` function creates the appropriate provider based on:
- Environment variable `E2E_FAULT_INJECTOR` (iptables|networkfence|none)
- Cluster capabilities (privileged DaemonSets, NetworkFence support)
- Automatic fallback to NoOp provider if specified backend is unsupported

## Environment Configuration

### Required Environment Variables

- `E2E_FAULT_INJECTOR`: Specifies which provider to use
  - `iptables` - Use iptables-based fault injection via privileged DaemonSets
  - `networkfence` - Use NetworkFence CRDs (requires CSI-Addons controller)
  - `none` or unset - Use NoOp provider (no actual fault injection)

### Optional Environment Variables (for iptables provider)

- `E2E_IPTABLES_TARGET_NODES`: Comma-separated list of node names where DaemonSets should run
- `E2E_IPTABLES_IMAGE`: Container image to use for iptables DaemonSet (default: alpine:latest)

## Usage in Tests

### Basic Setup

```go
import "github.com/csi-addons/kubernetes-csi-addons/test/e2e/helpers"

// Create provider
config := helpers.FaultInjectionConfig{
    Client:    k8sClient,
    Namespace: "test-namespace",
}

provider, err := helpers.NewFaultInjectionProvider(config)
if err != nil {
    return err
}

// Check if provider is supported in this cluster
if !provider.IsSupported(ctx) {
    Skip("Fault injection not supported in this cluster")
}

// Cleanup resources when done
defer provider.Cleanup(ctx)
```

### Network Fencing Operations

```go
// Block access to a specific IP/CIDR
err := provider.FenceIP(ctx, "192.168.1.100/32", map[string]string{
    "reason": "testing-network-partition",
})

// Verify the IP is blocked
fenced, err := provider.VerifyConnectivity(ctx, "192.168.1.100/32", true)
if err != nil || !fenced {
    // Handle error or unexpected connectivity
}

// Restore access
err = provider.UnfenceIP(ctx, "192.168.1.100/32", nil)

// Verify connectivity is restored
unfenced, err := provider.VerifyConnectivity(ctx, "192.168.1.100/32", false)
```

### Suite-Level Integration

The framework integrates with the BeforeSuite/AfterSuite lifecycle:

```go
var _ = BeforeSuite(func() {
    // Capability detection is performed automatically
    privilegedDaemonSetCached = helpers.HasPrivilegedDaemonSetSupport(ctx, k8sClient)
})

var _ = AfterSuite(func() {
    // Emergency cleanup is handled by provider.Cleanup()
})
```

## Capability Detection

The framework automatically detects cluster capabilities:

### For Iptables Provider
- Tests ability to create privileged DaemonSets with NET_ADMIN capabilities
- Verifies security policies allow privileged containers
- Checks for required node operating system support

### For NetworkFence Provider  
- Examines CSIAddonsNode resources for NetworkFence capability
- Validates NetworkFence CRDs are installed
- Confirms CSI-Addons controller is available

## Implementation Details

### Iptables Provider Technical Details

1. **DaemonSet Creation**: Creates a privileged DaemonSet with:
   - `NET_ADMIN` capability for iptables manipulation
   - Host network access
   - Tolerations for all node taints
   - ConfigMap volume mount for rule management

2. **Rule Management**: Uses ConfigMaps containing shell scripts that:
   - Add/remove iptables rules for OUTPUT chain
   - Use `REJECT --reject-with icmp-host-unreachable` for clean failures
   - Include safety checks (`-C` before `-I`) to avoid duplicate rules

3. **Connectivity Verification**: Deploys temporary ping Jobs to test actual connectivity

### NetworkFence Provider Technical Details

1. **CRD Integration**: Creates NetworkFenceClass and NetworkFence resources
2. **Status Monitoring**: Polls NetworkFence status for completion
3. **Capability Detection**: Uses existing CSIAddonsNode capability checks

## Testing Strategy

The framework includes comprehensive integration tests covering:

- Provider capability detection and fallback behavior
- Basic fence/unfence operations with connectivity verification
- Multiple target handling and cleanup
- Error conditions and invalid input handling
- Environment variable configuration testing

### Running Tests

```bash
# Use iptables provider (requires privileged containers)
E2E_FAULT_INJECTOR=iptables go test ./test/e2e/replication/

# Use NetworkFence provider (requires CSI-Addons controller)
E2E_FAULT_INJECTOR=networkfence go test ./test/e2e/replication/

# Use NoOp provider (no actual fault injection)
E2E_FAULT_INJECTOR=none go test ./test/e2e/replication/
```

## Security Considerations

### Iptables Provider
- Requires privileged container capabilities
- Uses NET_ADMIN for iptables rule manipulation  
- Should only be used in test environments
- DaemonSet pods have broad network access

### NetworkFence Provider
- Uses standard RBAC permissions
- Relies on CSI-Addons controller for actual implementation
- More suitable for production-like testing environments

## Troubleshooting

### Common Issues

1. **"Provider not supported"**
   - Check cluster security policies for privileged containers (iptables)
   - Verify CSI-Addons controller is running (networkfence)
   - Ensure correct environment variable configuration

2. **"DaemonSet creation failed"**
   - Check RBAC permissions for test service account
   - Verify node selectors and tolerations
   - Review security context constraints (OpenShift)

3. **"Connectivity verification failed"**
   - Ensure test cluster has internet access for ping targets
   - Check if firewall rules interfere with test traffic
   - Verify pod networking configuration

### Debugging Commands

```bash
# Check privileged DaemonSet support
kubectl auth can-i create daemonsets --as=system:serviceaccount:test:default

# Examine iptables DaemonSet logs
kubectl logs -n test-namespace -l app=csi-addons-iptables-manager

# Check NetworkFence resources
kubectl get networkfence,networkfenceclass -n test-namespace

# View CSIAddonsNode capabilities
kubectl get csiaddonsnode -o yaml
```

## Future Enhancements

Potential improvements to the framework:

1. **Additional Providers**: Support for other fault injection mechanisms (tc, network policies)
2. **Advanced Rules**: Port-specific filtering, bandwidth limiting, latency injection
3. **Observability**: Metrics and tracing for fault injection operations
4. **Declarative Config**: YAML-based configuration for complex scenarios
5. **Recovery Testing**: Automatic verification of system recovery after unfencing

## Contributing

When adding new providers or modifying existing ones:

1. Implement the `PeerFenceProvider` interface completely
2. Add comprehensive capability detection logic
3. Include proper resource cleanup in `Cleanup()` method
4. Add integration tests covering error conditions
5. Update this documentation with configuration details

For questions or issues related to the fault injection framework, please refer to the project's issue tracker or contact the maintainers.