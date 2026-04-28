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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
)

// NOTE: This file contains discovery functions copied from test/e2e/replication/helpers.go.
// These implementations are used by handlers to discover fault injection targets.
// The original functions remain in replication/helpers.go for backward compatibility with existing tests.
// This avoids circular imports while allowing handlers to use unified discovery logic from L1-E-003.

const (
	pollInterval                   = 2 * time.Second
	fenceCIDRProbeTimeout          = 30 * time.Second
	fenceTargetServicesEnv         = "FENCE_TARGET_SERVICES"
	fencePeerServicesEnv           = "FENCE_PEER_SERVICES"
	fenceAutoEndpointNamespacesEnv = "FENCE_AUTO_ENDPOINT_NAMESPACES"
	fenceAutoDiscoveryMaxCIDRs     = 32
)

// IsFullDRMode checks if full-DR environment is configured (both DR1_CONTEXT and DR2_CONTEXT).
func IsFullDRMode() bool {
	dr1 := strings.TrimSpace(os.Getenv("DR1_CONTEXT"))
	dr2 := strings.TrimSpace(os.Getenv("DR2_CONTEXT"))
	return dr1 != "" && dr2 != ""
}

// DiscoverFenceCIDRsForIptables discovers fence targets for iptables fault injection (single-cluster).
// Order: 1) FENCE_CIDRS env, 2) service/backend IPs (port-aware discovery), 3) auto-discovery → none
func DiscoverFenceCIDRsForIptables(ctx context.Context, c client.Client) []string {
	return GetFenceCIDRsForFaultInjection(ctx, c)
}

// DiscoverFenceCIDRsForIptablesPeer discovers fence targets for full-DR iptables fault injection.
// Backends come from peer cluster; fencing-cluster node IPs are excluded.
func DiscoverFenceCIDRsForIptablesPeer(ctx context.Context, fencingClient, peerClient client.Client) []string {
	return GetFenceCIDRsForFaultInjectionPeer(ctx, fencingClient, peerClient)
}

// DiscoverFenceCIDRsForNetworkFence discovers fence targets for NetworkFence fault injection.
// Order: 1) FENCE_CIDRS, 2) CSIAddonsNode status.networkFenceClientStatus, 3) node InternalIPs → none
func DiscoverFenceCIDRsForNetworkFence(ctx context.Context, c client.Client, provisioner, networkFenceClassName string) []string {
	return GetFenceCIDRs(ctx, c, provisioner, networkFenceClassName)
}

// GetFenceCIDRsForFaultInjection returns CIDRs for the iptables fault injector only (not NetworkFence).
// Order: 1) FENCE_CIDRS env, 2) service/backend IPs (port-aware discovery), 3) auto-discovery from endpoints.
// Uses GetNodeIPsForFencing for consistency with NetworkFence; GetNodeIPsForFencing excludes raw node
// InternalIPs from iptables targets (avoids blocking control-plane traffic).
func GetFenceCIDRsForFaultInjection(ctx context.Context, c client.Client) []string {
	if cidrs := parseFenceCIDRSFromEnv(); len(cidrs) > 0 {
		Logf("[DEBUG]", "GetFenceCIDRsForFaultInjection: Using FENCE_CIDRS env: %v", cidrs)
		return cidrs
	}
	// Delegate to GetNodeIPsForFencing for consistency with L1-E-003 and NetworkFence.
	// It tries: 1) FENCE_TARGET_SERVICES service backends, 2) auto-discovered endpoints from default namespaces
	return GetNodeIPsForFencing(ctx, c)
}

