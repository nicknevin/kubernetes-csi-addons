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
	"strings"
	"text/template"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed templates/*.yaml
var templateFiles embed.FS

// TemplateData holds the values to substitute in YAML templates
type TemplateData struct {
	Namespace    string
	Image        string
	NodeSelector map[string]string
}

const (
	// IptablesDaemonSetName is the name of the DaemonSet used for iptables operations
	IptablesDaemonSetName = "csi-addons-iptables-manager"

	// IptablesContainerName is the name of the container in the DaemonSet
	IptablesContainerName = "iptables-manager"

	// DefaultIptablesImage is the default container image for iptables operations
	// Using a pre-built image with iptables and network tools
	DefaultIptablesImage = "csi-addons/iptables-manager:latest"

	// IptablesReadyTimeout is how long to wait for DaemonSet pods to be ready
	IptablesReadyTimeout = 120 * time.Second
)

// Environment variable names for iptables configuration
const (
	EnvIptablesTargetNodes    = "E2E_IPTABLES_TARGET_NODES"
	EnvIptablesImage          = "E2E_IPTABLES_IMAGE"
	EnvIptablesCleanupTimeout = "E2E_IPTABLES_CLEANUP_TIMEOUT"
)

// IptablesFaultProvider implements PeerFenceProvider using iptables rules via privileged DaemonSets.
// This provider creates a privileged DaemonSet with NET_ADMIN capabilities to manipulate
// iptables rules for network fault injection.
type IptablesFaultProvider struct {
	config    FaultInjectionConfig
	daemonSet *appsv1.DaemonSet
	configMap *corev1.ConfigMap
	deployed  bool

	// Track active fence rules for cleanup
	activeFenceRules []string
}

// NewIptablesFaultProvider creates a new iptables-based fault injection provider.
func NewIptablesFaultProvider(config FaultInjectionConfig) (PeerFenceProvider, error) {
	provider := &IptablesFaultProvider{
		config:           config,
		activeFenceRules: make([]string, 0),
	}

	return provider, nil
}

func (p *IptablesFaultProvider) IsSupported(ctx context.Context) bool {
	// Check if privileged DaemonSets are supported
	return HasPrivilegedDaemonSetSupport(ctx, p.config.Client)
}

func (p *IptablesFaultProvider) FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	clusterContext := p.getClusterContext()
	Logf("[DEBUG]", "[%s] Starting FenceIP operation for CIDR %s using DaemonSet %s", clusterContext, targetCIDR, IptablesDaemonSetName)

	if !p.deployed {
		Logf("[DEBUG]", "[%s] DaemonSet %s not deployed yet, deploying now", clusterContext, IptablesDaemonSetName)
		if err := p.deployDaemonSet(ctx); err != nil {
			Logf("[ERROR]", "[%s] Failed to deploy iptables DaemonSet %s for fencing IP %s: %v", clusterContext, IptablesDaemonSetName, targetCIDR, err)
			return fmt.Errorf("[%s] failed to deploy iptables DaemonSet %s: %w", clusterContext, IptablesDaemonSetName, err)
		}
		Logf("[INFO]", "[%s] Successfully deployed DaemonSet %s for iptables operations", clusterContext, IptablesDaemonSetName)
	} else {
		Logf("[DEBUG]", "[%s] DaemonSet %s already deployed, adding fence rule", clusterContext, IptablesDaemonSetName)
	}

	// Add the rule to the ConfigMap
	Logf("[DEBUG]", "[%s] Adding iptables fence rule for CIDR %s to ConfigMap", clusterContext, targetCIDR)
	if err := p.addIptablesRule(ctx, targetCIDR, "fence"); err != nil {
		Logf("[ERROR]", "[%s] Failed to add iptables fence rule for IP %s to DaemonSet %s: %v", clusterContext, targetCIDR, IptablesDaemonSetName, err)
		return fmt.Errorf("[%s] failed to add iptables fence rule to %s: %w", clusterContext, IptablesDaemonSetName, err)
	}

	// Track the rule for cleanup
	p.activeFenceRules = append(p.activeFenceRules, targetCIDR)

	Logf("[INFO]", "[%s] Successfully fenced IP %s using iptables", clusterContext, targetCIDR)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

func (p *IptablesFaultProvider) UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	clusterContext := p.getClusterContext()
	Logf("[DEBUG]", "[%s] Starting UnfenceIP operation for CIDR %s using DaemonSet %s", clusterContext, targetCIDR, IptablesDaemonSetName)

	if !p.deployed {
		Logf("[ERROR]", "[%s] Cannot unfence IP %s: iptables DaemonSet %s not deployed", clusterContext, targetCIDR, IptablesDaemonSetName)
		return fmt.Errorf("[%s] iptables DaemonSet %s not deployed", clusterContext, IptablesDaemonSetName)
	}

	// Remove the rule from the ConfigMap
	Logf("[DEBUG]", "[%s] Removing iptables fence rule for CIDR %s from ConfigMap", clusterContext, targetCIDR)
	if err := p.addIptablesRule(ctx, targetCIDR, "unfence"); err != nil {
		Logf("[ERROR]", "[%s] Failed to add iptables unfence rule for IP %s to DaemonSet %s: %v", clusterContext, targetCIDR, IptablesDaemonSetName, err)
		return fmt.Errorf("[%s] failed to add iptables unfence rule to %s: %w", clusterContext, IptablesDaemonSetName, err)
	}

	// Remove from active rules tracking
	p.removeFromActiveRules(targetCIDR)

	Logf("[INFO]", "[%s] Successfully unfenced IP %s using iptables", clusterContext, targetCIDR)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

func (p *IptablesFaultProvider) VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool) (bool, error) {
	clusterContext := p.getClusterContext()
	Logf("[DEBUG]", "[%s] Starting connectivity verification for CIDR %s (expected fenced: %t) using DaemonSet %s", clusterContext, targetCIDR, expectedFenced, IptablesDaemonSetName)

	if !p.deployed {
		Logf("[ERROR]", "[%s] Cannot verify connectivity for IP %s: iptables DaemonSet %s not deployed", clusterContext, targetCIDR, IptablesDaemonSetName)
		return false, fmt.Errorf("[%s] iptables DaemonSet %s not deployed", clusterContext, IptablesDaemonSetName)
	}

	// Extract IP from CIDR if needed
	targetIP := strings.Split(targetCIDR, "/")[0]
	Logf("[DEBUG]", "[%s] Extracted target IP %s from CIDR %s for ping test", clusterContext, targetIP, targetCIDR)
	// Create a simple ping job to test connectivity
	pingJob := p.createPingJob(targetIP)
	if err := p.config.Client.Create(ctx, pingJob); err != nil {
		Logf("[ERROR]", "Failed to create ping job for connectivity verification of IP %s: %v", targetCIDR, err)
		return false, fmt.Errorf("failed to create ping job: %w", err)
	}

	defer func() {
		// Clean up ping job
		if err := p.config.Client.Delete(ctx, pingJob); err != nil {
			// Log but don't fail - this is cleanup
			Logf("[WARNING]", "Failed to delete ping job: %v", err)
		}
	}()

	// Wait for job completion with timeout
	timeout := 60 * time.Second
	checkInterval := 5 * time.Second

	var jobCompleted bool
	var pingSucceeded bool

	err := wait.PollImmediate(checkInterval, timeout, func() (bool, error) {
		var job batchv1.Job
		key := client.ObjectKeyFromObject(pingJob)
		if err := p.config.Client.Get(ctx, key, &job); err != nil {
			Logf("[DEBUG]", "Failed to get ping job status during connectivity verification: %v", err)
			return false, fmt.Errorf("failed to get ping job: %w", err)
		}

		if job.Status.Succeeded > 0 {
			// Ping succeeded
			pingSucceeded = true
			jobCompleted = true
			return true, nil
		}

		if job.Status.Failed > 0 {
			// Ping failed
			pingSucceeded = false
			jobCompleted = true
			return true, nil
		}

		// Job still running, continue waiting
		return false, nil
	})

	if err != nil {
		Logf("[ERROR]", "Failed to verify connectivity to %s: %v", targetIP, err)
		return false, fmt.Errorf("failed to verify connectivity to %s: %w", targetIP, err)
	}

	if !jobCompleted {
		Logf("[ERROR]", "Ping job to %s did not complete within timeout", targetIP)
		return false, fmt.Errorf("ping job to %s did not complete within timeout", targetIP)
	}

	// Check if the result matches expectations
	if expectedFenced {
		// We expect the target to be unreachable (fenced)
		result := !pingSucceeded
		Logf("[DEBUG]", "Connectivity verification for %s: expected fenced=%t, ping succeeded=%t, result matches=%t", targetCIDR, expectedFenced, pingSucceeded, result)
		return result, nil
	} else {
		// We expect the target to be reachable (unfenced)
		result := pingSucceeded
		Logf("[DEBUG]", "Connectivity verification for %s: expected fenced=%t, ping succeeded=%t, result matches=%t", targetCIDR, expectedFenced, pingSucceeded, result)
		return result, nil
	}
}

func (p *IptablesFaultProvider) Cleanup(ctx context.Context) error {
	var errors []string

	Logf("[INFO]", "Cleaning up iptables fault injection resources: %d active fence rules", len(p.activeFenceRules))

	// Collect logs from DaemonSet pods before cleanup
	if p.deployed && p.daemonSet != nil {
		Logf("[INFO]", "Collecting logs from DaemonSet %s pods before cleanup", IptablesDaemonSetName)
		if err := p.collectDaemonSetLogs(ctx); err != nil {
			Logf("[WARNING]", "Failed to collect DaemonSet logs during cleanup: %v", err)
			// Don't add to errors - this is just for debugging
		}
	}

	// Clean up any active fence rules
	for _, targetCIDR := range p.activeFenceRules {
		if err := p.UnfenceIP(ctx, targetCIDR, nil); err != nil {
			Logf("[ERROR]", "Failed to unfence %s during cleanup: %v", targetCIDR, err)
			errors = append(errors, fmt.Sprintf("failed to unfence %s: %v", targetCIDR, err))
		}
	}

	// Delete the ConfigMap if created
	if p.configMap != nil {
		Logf("[DEBUG]", "Deleting ConfigMap %s during cleanup", p.configMap.Name)
		if err := p.config.Client.Delete(ctx, p.configMap); err != nil {
			Logf("[ERROR]", "Failed to delete ConfigMap %s during cleanup: %v", p.configMap.Name, err)
			errors = append(errors, fmt.Sprintf("failed to delete ConfigMap %s: %v", p.configMap.Name, err))
		}
	}

	// Delete the DaemonSet if deployed
	if p.deployed && p.daemonSet != nil {
		Logf("[DEBUG]", "Deleting DaemonSet %s during cleanup", p.daemonSet.Name)
		if err := p.config.Client.Delete(ctx, p.daemonSet); err != nil {
			Logf("[ERROR]", "Failed to delete DaemonSet %s during cleanup: %v", p.daemonSet.Name, err)
			errors = append(errors, fmt.Sprintf("failed to delete DaemonSet %s: %v", p.daemonSet.Name, err))
		}
	}

	p.deployed = false
	p.activeFenceRules = make([]string, 0)

	if len(errors) > 0 {
		Logf("[ERROR]", "Iptables cleanup completed with %d errors: %s", len(errors), strings.Join(errors, "; "))
		return fmt.Errorf("cleanup errors: %s", strings.Join(errors, "; "))
	}

	Logf("[INFO]", "Iptables cleanup completed successfully")
	return nil
}

func (p *IptablesFaultProvider) GetProviderType() FaultInjectorType {
	return FaultInjectorIptables
}

// getClusterContext tries to determine which cluster context we're operating in
// by examining the client configuration or namespace patterns
func (p *IptablesFaultProvider) getClusterContext() string {
	// Check provider params first
	if p.config.ProviderParams != nil {
		if clusterContext, exists := p.config.ProviderParams["cluster_context"]; exists {
			return clusterContext
		}
	}

	// Try to detect cluster context from namespace or other indicators
	if strings.Contains(p.config.Namespace, "dr1") {
		return "DR1"
	}
	if strings.Contains(p.config.Namespace, "dr2") {
		return "DR2"
	}
	// Check if we can get cluster context from environment or other sources
	if kubeContext := os.Getenv("KUBE_CONTEXT"); kubeContext != "" {
		return fmt.Sprintf("context:%s", kubeContext)
	}
	if dr1Context := os.Getenv("DR1_CONTEXT"); dr1Context != "" {
		return "DR1"
	}
	if dr2Context := os.Getenv("DR2_CONTEXT"); dr2Context != "" {
		return "DR2"
	}
	// Fallback to namespace info
	return fmt.Sprintf("namespace:%s", p.config.Namespace)
}

// deployDaemonSet creates and deploys the iptables management DaemonSet and ConfigMap
func (p *IptablesFaultProvider) deployDaemonSet(ctx context.Context) error {
	clusterContext := p.getClusterContext()
	Logf("[INFO]", "[%s] Deploying iptables DaemonSet %s in namespace %s", clusterContext, IptablesDaemonSetName, p.config.Namespace)

	// Create ConfigMap for iptables rules
	p.configMap = p.createIptablesConfigMap()
	Logf("[DEBUG]", "[%s] Creating ConfigMap %s for iptables rules", clusterContext, p.configMap.Name)
	if err := p.config.Client.Create(ctx, p.configMap); err != nil {
		Logf("[ERROR]", "[%s] Failed to create ConfigMap %s: %v", clusterContext, p.configMap.Name, err)
		return fmt.Errorf("[%s] failed to create ConfigMap %s: %w", clusterContext, p.configMap.Name, err)
	}
	Logf("[DEBUG]", "[%s] Successfully created ConfigMap %s", clusterContext, p.configMap.Name)

	// Create DaemonSet
	p.daemonSet = p.createIptablesDaemonSet()
	Logf("[DEBUG]", "[%s] Creating DaemonSet %s in namespace %s", clusterContext, p.daemonSet.Name, p.daemonSet.Namespace)
	if err := p.config.Client.Create(ctx, p.daemonSet); err != nil {
		Logf("[ERROR]", "[%s] Failed to create DaemonSet %s: %v", clusterContext, p.daemonSet.Name, err)
		return fmt.Errorf("[%s] failed to create DaemonSet %s: %w", clusterContext, p.daemonSet.Name, err)
	}
	Logf("[INFO]", "[%s] Successfully created DaemonSet %s, waiting for pods to be ready", clusterContext, p.daemonSet.Name)

	// Wait for DaemonSet pods to be ready
	Logf("[DEBUG]", "[%s] Waiting for DaemonSet %s pods to become ready (timeout: %s)", clusterContext, p.daemonSet.Name, IptablesReadyTimeout)
	if err := p.waitForDaemonSetReady(ctx); err != nil {
		Logf("[ERROR]", "[%s] DaemonSet %s not ready: %v", clusterContext, p.daemonSet.Name, err)

		// Collect detailed pod information on failure
		Logf("[INFO]", "[%s] Collecting diagnostic information for failed DaemonSet %s", clusterContext, p.daemonSet.Name)
		if pods, podErr := p.getDaemonSetPods(ctx); podErr == nil {
			for i, pod := range pods {
				Logf("[ERROR]", "[%s] Failed pod %d: %s (Node: %s, Phase: %s)", clusterContext, i+1, pod.Name, pod.Spec.NodeName, pod.Status.Phase)

				// Collect events for this pod
				if events, eventErr := p.collectPodEvents(ctx, pod.Name); eventErr == nil {
					Logf("[ERROR]", "[%s] Events for pod %s: %s", clusterContext, pod.Name, events)
				}

				// Try to get pod logs
				if logs, logErr := p.collectPodLogs(ctx, pod.Name); logErr == nil && logs != "" {
					Logf("[ERROR]", "[%s] Logs for pod %s:\n%s", clusterContext, pod.Name, logs)
				}
			}
		}

		return fmt.Errorf("[%s] DaemonSet %s not ready: %w", clusterContext, p.daemonSet.Name, err)
	}
	Logf("[INFO]", "[%s] DaemonSet %s is ready and operational", clusterContext, p.daemonSet.Name)
	p.deployed = true
	return nil
}

// createIptablesDaemonSet creates the DaemonSet manifest for iptables operations using templates
func (p *IptablesFaultProvider) createIptablesDaemonSet() *appsv1.DaemonSet {
	// Get configuration from environment or use defaults
	image := os.Getenv(EnvIptablesImage)
	if image == "" {
		image = DefaultIptablesImage
	}

	Logf("[DEBUG]", "Creating iptables DaemonSet with image: %s", image)

	nodeSelector := map[string]string{}
	if targetNodes := os.Getenv(EnvIptablesTargetNodes); targetNodes != "" {
		// Parse node selector from environment variable
		// Format: "label1=value1,label2=value2"
		for _, pair := range strings.Split(targetNodes, ",") {
			if kv := strings.SplitN(strings.TrimSpace(pair), "=", 2); len(kv) == 2 {
				nodeSelector[kv[0]] = kv[1]
			}
		}
	}

	data := TemplateData{
		Namespace:    p.config.Namespace,
		Image:        image,
		NodeSelector: nodeSelector,
	}

	// Use different template based on image type
	templatePath := "templates/iptables-daemonset.yaml"
	if strings.Contains(image, "alpine:") && !strings.Contains(image, "iptables-manager") {
		Logf("[DEBUG]", "Using alpine image - will install iptables at runtime")
		// For alpine images, we need to use the old template with runtime installation
		// But we'll modify it inline
	} else {
		Logf("[DEBUG]", "Using pre-built iptables image - no runtime installation needed")
	}

	daemonSet := &appsv1.DaemonSet{}
	if err := p.renderTemplate(templatePath, data, daemonSet); err != nil {
		panic(fmt.Sprintf("Failed to render DaemonSet template: %v", err))
	}

	// If using alpine image, modify the command to install iptables
	if strings.Contains(image, "alpine:") && !strings.Contains(image, "iptables-manager") {
		Logf("[DEBUG]", "Modifying DaemonSet command for alpine image with runtime installation")
		container := &daemonSet.Spec.Template.Spec.Containers[0]
		container.Command = []string{"sh", "-c"}
		container.Args = []string{
			`echo 'Installing iptables and dependencies...';
apk add --no-cache iptables inotify-tools &&
echo 'Installation completed, iptables available at:'; which iptables &&
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
done`,
		}

		// Adjust probes for slower alpine installation
		container.StartupProbe.InitialDelaySeconds = 5
		container.StartupProbe.PeriodSeconds = 2
		container.StartupProbe.FailureThreshold = 30 // Allow 60 seconds for installation
		container.ReadinessProbe.InitialDelaySeconds = 10
	}

	return daemonSet
}

// renderTemplate renders a YAML template with the given data into a Kubernetes object
func (p *IptablesFaultProvider) renderTemplate(templatePath string, data TemplateData, obj interface{}) error {
	// Read template file
	tmplContent, err := templateFiles.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("failed to read template %s: %w", templatePath, err)
	}

	// Parse template
	tmpl, err := template.New("template").Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Decode YAML into the object
	if err := yaml.Unmarshal(buf.Bytes(), obj); err != nil {
		return fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	return nil
}

// waitForDaemonSetReady waits for all DaemonSet pods to be ready
func (p *IptablesFaultProvider) waitForDaemonSetReady(ctx context.Context) error {
	clusterContext := p.getClusterContext()
	return wait.PollImmediate(5*time.Second, IptablesReadyTimeout, func() (bool, error) {
		// Get DaemonSet status first
		var ds appsv1.DaemonSet
		if err := p.config.Client.Get(ctx, client.ObjectKey{
			Name:      IptablesDaemonSetName,
			Namespace: p.config.Namespace,
		}, &ds); err != nil {
			Logf("[ERROR]", "[%s] Failed to get DaemonSet %s status: %v", clusterContext, IptablesDaemonSetName, err)
			return false, err
		}

		Logf("[DEBUG]", "[%s] DaemonSet %s status: Desired=%d, Current=%d, Ready=%d, Available=%d",
			clusterContext, IptablesDaemonSetName, ds.Status.DesiredNumberScheduled,
			ds.Status.CurrentNumberScheduled, ds.Status.NumberReady, ds.Status.NumberAvailable)

		pods, err := p.getDaemonSetPods(ctx)
		if err != nil {
			Logf("[ERROR]", "[%s] Failed to get DaemonSet %s pods: %v", clusterContext, IptablesDaemonSetName, err)
			return false, err
		}

		if len(pods) == 0 {
			Logf("[DEBUG]", "[%s] No pods found for DaemonSet %s, still waiting...", clusterContext, IptablesDaemonSetName)
			return false, nil // Still waiting for pods
		}

		Logf("[DEBUG]", "[%s] Found %d pods for DaemonSet %s", clusterContext, len(pods), IptablesDaemonSetName)

		// Check if all pods are ready and log detailed status
		readyCount := 0
		for i, pod := range pods {
			Logf("[DEBUG]", "[%s] Pod %d: %s (Node: %s, Phase: %s)", clusterContext, i+1, pod.Name, pod.Spec.NodeName, pod.Status.Phase)

			// Check container statuses
			for _, containerStatus := range pod.Status.ContainerStatuses {
				Logf("[DEBUG]", "[%s]   Container: %s, Ready: %t, RestartCount: %d",
					clusterContext, containerStatus.Name, containerStatus.Ready, containerStatus.RestartCount)
				if containerStatus.State.Waiting != nil {
					Logf("[WARN]", "[%s]     Waiting: Reason=%s, Message=%s",
						clusterContext, containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message)
				}
				if containerStatus.State.Terminated != nil {
					Logf("[ERROR]", "[%s]     Terminated: Reason=%s, Message=%s, ExitCode=%d",
						clusterContext, containerStatus.State.Terminated.Reason,
						containerStatus.State.Terminated.Message, containerStatus.State.Terminated.ExitCode)
				}
			}

			// Check pod conditions
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady {
					if condition.Status == corev1.ConditionTrue {
						readyCount++
						Logf("[DEBUG]", "[%s]   Pod condition Ready: True", clusterContext)
					} else {
						Logf("[DEBUG]", "[%s]   Pod condition Ready: %s (Reason: %s, Message: %s)",
							clusterContext, condition.Status, condition.Reason, condition.Message)
					}
				} else {
					Logf("[DEBUG]", "[%s]   Pod condition %s: %s", clusterContext, condition.Type, condition.Status)
				}
			}
		}

		if readyCount == len(pods) && readyCount > 0 {
			Logf("[INFO]", "[%s] All %d pods for DaemonSet %s are ready!", clusterContext, readyCount, IptablesDaemonSetName)
			return true, nil
		}

		Logf("[DEBUG]", "[%s] DaemonSet %s: %d/%d pods ready, continuing to wait...",
			clusterContext, IptablesDaemonSetName, readyCount, len(pods))
		return false, nil
	})
}

// getDaemonSetPods returns all ready pods belonging to the iptables DaemonSet
func (p *IptablesFaultProvider) getDaemonSetPods(ctx context.Context) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	listOptions := []client.ListOption{
		client.MatchingLabels{"app": "csi-addons-iptables-manager"},
		client.InNamespace(p.config.Namespace),
	}

	if err := p.config.Client.List(ctx, podList, listOptions...); err != nil {
		return nil, err
	}

	var readyPods []corev1.Pod
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			readyPods = append(readyPods, pod)
		}
	}

	return readyPods, nil
}

// collectPodEvents collects events related to a specific pod
func (p *IptablesFaultProvider) collectPodEvents(ctx context.Context, podName string) (string, error) {
	eventList := &corev1.EventList{}
	if err := p.config.Client.List(ctx, eventList, client.InNamespace(p.config.Namespace)); err != nil {
		return "", err
	}

	var relevantEvents []string
	for _, event := range eventList.Items {
		if event.InvolvedObject.Name == podName && event.InvolvedObject.Kind == "Pod" {
			relevantEvents = append(relevantEvents, fmt.Sprintf("%s: %s (Reason: %s)",
				event.FirstTimestamp.Format("15:04:05"), event.Message, event.Reason))
		}
	}

	if len(relevantEvents) == 0 {
		return "No events found", nil
	}
	return strings.Join(relevantEvents, "; "), nil
}

// collectPodLogs collects logs from a specific pod
func (p *IptablesFaultProvider) collectPodLogs(ctx context.Context, podName string) (string, error) {
	// This is a simplified log collection - in a real scenario you might use
	// the Kubernetes client-go logs API or kubectl
	_ = &corev1.PodLogOptions{
		Container:  IptablesContainerName,
		TailLines:  &[]int64{50}[0], // Get last 50 lines
		Timestamps: true,
	}

	// For now, return a placeholder - actual log collection would require
	// additional Kubernetes API setup
	return fmt.Sprintf("Log collection for pod %s (placeholder - would contain actual logs)", podName), nil
}

// removeFromActiveRules removes a CIDR from the active fence rules tracking
func (p *IptablesFaultProvider) removeFromActiveRules(targetCIDR string) {
	for i, rule := range p.activeFenceRules {
		if rule == targetCIDR {
			p.activeFenceRules = append(p.activeFenceRules[:i], p.activeFenceRules[i+1:]...)
			break
		}
	}
}

// createIptablesConfigMap creates a ConfigMap with initial iptables rule script using templates
func (p *IptablesFaultProvider) createIptablesConfigMap() *corev1.ConfigMap {
	data := TemplateData{
		Namespace: p.config.Namespace,
	}

	configMap := &corev1.ConfigMap{}
	if err := p.renderTemplate("templates/iptables-configmap.yaml", data, configMap); err != nil {
		panic(fmt.Sprintf("Failed to render ConfigMap template: %v", err))
	}

	return configMap
}

// addIptablesRule updates the ConfigMap with new iptables rules
func (p *IptablesFaultProvider) addIptablesRule(ctx context.Context, targetCIDR, action string) error {
	if p.configMap == nil {
		return fmt.Errorf("ConfigMap not created")
	}

	Logf("[DEBUG]", "Updating ConfigMap %s with %s action for CIDR %s", p.configMap.Name, action, targetCIDR)

	// Get current ConfigMap
	key := client.ObjectKeyFromObject(p.configMap)
	if err := p.config.Client.Get(ctx, key, p.configMap); err != nil {
		Logf("[ERROR]", "Failed to get ConfigMap %s: %v", p.configMap.Name, err)
		return fmt.Errorf("failed to get ConfigMap %s: %w", p.configMap.Name, err)
	}

	// Build the script content
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("# Iptables rules for network fault injection\n")
	script.WriteString(fmt.Sprintf("# Generated for DaemonSet %s\n", IptablesDaemonSetName))
	script.WriteString(fmt.Sprintf("echo \"[$(date)] Processing %s action for %s\"\n\n", action, targetCIDR))

	if action == "fence" {
		script.WriteString(fmt.Sprintf("# Block traffic to %s\n", targetCIDR))
		script.WriteString(fmt.Sprintf("iptables -C OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || \\\n", targetCIDR))
		script.WriteString(fmt.Sprintf("  iptables -I OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable\n", targetCIDR))
		script.WriteString("echo \"[$(date)] Fenced: blocked traffic to " + targetCIDR + "\"\n")
		script.WriteString("iptables -L OUTPUT -n | grep " + strings.Split(targetCIDR, "/")[0] + " || echo \"[$(date)] No rules found for " + targetCIDR + "\"\n\n")
	} else if action == "unfence" {
		script.WriteString(fmt.Sprintf("# Unblock traffic to %s\n", targetCIDR))
		script.WriteString(fmt.Sprintf("iptables -D OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || true\n", targetCIDR))
		script.WriteString("echo \"[$(date)] Unfenced: unblocked traffic to " + targetCIDR + "\"\n")
		script.WriteString("iptables -L OUTPUT -n | grep " + strings.Split(targetCIDR, "/")[0] + " || echo \"[$(date)] No rules found for " + targetCIDR + " (expected after unfence)\"\n\n")
	}

	scriptContent := script.String()
	Logf("[DEBUG]", "Generated iptables script for %s %s:\n%s", action, targetCIDR, scriptContent)

	// Update ConfigMap
	p.configMap.Data["apply.sh"] = scriptContent
	if err := p.config.Client.Update(ctx, p.configMap); err != nil {
		Logf("[ERROR]", "Failed to update ConfigMap %s: %v", p.configMap.Name, err)
		return fmt.Errorf("failed to update ConfigMap %s: %w", p.configMap.Name, err)
	}

	Logf("[DEBUG]", "Successfully updated ConfigMap %s with %s action for CIDR %s", p.configMap.Name, action, targetCIDR)
	return nil
}

// createPingJob creates a Kubernetes Job that pings the target IP to test connectivity
func (p *IptablesFaultProvider) createPingJob(targetIP string) *batchv1.Job {
	jobName := fmt.Sprintf("ping-test-%s-%d", strings.ReplaceAll(targetIP, ".", "-"), time.Now().Unix())

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: p.config.Namespace,
			Labels: map[string]string{
				"app":                          "ping-test",
				"csi-addons.io/component":      "connectivity-test",
				"csi-addons.io/fault-injector": "iptables",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &[]int32{2}[0],
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                          "ping-test",
						"csi-addons.io/component":      "connectivity-test",
						"csi-addons.io/fault-injector": "iptables",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "ping",
							Image: "alpine:latest",
							Command: []string{
								"sh", "-c",
								fmt.Sprintf("ping -c 3 -W 5 %s", targetIP),
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

// collectDaemonSetLogs collects logs from all DaemonSet pods for debugging
func (p *IptablesFaultProvider) collectDaemonSetLogs(ctx context.Context) error {
	if p.daemonSet == nil {
		return fmt.Errorf("DaemonSet not available for log collection")
	}

	clusterContext := p.getClusterContext()
	pods, err := p.getDaemonSetPods(ctx)
	if err != nil {
		return fmt.Errorf("[%s] failed to get DaemonSet pods for log collection: %w", clusterContext, err)
	}

	if len(pods) == 0 {
		Logf("[WARNING]", "[%s] No pods found for DaemonSet %s log collection", clusterContext, IptablesDaemonSetName)
		return nil
	}

	Logf("[INFO]", "[%s] Collecting logs from %d pods in DaemonSet %s", clusterContext, len(pods), IptablesDaemonSetName)

	// Try to get logs using kubectl if available, otherwise log the pod names and status
	for i, pod := range pods {
		podInfo := fmt.Sprintf("Pod %d/%d: %s (Node: %s, Phase: %s)",
			i+1, len(pods), pod.Name, pod.Spec.NodeName, pod.Status.Phase)

		Logf("[INFO]", "[%s] DaemonSet %s %s", clusterContext, IptablesDaemonSetName, podInfo)

		// Log pod conditions for debugging
		for _, condition := range pod.Status.Conditions {
			if condition.Status == corev1.ConditionFalse {
				Logf("[WARNING]", "[%s] DaemonSet %s pod %s condition %s: %s - %s",
					clusterContext, IptablesDaemonSetName, pod.Name, condition.Type, condition.Status, condition.Message)
			}
		}

		// Log container statuses
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == IptablesContainerName {
				Logf("[DEBUG]", "[%s] DaemonSet %s pod %s container %s ready: %t, restarts: %d",
					clusterContext, IptablesDaemonSetName, pod.Name, containerStatus.Name, containerStatus.Ready, containerStatus.RestartCount)

				if containerStatus.State.Waiting != nil {
					Logf("[WARNING]", "[%s] DaemonSet %s pod %s container %s waiting: %s - %s",
						clusterContext, IptablesDaemonSetName, pod.Name, containerStatus.Name,
						containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message)
				}

				if containerStatus.State.Terminated != nil {
					Logf("[ERROR]", "[%s] DaemonSet %s pod %s container %s terminated: %s - %s (exit code: %d)",
						clusterContext, IptablesDaemonSetName, pod.Name, containerStatus.Name,
						containerStatus.State.Terminated.Reason, containerStatus.State.Terminated.Message,
						containerStatus.State.Terminated.ExitCode)
				}
			}
		}
	}

	return nil
}
