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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// IptablesDaemonSetName is the name of the DaemonSet used for iptables operations
	IptablesDaemonSetName = "csi-addons-iptables-manager"

	// IptablesContainerName is the name of the container in the DaemonSet
	IptablesContainerName = "iptables-manager"

	// DefaultIptablesImage is the default container image for iptables operations
	DefaultIptablesImage = "alpine:latest"

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
	if !p.deployed {
		if err := p.deployDaemonSet(ctx); err != nil {
			Logf("[ERROR]", "Failed to deploy iptables DaemonSet for fencing IP %s: %v", targetCIDR, err)
			return fmt.Errorf("failed to deploy iptables DaemonSet: %w", err)
		}
	}

	// Add the rule to the ConfigMap
	if err := p.addIptablesRule(ctx, targetCIDR, "fence"); err != nil {
		Logf("[ERROR]", "Failed to add iptables fence rule for IP %s: %v", targetCIDR, err)
		return fmt.Errorf("failed to add iptables fence rule: %w", err)
	}

	// Track the rule for cleanup
	p.activeFenceRules = append(p.activeFenceRules, targetCIDR)

	Logf("[INFO]", "Successfully fenced IP %s using iptables", targetCIDR)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

func (p *IptablesFaultProvider) UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	if !p.deployed {
		Logf("[ERROR]", "Cannot unfence IP %s: iptables DaemonSet not deployed", targetCIDR)
		return fmt.Errorf("iptables DaemonSet not deployed")
	}

	// Remove the rule from the ConfigMap
	if err := p.addIptablesRule(ctx, targetCIDR, "unfence"); err != nil {
		Logf("[ERROR]", "Failed to add iptables unfence rule for IP %s: %v", targetCIDR, err)
		return fmt.Errorf("failed to add iptables unfence rule: %w", err)
	}

	// Remove from active rules tracking
	p.removeFromActiveRules(targetCIDR)

	Logf("[INFO]", "Successfully unfenced IP %s using iptables", targetCIDR)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

func (p *IptablesFaultProvider) VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool) (bool, error) {
	if !p.deployed {
		Logf("[ERROR]", "Cannot verify connectivity for IP %s: iptables DaemonSet not deployed", targetCIDR)
		return false, fmt.Errorf("iptables DaemonSet not deployed")
	}

	// Extract IP from CIDR if needed
	targetIP := strings.Split(targetCIDR, "/")[0]

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

	// Clean up any active fence rules
	for _, targetCIDR := range p.activeFenceRules {
		if err := p.UnfenceIP(ctx, targetCIDR, nil); err != nil {
			Logf("[ERROR]", "Failed to unfence %s during cleanup: %v", targetCIDR, err)
			errors = append(errors, fmt.Sprintf("failed to unfence %s: %v", targetCIDR, err))
		}
	}

	// Delete the ConfigMap if created
	if p.configMap != nil {
		if err := p.config.Client.Delete(ctx, p.configMap); err != nil {
			Logf("[ERROR]", "Failed to delete ConfigMap during cleanup: %v", err)
			errors = append(errors, fmt.Sprintf("failed to delete ConfigMap: %v", err))
		}
	}

	// Delete the DaemonSet if deployed
	if p.deployed && p.daemonSet != nil {
		if err := p.config.Client.Delete(ctx, p.daemonSet); err != nil {
			Logf("[ERROR]", "Failed to delete DaemonSet during cleanup: %v", err)
			errors = append(errors, fmt.Sprintf("failed to delete DaemonSet: %v", err))
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

// deployDaemonSet creates and deploys the iptables management DaemonSet and ConfigMap
func (p *IptablesFaultProvider) deployDaemonSet(ctx context.Context) error {
	// Create ConfigMap for iptables rules
	p.configMap = p.createIptablesConfigMap()
	if err := p.config.Client.Create(ctx, p.configMap); err != nil {
		return fmt.Errorf("failed to create ConfigMap: %w", err)
	}

	// Create DaemonSet
	p.daemonSet = p.createIptablesDaemonSet()
	if err := p.config.Client.Create(ctx, p.daemonSet); err != nil {
		return fmt.Errorf("failed to create DaemonSet: %w", err)
	}

	// Wait for DaemonSet pods to be ready
	if err := p.waitForDaemonSetReady(ctx); err != nil {
		return fmt.Errorf("DaemonSet not ready: %w", err)
	}

	p.deployed = true
	return nil
}

// createIptablesDaemonSet creates the DaemonSet manifest for iptables operations
func (p *IptablesFaultProvider) createIptablesDaemonSet() *appsv1.DaemonSet {
	privileged := true
	hostNetwork := true
	runAsUser := int64(0)

	// Get configuration from environment or use defaults
	image := os.Getenv(EnvIptablesImage)
	if image == "" {
		image = DefaultIptablesImage
	}

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

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IptablesDaemonSetName,
			Namespace: p.config.Namespace,
			Labels: map[string]string{
				"app":                          "csi-addons-iptables-manager",
				"csi-addons.io/component":      "fault-injection",
				"csi-addons.io/fault-injector": "iptables",
			},
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
					HostNetwork:  hostNetwork,
					NodeSelector: nodeSelector,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: &runAsUser,
					},
					Containers: []corev1.Container{
						{
							Name:  IptablesContainerName,
							Image: image,
							Command: []string{
								"sh", "-c",
								// Install iptables and run monitoring loop
								"apk add --no-cache iptables inotify-tools 2>/dev/null || true; " +
									"while true; do " +
									"  if [ -f /rules/apply.sh ]; then " +
									"    chmod +x /rules/apply.sh && /rules/apply.sh; " +
									"  fi; " +
									"  sleep 10; " +
									"done",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"NET_ADMIN",
										"NET_RAW", // May be needed for ping
									},
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
										Command: []string{"which", "iptables"},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "iptables-rules",
									MountPath: "/rules",
									ReadOnly:  true,
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
						},
					},
					TerminationGracePeriodSeconds: &[]int64{10}[0],
					Volumes: []corev1.Volume{
						{
							Name: "iptables-rules",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: IptablesDaemonSetName + "-rules",
									},
									DefaultMode: &[]int32{0755}[0],
								},
							},
						},
					},
				},
			},
		},
	}
}