// GetFenceCIDRsForFaultInjectionPeer resolves fence targets for full-DR iptables only (not NetworkFence).
// Backends come from the peer cluster; fencing-cluster node InternalIPs are excluded so the API path is not
// self-fenced. Raw node IPs are never chosen as targets. Order: 1) FENCE_CIDRS, 2) FENCE_PEER_SERVICES or
// FENCE_TARGET_SERVICES on peerClient.
func GetFenceCIDRsForFaultInjectionPeer(ctx context.Context, fencingClient, peerClient client.Client) []string {
	if cidrs := parseFenceCIDRSFromEnv(); len(cidrs) > 0 {
		Logf("[DEBUG]", "GetFenceCIDRsForFaultInjectionPeer: Using FENCE_CIDRS env: %v", cidrs)
		return cidrs
	}
	fencingNodeIPs := collectNodeInternalIPSet(ctx, fencingClient)
	keys := parseFencePeerServicesFromEnv()
	// If FENCE_PEER_SERVICES not set, fall back to FENCE_TARGET_SERVICES
	if len(keys) == 0 {
		keys = parseFenceTargetServicesFromEnv()
	}
	if len(keys) == 0 {
		Logf("[WARN]", "GetFenceCIDRsForFaultInjectionPeer: set %s or %s (namespace/service list) or FENCE_CIDRS",
			fencePeerServicesEnv, fenceTargetServicesEnv)
		return nil
	}
	var merged []string
	for _, key := range keys {
		// Use pod fallback discovery for peer services (handles cases where service doesn't exist)
		ips := collectServiceBackendIPsWithPodFallback(ctx, peerClient, key)
		Logf("[INFO]", "peer fence: %s/%s backend IPs on peer cluster (with pod fallback): %v", key.Namespace, key.Name, ips)
		merged = append(merged, ips...)
	}
	out := filterEndpointIPsToCIDRs(merged, fencingNodeIPs)
	if len(out) > 0 {
		Logf("[INFO]", "GetFenceCIDRsForFaultInjectionPeer: CIDRs after excluding fencing-cluster node InternalIPs: %v", out)
		return capFenceCIDRList(out)
	}
	if len(merged) > 0 {
		Logf("[WARN]", "GetFenceCIDRsForFaultInjectionPeer: all peer backend IPs match a fencing-cluster node InternalIP; nothing left to fence")
	} else {
		Logf("[WARN]", "GetFenceCIDRsForFaultInjectionPeer: no backend IPs from peer services (check Endpoints/EndpointSlices on peer)")
	}
	return nil
}

// GetFenceCIDRs returns CIDRs for the NetworkFence fault injector only (iptables uses GetFenceCIDRsForFaultInjection*).
// Order: 1) FENCE_CIDRS, 2) CSIAddonsNode status.networkFenceClientStatus for networkFenceClassName, 3) node
// InternalIPs as host routes (CSI did not publish CIDRs in time). Iptables never uses this path and never falls
// back to raw node IPs as fence targets. For full-DR, if the NetworkFenceClass lives on c but peer nodes are on
// another cluster, use GetFenceCIDRsWithPeerNodeClient.
func GetFenceCIDRs(ctx context.Context, c client.Client, provisioner, networkFenceClassName string) []string {
	return getFenceCIDRs(ctx, c, provisioner, networkFenceClassName, nil)
}

// GetFenceCIDRsWithPeerNodeClient is like GetFenceCIDRs but, when falling back to node InternalIPs after a CSI
// timeout, uses peerNodeClient for node discovery instead of c. Pass the peer cluster client when c is the
// cluster where you list CSIAddonsNode / create NetworkFenceClass but the fenced peer is the other cluster.
func GetFenceCIDRsWithPeerNodeClient(ctx context.Context, c, peerNodeClient client.Client, provisioner, networkFenceClassName string) []string {
	return getFenceCIDRs(ctx, c, provisioner, networkFenceClassName, peerNodeClient)
}

