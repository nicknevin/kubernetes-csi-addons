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
	"net"
	"os"
	"strings"
	"time"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkFenceFaultProvider implements PeerFenceProvider using NetworkFence CRDs.
// This provider wraps the existing NetworkFence functionality to provide compatibility
// with the new fault injection framework.
type NetworkFenceFaultProvider struct {
	config         FaultInjectionConfig
	provisioner    string
	isSupported    bool
	checkedSupport bool

	// Track active NetworkFence resources for cleanup
	activeFences []networkFenceResource
}

type networkFenceResource struct {
	networkFenceClass *csiaddonsv1alpha1.NetworkFenceClass
	networkFence      *csiaddonsv1alpha1.NetworkFence
	targetCIDR        string
}

// NewNetworkFenceFaultProvider creates a new NetworkFence CRD-based fault injection provider.
func NewNetworkFenceFaultProvider(config FaultInjectionConfig) (PeerFenceProvider, error) {
	// Get provisioner from params, environment, or test configuration
	provisioner := config.ProviderParams["provisioner"]
	if provisioner == "" {
		// Try to get from environment variables used in tests
		if p := os.Getenv("CSI_PROVISIONER"); p != "" {
			provisioner = p
		} else if p := os.Getenv("CSI_DRIVER_NAME"); p != "" {
			provisioner = p
		} else if p := os.Getenv("REPLICATION_SECRET_NAME"); p != "" {
			// If replication secret is set, try common Rook provisioner name
			provisioner = "rook-ceph.rbd.csi.ceph.com"
		}
	}

	return &NetworkFenceFaultProvider{
		config:       config,
		provisioner:  provisioner,
		activeFences: make([]networkFenceResource, 0),
	}, nil
}

func (p *NetworkFenceFaultProvider) IsSupported(ctx context.Context) bool {
	if p.checkedSupport {
		return p.isSupported
	}

	// Use the existing HasNetworkFenceSupport function
	// This requires importing from the replication package - we'll use a direct implementation for now
	p.isSupported = p.hasNetworkFenceSupport(ctx)
	p.checkedSupport = true

	return p.isSupported
}

// hasNetworkFenceSupport checks if NetworkFence CRDs are available and CSI driver supports it
// This is based on the existing HasNetworkFenceSupport function from replication/helpers.go
func (p *NetworkFenceFaultProvider) hasNetworkFenceSupport(ctx context.Context) bool {
	if p.provisioner == "" {
		Logf("[DEBUG]", "NetworkFence not supported: no provisioner configured")
		return false
	}

	// Check if NetworkFence and NetworkFenceClass CRDs are installed
	nfList := &csiaddonsv1alpha1.NetworkFenceList{}
	if err := p.config.Client.List(ctx, nfList); err != nil {
		Logf("[ERROR]", "NetworkFence not supported: failed to list NetworkFence CRDs: %v", err)
		return false
	}
	nfcList := &csiaddonsv1alpha1.NetworkFenceClassList{}
	if err := p.config.Client.List(ctx, nfcList); err != nil {
		Logf("[ERROR]", "NetworkFence not supported: failed to list NetworkFenceClass CRDs: %v", err)
		return false
	}

	// Check if CSIAddonsNode advertises NETWORK_FENCE capability
	list := &csiaddonsv1alpha1.CSIAddonsNodeList{}
	if err := p.config.Client.List(ctx, list); err != nil {
		Logf("[ERROR]", "NetworkFence not supported: failed to list CSIAddonsNodes: %v", err)
		return false
	}

	for i := range list.Items {
		node := &list.Items[i]
		if node.Spec.Driver.Name != p.provisioner {
			continue
		}
		if node.Status.State != csiaddonsv1alpha1.CSIAddonsNodeStateConnected {
			Logf("[DEBUG]", "CSIAddonsNode %s for provisioner %s not connected (state: %s)", node.Name, p.provisioner, node.Status.State)
			continue
		}
		for _, cap := range node.Status.Capabilities {
			if cap == "network_fence.NETWORK_FENCE" {
				Logf("[INFO]", "NetworkFence supported: found NETWORK_FENCE capability on CSIAddonsNode %s", node.Name)
				return true
			}
		}
	}
	Logf("[DEBUG]", "NetworkFence not supported: no CSIAddonsNode with NETWORK_FENCE capability found for provisioner %s", p.provisioner)
	return false
}

