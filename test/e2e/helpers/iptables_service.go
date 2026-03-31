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
	"os/exec"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeployIptablesService deploys the iptables manager DaemonSet to the cluster.
// This function:
// 1. Loads the pre-built iptables image to cluster nodes (via minikube/kind/k3d)
// 2. Deploys the iptables-manager DaemonSet with imagePullPolicy=Never
// 3. Waits for the DaemonSet to be ready on all nodes
// This is essential for fault injection testing that requires iptables rules.
func DeployIptablesService(ctx context.Context, c client.Client) error {
	iptablesImage := "localhost/csi-addons/iptables-manager:latest"

	// First, try to load the image to the cluster (for minikube/kind/k3d)
	Logf("[IPTABLES-SERVICE]", "attempting to load iptables image to cluster nodes...")
	if err := loadImageToCluster(ctx, iptablesImage); err != nil {
		Logf("[IPTABLES-SERVICE]", "WARNING: could not load image to cluster: %v (image should be pre-loaded via preload script)", err)
	}

	deployCtx, deployCancel := context.WithTimeout(ctx, 60*time.Second)
	defer deployCancel()

	// Create/verify the namespace for the DaemonSet
	daemonsetNamespace := "csi-addons-system"
	daemonsetNs := &corev1.Namespace{}
	if err := c.Get(deployCtx, client.ObjectKey{Name: daemonsetNamespace}, daemonsetNs); err != nil {
		// Namespace doesn't exist, create it
		daemonsetNs = CreateNamespace(deployCtx, c, daemonsetNamespace)
	}
	Logf("[IPTABLES-SERVICE]", "using namespace: %s", daemonsetNs.Name)

	// Deploy the DaemonSet
	daemonsetName := "csi-addons-iptables-manager"
	daemonset := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonsetName,
			Namespace: daemonsetNamespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "csi-addons-iptables-manager",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "csi-addons-iptables-manager",
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: func() *int64 { i := int64(0); return &i }(),
					},
					Containers: []corev1.Container{
						{
							Name:            "iptables-manager",
							Image:           iptablesImage,
							ImagePullPolicy: corev1.PullNever,
							Command: []string{"sh", "-c", `
echo 'CSI-Addons Iptables Manager starting...';
echo 'Iptables version:'; iptables --version &&
echo 'Available tools:'; which iptables ping nc nslookup &&
echo 'Creating readiness marker...';
touch /tmp/iptables-ready &&
echo 'Starting iptables rules monitoring loop...';
while true; do
  if [ -f /rules/apply.sh ]; then
    echo '[$(date)] Executing /rules/apply.sh';
    chmod +x /rules/apply.sh && /rules/apply.sh 2>&1 | tee -a /var/log/iptables.log;
  else
    echo '[$(date)] No rules file found, waiting...';
  fi;
  sleep 10;
done
`},
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"sh", "-c", "test -f /tmp/iptables-ready && iptables --version"},
									},
								},
								InitialDelaySeconds: 2,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
						},
					},
					TerminationGracePeriodSeconds: func() *int64 { i := int64(10); return &i }(),
				},
			},
		},
	}

	Logf("[IPTABLES-SERVICE]", "deploying iptables-manager DaemonSet to namespace: %s", daemonsetNamespace)
	if err := c.Create(deployCtx, daemonset); err != nil {
		// Check if already exists and update if needed
		if client.IgnoreAlreadyExists(err) != nil {
			existingDs := &appsv1.DaemonSet{}
			if getErr := c.Get(deployCtx, client.ObjectKey{Name: daemonsetName, Namespace: daemonsetNamespace}, existingDs); getErr == nil {
				daemonset.ResourceVersion = existingDs.ResourceVersion
				if updateErr := c.Update(deployCtx, daemonset); updateErr != nil {
					Logf("[IPTABLES-SERVICE]", "WARNING: failed to update iptables DaemonSet: %v", updateErr)
					return fmt.Errorf("failed to update iptables DaemonSet: %w", updateErr)
				}
				Logf("[IPTABLES-SERVICE]", "updated existing iptables DaemonSet")
			} else {
				Logf("[IPTABLES-SERVICE]", "WARNING: failed to create iptables DaemonSet: %v", err)
				return fmt.Errorf("failed to create iptables DaemonSet: %w", err)
			}
		} else {
			Logf("[IPTABLES-SERVICE]", "WARNING: failed to create iptables DaemonSet: %v", err)
			return fmt.Errorf("failed to create iptables DaemonSet: %w", err)
		}
	}

	Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet deployed, waiting for readiness...")

	// Wait for DaemonSet to be ready
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		currentDs := &appsv1.DaemonSet{}
		if err := c.Get(deployCtx, client.ObjectKey{Name: daemonsetName, Namespace: daemonsetNamespace}, currentDs); err != nil {
			Logf("[IPTABLES-SERVICE]", "ERROR: could not retrieve DaemonSet status: %v", err)
			return fmt.Errorf("failed to get DaemonSet status: %w", err)
		}

		// Check if all replicas are ready
		if currentDs.Status.DesiredNumberScheduled == currentDs.Status.NumberReady {
			Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet is ready (%d/%d pods ready)",
				currentDs.Status.NumberReady, currentDs.Status.DesiredNumberScheduled)
			return nil
		}

		Logf("[IPTABLES-SERVICE]", "waiting for iptables DaemonSet... (%d/%d pods ready)",
			currentDs.Status.NumberReady, currentDs.Status.DesiredNumberScheduled)
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("iptables DaemonSet failed to reach ready state within timeout")
}

