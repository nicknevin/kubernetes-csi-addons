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
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestQuickTemplateCheck provides a quick way to test template rendering
func TestQuickTemplateCheck(t *testing.T) {
	config := FaultInjectionConfig{
		Type:      FaultInjectorIptables,
		Client:    fake.NewClientBuilder().Build(),
		Namespace: "default",
		ProviderParams: map[string]string{
			"image": DefaultIptablesImageWithRegistry,
		},
	}

	provider, err := NewIptablesFaultProvider(config)
	if err != nil {
		t.Fatalf("Failed to create IptablesFaultProvider: %v", err)
	}

	iptablesProvider := provider.(*IptablesFaultProvider)

	templateData := TemplateData{
		Namespace:    "default",
		Image:        "docker.io/csi-addons/iptables-manager:latest",
		NodeSelector: map[string]string{"test": "node"},
	}

	t.Run("Quick ConfigMap test", func(t *testing.T) {
		configMap := &corev1.ConfigMap{}
		err := iptablesProvider.renderTemplate("templates/iptables-configmap.yaml", templateData, configMap)
		if err != nil {
			t.Errorf("ConfigMap template failed: %v", err)
			return
		}

		fmt.Printf("ConfigMap rendered successfully:\n")
		fmt.Printf("  Name: %s\n", configMap.Name)
		fmt.Printf("  Namespace: %s\n", configMap.Namespace)
		for key, value := range configMap.Data {
			fmt.Printf("  %s: %d bytes\n", key, len(value))
		}
	})

	t.Run("Quick DaemonSet test", func(t *testing.T) {
		daemonSet := &appsv1.DaemonSet{}
		err := iptablesProvider.renderTemplate("templates/iptables-daemonset.yaml", templateData, daemonSet)
		if err != nil {
			t.Errorf("DaemonSet template failed: %v", err)
			return
		}

		fmt.Printf("DaemonSet rendered successfully:\n")
		fmt.Printf("  Name: %s\n", daemonSet.Name)
		fmt.Printf("  Namespace: %s\n", daemonSet.Namespace)
		fmt.Printf("  Image: %s\n", daemonSet.Spec.Template.Spec.Containers[0].Image)
		fmt.Printf("  Volumes: %d\n", len(daemonSet.Spec.Template.Spec.Volumes))

		for i, vol := range daemonSet.Spec.Template.Spec.Volumes {
			fmt.Printf("    Volume %d: %s\n", i+1, vol.Name)
		}

		container := daemonSet.Spec.Template.Spec.Containers[0]
		fmt.Printf("  Volume Mounts: %d\n", len(container.VolumeMounts))

		for i, mount := range container.VolumeMounts {
			fmt.Printf("    Mount %d: %s -> %s\n", i+1, mount.Name, mount.MountPath)
		}
	})

	t.Run("Full DaemonSet creation", func(t *testing.T) {
		// Test the full createIptablesDaemonSet method
		daemonSet := iptablesProvider.createIptablesDaemonSet()

		if daemonSet == nil {
			t.Fatal("createIptablesDaemonSet should return a valid DaemonSet")
		}

		fmt.Printf("Full DaemonSet created successfully:\n")
		fmt.Printf("  Name: %s\n", daemonSet.Name)
		container := daemonSet.Spec.Template.Spec.Containers[0]
		fmt.Printf("  Image: %s\n", container.Image)
		fmt.Printf("  Volumes: %d\n", len(daemonSet.Spec.Template.Spec.Volumes))
		fmt.Printf("  Volume Mounts: %d\n", len(container.VolumeMounts))
	})

	t.Run("E2E scenario test", func(t *testing.T) {
		// Simulate the exact e2e test scenario
		config := FaultInjectionConfig{
			Type:      FaultInjectorIptables,
			Client:    fake.NewClientBuilder().Build(),
			Namespace: "test-namespace-12345", // Simulate unique namespace like e2e test
			ProviderParams: map[string]string{
				"image": DefaultIptablesImageWithRegistry,
			},
		}

		provider, err := NewFaultInjectionProvider(config)
		if err != nil {
			t.Fatalf("Failed to create fault injection provider: %v", err)
		}

		iptablesProvider := provider.(*IptablesFaultProvider)

		// Test the full createIptablesDaemonSet method like e2e would
		daemonSet := iptablesProvider.createIptablesDaemonSet()

		if daemonSet == nil {
			t.Fatal("createIptablesDaemonSet should return a valid DaemonSet")
		}

		// Check that namespace is properly set
		if daemonSet.Namespace != "test-namespace-12345" {
			t.Errorf("Expected namespace 'test-namespace-12345', got: %s", daemonSet.Namespace)
		}

		// Check volumes (template uses tmp-dir emptyDir for readiness marker)
		if len(daemonSet.Spec.Template.Spec.Volumes) != 1 {
			t.Errorf("Expected 1 volume, got: %d", len(daemonSet.Spec.Template.Spec.Volumes))
		}
		if daemonSet.Spec.Template.Spec.Volumes[0].Name != "tmp-dir" {
			t.Errorf("Expected tmp-dir volume, got: %s", daemonSet.Spec.Template.Spec.Volumes[0].Name)
		}

		container := daemonSet.Spec.Template.Spec.Containers[0]
		if len(container.VolumeMounts) != 1 {
			t.Errorf("Expected 1 volume mount, got: %d", len(container.VolumeMounts))
		}
		if container.VolumeMounts[0].Name != "tmp-dir" || container.VolumeMounts[0].MountPath != "/tmp" {
			t.Errorf("Unexpected volume mount: %+v", container.VolumeMounts[0])
		}

		fmt.Printf("E2E scenario test passed:\n")
		fmt.Printf("  DaemonSet: %s\n", daemonSet.Name)
		fmt.Printf("  Namespace: %s\n", daemonSet.Namespace)
		fmt.Printf("  Image: %s\n", container.Image)
		fmt.Printf("  Volumes: %d\n", len(daemonSet.Spec.Template.Spec.Volumes))
		fmt.Printf("  Volume Mounts: %d\n", len(container.VolumeMounts))

		for _, vol := range daemonSet.Spec.Template.Spec.Volumes {
			if vol.ConfigMap != nil {
				fmt.Printf("  ConfigMap Volume: %s -> %s\n", vol.Name, vol.ConfigMap.Name)
			}
		}
	})
}