func (p *NetworkFenceFaultProvider) FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	if !p.IsSupported(ctx) {
		Logf("[ERROR]", "Cannot fence IP %s: NetworkFence not supported in this cluster", targetCIDR)
		return fmt.Errorf("NetworkFence not supported")
	}

	// Validate CIDR format before creating resources
	if _, _, err := net.ParseCIDR(targetCIDR); err != nil {
		// Also try parsing as single IP
		if ip := net.ParseIP(targetCIDR); ip == nil {
			Logf("[ERROR]", "Cannot fence %s: invalid IP address or CIDR format", targetCIDR)
			return fmt.Errorf("invalid IP address or CIDR format: %s", targetCIDR)
		}
		// Convert single IP to CIDR notation
		if strings.Contains(targetCIDR, ":") {
			targetCIDR = targetCIDR + "/128" // IPv6
		} else {
			targetCIDR = targetCIDR + "/32" // IPv4
		}
	}

	// Get secrets from params or environment
	secretName := params["secretName"]
	secretNamespace := params["secretNamespace"]
	if secretName == "" {
		secretName = os.Getenv("REPLICATION_SECRET_NAME")
	}
	if secretNamespace == "" {
		secretNamespace = os.Getenv("REPLICATION_SECRET_NAMESPACE")
	}

	if secretName == "" || secretNamespace == "" {
		Logf("[ERROR]", "Cannot fence IP %s: missing required NetworkFence secrets (secretName=%s, secretNamespace=%s)", targetCIDR, secretName, secretNamespace)
		return fmt.Errorf("missing required NetworkFence secrets: secretName=%s, secretNamespace=%s", secretName, secretNamespace)
	}

	// Create unique names for NetworkFence resources
	nfcName := fmt.Sprintf("nfc-fault-%s", generateUniqueID())
	nfName := fmt.Sprintf("nf-fault-%s", generateUniqueID())

	// Create NetworkFenceClass
	nfc := p.createNetworkFenceClass(ctx, nfcName, secretName, secretNamespace)

	// Create NetworkFence with the target CIDR
	cidrs := []string{targetCIDR}
	nf := p.createNetworkFence(ctx, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)

	// Wait for fence to be applied
	if err := p.waitForNetworkFenceResult(ctx, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded); err != nil {
		Logf("[ERROR]", "Failed to fence IP %s using NetworkFence %s: %v", targetCIDR, nf.Name, err)
		return fmt.Errorf("failed to wait for NetworkFence to complete: %w", err)
	}

	Logf("[INFO]", "Successfully fenced IP %s using NetworkFence %s", targetCIDR, nf.Name)

	// Track the resources for cleanup
	p.activeFences = append(p.activeFences, networkFenceResource{
		networkFenceClass: nfc,
		networkFence:      nf,
		targetCIDR:        targetCIDR,
	})

	return nil
}

func (p *NetworkFenceFaultProvider) UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	if !p.IsSupported(ctx) {
		Logf("[ERROR]", "Cannot unfence IP %s: NetworkFence not supported in this cluster", targetCIDR)
		return fmt.Errorf("NetworkFence not supported")
	}

	// Find the NetworkFence resource for this CIDR
	var targetResource *networkFenceResource
	for i := range p.activeFences {
		if p.activeFences[i].targetCIDR == targetCIDR {
			targetResource = &p.activeFences[i]
			break
		}
	}

	if targetResource == nil {
		Logf("[ERROR]", "Cannot unfence IP %s: no active NetworkFence found for this CIDR", targetCIDR)
		return fmt.Errorf("no active NetworkFence found for CIDR %s", targetCIDR)
	}

	// Update the NetworkFence to Unfenced state
	nf := targetResource.networkFence
	key := client.ObjectKeyFromObject(nf)
	if err := p.config.Client.Get(ctx, key, nf); err != nil {
		Logf("[ERROR]", "Failed to get NetworkFence %s for unfencing IP %s: %v", nf.Name, targetCIDR, err)
		return fmt.Errorf("failed to get NetworkFence: %w", err)
	}

	nf.Spec.FenceState = csiaddonsv1alpha1.Unfenced
	if err := p.config.Client.Update(ctx, nf); err != nil {
		Logf("[ERROR]", "Failed to update NetworkFence %s to unfenced state for IP %s: %v", nf.Name, targetCIDR, err)
		return fmt.Errorf("failed to update NetworkFence to unfenced: %w", err)
	}

	// Wait for unfence to complete
	if err := p.waitForNetworkFenceResult(ctx, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded); err != nil {
		Logf("[ERROR]", "Failed to wait for NetworkFence %s unfence operation to complete for IP %s: %v", nf.Name, targetCIDR, err)
		return fmt.Errorf("failed to wait for NetworkFence unfence to complete: %w", err)
	}

	Logf("[INFO]", "Successfully unfenced IP %s using NetworkFence %s", targetCIDR, nf.Name)
	return nil
}