// waitForDaemonSetReady waits for all DaemonSet pods to be ready
func (p *IptablesFaultProvider) waitForDaemonSetReady(ctx context.Context) error {
	return wait.PollImmediate(5*time.Second, IptablesReadyTimeout, func() (bool, error) {
		pods, err := p.getDaemonSetPods(ctx)
		if err != nil {
			return false, err
		}

		if len(pods) == 0 {
			return false, nil // Still waiting for pods
		}

		// Check if all pods are ready
		for _, pod := range pods {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					goto nextPod
				}
			}
			return false, nil // Pod not ready
		nextPod:
		}

		return true, nil // All pods ready
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

// removeFromActiveRules removes a CIDR from the active fence rules tracking
func (p *IptablesFaultProvider) removeFromActiveRules(targetCIDR string) {
	for i, rule := range p.activeFenceRules {
		if rule == targetCIDR {
			p.activeFenceRules = append(p.activeFenceRules[:i], p.activeFenceRules[i+1:]...)
			break
		}
	}
}

// createIptablesConfigMap creates a ConfigMap with initial iptables rule script
func (p *IptablesFaultProvider) createIptablesConfigMap() *corev1.ConfigMap {
	initialScript := `#!/bin/sh
# Initial iptables rules script
# Rules will be updated dynamically by the fault injection provider
echo "Iptables rules manager initialized"
`

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IptablesDaemonSetName + "-rules",
			Namespace: p.config.Namespace,
			Labels: map[string]string{
				"app":                          "csi-addons-iptables-manager",
				"csi-addons.io/component":      "fault-injection",
				"csi-addons.io/fault-injector": "iptables",
			},
		},
		Data: map[string]string{
			"apply.sh": initialScript,
		},
	}
}

// addIptablesRule updates the ConfigMap with new iptables rules
func (p *IptablesFaultProvider) addIptablesRule(ctx context.Context, targetCIDR, action string) error {
	if p.configMap == nil {
		return fmt.Errorf("ConfigMap not created")
	}

	// Get current ConfigMap
	key := client.ObjectKeyFromObject(p.configMap)
	if err := p.config.Client.Get(ctx, key, p.configMap); err != nil {
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Build the script content
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("# Iptables rules for network fault injection\n\n")

	if action == "fence" {
		script.WriteString(fmt.Sprintf("# Block traffic to %s\n", targetCIDR))
		script.WriteString(fmt.Sprintf("iptables -C OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || \\\n", targetCIDR))
		script.WriteString(fmt.Sprintf("  iptables -I OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable\n", targetCIDR))
		script.WriteString("echo \"Fenced: blocked traffic to " + targetCIDR + "\"\n\n")
	} else if action == "unfence" {
		script.WriteString(fmt.Sprintf("# Unblock traffic to %s\n", targetCIDR))
		script.WriteString(fmt.Sprintf("iptables -D OUTPUT -d %s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || true\n", targetCIDR))
		script.WriteString("echo \"Unfenced: unblocked traffic to " + targetCIDR + "\"\n\n")
	}

	// Update ConfigMap
	p.configMap.Data["apply.sh"] = script.String()
	if err := p.config.Client.Update(ctx, p.configMap); err != nil {
		return fmt.Errorf("failed to update ConfigMap: %w", err)
	}

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
