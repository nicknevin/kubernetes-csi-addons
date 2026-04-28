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
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

//go:embed templates/iptables-daemonset.yaml
var daemonsetTemplateFS embed.FS

// EnvE2EIptablesSkipSuiteDSRecreate when set to "true", EnsureFreshSuiteIptablesDaemonSet skips deleting
// the existing csi-addons-iptables-manager DaemonSet (faster if image is already correct).
const EnvE2EIptablesSkipSuiteDSRecreate = "E2E_IPTABLES_SKIP_SUITE_DS_RECREATE"

// EnsureFreshSuiteIptablesDaemonSet removes any prior suite iptables DaemonSet and fence ConfigMap, then deploys
// the current template (so pod template/image matches this test binary). Set E2E_IPTABLES_SKIP_SUITE_DS_RECREATE=true to skip deletion.
func EnsureFreshSuiteIptablesDaemonSet(ctx context.Context, c client.Client, namespace string) error {
	if os.Getenv(EnvE2EIptablesSkipSuiteDSRecreate) == "true" {
		Logf("[IPTABLES-SERVICE]", "skipping DaemonSet recreate (%s=true); deploying/updating only", EnvE2EIptablesSkipSuiteDSRecreate)
		return DeployIptablesServiceWithConfigMap(ctx, c, namespace)
	}
	Logf("[IPTABLES-SERVICE]", "suite start: removing stale iptables DaemonSet %s/%s and fence ConfigMap (if any), then redeploying", namespace, IptablesDaemonSetName)
	if err := deleteSuiteIptablesDaemonSetAndFenceCM(ctx, c, namespace); err != nil {
		return fmt.Errorf("reset suite iptables resources: %w", err)
	}
	return DeployIptablesServiceWithConfigMap(ctx, c, namespace)
}

func deleteSuiteIptablesDaemonSetAndFenceCM(ctx context.Context, c client.Client, namespace string) error {
	cmKey := client.ObjectKey{Namespace: namespace, Name: IptablesFenceStateConfigMapName}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, cmKey, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ConfigMap %s: %w", IptablesFenceStateConfigMapName, err)
		}
	} else if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ConfigMap %s: %w", IptablesFenceStateConfigMapName, err)
	}

	dsKey := client.ObjectKey{Namespace: namespace, Name: IptablesDaemonSetName}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, dsKey, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get DaemonSet: %w", err)
	}
	if err := c.Delete(ctx, &ds); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete DaemonSet: %w", err)
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, dsKey, &appsv1.DaemonSet{})
		if apierrors.IsNotFound(err) {
			Logf("[IPTABLES-SERVICE]", "prior DaemonSet %s deleted", IptablesDaemonSetName)
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for DaemonSet deletion: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for DaemonSet %s to be deleted", IptablesDaemonSetName)
}

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

	// Deploy the DaemonSet using template
	daemonset, err := createIptablesDaemonSetFromTemplate(daemonsetNamespace, iptablesImage)
	if err != nil {
		Logf("[IPTABLES-SERVICE]", "ERROR: failed to create DaemonSet from template: %v", err)
		return fmt.Errorf("failed to create DaemonSet from template: %w", err)
	}

	daemonsetName := "csi-addons-iptables-manager"
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

		desired := currentDs.Status.DesiredNumberScheduled
		ready := currentDs.Status.NumberReady
		// 0==0 must not count as ready: no pods scheduled yet (image missing, no nodes, etc.).
		if desired > 0 && ready == desired {
			Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet is ready (%d/%d pods ready)", ready, desired)
			return nil
		}

		if desired == 0 && i == 4 {
			logIptablesDaemonSetZeroDesiredDiagnostics(ctx, c)
		}

		Logf("[IPTABLES-SERVICE]", "waiting for iptables DaemonSet... (%d/%d pods ready)", ready, desired)
		time.Sleep(2 * time.Second)
	}

	lastReady, lastDesired := lastDaemonSetReadyCounts(c, ctx, daemonsetName, daemonsetNamespace)
	if lastDesired == 0 {
		logIptablesDaemonSetZeroDesiredDiagnostics(ctx, c)
	}
	return fmt.Errorf("iptables DaemonSet failed to reach ready state within timeout (last observed %d/%d ready; if desired=0, check Nodes are schedulable (kubectl uncordon) and Ready; if 0/0 with schedulable nodes, preload iptables-manager image or set imagePullPolicy)",
		lastReady, lastDesired)
}

func lastDaemonSetReadyCounts(c client.Client, ctx context.Context, name, namespace string) (ready, desired int32) {
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, ds); err != nil {
		return -1, -1
	}
	return ds.Status.NumberReady, ds.Status.DesiredNumberScheduled
}