// EstablishConnectivityBaseline is a no-op for NetworkFence: fencing is enforced in the storage
// layer; reachability for replication is observed on VolumeReplication and related CRs, not via Jobs.
func (p *NetworkFenceFaultProvider) EstablishConnectivityBaseline(ctx context.Context, targetCIDR string) (*ConnectivityBaseline, error) {
	_ = ctx
	_ = targetCIDR
	return nil, nil
}

// VerifyConnectivity compares expected fence state to the NetworkFence CR (and tracked resources).
// baseline is ignored—packet-level connectivity tests apply only to the iptables injector.
func (p *NetworkFenceFaultProvider) VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool, baseline *ConnectivityBaseline) (bool, error) {
	_ = baseline
	if !p.IsSupported(ctx) {
		Logf("[ERROR]", "Cannot verify connectivity for IP %s: NetworkFence not supported", targetCIDR)
		return false, fmt.Errorf("NetworkFence not supported")
	}

	// Find the NetworkFence resource for this CIDR
	var targetResource *networkFenceResource
	for i := range p.activeFences {
		if p.activeFences[i].targetCIDR == targetCIDR {
			targetResource = &p.activeFences[i]
			break
		}
	}

	if targetResource == nil {
		// No active fence for this CIDR - connectivity should be available
		Logf("[DEBUG]", "No active NetworkFence found for IP %s, connectivity should be available", targetCIDR)
		return !expectedFenced, nil
	}

	// Check the current NetworkFence status
	nf := targetResource.networkFence
	key := client.ObjectKeyFromObject(nf)
	if err := p.config.Client.Get(ctx, key, nf); err != nil {
		Logf("[ERROR]", "Failed to get NetworkFence %s status for connectivity verification of IP %s: %v", nf.Name, targetCIDR, err)
		return false, fmt.Errorf("failed to get NetworkFence status: %w", err)
	}

	// Verify the fence state matches expectations
	currentlyFenced := nf.Spec.FenceState == csiaddonsv1alpha1.Fenced &&
		nf.Status.Result == csiaddonsv1alpha1.FencingOperationResultSucceeded

	Logf("[DEBUG]", "NetworkFence %s status for IP %s: FenceState=%s, Result=%s, CurrentlyFenced=%t, ExpectedFenced=%t",
		nf.Name, targetCIDR, nf.Spec.FenceState, nf.Status.Result, currentlyFenced, expectedFenced)

	return currentlyFenced == expectedFenced, nil
}