func getFenceCIDRs(ctx context.Context, c client.Client, provisioner, networkFenceClassName string, peerNodeClient client.Client) []string {
	if cidrs := parseFenceCIDRSFromEnv(); len(cidrs) > 0 {
		Logf("[DEBUG]", "GetFenceCIDRs: Using FENCE_CIDRS env var: %v", cidrs)
		return cidrs
	}

	// Try service discovery BEFORE CSIAddonsNode polling (avoids 30s timeout)
	// This allows NetworkFence to use FENCE_TARGET_SERVICES like iptables does
	nodeIPs := collectNodeInternalIPSet(ctx, c)
	if cidrs := fenceCIDRsFromConfiguredTargetServices(ctx, c, nodeIPs); len(cidrs) > 0 {
		Logf("[INFO]", "GetFenceCIDRs: Found CIDRs from FENCE_TARGET_SERVICES service discovery: %v", cidrs)
		return capFenceCIDRList(cidrs)
	}
	if cidrs := fenceCIDRsFromAutoDiscoveredEndpoints(ctx, c, nodeIPs); len(cidrs) > 0 {
		Logf("[INFO]", "GetFenceCIDRs: Found CIDRs from auto-discovered endpoints: %v", cidrs)
		return capFenceCIDRList(cidrs)
	}

	Logf("[DEBUG]", "GetFenceCIDRs: Service discovery found no CIDRs, checking CSIAddonsNode for networkFenceClientStatus (provisioner=%s, class=%s)", provisioner, networkFenceClassName)
	deadline := time.Now().Add(fenceCIDRProbeTimeout)
	var cidrs []string
	for time.Now().Before(deadline) {
		list := &csiaddonsv1alpha1.CSIAddonsNodeList{}
		err := c.List(ctx, list)
		if err != nil {
			Logf("[DEBUG]", "GetFenceCIDRs: Failed to list CSIAddonsNodes: %v", err)
			time.Sleep(pollInterval)
			continue
		}
		Logf("[DEBUG]", "GetFenceCIDRs: Found %d CSIAddonsNodes", len(list.Items))
		cidrs = nil
		for i := range list.Items {
			node := &list.Items[i]
			Logf("[DEBUG]", "GetFenceCIDRs: Checking CSIAddonsNode %s (driver=%s)", node.Name, node.Spec.Driver.Name)
			if node.Spec.Driver.Name != provisioner {
				continue
			}
			Logf("[DEBUG]", "GetFenceCIDRs: Driver matches, checking networkFenceClientStatus (%d statuses)", len(node.Status.NetworkFenceClientStatus))
			for _, nfcs := range node.Status.NetworkFenceClientStatus {
				Logf("[DEBUG]", "GetFenceCIDRs:   NetworkFenceClass: %s (looking for %s)", nfcs.NetworkFenceClassName, networkFenceClassName)
				if nfcs.NetworkFenceClassName != networkFenceClassName {
					continue
				}
				Logf("[DEBUG]", "GetFenceCIDRs:   Class matches, found %d client details", len(nfcs.ClientDetails))
				for _, detail := range nfcs.ClientDetails {
					cidrs = append(cidrs, detail.Cidrs...)
				}
			}
		}
		if len(cidrs) > 0 {
			Logf("[INFO]", "GetFenceCIDRs: Found CIDRs from CSIAddonsNode: %v", cidrs)
			return cidrs
		}
		Logf("[DEBUG]", "GetFenceCIDRs: No CIDRs found yet, retrying in %v...", pollInterval)
		time.Sleep(pollInterval)
	}
	Logf("[WARN]", "GetFenceCIDRs: Timeout waiting for CSIAddonsNode networkFenceClientStatus for class %q", networkFenceClassName)
	nodeClient := c
	if peerNodeClient != nil {
		nodeClient = peerNodeClient
		Logf("[INFO]", "GetFenceCIDRs: Using peer cluster client for node-IP fallback (not the CSI list client)")
	}
	fallback := collectAllNodeInternalIPCIDRs(ctx, nodeClient)
	if len(fallback) == 0 {
		Logf("[WARN]", "GetFenceCIDRs: Node InternalIP fallback found no nodes; set FENCE_CIDRS explicitly")
		return nil
	}
	Logf("[WARN]", "GetFenceCIDRs: Using node InternalIP fallback CIDRs (driver did not publish client CIDRs in time): %v", fallback)
	return capFenceCIDRList(fallback)
}

// collectAllNodeInternalIPCIDRs returns each node's primary InternalIP as a host route (IPv4 /32, IPv6 /128), sorted.
func collectAllNodeInternalIPCIDRs(ctx context.Context, c client.Client) []string {
	set := collectNodeInternalIPSet(ctx, c)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for ipStr := range set {
		if ipStr == "" {
			continue
		}
		parsed := net.ParseIP(ipStr)
		if parsed == nil {
			continue
		}
		if parsed.To4() != nil {
			out = append(out, fmt.Sprintf("%s/32", ipStr))
		} else {
			out = append(out, fmt.Sprintf("%s/128", ipStr))
		}
	}
	sort.Strings(out)
	return out
}