// logIptablesDaemonSetZeroDesiredDiagnostics prints node schedulability when DaemonSet DesiredNumberScheduled is 0.
// Cordoned nodes (spec.unschedulable) are excluded from DaemonSet placement; existing pods may still appear Running.
func logIptablesDaemonSetZeroDesiredDiagnostics(ctx context.Context, c client.Client) {
	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		Logf("[IPTABLES-SERVICE]", "DaemonSet desired=0 diagnostics: list Nodes: %v", err)
		return
	}
	if len(nl.Items) == 0 {
		Logf("[IPTABLES-SERVICE]", "DaemonSet desired=0 diagnostics: no Nodes in cluster")
		return
	}
	Logf("[IPTABLES-SERVICE]", "DaemonSet desired=0: node summary (Unschedulable/cordoned nodes get no DaemonSet pods; old pods can still show Running)")
	for _, n := range nl.Items {
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = cond.Status == corev1.ConditionTrue
				break
			}
		}
		var taintParts []string
		for _, t := range n.Spec.Taints {
			taintParts = append(taintParts, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
		}
		taints := strings.Join(taintParts, ", ")
		if taints == "" {
			taints = "<none>"
		}
		Logf("[IPTABLES-SERVICE]", "  node=%s Ready=%v Unschedulable=%v Taints=%s", n.Name, ready, n.Spec.Unschedulable, taints)
	}
	Logf("[IPTABLES-SERVICE]", "hint: kubectl uncordon <node> when Unschedulable is true; then delete stale %s pods if counts stay wrong", IptablesDaemonSetName)
}

// createIptablesDaemonSetFromTemplate creates a DaemonSet from the embedded template.
func createIptablesDaemonSetFromTemplate(namespace, image string) (*appsv1.DaemonSet, error) {
	// Read the template
	templateBytes, err := daemonsetTemplateFS.ReadFile("templates/iptables-daemonset.yaml")
	if err != nil {
		return nil, fmt.Errorf("failed to read template: %w", err)
	}

	// Parse and execute template
	tmpl, err := template.New("iptables-daemonset").Parse(string(templateBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	var renderedYAML bytes.Buffer
	templateValues := map[string]interface{}{
		"Namespace": namespace,
		"Image":     image,
	}

	if err := tmpl.Execute(&renderedYAML, templateValues); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	// Decode YAML into DaemonSet
	daemonset := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal(renderedYAML.Bytes(), daemonset); err != nil {
		return nil, fmt.Errorf("failed to decode DaemonSet YAML: %w", err)
	}

	return daemonset, nil
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
		if werr := saveCmd.Wait(); werr != nil {
			return fmt.Errorf("failed to load image via minikube: %w (save wait: %v)", err, werr)
		}
		return fmt.Errorf("failed to load image via minikube: %w", err)
	}
	if err := saveCmd.Wait(); err != nil {
		return fmt.Errorf("container save did not complete: %w", err)
	}
	return nil
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

// DeployIptablesServiceWithConfigMap deploys the iptables manager DaemonSet (suite bootstrap).
// It does not create the placeholder rules ConfigMap (templates/iptables-configmap.yaml); fencing never
// consumes that object. Runtime fence history uses csi-addons-iptables-fence-state, written by IptablesFaultProvider.
// Steps:
//  1. Load the pre-built iptables image to cluster nodes when possible
//  2. Ensure namespace exists
//  3. Render and create the DaemonSet from templates/iptables-daemonset.yaml
//  4. Wait until the DaemonSet is ready on all schedulable nodes
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

	// Create the DaemonSet using template (no ConfigMap needed)
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

		if desiredNodes > 0 && readyNodes == desiredNodes {
			Logf("[IPTABLES-SERVICE]", "✓ iptables-manager DaemonSet is ready (%d/%d pods ready)", readyNodes, desiredNodes)
			var ds appsv1.DaemonSet
			if err := c.Get(ctx, client.ObjectKey{Name: daemonset.Name, Namespace: namespace}, &ds); err == nil {
				for _, co := range ds.Spec.Template.Spec.Containers {
					if co.Name == IptablesContainerName {
						Logf("[IPTABLES-SERVICE]", "DaemonSet container %q image: %s imagePullPolicy=%s", co.Name, co.Image, co.ImagePullPolicy)
						break
					}
				}
			}
			return nil
		}

		if desiredNodes == 0 && i == 4 {
			logIptablesDaemonSetZeroDesiredDiagnostics(ctx, c)
		}

		Logf("[IPTABLES-SERVICE]", "waiting for DaemonSet readiness... (%d/%d pods ready)", readyNodes, desiredNodes)
		time.Sleep(2 * time.Second)
	}

	lastReady, lastDesired := lastDaemonSetReadyCounts(c, ctx, daemonset.Name, namespace)
	if lastDesired == 0 {
		logIptablesDaemonSetZeroDesiredDiagnostics(ctx, c)
	}
	return fmt.Errorf("timed out waiting for iptables DaemonSet to be ready (last observed %d/%d; if desired=0, check Nodes are schedulable (kubectl uncordon) and Ready; if 0/0 with schedulable nodes, preload iptables-manager image or adjust imagePullPolicy)",
		lastReady, lastDesired)
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