// loadImageToCluster attempts to load the iptables image to the cluster using minikube, kind, or k3d.
// This ensures the image is available on cluster nodes for local-only (imagePullPolicy: Never) usage.
func loadImageToCluster(ctx context.Context, image string) error {
	// Try minikube first (most common for local testing)
	if minikubeContext := getMinikubeContext(); minikubeContext != "" {
		Logf("[IPTABLES-SERVICE]", "loading image to minikube cluster: %s", minikubeContext)
		if minikubeErr := loadImageViaMinikube(image, minikubeContext); minikubeErr == nil {
			Logf("[IPTABLES-SERVICE]", "✓ image loaded via minikube")
			return nil
		} else {
			Logf("[IPTABLES-SERVICE]", "minikube load failed: %v", minikubeErr)
		}
	}

	// Try kind
	if kindContext := getKindContext(); kindContext != "" {
		Logf("[IPTABLES-SERVICE]", "loading image to kind cluster: %s", kindContext)
		if kindErr := loadImageViaKind(image, kindContext); kindErr == nil {
			Logf("[IPTABLES-SERVICE]", "✓ image loaded via kind")
			return nil
		} else {
			Logf("[IPTABLES-SERVICE]", "kind load failed: %v", kindErr)
		}
	}

	// Try k3d
	if k3dContext := getK3dContext(); k3dContext != "" {
		Logf("[IPTABLES-SERVICE]", "loading image to k3d cluster: %s", k3dContext)
		if k3dErr := loadImageViaK3d(image, k3dContext); k3dErr == nil {
			Logf("[IPTABLES-SERVICE]", "✓ image loaded via k3d")
			return nil
		} else {
			Logf("[IPTABLES-SERVICE]", "k3d load failed: %v", k3dErr)
		}
	}

	return fmt.Errorf("could not determine cluster type for image loading")
}

// getMinikubeContext returns the current minikube context if available
func getMinikubeContext() string {
	// Check KUBECONFIG or use default kubectl
	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	context := strings.TrimSpace(string(output))
	if strings.Contains(context, "minikube") {
		return context
	}
	return ""
}

// getKindContext returns the current kind context if available
func getKindContext() string {
	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	context := strings.TrimSpace(string(output))
	if strings.Contains(context, "kind-") {
		return context
	}
	return ""
}

// getK3dContext returns the current k3d context if available
func getK3dContext() string {
	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	context := strings.TrimSpace(string(output))
	if strings.Contains(context, "k3d-") {
		return context
	}
	return ""
}

// loadImageViaMinikube loads an image via minikube image load command
func loadImageViaMinikube(image, context string) error {
	// Extract cluster name from context
	clusterName := strings.TrimPrefix(context, "minikube-")

	// Determine container runtime
	containerCmd := "podman"
	if _, err := exec.LookPath("podman"); err != nil {
		containerCmd = "docker"
		if _, err := exec.LookPath("docker"); err != nil {
			return fmt.Errorf("neither podman nor docker found")
		}
	}

	// Use: podman save <image> | minikube image load --profile=<cluster> -
	saveCmd := exec.Command(containerCmd, "save", image)
	loadCmd := exec.Command("minikube", "image", "load", "--profile="+clusterName, "-")

	// Connect stdout of save to stdin of load
	loadCmd.Stdin, _ = saveCmd.StdoutPipe()
	loadCmd.Stdout = os.Stdout
	loadCmd.Stderr = os.Stderr

	if err := saveCmd.Start(); err != nil {
		return fmt.Errorf("failed to start container save: %w", err)
	}
	if err := loadCmd.Run(); err != nil {
		saveCmd.Wait() // Clean up save process if load fails
		return fmt.Errorf("failed to load image via minikube: %w", err)
	}
	return saveCmd.Wait()
}