// GetNodeIPsForFencing resolves iptables fence CIDRs from service backends and auto-discovered Endpoints:
// 1) FENCE_TARGET_SERVICES (Endpoints + EndpointSlice); 2) auto-discovered Endpoints in FENCE_AUTO_ENDPOINT_NAMESPACES.
// Node InternalIPs are excluded from picks; raw node IPs are never used as iptables fence targets (use FENCE_CIDRS to target a node explicitly).
func GetNodeIPsForFencing(ctx context.Context, c client.Client) []string {
	nodeIPs := collectNodeInternalIPSet(ctx, c)
	Logf("[DEBUG]", "GetNodeIPsForFencing: node InternalIPs (excluded from endpoint picks): %v", sortedKeys(nodeIPs))

	if cidrs := fenceCIDRsFromConfiguredTargetServices(ctx, c, nodeIPs); len(cidrs) > 0 {
		return capFenceCIDRList(cidrs)
	}
	if cidrs := fenceCIDRsFromAutoDiscoveredEndpoints(ctx, c, nodeIPs); len(cidrs) > 0 {
		return capFenceCIDRList(cidrs)
	}

	Logf("[WARN]", "GetNodeIPsForFencing: no usable backend IPs after excluding node InternalIPs; set %s or FENCE_CIDRS (iptables does not use raw node IPs as targets)",
		fenceTargetServicesEnv)
	return nil
}

// parseFenceCIDRSFromEnv returns FENCE_CIDRS from environment if set.
func parseFenceCIDRSFromEnv() []string {
	s := strings.TrimSpace(os.Getenv("FENCE_CIDRS"))
	if s == "" {
		return nil
	}
	var cidrs []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			cidrs = append(cidrs, part)
		}
	}
	return cidrs
}

// collectNodeInternalIPSet collects the primary InternalIP for each node.
func collectNodeInternalIPSet(ctx context.Context, c client.Client) map[string]struct{} {
	out := make(map[string]struct{})
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		Logf("[DEBUG]", "collectNodeInternalIPSet: list nodes: %v", err)
		return out
	}
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
				out[addr.Address] = struct{}{}
				break
			}
		}
	}
	return out
}

// sortedKeys returns sorted keys from a map.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseFenceTargetServicesFromEnv returns service keys from FENCE_TARGET_SERVICES env.
func parseFenceTargetServicesFromEnv() []client.ObjectKey {
	return parseFenceNamespaceServiceList(os.Getenv(fenceTargetServicesEnv), fenceTargetServicesEnv)
}

// parseFencePeerServicesFromEnv returns service keys for peer-cluster lookup.
func parseFencePeerServicesFromEnv() []client.ObjectKey {
	s := strings.TrimSpace(os.Getenv(fencePeerServicesEnv))
	if s != "" {
		return parseFenceNamespaceServiceList(s, fencePeerServicesEnv)
	}
	return parseFenceTargetServicesFromEnv()
}

// parseFenceNamespaceServiceList parses "namespace/service,namespace/service,..." format.
func parseFenceNamespaceServiceList(raw, envVar string) []client.ObjectKey {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var keys []client.ObjectKey
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ns, name, ok := strings.Cut(part, "/")
		if !ok {
			Logf("[WARNING]", "%s: skip %q (want namespace/service)", envVar, part)
			continue
		}
		ns, name = strings.TrimSpace(ns), strings.TrimSpace(name)
		if ns == "" || name == "" {
			continue
		}
		keys = append(keys, client.ObjectKey{Namespace: ns, Name: name})
	}
	return keys
}

// ipToFenceCIDR converts plain IP to CIDR host route format.
func ipToFenceCIDR(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() == nil {
		return ip + "/128"
	}
	return ip + "/32"
}