func (p *NetworkFenceFaultProvider) Cleanup(ctx context.Context) error {
	var cleanupErrors []string

	Logf("[INFO]", "Cleaning up %d active NetworkFence resources", len(p.activeFences))

	// Clean up all active NetworkFence resources
	for i := range p.activeFences {
		resource := &p.activeFences[i]

		// Try to unfence if currently fenced, but don't fail cleanup on error
		if err := p.UnfenceIP(ctx, resource.targetCIDR, nil); err != nil {
			Logf("[WARN]", "UnfenceIP failed (may be expected if resource is already fenced or invalid): %v", err)
			// Don't add to errors - unfencing failure should not block deletion
		}

		// Delete NetworkFence (with force delete capability for stuck resources)
		if resource.networkFence != nil {
			if err := p.deleteNetworkFence(ctx, resource.networkFence); err != nil {
				Logf("[ERROR]", "Failed to delete NetworkFence %s during cleanup: %v", resource.networkFence.Name, err)
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("failed to delete NetworkFence %s: %v", resource.networkFence.Name, err))
			}
		}

		// Delete NetworkFenceClass (may fail if NetworkFence still has it referenced)
		if resource.networkFenceClass != nil {
			if err := p.config.Client.Delete(ctx, resource.networkFenceClass); err != nil && !errors.IsNotFound(err) {
				Logf("[WARN]", "Failed to delete NetworkFenceClass %s during cleanup: %v (may retry after NetworkFence deletion)", resource.networkFenceClass.Name, err)
				// Try again with finalizer removal
				retryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				nfc := resource.networkFenceClass
				if len(nfc.Finalizers) > 0 {
					nfc.Finalizers = nil
					if err2 := p.config.Client.Update(retryCtx, nfc); err2 == nil {
						_ = p.config.Client.Delete(retryCtx, nfc)
					}
				}
				cancel()
			}
		}
	}

	// Clear the active fences
	p.activeFences = make([]networkFenceResource, 0)

	if len(cleanupErrors) > 0 {
		Logf("[WARN]", "NetworkFence cleanup completed with %d errors: %s", len(cleanupErrors), strings.Join(cleanupErrors, "; "))
		// Don't return error - cleanup should be best-effort
		return nil
	}

	Logf("[INFO]", "NetworkFence cleanup completed successfully")
	return nil
}

func (p *NetworkFenceFaultProvider) GetProviderType() FaultInjectorType {
	return FaultInjectorNetworkFence
}

// Helper methods for NetworkFence operations

// generateUniqueID creates a simple unique identifier for resource names
func generateUniqueID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}

// createNetworkFenceClass creates a NetworkFenceClass with the given parameters
func (p *NetworkFenceFaultProvider) createNetworkFenceClass(ctx context.Context, name, secretName, secretNamespace string) *csiaddonsv1alpha1.NetworkFenceClass {
	params := map[string]string{
		"csiaddons.openshift.io/networkfence-secret-name":      secretName,
		"csiaddons.openshift.io/networkfence-secret-namespace": secretNamespace,
	}

	// Add clusterID for Ceph CSI if available
	if clusterID := os.Getenv("FENCE_CLUSTER_ID"); clusterID != "" {
		params["clusterID"] = clusterID
	} else if strings.Contains(strings.ToLower(p.provisioner), "ceph") {
		// For Rook/Ceph, use namespace as clusterID
		params["clusterID"] = secretNamespace
	}

	nfc := &csiaddonsv1alpha1.NetworkFenceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: csiaddonsv1alpha1.NetworkFenceClassSpec{
			Provisioner: p.provisioner,
			Parameters:  params,
		},
	}

	err := p.config.Client.Create(ctx, nfc)
	if err != nil {
		Logf("[ERROR]", "Failed to create NetworkFenceClass %s: %v", name, err)
		panic(fmt.Errorf("failed to create NetworkFenceClass: %w", err))
	}

	Logf("[INFO]", "Created NetworkFenceClass %s for provisioner %s", name, p.provisioner)
	return nfc
}

// createNetworkFence creates a NetworkFence resource
func (p *NetworkFenceFaultProvider) createNetworkFence(ctx context.Context, name, networkFenceClassName string, cidrs []string, fenceState csiaddonsv1alpha1.FenceState) *csiaddonsv1alpha1.NetworkFence {
	nf := &csiaddonsv1alpha1.NetworkFence{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: csiaddonsv1alpha1.NetworkFenceSpec{
			NetworkFenceClassName: networkFenceClassName,
			FenceState:            fenceState,
			Cidrs:                 cidrs,
		},
	}

	err := p.config.Client.Create(ctx, nf)
	if err != nil {
		Logf("[ERROR]", "Failed to create NetworkFence %s with CIDRs %v and state %s: %v", name, cidrs, fenceState, err)
		panic(fmt.Errorf("failed to create NetworkFence: %w", err))
	}

	Logf("[INFO]", "Created NetworkFence %s with CIDRs %v and state %s", name, cidrs, fenceState)
	return nf
}

