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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DaemonSetTestTimeout defines the timeout for privileged DaemonSet capability tests
	DaemonSetTestTimeout = 30 * time.Second

	// DaemonSetTestNamePrefix is the prefix for test DaemonSet names
	DaemonSetTestNamePrefix = "csi-addons-privileged-test"
)

// HasPrivilegedDaemonSetSupport checks if the cluster allows creation of privileged DaemonSets
// with NET_ADMIN capabilities required for iptables-based network fencing.
//
// This function creates a minimal test DaemonSet with privileged security context and NET_ADMIN
// capability, then immediately deletes it. It returns true if the DaemonSet can be created
// successfully, indicating that the cluster security policies allow such workloads.
//
// The test is performed in the kube-system namespace to avoid namespace-specific security
// restrictions that might exist in user namespaces.
func HasPrivilegedDaemonSetSupport(ctx context.Context, c client.Client) bool {
	// Create a test DaemonSet name with timestamp to avoid conflicts
	testName := fmt.Sprintf("%s-%d", DaemonSetTestNamePrefix, time.Now().Unix())
	testNamespace := "kube-system"

	// Create minimal privileged DaemonSet for testing
	testDaemonSet := createTestPrivilegedDaemonSet(testName, testNamespace)

	// Use timeout context for the test
	testCtx, cancel := context.WithTimeout(ctx, DaemonSetTestTimeout)
	defer cancel()

	// Try to create the DaemonSet
	if err := c.Create(testCtx, testDaemonSet); err != nil {
		// Creation failed - privileged DaemonSets not supported
		return false
	}

	// DaemonSet created successfully, clean it up immediately
	// Use background deletion to avoid waiting for pod termination
	deletePolicy := metav1.DeletePropagationBackground
	deleteOptions := &client.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	if err := c.Delete(testCtx, testDaemonSet, deleteOptions); err != nil {
		// Log the error but don't fail the test - creation succeeded
		// The DaemonSet will be cleaned up eventually
	}

	return true
}

// createTestPrivilegedDaemonSet creates a minimal DaemonSet with privileged security context
// and NET_ADMIN capability for testing cluster support.
func createTestPrivilegedDaemonSet(name, namespace string) *appsv1.DaemonSet {
	privileged := true
	hostNetwork := true

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                          "csi-addons-privileged-test",
				"csi-addons.io/test-component": "privileged-capability-check",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "csi-addons-privileged-test",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "csi-addons-privileged-test",
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: hostNetwork,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: &[]int64{0}[0], // Run as root
					},
					Containers: []corev1.Container{
						{
							Name:  "privileged-test",
							Image: "alpine:latest",
							Command: []string{
								"sh", "-c", "sleep 1", // Minimal command that exits quickly
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"NET_ADMIN", // Required for iptables operations
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								// Minimal resource requests to reduce scheduling impact
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    parseQuantity("10m"),
									corev1.ResourceMemory: parseQuantity("16Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    parseQuantity("100m"),
									corev1.ResourceMemory: parseQuantity("64Mi"),
								},
							},
						},
					},
					// Tolerate all taints to ensure scheduling on any node
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
						},
					},
					// Use short termination grace period for faster cleanup
					TerminationGracePeriodSeconds: &[]int64{1}[0],
					// Don't restart the container - we just want to test creation
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
}

// parseQuantity is a helper to create resource quantities safely
func parseQuantity(s string) resource.Quantity {
	if q, err := resource.ParseQuantity(s); err == nil {
		return q
	}
	// Return zero quantity if parsing fails
	return resource.Quantity{}
}