// filterEndpointIPsToCIDRs filters IPs, excluding node IPs and converting to CIDR format.
func filterEndpointIPsToCIDRs(ips []string, nodeIPs map[string]struct{}) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ip := range ips {
		if _, onNode := nodeIPs[ip]; onNode {
			Logf("[DEBUG]", "filterEndpointIPs: skip %s (matches node InternalIP — avoids fencing apiserver/kubelet host)", ip)
			continue
		}
		cidr := ipToFenceCIDR(ip)
		if cidr == "" {
			continue
		}
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		out = append(out, cidr)
	}
	return out
}

// collectServiceBackendIPs collects backend IPs from EndpointSlices labeled for the Service.
func collectServiceBackendIPs(ctx context.Context, c client.Client, key client.ObjectKey) []string {
	seen := make(map[string]struct{})
	var ips []string
	add := func(ip string) {
		if ip == "" {
			return
		}
		if _, ok := seen[ip]; ok {
			return
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	sliceList := &discoveryv1.EndpointSliceList{}
	listOpts := []client.ListOption{
		client.InNamespace(key.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: key.Name},
	}
	if err := c.List(ctx, sliceList, listOpts...); err != nil {
		Logf("[DEBUG]", "collectServiceBackendIPs: list EndpointSlices %s/%s: %v", key.Namespace, key.Name, err)
		return ips
	}
	for i := range sliceList.Items {
		for j := range sliceList.Items[i].Endpoints {
			for _, addr := range sliceList.Items[i].Endpoints[j].Addresses {
				add(addr)
			}
		}
	}
	return ips
}

// collectServiceBackendIPsWithPorts collects backend IPs and service ports from EndpointSlices.
func collectServiceBackendIPsWithPorts(ctx context.Context, c client.Client, key client.ObjectKey) []string {
	sliceList := &discoveryv1.EndpointSliceList{}
	listOpts := []client.ListOption{
		client.InNamespace(key.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: key.Name},
	}
	if err := c.List(ctx, sliceList, listOpts...); err != nil {
		Logf("[DEBUG]", "collectServiceBackendIPsWithPorts: list EndpointSlices %s/%s: %v", key.Namespace, key.Name, err)
		return nil
	}

	var result []string
	seen := make(map[string]struct{})

	for i := range sliceList.Items {
		slice := &sliceList.Items[i]

		// Get the port from EndpointSlice (if available)
		var port int32
		hasPort := false
		if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
			port = *slice.Ports[0].Port
			hasPort = true
		}

		// Add endpoints with port info if available
		for j := range slice.Endpoints {
			for _, addr := range slice.Endpoints[j].Addresses {
				if addr == "" {
					continue
				}

				var pair string
				if hasPort {
					pair = fmt.Sprintf("%s:%d", addr, port)
				} else {
					pair = addr
				}

				if _, ok := seen[pair]; !ok {
					seen[pair] = struct{}{}
					result = append(result, pair)
				}
			}
		}
	}

	return result
}

// ipPortToCIDR converts "IP:port" or plain IP to "IP/MASK:port" format.
func ipPortToCIDR(ipPort string) string {
	// Check if port is present
	if idx := strings.LastIndex(ipPort, ":"); idx > 0 && ipPort[0:idx] != "[" { // [ipv6]:port case
		ip := ipPort[0:idx]
		port := ipPort[idx:] // includes the colon
		cidr := ipToFenceCIDR(ip)
		if cidr == "" {
			return ""
		}
		// Keep port in CIDR format (needed for iptables to avoid blocking entire node)
		return cidr + port
	}

	// No port - just convert IP to CIDR
	return ipToFenceCIDR(ipPort)
}

// filterEndpointIPsWithPortsToCIDRs filters IP:port pairs, excluding node IPs without ports.
// When IP:port is specified (service-specific), allow node IPs because:
// - Ports target specific services, not general node traffic (safe for iptables)
// - NetworkFence operates at storage driver layer, not network layer (node IP doesn't matter)
func filterEndpointIPsWithPortsToCIDRs(ipPorts []string, nodeIPs map[string]struct{}) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ipPort := range ipPorts {
		// Extract IP from "IP:port" format
		ip := ipPort
		hasPort := false
		if idx := strings.LastIndex(ipPort, ":"); idx > 0 && ipPort[0:idx] != "[" {
			ip = ipPort[0:idx]
			hasPort = true
		}

		// Skip if IP matches a node InternalIP, BUT ONLY if there's no port
		// When port is specified (IP:port), it's a service-specific target and safe to use even on nodes
		if !hasPort {
			if _, onNode := nodeIPs[ip]; onNode {
				Logf("[DEBUG]", "filterEndpointIPsWithPorts: skip %s (IP %s matches node InternalIP — avoids fencing apiserver/kubelet host)", ipPort, ip)
				continue
			}
		}

		// Convert to CIDR format
		cidr := ipPortToCIDR(ipPort)
		if cidr == "" {
			continue
		}
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		out = append(out, cidr)
	}
	return out
}