// loadImageViaKind loads an image via kind load docker-image command
func loadImageViaKind(image, context string) error {
	// Extract cluster name from context
	clusterName := strings.TrimPrefix(context, "kind-")

	cmd := exec.Command("kind", "load", "docker-image", image, "--name="+clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// DeployIptablesServiceWithConfigMap deploys the iptables manager DaemonSet and creates the initial ConfigMap.
// This enhanced function:
// 1. Loads the pre-built iptables image to cluster nodes
// 2. Creates the initial ConfigMap using template rendering
// 3. Deploys the iptables-manager DaemonSet using template rendering
// 4. Waits for the DaemonSet to be ready on all nodes
// This ensures both DaemonSet and ConfigMap are available for fault injection testing.
func DeployIptablesServiceWithConfigMap(ctx context.Context, c client.Client, namespace string) error {
	iptablesImage := DefaultIptablesImageWithRegistry

	// First, try to load the image to the cluster (for minikube/kind/k3d)
	Logf("[IPTABLES-SERVICE]", "attempting to load iptables image to cluster nodes...")
	if err := loadImageToCluster(ctx, iptablesImage); err != nil {
		Logf("[IPTABLES-SERVICE]", "WARNING: could not load image to cluster: %v (image should be pre-loaded via preload script)", err)
	}

	deployCtx, deployCancel := context.WithTimeout(ctx, 60*time.Second)
	defer deployCancel()

	// Create/verify the namespace
	daemonsetNs := &corev1.Namespace{}
	if err := c.Get(deployCtx, client.ObjectKey{Name: namespace}, daemonsetNs); err != nil {
		// Namespace doesn't exist, create it
		daemonsetNs = CreateNamespace(deployCtx, c, namespace)
	}
	Logf("[IPTABLES-SERVICE]", "using namespace: %s", daemonsetNs.Name)

	// Create a temporary provider instance to use the template rendering
	tempConfig := FaultInjectionConfig{
		Type:      FaultInjectorIptables,
		Client:    c,
		Namespace: namespace,
		ProviderParams: map[string]string{
			"image": iptablesImage,
		},
	}
	tempProvider := &IptablesFaultProvider{config: tempConfig}

	// Create the initial ConfigMap using template
	configMap := tempProvider.createIptablesConfigMap()
	Logf("[IPTABLES-SERVICE]", "creating initial ConfigMap: %s", configMap.Name)
	if err := c.Create(deployCtx, configMap); err != nil {
		if client.IgnoreAlreadyExists(err) != nil {
			Logf("[IPTABLES-SERVICE]", "WARNING: failed to create ConfigMap: %v", err)
			return fmt.Errorf("failed to create ConfigMap: %w", err)
		} else {
			Logf("[IPTABLES-SERVICE]", "ConfigMap %s already exists", configMap.Name)
		}
	} else {
		Logf("[IPTABLES-SERVICE]", "✓ ConfigMap %s created", configMap.Name)
	}

	// Create the DaemonSet using template
	templateData := TemplateData{
		Namespace: namespace,
		Image:     iptablesImage,
	}
	daemonset := &appsv1.DaemonSet{}
	if err := tempProvider.renderTemplate("templates/iptables-daemonset.yaml", templateData, daemonset); err != nil {
		return fmt.Errorf("failed to render DaemonSet template: %w", err)
	}

	Logf("[IPTABLES-SERVICE]", "deploying iptables-manager DaemonSet to namespace: %s", namespace)
	if err := c.Create(deployCtx, daemonset); err != nil {
		if client.IgnoreAlreadyExists(err) != nil {
			Logf("[IPTABLES-SERVICE]", "WARNING: failed to create iptables DaemonSet: %v", err)
			return fmt.Errorf("failed to create iptables DaemonSet: %w", err)
		} else {
			Logf("[IPTABLES-SERVICE]", "DaemonSet %s already exists", daemonset.Name)
		}
	}

	Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet deployed, waiting for readiness...")

	// Wait for DaemonSet to be ready
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		currentDs := &appsv1.DaemonSet{}
		if err := c.Get(deployCtx, client.ObjectKey{Name: daemonset.Name, Namespace: namespace}, currentDs); err != nil {
			Logf("[IPTABLES-SERVICE]", "ERROR: could not retrieve DaemonSet status: %v", err)
			return fmt.Errorf("failed to get DaemonSet status: %w", err)
		}

		readyNodes := currentDs.Status.NumberReady
		desiredNodes := currentDs.Status.DesiredNumberScheduled

		if readyNodes == desiredNodes {
			Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet is ready (%d/%d pods ready)", readyNodes, desiredNodes)
			return nil
		}

		Logf("[IPTABLES-SERVICE]", "waiting for DaemonSet readiness... (%d/%d pods ready)", readyNodes, desiredNodes)
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timed out waiting for iptables DaemonSet to be ready")
}

// loadImageViaK3d loads an image via k3d image import command
func loadImageViaK3d(image, context string) error {
	// Extract cluster name from context
	clusterName := strings.TrimPrefix(context, "k3d-")

	cmd := exec.Command("k3d", "image", "import", image, "--cluster="+clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