// waitForNetworkFenceResult waits for the NetworkFence to reach the expected result
func (p *NetworkFenceFaultProvider) waitForNetworkFenceResult(ctx context.Context, nf *csiaddonsv1alpha1.NetworkFence, expectedResult csiaddonsv1alpha1.FencingOperationResult) error {
	key := client.ObjectKeyFromObject(nf)

	Logf("[DEBUG]", "Waiting for NetworkFence %s to reach result %s", nf.Name, expectedResult)

	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(pollCtx context.Context) (bool, error) {
		err := p.config.Client.Get(pollCtx, key, nf)
		if err != nil {
			Logf("[DEBUG]", "Failed to get NetworkFence %s status while waiting: %v", nf.Name, err)
			return false, nil // Keep trying
		}
		Logf("[DEBUG]", "NetworkFence %s current result: %s (waiting for %s)", nf.Name, nf.Status.Result, expectedResult)
		return nf.Status.Result == expectedResult, nil
	})
}

// deleteNetworkFence removes a NetworkFence resource with proper unfencing first
func (p *NetworkFenceFaultProvider) deleteNetworkFence(ctx context.Context, nf *csiaddonsv1alpha1.NetworkFence) error {
	// First ensure it's unfenced
	key := client.ObjectKeyFromObject(nf)
	if err := p.config.Client.Get(ctx, key, nf); err != nil {
		if errors.IsNotFound(err) {
			return nil // Already deleted
		}
		return err
	}

	// Update to unfenced state if needed
	if nf.Spec.FenceState == csiaddonsv1alpha1.Fenced {
		Logf("[DEBUG]", "Unfencing NetworkFence %s before deletion", nf.Name)
		nf.Spec.FenceState = csiaddonsv1alpha1.Unfenced
		if err := p.config.Client.Update(ctx, nf); err != nil {
			Logf("[ERROR]", "Failed to unfence NetworkFence %s before deletion: %v", nf.Name, err)
			// Continue anyway - not fatal for deletion
		} else {
			// Only wait if update succeeded; use shorter timeout for invalid CIDRs
			if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, true, func(pollCtx context.Context) (bool, error) {
				err := p.config.Client.Get(pollCtx, key, nf)
				if err != nil {
					return false, nil // Keep trying
				}
				return nf.Status.Result == csiaddonsv1alpha1.FencingOperationResultSucceeded ||
					nf.Status.Result == csiaddonsv1alpha1.FencingOperationResultFailed, nil
			}); err != nil {
				Logf("[WARN]", "NetworkFence %s unfencing timed out or failed - will force delete", nf.Name)
			}
		}
	}

	// Remove finalizer if present
	if len(nf.Finalizers) > 0 {
		Logf("[DEBUG]", "Removing finalizers from NetworkFence %s before deletion", nf.Name)
		nf.Finalizers = nil
		if err := p.config.Client.Update(ctx, nf); err != nil {
			Logf("[ERROR]", "Failed to remove finalizers from NetworkFence %s: %v", nf.Name, err)
			// Continue anyway - try force delete
		}
	}

	// Delete the resource with propagation policy
	Logf("[INFO]", "Deleting NetworkFence %s", nf.Name)
	var gracePeriod int64 = 0
	err := p.config.Client.Delete(ctx, nf, client.GracePeriodSeconds(gracePeriod))
	if err != nil {
		// If deletion fails, try to refresh and force delete
		if err2 := p.config.Client.Get(ctx, key, nf); err2 == nil && nf.DeletionTimestamp != nil {
			Logf("[WARN]", "NetworkFence %s stuck in deletion, removing finalizers", nf.Name)
			nf.Finalizers = nil
			if err3 := p.config.Client.Update(ctx, nf); err3 != nil {
				Logf("[ERROR]", "Failed to remove finalizers from stuck NetworkFence %s: %v", nf.Name, err3)
			}
		}
		Logf("[ERROR]", "Failed to delete NetworkFence %s: %v", nf.Name, err)
		return err
	}
	return nil
}