// collectServiceBackendIPsWithPodFallback discovers service backend IPs, with fallback to pod discovery.
// Order: 1) EndpointSlices (service-based discovery), 2) Pod discovery by service-name labels.
// This handles cases where services don't exist but pods do (e.g., rook-ceph-mon, apps deployed directly).
func collectServiceBackendIPsWithPodFallback(ctx context.Context, c client.Client, key client.ObjectKey) []string {
	// Try service backend IPs first (EndpointSlices)
	ips := collectServiceBackendIPs(ctx, c, key)
	if len(ips) > 0 {
		return ips
	}
	// Service has no endpoints; try pod discovery as fallback.
	// Look for pods with label app=<service-name> (common pattern for Rook and other operators).
	podIPs := collectPodIPsByLabel(ctx, c, key.Namespace, map[string]string{"app": key.Name})
	if len(podIPs) > 0 {
		Logf("[DEBUG]", "collectServiceBackendIPsWithPodFallback: service %s/%s has no EndpointSlices; discovered pod IPs via label app=%s: %v",
			key.Namespace, key.Name, key.Name, podIPs)
		return podIPs
	}
	// Additional fallback for specific service name patterns
	// (e.g., "rook-ceph-mon" might have different naming conventions)
	if strings.Contains(key.Name, "rook-ceph-mon") {
		// Try the exact label app=rook-ceph-mon used by all Rook installs
		podIPs := collectPodIPsByLabel(ctx, c, key.Namespace, map[string]string{"app": "rook-ceph-mon"})
		if len(podIPs) > 0 {
			Logf("[DEBUG]", "collectServiceBackendIPsWithPodFallback: discovered rook-ceph-mon pods: %v", podIPs)
			return podIPs
		}
	}
	return nil
}

// collectPodIPsByLabel discovers pod IPs in a namespace matching the given labels.
// Returns the pod IP addresses for running pods with the specified labels.
func collectPodIPsByLabel(ctx context.Context, c client.Client, namespace string, labels map[string]string) []string {
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels(labels),
	}
	if err := c.List(ctx, podList, listOpts...); err != nil {
		Logf("[DEBUG]", "collectPodIPsByLabel: list pods in %s with labels %v: %v", namespace, labels, err)
		return nil
	}

	var ips []string
	seen := make(map[string]struct{})
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Only include running pods with assigned IPs
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			if _, ok := seen[pod.Status.PodIP]; !ok {
				ips = append(ips, pod.Status.PodIP)
				seen[pod.Status.PodIP] = struct{}{}
			}
		}
	}
	return ips
}

// fenceCIDRsFromConfiguredTargetServices discovers fence targets from FENCE_TARGET_SERVICES.
func fenceCIDRsFromConfiguredTargetServices(ctx context.Context, c client.Client, nodeIPs map[string]struct{}) []string {
	keys := parseFenceTargetServicesFromEnv()
	if len(keys) == 0 {
		return nil
	}
	var merged []string
	for _, key := range keys {
		// Try port-aware discovery first
		ipPorts := collectServiceBackendIPsWithPorts(ctx, c, key)
		if len(ipPorts) > 0 {
			Logf("[INFO]", "%s: service %s/%s backend IPs with ports (Endpoints+EndpointSlice): %v", fenceTargetServicesEnv, key.Namespace, key.Name, ipPorts)
			merged = append(merged, ipPorts...)
			continue
		}
		// Fall back to service+pod discovery (service IPs, then pods if service has no endpoints)
		ips := collectServiceBackendIPsWithPodFallback(ctx, c, key)
		if len(ips) > 0 {
			Logf("[INFO]", "%s: service %s/%s backend IPs (Endpoints+EndpointSlice, with pod fallback): %v", fenceTargetServicesEnv, key.Namespace, key.Name, ips)
			merged = append(merged, ips...)
		}
	}
	out := filterEndpointIPsWithPortsToCIDRs(merged, nodeIPs)
	if len(out) > 0 {
		Logf("[INFO]", "%s: fence CIDRs with ports after excluding node InternalIPs: %v", fenceTargetServicesEnv, out)
	} else if len(merged) > 0 {
		Logf("[WARN]", "%s: all backend IPs matched node InternalIPs; nothing to fence from configured services", fenceTargetServicesEnv)
	}
	return out
}

// autoDiscoverFenceEndpointNamespaces returns namespaces to auto-discover endpoints in.
func autoDiscoverFenceEndpointNamespaces() []string {
	s := strings.TrimSpace(os.Getenv(fenceAutoEndpointNamespacesEnv))
	if s == "" {
		return []string{"rook-ceph", "csi-addons-system"}
	}
	var ns []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			ns = append(ns, t)
		}
	}
	if len(ns) == 0 {
		return []string{"rook-ceph", "csi-addons-system"}
	}
	return ns
}

// endpointNameLikelyStorageOrCSI checks if an endpoint name suggests storage or CSI service.
func endpointNameLikelyStorageOrCSI(name string) bool {
	n := strings.ToLower(name)
	switch {
	case n == "kubernetes":
		return false
	case strings.HasPrefix(n, "rook-ceph"):
		return true
	case strings.Contains(n, "csi-addons"):
		return true
	}
	for _, tok := range []string{"mon", "mgr", "ceph", "rbd", "osd", "mds", "nfs", "csi"} {
		if strings.Contains(n, tok) {
			return true
		}
	}
	return false
}

// fenceCIDRsFromAutoDiscoveredEndpoints discovers fence targets from auto-discovered endpoints.
func fenceCIDRsFromAutoDiscoveredEndpoints(ctx context.Context, c client.Client, nodeIPs map[string]struct{}) []string {
	var allIPs []string
	for _, ns := range autoDiscoverFenceEndpointNamespaces() {
		epList := &corev1.EndpointsList{}
		if err := c.List(ctx, epList, client.InNamespace(ns)); err != nil {
			Logf("[DEBUG]", "auto-discover Endpoints in namespace %q: %v", ns, err)
			continue
		}
		for i := range epList.Items {
			ep := &epList.Items[i]
			if len(ep.Subsets) == 0 || !endpointNameLikelyStorageOrCSI(ep.Name) {
				continue
			}
			for _, sub := range ep.Subsets {
				for _, a := range sub.Addresses {
					if a.IP != "" {
						allIPs = append(allIPs, a.IP)
					}
				}
			}
		}
	}
	out := filterEndpointIPsToCIDRs(allIPs, nodeIPs)
	if len(out) > 0 {
		Logf("[INFO]", "GetNodeIPsForFencing: auto-discovered CIDRs from Endpoints (%s=%q): %v",
			fenceAutoEndpointNamespacesEnv, strings.Join(autoDiscoverFenceEndpointNamespaces(), ","), out)
	}
	return out
}

// capFenceCIDRList caps the maximum number of fence CIDRs to avoid excessive rule lists.
func capFenceCIDRList(cidrs []string) []string {
	if len(cidrs) <= fenceAutoDiscoveryMaxCIDRs {
		return cidrs
	}
	Logf("[WARN]", "capping fence CIDR list from %d to %d (set FENCE_CIDRS to be explicit)", len(cidrs), fenceAutoDiscoveryMaxCIDRs)
	return append([]string(nil), cidrs[:fenceAutoDiscoveryMaxCIDRs]...)
}
