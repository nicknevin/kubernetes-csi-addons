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
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
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

	// DefaultIptablesImageWithRegistry is the default image with docker.io prefix
	// to avoid containerd localhost/ resolution issues in e2e tests
	DefaultIptablesImageWithRegistry = "docker.io/csi-addons/iptables-manager:latest"

	// IptablesReadyTimeout is how long to wait for DaemonSet pods to be ready
	IptablesReadyTimeout = 120 * time.Second
)

// Environment variable names for iptables configuration
const (
	EnvIptablesTargetNodes          = "E2E_IPTABLES_TARGET_NODES"
	EnvIptablesImage                = "E2E_IPTABLES_IMAGE"
	EnvIptablesCleanupTimeout       = "E2E_IPTABLES_CLEANUP_TIMEOUT"
	EnvIptablesDaemonSetNamespace   = "E2E_IPTABLES_DAEMONSET_NAMESPACE"
	defaultIptablesSuiteDSNamespace = "csi-addons-system"

	// IptablesFenceStateConfigMapName holds active fence CIDRs for visibility (kubectl get -n <ds-ns> cm <name>).
	IptablesFenceStateConfigMapName = "csi-addons-iptables-fence-state"

	iptablesFenceEventComponent = "csi-addons-e2e-iptables"

	// DaemonSet annotations: survive default event TTL (~1h). Inspect: kubectl -n <ns> get ds csi-addons-iptables-manager -o yaml | grep csi-addons.io/e2e-iptables
	annotationIptablesFenceReason  = "csi-addons.io/e2e-iptables-last-reason"
	annotationIptablesFenceUTC     = "csi-addons.io/e2e-iptables-last-utc"
	annotationIptablesFenceSummary = "csi-addons.io/e2e-iptables-last-summary"
	iptablesFenceEventAction       = "FenceStateChange"

	// Event reasons (kubectl get events -n <ds-ns>; involved object may be listed as regarding, not involvedObject on events.k8s.io)
	EventReasonIptablesFenceStarting = "IptablesFenceStarting"
	EventReasonIptablesFenceApplied  = "IptablesFenceApplied"
	EventReasonIptablesFenceRemoved  = "IptablesFenceRemoved"
	EventReasonIptablesFenceCleanup  = "IptablesFenceTeardownCleanup"
)

// csiAddonsStagedRejectCleanupShell removes OUTPUT and FORWARD rules inserted by FenceIP (--reject-with icmp-host-unreachable).
// FORWARD is required so pod-originated traffic (CNI) to the peer is dropped; OUTPUT alone only affects host netns.
// It intentionally does not remove other host firewall REJECT rules.
const csiAddonsStagedRejectCleanupShell = `
			echo "[$(date)] CSI-Addons: removing staged OUTPUT/FORWARD REJECT rules (icmp-host-unreachable only)"
			for ipt_cmd in iptables-legacy iptables-nft iptables; do
				if command -v $ipt_cmd >/dev/null 2>&1; then
					echo "[$(date)] Using $ipt_cmd for staged fence cleanup"
					for chain in OUTPUT FORWARD; do
						$ipt_cmd -S "$chain" | grep 'icmp-host-unreachable' | sed 's/^-A/-D/' | while read rule; do
							echo "[$(date)] Removing rule: $rule"
							$ipt_cmd $rule 2>/dev/null || true
						done
					done
					break
				fi
			done
			echo "[$(date)] CSI-Addons staged fence cleanup finished"
`

// IptablesFaultProvider implements PeerFenceProvider using iptables rules via privileged DaemonSets.
// This provider creates a privileged DaemonSet with NET_ADMIN capabilities to manipulate
// iptables rules for network fault injection.
type IptablesFaultProvider struct {
	config    FaultInjectionConfig
	daemonSet *appsv1.DaemonSet
	deployed  bool

	// dsNamespace is the namespace where csi-addons-iptables-manager lives (may differ from config.Namespace).
	dsNamespace string
	// ownsDaemonSet is true when this provider created the DaemonSet (suite-level DS is adopted, not owned).
	ownsDaemonSet bool

	// Track active fence rules for cleanup
	activeFenceRules []string
}

// NewIptablesFaultProvider creates a new iptables-based fault injection provider.
func NewIptablesFaultProvider(config FaultInjectionConfig) (PeerFenceProvider, error) {
	provider := &IptablesFaultProvider{
		config:           config,
		activeFenceRules: make([]string, 0),
	}

	// Perform emergency cleanup of any leftover fence rules from previous runs
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := provider.emergencyCleanup(ctx); err != nil {
		Logf("[WARNING]", "Failed to perform emergency cleanup: %v", err)
		// Don't fail provider creation - this is just cleanup
	}

	return provider, nil
}

func (p *IptablesFaultProvider) IsSupported(ctx context.Context) bool {
	// Check if privileged DaemonSets are supported
	return HasPrivilegedDaemonSetSupport(ctx, p.config.Client)
}

func (p *IptablesFaultProvider) FenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	clusterContext := p.getClusterContext()
	Logf("[INFO]", "[%s] FenceIP: applying iptables block for CIDR %s (DaemonSet %s)", clusterContext, targetCIDR, IptablesDaemonSetName)
	Logf("[DEBUG]", "[%s] Starting FenceIP operation for CIDR %s using DaemonSet %s", clusterContext, targetCIDR, IptablesDaemonSetName)

	if err := p.ensureDaemonSet(ctx); err != nil {
		Logf("[ERROR]", "[%s] Failed to deploy iptables DaemonSet %s for fencing IP %s: %v", clusterContext, IptablesDaemonSetName, targetCIDR, err)
		return fmt.Errorf("[%s] failed to deploy iptables DaemonSet %s: %w", clusterContext, IptablesDaemonSetName, err)
	}
	if p.ownsDaemonSet {
		Logf("[DEBUG]", "[%s] Using provider-owned DaemonSet %s in namespace %s", clusterContext, IptablesDaemonSetName, p.dsNamespace)
	} else {
		Logf("[DEBUG]", "[%s] Using existing DaemonSet %s in namespace %s", clusterContext, IptablesDaemonSetName, p.dsNamespace)
	}

	// Record event + ConfigMap *before* iptables runs: fencing the API/control-plane node breaks subsequent API calls.
	p.emitIptablesFenceEvent(ctx, EventReasonIptablesFenceStarting, fmt.Sprintf(
		"About to apply OUTPUT+FORWARD REJECT to %s (workload namespace %q). If this CIDR is the apiserver or node network, API access may fail until unfence.",
		targetCIDR, p.config.Namespace))
	if err := p.syncFenceStateConfigMapPreApply(ctx, targetCIDR); err != nil {
		Logf("[WARNING]", "sync pre-apply fence state ConfigMap: %v", err)
	}

	Logf("[DEBUG]", "[%s] Adding iptables fence rule for CIDR %s to DaemonSet pods", clusterContext, targetCIDR)
	if err := p.executeIptablesCommand(ctx, targetCIDR, "fence"); err != nil {
		Logf("[ERROR]", "[%s] Failed to add iptables fence rule for IP %s to DaemonSet %s: %v", clusterContext, targetCIDR, IptablesDaemonSetName, err)
		return fmt.Errorf("[%s] failed to add iptables fence rule to %s: %w", clusterContext, IptablesDaemonSetName, err)
	}

	if !slices.Contains(p.activeFenceRules, targetCIDR) {
		p.activeFenceRules = append(p.activeFenceRules, targetCIDR)
	}

	// Best-effort: API may be unreachable if the fenced CIDR includes the control-plane node/network.
	if err := p.syncFenceStateConfigMap(ctx); err != nil {
		// Don't treat ConfigMap sync failures as fatal - if the API is blocked by the new fence rule, this will likely fail. The important part is that we attempted to record the intended state before applying the fence.
		Logf("[WARNING]", "post-fence sync fence state ConfigMap (may fail if API path is blocked): %v", err)
	}
	// Record event after successfully applying the fence, ignoring ConfigMap sync errors which may indicate API access issues due to the new fence rule.
	p.emitIptablesFenceEvent(ctx, EventReasonIptablesFenceApplied, fmt.Sprintf(
		"Applied OUTPUT+FORWARD REJECT to %s (workload namespace %q). State: kubectl -n %s get configmap %s -o yaml",
		targetCIDR, p.config.Namespace, p.dsNamespace, IptablesFenceStateConfigMapName))

	Logf("[INFO]", "[%s] Successfully fenced %s using iptables (tracked CIDRs: %v; ConfigMap %s/%s)",
		clusterContext, targetCIDR, p.activeFenceRules, p.dsNamespace, IptablesFenceStateConfigMapName)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

func (p *IptablesFaultProvider) UnfenceIP(ctx context.Context, targetCIDR string, params map[string]string) error {
	clusterContext := p.getClusterContext()
	Logf("[INFO]", "[%s] UnfenceIP: removing iptables block for CIDR %s (DaemonSet %s)", clusterContext, targetCIDR, IptablesDaemonSetName)
	Logf("[DEBUG]", "[%s] Starting UnfenceIP operation for CIDR %s using DaemonSet %s", clusterContext, targetCIDR, IptablesDaemonSetName)

	if !p.deployed {
		Logf("[ERROR]", "[%s] Cannot unfence IP %s: iptables DaemonSet %s not deployed", clusterContext, targetCIDR, IptablesDaemonSetName)
		return fmt.Errorf("[%s] iptables DaemonSet %s not deployed", clusterContext, IptablesDaemonSetName)
	}

	// Remove the rule from the DaemonSet pods
	Logf("[DEBUG]", "[%s] Removing iptables fence rule for CIDR %s from DaemonSet pods", clusterContext, targetCIDR)
	if err := p.executeIptablesCommand(ctx, targetCIDR, "unfence"); err != nil {
		Logf("[ERROR]", "[%s] Failed to add iptables unfence rule for IP %s to DaemonSet %s: %v", clusterContext, targetCIDR, IptablesDaemonSetName, err)
		return fmt.Errorf("[%s] failed to add iptables unfence rule to %s: %w", clusterContext, IptablesDaemonSetName, err)
	}

	p.removeFromActiveRules(targetCIDR)

	// Record event + ConfigMap after unfencing the API/control-plane node, so API calls succeed.
	p.emitIptablesFenceEvent(ctx, EventReasonIptablesFenceRemoved, fmt.Sprintf(
		"Removed OUTPUT+FORWARD REJECT for %s (workload namespace %q)", targetCIDR, p.config.Namespace))
	if err := p.syncFenceStateConfigMap(ctx); err != nil {
		Logf("[WARNING]", "sync fence state ConfigMap after unfence: %v", err)
	}

	Logf("[INFO]", "[%s] Successfully unfenced %s using iptables (remaining tracked: %v)", clusterContext, targetCIDR, p.activeFenceRules)

	// Give DaemonSet pods time to reload and apply the rules
	time.Sleep(5 * time.Second)

	return nil
}

// connectivityProbePollInterval and connectivityProbeTimeout bound how long we wait for the probe Job
// (ping / ip route / traceroute when available in the pre-built image — no runtime package installs).
const (
	connectivityProbePollInterval = 2 * time.Second
	// Allow slow image pull / scheduling (probe uses same pre-built image as the DaemonSet).
	connectivityProbeTimeout = 120 * time.Second
)

// EstablishConnectivityBaseline runs iptables-only Jobs (ping / ip route / traceroute) before applying fence rules.
// NetworkFence paths use VolumeReplication status instead; they do not call this.
func (p *IptablesFaultProvider) EstablishConnectivityBaseline(ctx context.Context, targetCIDR string) (*ConnectivityBaseline, error) {
	clusterContext := p.getClusterContext()
	targetIP := strings.Split(targetCIDR, "/")[0]
	Logf("[DEBUG]", "[%s] EstablishConnectivityBaseline for %s (IP %s)", clusterContext, targetCIDR, targetIP)

	b, err := p.runConnectivityProbeJob(ctx, targetIP, false)
	if err != nil {
		return nil, err
	}
	Logf("[INFO]", "[%s] Connectivity baseline for %s: %s", clusterContext, targetCIDR, b.String())
	if !b.AnyProbeSucceeded() {
		return b, fmt.Errorf("connectivity baseline: no probe succeeded for %s (ICMP may be blocked; no route/traceroute) — cannot verify fencing", targetCIDR)
	}
	return b, nil
}

func (p *IptablesFaultProvider) VerifyConnectivity(ctx context.Context, targetCIDR string, expectedFenced bool, baseline *ConnectivityBaseline) (bool, error) {
	clusterContext := p.getClusterContext()
	Logf("[DEBUG]", "[%s] VerifyConnectivity for %s (expected fenced: %t, has baseline: %t)", clusterContext, targetCIDR, expectedFenced, baseline != nil)

	if !p.deployed && expectedFenced {
		Logf("[ERROR]", "[%s] Cannot verify fenced connectivity for %s: iptables DaemonSet %s not ready", clusterContext, targetCIDR, IptablesDaemonSetName)
		return false, fmt.Errorf("[%s] iptables DaemonSet %s not deployed", clusterContext, IptablesDaemonSetName)
	}

	targetIP := strings.Split(targetCIDR, "/")[0]
	cur, err := p.runConnectivityProbeJob(ctx, targetIP, expectedFenced)
	if err != nil {
		return false, err
	}

	if baseline != nil {
		match := CompareProbeResultsToBaseline(baseline, cur, expectedFenced)
		Logf("[INFO]", "[%s] VerifyConnectivity %s vs baseline: expected fenced=%t, match=%t (now: %s)", clusterContext, targetCIDR, expectedFenced, match, cur.String())
		return match, nil
	}

	// No baseline: "reachable" if any probe succeeds (handles ICMP blocked on some paths).
	anyOK := cur.PingOK || cur.IPRouteOK || cur.TracerouteOK
	if expectedFenced {
		match := !anyOK
		Logf("[INFO]", "[%s] VerifyConnectivity %s (no baseline): expected fenced=%t, any probe ok=%t, match=%t", clusterContext, targetCIDR, expectedFenced, anyOK, match)
		return match, nil
	}
	Logf("[INFO]", "[%s] VerifyConnectivity %s (no baseline): expected reachable, any probe ok=%t", clusterContext, targetCIDR, anyOK)
	return anyOK, nil
}

// runConnectivityProbeJob runs ping, ip route get, and traceroute (pre-installed in image) in a short-lived Job; parses CSI_BASELINE from logs.
// expectedFenced only affects log level on watch timeout (job infra vs path loss).
func (p *IptablesFaultProvider) runConnectivityProbeJob(ctx context.Context, targetIP string, expectedFenced bool) (*ConnectivityBaseline, error) {
	job := p.createConnectivityProbeJob(targetIP)
	if err := p.config.Client.Create(ctx, job); err != nil {
		Logf("[ERROR]", "Failed to create connectivity probe job for IP %s: %v", targetIP, err)
		return nil, fmt.Errorf("failed to create connectivity probe job: %w", err)
	}
	jobKey := client.ObjectKeyFromObject(job)

	defer func() {
		del := &batchv1.Job{}
		if err := p.config.Client.Get(ctx, jobKey, del); err == nil {
			_ = p.config.Client.Delete(ctx, del)
		}
	}()

	err := wait.PollUntilContextTimeout(ctx, connectivityProbePollInterval, connectivityProbeTimeout, true, func(pollCtx context.Context) (bool, error) {
		var j batchv1.Job
		if err := p.config.Client.Get(pollCtx, jobKey, &j); err != nil {
			return false, fmt.Errorf("get probe job: %w", err)
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		var j batchv1.Job
		lateOK := p.config.Client.Get(ctx, jobKey, &j) == nil && (j.Status.Succeeded > 0 || j.Status.Failed > 0)
		if !lateOK {
			if wait.Interrupted(err) {
				if expectedFenced {
					Logf("[INFO]", "Connectivity probe job for %s did not finish within %v (expected fenced check); often image pull/scheduling — not ICMP proof",
						targetIP, connectivityProbeTimeout)
				} else {
					Logf("[ERROR]", "Connectivity probe job for %s: %v", targetIP, err)
				}
			}
			return nil, fmt.Errorf("connectivity probe job timeout for %s: %w", targetIP, err)
		}
	}

	logs, logErr := p.fetchProbeJobLogs(ctx, job.Namespace, job.Name)
	if logErr != nil {
		return nil, logErr
	}
	b, parseErr := ParseConnectivityBaselineFromLog(targetIP, logs)
	if parseErr != nil {
		Logf("[WARNING]", "probe log parse: %v; raw: %q", parseErr, truncateRunes(logs, 512))
		return nil, parseErr
	}
	return b, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func (p *IptablesFaultProvider) fetchProbeJobLogs(ctx context.Context, namespace, jobName string) (string, error) {
	if p.config.RESTConfig == nil {
		return "", fmt.Errorf("RESTConfig is required to read probe job logs")
	}
	cs, err := kubernetes.NewForConfig(p.config.RESTConfig)
	if err != nil {
		return "", fmt.Errorf("kubernetes clientset: %w", err)
	}
	podList := &corev1.PodList{}
	var listErr error
	for _, lbl := range []map[string]string{
		{"job-name": jobName},
		{"batch.kubernetes.io/job-name": jobName},
	} {
		podList = &corev1.PodList{}
		listErr = p.config.Client.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels(lbl))
		if listErr != nil {
			break
		}
		if len(podList.Items) > 0 {
			break
		}
	}
	if listErr != nil {
		return "", fmt.Errorf("list job pods: %w", listErr)
	}
	if len(podList.Items) == 0 {
		return "", fmt.Errorf("no pods for job %s", jobName)
	}
	podName := podList.Items[0].Name
	raw, err := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("get pod logs %s: %w", podName, err)
	}
	return string(raw), nil
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

	toUnfence := slices.Clone(p.activeFenceRules)
	for _, targetCIDR := range toUnfence {
		if err := p.UnfenceIP(ctx, targetCIDR, nil); err != nil {
			Logf("[ERROR]", "Failed to unfence %s during cleanup: %v", targetCIDR, err)
			errors = append(errors, fmt.Sprintf("failed to unfence %s: %v", targetCIDR, err))
		}
	}

	if err := p.removeCSIAddonsStagedRejectRulesOnAllManagerPods(ctx); err != nil {
		Logf("[WARNING]", "teardown: staged iptables sweep on manager pods: %v", err)
	}
	if err := p.deleteFenceStateConfigMap(ctx); err != nil {
		Logf("[WARNING]", "teardown: delete fence state ConfigMap: %v", err)
	}
	p.emitIptablesFenceEvent(ctx, EventReasonIptablesFenceCleanup, fmt.Sprintf(
		"Provider Cleanup finished for workload namespace %q (per-CIDR unfence + staged REJECT sweep)", p.config.Namespace))

	// Delete the DaemonSet only if this provider created it (suite-level DS stays for other tests).
	if p.deployed && p.daemonSet != nil && p.ownsDaemonSet {
		Logf("[DEBUG]", "Deleting provider-owned DaemonSet %s/%s during cleanup", p.dsNamespace, p.daemonSet.Name)
		if err := p.config.Client.Delete(ctx, p.daemonSet); err != nil {
			Logf("[ERROR]", "Failed to delete DaemonSet %s during cleanup: %v", p.daemonSet.Name, err)
			errors = append(errors, fmt.Sprintf("failed to delete DaemonSet %s: %v", p.daemonSet.Name, err))
		}
	} else if p.deployed && p.daemonSet != nil && !p.ownsDaemonSet {
		Logf("[INFO]", "Leaving adopted iptables DaemonSet %s/%s in place (suite or shared resource)", p.dsNamespace, p.daemonSet.Name)
	}

	p.deployed = false
	p.ownsDaemonSet = false
	p.dsNamespace = ""
	p.daemonSet = nil
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
	// Check provider params first (tests should set this in full-DR so logs match the client cluster).
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
	// Avoid preferring DR1 when both DR*_CONTEXT are set (ambiguous for full-DR).
	if dr1 := os.Getenv("DR1_CONTEXT"); dr1 != "" && os.Getenv("DR2_CONTEXT") == "" {
		return "DR1"
	}
	if dr2 := os.Getenv("DR2_CONTEXT"); dr2 != "" && os.Getenv("DR1_CONTEXT") == "" {
		return "DR2"
	}
	// Fallback to namespace info
	return fmt.Sprintf("namespace:%s", p.config.Namespace)
}

func (p *IptablesFaultProvider) emitIptablesFenceEvent(ctx context.Context, reason, message string) {
	if p.dsNamespace == "" {
		return
	}
	key := client.ObjectKey{Namespace: p.dsNamespace, Name: IptablesDaemonSetName}
	var ds appsv1.DaemonSet
	if err := p.config.Client.Get(ctx, key, &ds); err != nil {
		Logf("[WARNING]", "iptables fence event skipped (%s): get DaemonSet %s: %v", reason, key, err)
		return
	}

	// Annotations persist after Kubernetes event garbage collection (default ~1h).
	p.patchDaemonSetIptablesFenceAnnotations(ctx, &ds, reason, message)

	note := truncateIptablesEventNote(message)
	r := truncateASCII(reason, 128)
	inst := iptablesFenceReportingInstance()

	ev1 := &eventsv1.Event{
		TypeMeta: metav1.TypeMeta{
			APIVersion: eventsv1.SchemeGroupVersion.String(),
			Kind:       "Event",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    p.dsNamespace,
			GenerateName: "iptables-fence-",
		},
		EventTime:           metav1.NowMicro(),
		ReportingController: iptablesFenceEventComponent,
		ReportingInstance:   inst,
		Action:              truncateASCII(iptablesFenceEventAction, 128),
		Reason:              r,
		Regarding: corev1.ObjectReference{
			Kind:       "DaemonSet",
			Namespace:  ds.Namespace,
			Name:       ds.Name,
			UID:        ds.UID,
			APIVersion: "apps/v1",
		},
		Note: note,
		Type: corev1.EventTypeNormal,
	}
	if err := p.config.Client.Create(ctx, ev1); err != nil {
		Logf("[WARNING]", "create events.k8s.io/v1 Event (%s): %v — falling back to core/v1 Event", reason, err)
		p.emitLegacyCoreV1Event(ctx, &ds, r, message)
		return
	}
	Logf("[DEBUG]", "recorded events.k8s.io/v1 Event reason=%s namespace=%s", r, p.dsNamespace)
}

func (p *IptablesFaultProvider) patchDaemonSetIptablesFenceAnnotations(ctx context.Context, ds *appsv1.DaemonSet, reason, message string) {
	original := ds.DeepCopy()
	if ds.Annotations == nil {
		ds.Annotations = map[string]string{}
	}
	ds.Annotations[annotationIptablesFenceReason] = truncateASCII(reason, 128)
	ds.Annotations[annotationIptablesFenceUTC] = time.Now().UTC().Format(time.RFC3339Nano)
	ds.Annotations[annotationIptablesFenceSummary] = truncateASCII(strings.ReplaceAll(strings.ReplaceAll(message, "\n", " "), "\r", " "), 512)
	if err := p.config.Client.Patch(ctx, ds, client.MergeFrom(original)); err != nil {
		Logf("[WARNING]", "patch DaemonSet %s/%s fence annotations: %v", ds.Namespace, ds.Name, err)
	}
}

func (p *IptablesFaultProvider) emitLegacyCoreV1Event(ctx context.Context, ds *appsv1.DaemonSet, reason, message string) {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "iptables-fence-",
			Namespace:    p.dsNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "DaemonSet",
			Namespace:  ds.Namespace,
			Name:       ds.Name,
			UID:        ds.UID,
			APIVersion: "apps/v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: iptablesFenceEventComponent},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if err := p.config.Client.Create(ctx, ev); err != nil {
		Logf("[WARNING]", "failed to record core/v1 iptables fence event (%s): %v", reason, err)
	}
}

func iptablesFenceReportingInstance() string {
	s := fmt.Sprintf("%s-%d", iptablesFenceEventComponent, time.Now().UnixNano())
	if len(s) > 128 {
		return s[:128]
	}
	return s
}

func truncateASCII(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func truncateIptablesEventNote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	return truncateASCII(s, 1024)
}

func (p *IptablesFaultProvider) fenceStateConfigMapLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "csi-addons-iptables-fence-state",
		"app.kubernetes.io/component":  "fault-injection",
		"csi-addons.io/fault-injector": "iptables",
	}
}

// syncFenceStateConfigMapPreApply writes ConfigMap state before OUTPUT REJECT runs (API may be lost afterward).
func (p *IptablesFaultProvider) syncFenceStateConfigMapPreApply(ctx context.Context, targetCIDR string) error {
	if p.dsNamespace == "" {
		return nil
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IptablesFenceStateConfigMapName,
			Namespace: p.dsNamespace,
			Labels:    p.fenceStateConfigMapLabels(),
		},
		Data: map[string]string{
			"fence-phase":         "applying",
			"applying-cidr":       targetCIDR,
			"active-cidrs":        strings.Join(p.activeFenceRules, "\n"),
			"workload-namespace":  p.config.Namespace,
			"cluster-label":       p.getClusterContext(),
			"fence-mode":          "iptables-output-reject-icmp-host-unreachable",
			"daemonset-namespace": p.dsNamespace,
			"updated":             time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	return p.createOrUpdateFenceStateConfigMap(ctx, cm)
}

func (p *IptablesFaultProvider) syncFenceStateConfigMap(ctx context.Context) error {
	if p.dsNamespace == "" {
		return nil
	}
	if len(p.activeFenceRules) == 0 {
		return p.deleteFenceStateConfigMap(ctx)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      IptablesFenceStateConfigMapName,
			Namespace: p.dsNamespace,
			Labels:    p.fenceStateConfigMapLabels(),
		},
		Data: map[string]string{
			"fence-phase":         "active",
			"active-cidrs":        strings.Join(p.activeFenceRules, "\n"),
			"workload-namespace":  p.config.Namespace,
			"cluster-label":       p.getClusterContext(),
			"fence-mode":          "iptables-output-reject-icmp-host-unreachable",
			"daemonset-namespace": p.dsNamespace,
			"updated":             time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	return p.createOrUpdateFenceStateConfigMap(ctx, cm)
}

func (p *IptablesFaultProvider) createOrUpdateFenceStateConfigMap(ctx context.Context, cm *corev1.ConfigMap) error {
	cmKey := client.ObjectKeyFromObject(cm)
	existing := &corev1.ConfigMap{}
	if err := p.config.Client.Get(ctx, cmKey, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return p.config.Client.Create(ctx, cm)
		}
		return err
	}
	cm.ResourceVersion = existing.ResourceVersion
	return p.config.Client.Update(ctx, cm)
}

func (p *IptablesFaultProvider) deleteFenceStateConfigMap(ctx context.Context) error {
	if p.dsNamespace == "" {
		return nil
	}
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: p.dsNamespace, Name: IptablesFenceStateConfigMapName}
	if err := p.config.Client.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return p.config.Client.Delete(ctx, cm)
}

// removeCSIAddonsStagedRejectRulesOnAllManagerPods runs csiAddonsStagedRejectCleanupShell on every running manager pod.
func (p *IptablesFaultProvider) removeCSIAddonsStagedRejectRulesOnAllManagerPods(ctx context.Context) error {
	if p.dsNamespace == "" {
		return nil
	}
	var ds appsv1.DaemonSet
	if err := p.config.Client.Get(ctx, client.ObjectKey{Namespace: p.dsNamespace, Name: IptablesDaemonSetName}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return p.cleanupAllFenceRules(ctx, &ds)
}

// daemonSetSearchNamespaces lists namespaces to probe for an existing suite-deployed DaemonSet.
func (p *IptablesFaultProvider) daemonSetSearchNamespaces() []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(ns string) {
		if ns == "" {
			return
		}
		if _, ok := seen[ns]; ok {
			return
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	add(os.Getenv(EnvIptablesDaemonSetNamespace))
	add(defaultIptablesSuiteDSNamespace)
	add(p.config.Namespace)
	return out
}

func (p *IptablesFaultProvider) ensureDaemonSet(ctx context.Context) error {
	if p.deployed && p.daemonSet != nil {
		return nil
	}
	return p.deployDaemonSet(ctx)
}

// tryAdoptExistingDaemonSet looks for csi-addons-iptables-manager in known namespaces (e.g. suite BeforeSuite).
func (p *IptablesFaultProvider) tryAdoptExistingDaemonSet(ctx context.Context) (bool, error) {
	clusterContext := p.getClusterContext()
	for _, ns := range p.daemonSetSearchNamespaces() {
		var ds appsv1.DaemonSet
		err := p.config.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: IptablesDaemonSetName}, &ds)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("[%s] get DaemonSet %s/%s: %w", clusterContext, ns, IptablesDaemonSetName, err)
		}
		p.daemonSet = &ds
		p.dsNamespace = ns
		p.ownsDaemonSet = false
		Logf("[INFO]", "[%s] Adopted existing iptables DaemonSet %s/%s (not owned — will not delete on Cleanup)", clusterContext, ns, IptablesDaemonSetName)
		if err := p.waitForDaemonSetReady(ctx); err != nil {
			return false, err
		}
		p.deployed = true
		return true, nil
	}
	return false, nil
}

// deployDaemonSet creates the iptables DaemonSet only (no rules ConfigMap; see IptablesFenceStateConfigMapName for runtime CM).
func (p *IptablesFaultProvider) deployDaemonSet(ctx context.Context) error {
	clusterContext := p.getClusterContext()

	if adopted, err := p.tryAdoptExistingDaemonSet(ctx); err != nil {
		return err
	} else if adopted {
		return nil
	}

	p.dsNamespace = p.config.Namespace
	p.ownsDaemonSet = true
	Logf("[INFO]", "[%s] Deploying iptables DaemonSet %s in namespace %s", clusterContext, IptablesDaemonSetName, p.dsNamespace)

	// Create DaemonSet directly without ConfigMap
	p.daemonSet = p.createIptablesDaemonSet()
	Logf("[DEBUG]", "[%s] Creating DaemonSet %s in namespace %s", clusterContext, p.daemonSet.Name, p.dsNamespace)
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

// getIptablesImage returns the image for the iptables DaemonSet and connectivity probe Jobs.
// Tools must be present in the image (build Containerfile.iptables); pods must not run apk/apt at runtime.
// Order: ProviderParams["image"], E2E_IPTABLES_IMAGE, DefaultIptablesImage.
func (p *IptablesFaultProvider) getIptablesImage() string {
	if p.config.ProviderParams != nil {
		if image := strings.TrimSpace(p.config.ProviderParams["image"]); image != "" {
			return normalizeIptablesImageRef(image)
		}
	}
	image := strings.TrimSpace(os.Getenv(EnvIptablesImage))
	if image == "" {
		image = DefaultIptablesImage
	}
	return normalizeIptablesImageRef(image)
}

func normalizeIptablesImageRef(image string) string {
	if strings.HasPrefix(image, "localhost/") {
		out := strings.TrimPrefix(image, "localhost/")
		Logf("[DEBUG]", "Normalized image name by removing localhost/ prefix: %s", out)
		return out
	}
	return image
}

// createIptablesDaemonSet creates the DaemonSet manifest for iptables operations using templates
func (p *IptablesFaultProvider) createIptablesDaemonSet() *appsv1.DaemonSet {
	if p.dsNamespace == "" {
		p.dsNamespace = p.config.Namespace
	}
	image := p.getIptablesImage()
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
		Namespace:    p.dsNamespace,
		Image:        image,
		NodeSelector: nodeSelector,
	}

	const templatePath = "templates/iptables-daemonset.yaml"
	daemonSet := &appsv1.DaemonSet{}
	if err := p.renderTemplate(templatePath, data, daemonSet); err != nil {
		Logf("[ERROR]", "Failed to render DaemonSet template: %v", err)
		return daemonSet // Return empty DaemonSet instead of panicking
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
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, IptablesReadyTimeout, true, func(pollCtx context.Context) (bool, error) {
		// Get DaemonSet status first
		var ds appsv1.DaemonSet
		if err := p.config.Client.Get(pollCtx, client.ObjectKey{
			Name:      IptablesDaemonSetName,
			Namespace: p.dsNamespace,
		}, &ds); err != nil {
			Logf("[ERROR]", "[%s] Failed to get DaemonSet %s status: %v", clusterContext, IptablesDaemonSetName, err)
			return false, err
		}

		Logf("[DEBUG]", "[%s] DaemonSet %s status: Desired=%d, Current=%d, Ready=%d, Available=%d",
			clusterContext, IptablesDaemonSetName, ds.Status.DesiredNumberScheduled,
			ds.Status.CurrentNumberScheduled, ds.Status.NumberReady, ds.Status.NumberAvailable)

		pods, err := p.getDaemonSetPods(pollCtx)
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
	ns := p.dsNamespace
	if ns == "" {
		ns = p.config.Namespace
	}
	listOptions := []client.ListOption{
		client.MatchingLabels{"app": "csi-addons-iptables-manager"},
		client.InNamespace(ns),
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
	ns := p.dsNamespace
	if ns == "" {
		ns = p.config.Namespace
	}
	if err := p.config.Client.List(ctx, eventList, client.InNamespace(ns)); err != nil {
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

// parseTargetCIDRWithPort parses CIDR:port format and returns (cidr, port, error).
// Format examples: "192.168.1.10/32:6800" -> ("192.168.1.10/32", "6800", nil)
//
//	"192.168.1.10/32" -> ("192.168.1.10/32", "", nil)
func parseTargetCIDRWithPort(target string) (string, string, error) {
	// Split by the last colon (IPv6 addresses have colons in the CIDR part)
	idx := strings.LastIndex(target, ":")
	if idx <= 0 {
		// No port specified
		return target, "", nil
	}

	cidrPart := target[:idx]
	portPart := target[idx+1:]

	// Validate that portPart is numeric and cidrPart looks like a CIDR
	if _, err := strconv.Atoi(portPart); err != nil {
		// Not a port number - treat as part of the CIDR
		return target, "", nil
	}

	// Verify cidrPart has a /prefix
	if !strings.Contains(cidrPart, "/") {
		return target, "", nil
	}

	return cidrPart, portPart, nil
}

// buildIptablesRuleCommand builds iptables rule command with optional port specification.
// Example with port: iptables -I OUTPUT -d 192.168.1.10/32 -p tcp --dport 6800 -j REJECT ...
// Example without port: iptables -I OUTPUT -d 192.168.1.10/32 -j REJECT ...
func buildIptablesRuleCommand(action, chain, cidr, port string) string {
	portSpec := ""
	if port != "" {
		portSpec = fmt.Sprintf(" -p tcp --dport %s", port)
	}

	return fmt.Sprintf("$IPT_CMD -C %s -d %s%s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || \\\n"+
		"$IPT_CMD -I %s -d %s%s -j REJECT --reject-with icmp-host-unreachable",
		chain, cidr, portSpec, chain, cidr, portSpec)
}

// buildIptablesDeleteCommand builds iptables delete rule command with optional port specification.
func buildIptablesDeleteCommand(chain, cidr, port string) string {
	portSpec := ""
	if port != "" {
		portSpec = fmt.Sprintf(" -p tcp --dport %s", port)
	}

	return fmt.Sprintf("$IPT_CMD -D %s -d %s%s -j REJECT --reject-with icmp-host-unreachable 2>/dev/null || true",
		chain, cidr, portSpec)
}

// executeIptablesCommand executes iptables commands directly on DaemonSet pods via kubectl exec
func (p *IptablesFaultProvider) executeIptablesCommand(ctx context.Context, targetCIDR, action string) error {
	clusterContext := p.getClusterContext()
	Logf("[DEBUG]", "[%s] Executing %s command for CIDR %s on DaemonSet pods", clusterContext, action, targetCIDR)

	if err := p.ensureDaemonSet(ctx); err != nil {
		return err
	}

	// Get all DaemonSet pods
	podList := &corev1.PodList{}
	labelSelector := client.MatchingLabels{"app": "csi-addons-iptables-manager"}
	ns := p.dsNamespace
	if ns == "" {
		ns = p.config.Namespace
	}
	if err := p.config.Client.List(ctx, podList, client.InNamespace(ns), labelSelector); err != nil {
		return fmt.Errorf("failed to list DaemonSet pods: %w", err)
	}

	if len(podList.Items) == 0 {
		return fmt.Errorf("no DaemonSet pods found")
	}

	// Build the iptables command with auto-detection for compatibility
	var command string
	baseCommand := `
		echo "[$(date)] CSI-Addons iptables operation starting..."
		# Auto-detect the correct iptables command with compatibility checking
		IPT_CMD=""
		for iptables_variant in iptables-legacy iptables iptables-nft; do
			if command -v "$iptables_variant" >/dev/null 2>&1; then
				# Test if we can actually use the command and check for compatibility issues
				test_output=$("$iptables_variant" -L OUTPUT -n 2>&1)
				exit_code=$?
				
				# Check for nf_tables compatibility issues
				if echo "$test_output" | grep -q "Could not fetch rule set generation id"; then
					echo "[$(date)] WARNING: $iptables_variant has nf_tables compatibility issues, trying next..."
					continue
				fi
				
				# Accept if it works or gives expected permission errors
				if [ $exit_code -eq 0 ] || echo "$test_output" | grep -q "Permission denied\|you must be root"; then
					IPT_CMD="$iptables_variant"
					echo "[$(date)] Using iptables command: $IPT_CMD"
					break
				fi
			fi
		done
		if [ -z "$IPT_CMD" ]; then
			echo "[$(date)] ERROR: No working iptables command found"
			exit 1
		fi
	`

	switch action {
	case "fence":
		// Parse CIDR:port format
		cidr, port, err := parseTargetCIDRWithPort(targetCIDR)
		if err != nil {
			return fmt.Errorf("parse target CIDR format: %w", err)
		}

		if port != "" {
			Logf("[INFO]", "[%s] FenceIP: using port-aware blocking for %s (port %s)", clusterContext, cidr, port)
		}

		// Build fence rules for both OUTPUT and FORWARD chains
		outputRule := buildIptablesRuleCommand("fence", "OUTPUT", cidr, port)
		forwardRule := buildIptablesRuleCommand("fence", "FORWARD", cidr, port)

		command = baseCommand + fmt.Sprintf(`
			echo "[$(date)] Fencing %s (with%s port %s; OUTPUT+FORWARD: pod traffic uses FORWARD; host uses OUTPUT)"
			%s
			%s
			echo "[$(date)] Fenced: %s"
		`, cidr, map[bool]string{true: "", false: "out"}[port != ""], port, outputRule, forwardRule, cidr)
	case "unfence":
		// Parse CIDR:port format
		cidr, port, err := parseTargetCIDRWithPort(targetCIDR)
		if err != nil {
			return fmt.Errorf("parse target CIDR format: %w", err)
		}

		// Build unfence (delete) rules for both chains
		outputDelete := buildIptablesDeleteCommand("OUTPUT", cidr, port)
		forwardDelete := buildIptablesDeleteCommand("FORWARD", cidr, port)

		command = baseCommand + fmt.Sprintf(`
			echo "[$(date)] Unfencing %s"
			%s
			%s
			echo "[$(date)] Unfenced: %s"
		`, cidr, outputDelete, forwardDelete, cidr)
	default:
		return fmt.Errorf("invalid action: %s", action)
	}

	// Execute command on all pods
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			Logf("[WARNING]", "[%s] Skipping non-running pod %s (phase: %s)", clusterContext, pod.Name, pod.Status.Phase)
			continue
		}

		Logf("[DEBUG]", "[%s] Executing %s on pod %s: %s", clusterContext, action, pod.Name, command)

		// Use kubectl exec to run the command
		if err := p.execCommandInPod(ctx, &pod, command); err != nil {
			Logf("[ERROR]", "[%s] Failed to execute %s command on pod %s: %v", clusterContext, action, pod.Name, err)
			return fmt.Errorf("failed to execute %s command on pod %s: %w", action, pod.Name, err)
		}

		Logf("[DEBUG]", "[%s] Successfully executed %s command on pod %s", clusterContext, action, pod.Name)
	}

	Logf("[INFO]", "[%s] Successfully %sed IP %s using iptables", clusterContext, action, targetCIDR)
	return nil
}

// createConnectivityProbeJob runs ping, ip route get, and traceroute (if present in image). Prints CSI_BASELINE.
// Uses the same pre-built iptables-manager image as the DaemonSet — no apk/apt on the pod.
func (p *IptablesFaultProvider) createConnectivityProbeJob(targetIP string) *batchv1.Job {
	jobName := fmt.Sprintf("conn-probe-%s-%d", strings.ReplaceAll(targetIP, ".", "-"), time.Now().UnixNano())
	probeImage := p.getIptablesImage()

	script := fmt.Sprintf(`set +e
T=%q
P=0 R=0 X=0
ping -c 1 -W 2 "$T" >/dev/null 2>&1 && P=1
if ip route get "$T" >/dev/null 2>&1; then R=1; fi
if command -v traceroute >/dev/null 2>&1; then
  if traceroute -n -m 5 -w 1 -q 1 "$T" 2>/dev/null | head -12 | grep -qE '^[[:space:]]*[0-9]+'; then X=1; fi
fi
echo "CSI_BASELINE ping=${P} ip_route=${R} traceroute=${X}"
exit 0
`, targetIP)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: p.config.Namespace,
			Labels: map[string]string{
				"app":                          "conn-probe",
				"csi-addons.io/component":      "connectivity-test",
				"csi-addons.io/fault-injector": "iptables",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            func() *int32 { v := int32(1); return &v }(),
			ActiveDeadlineSeconds:   func() *int64 { v := int64(180); return &v }(),
			TTLSecondsAfterFinished: func() *int32 { v := int32(120); return &v }(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                          "conn-probe",
						"csi-addons.io/component":      "connectivity-test",
						"csi-addons.io/fault-injector": "iptables",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "probe",
							Image:           probeImage,
							ImagePullPolicy: corev1.PullNever,
							Command:         []string{"sh", "-c", script},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
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

// waitIptablesContainerReady waits until the iptables-manager container in the DaemonSet pod is ready to exec.
func (p *IptablesFaultProvider) waitIptablesContainerReady(ctx context.Context, pod *corev1.Pod) error {
	key := client.ObjectKeyFromObject(pod)
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var fresh corev1.Pod
		if err := p.config.Client.Get(ctx, key, &fresh); err != nil {
			return false, err
		}
		if fresh.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		for i := range fresh.Status.ContainerStatuses {
			cs := &fresh.Status.ContainerStatuses[i]
			if cs.Name != IptablesContainerName {
				continue
			}
			return cs.Ready && cs.State.Running != nil, nil
		}
		return false, nil
	})
}

// streamExec runs a shell script inside the iptables DaemonSet container via the Kubernetes exec subresource.
func (p *IptablesFaultProvider) streamExec(ctx context.Context, pod *corev1.Pod, script string) error {
	if p.config.RESTConfig == nil {
		return fmt.Errorf("RESTConfig is nil")
	}
	clientset, err := kubernetes.NewForConfig(p.config.RESTConfig)
	if err != nil {
		return fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: IptablesContainerName,
			Command:   []string{"sh", "-c", script},
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, clientscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(p.config.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("remotecommand.NewSPDYExecutor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	clusterContext := p.getClusterContext()
	if err != nil {
		Logf("[ERROR]", "[%s] pod exec %s/%s failed: %v; stderr=%s", clusterContext, pod.Namespace, pod.Name, err, strings.TrimSpace(stderr.String()))
		return fmt.Errorf("pod exec failed: %w", err)
	}
	out := strings.TrimSpace(stdout.String())
	if out != "" {
		Logf("[INFO]", "[%s] pod exec %s/%s: %s", clusterContext, pod.Namespace, pod.Name, out)
	}
	if se := strings.TrimSpace(stderr.String()); se != "" {
		Logf("[DEBUG]", "[%s] pod exec %s/%s stderr: %s", clusterContext, pod.Namespace, pod.Name, se)
	}
	return nil
}

// execCommandInPod runs iptables fence/unfence only by exec into the DaemonSet pod (requires RESTConfig).
func (p *IptablesFaultProvider) execCommandInPod(ctx context.Context, pod *corev1.Pod, command string) error {
	if p.config.RESTConfig == nil {
		return fmt.Errorf("iptables fault injection requires FaultInjectionConfig.RESTConfig for exec into DaemonSet pods")
	}
	clusterContext := p.getClusterContext()
	// Trailing newline before "&&" breaks POSIX sh (dash): "echo ...\n&& ..." is a syntax error.
	script := strings.TrimRight(command, "\n\t\r ") + " && echo 'Command completed successfully'"

	Logf("[INFO]", "[%s] iptables: exec into DaemonSet pod %s/%s on node %s", clusterContext, pod.Namespace, pod.Name, pod.Spec.NodeName)
	if err := p.waitIptablesContainerReady(ctx, pod); err != nil {
		return fmt.Errorf("wait for iptables container ready %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return p.streamExec(ctx, pod, script)
}

// emergencyCleanup performs cleanup of any leftover fence rules from previous test runs
func (p *IptablesFaultProvider) emergencyCleanup(ctx context.Context) error {
	Logf("[DEBUG]", "Performing emergency cleanup of leftover fence rules...")

	for _, ns := range p.daemonSetSearchNamespaces() {
		var ds appsv1.DaemonSet
		err := p.config.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: IptablesDaemonSetName}, &ds)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			Logf("[DEBUG]", "Get DaemonSet %s/%s: %v", ns, IptablesDaemonSetName, err)
			continue
		}
		Logf("[DEBUG]", "Found existing DaemonSet %s/%s, cleaning up fence rules", ds.Namespace, ds.Name)
		if err := p.cleanupAllFenceRules(ctx, &ds); err != nil {
			Logf("[WARNING]", "Failed to cleanup rules for DaemonSet %s: %v", ds.Name, err)
		}
	}

	Logf("[DEBUG]", "Emergency cleanup completed")
	return nil
}

// cleanupAllFenceRules removes CSI-Addons staged OUTPUT rules (--reject-with icmp-host-unreachable) from manager pods.
// It does not remove unrelated host REJECT rules. Use the emergency shell script with
// CSI_ADDONS_IPTABLES_LEGACY_FULL_REJECT=1 to strip every OUTPUT REJECT rule.
func (p *IptablesFaultProvider) cleanupAllFenceRules(ctx context.Context, ds *appsv1.DaemonSet) error {
	podList := &corev1.PodList{}
	labelSelector := client.MatchingLabels{"app": "csi-addons-iptables-manager"}
	if err := p.config.Client.List(ctx, podList, client.InNamespace(ds.Namespace), labelSelector); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		cleanupCmd := csiAddonsStagedRejectCleanupShell

		if err := p.createCleanupJob(ctx, &pod, cleanupCmd); err != nil {
			Logf("[WARNING]", "Failed to cleanup pod %s: %v", pod.Name, err)
		}
	}

	return nil
}

// createCleanupJob runs cleanup by remote exec into the DaemonSet pod (same transport as fence/unfence).
// Despite the name, this does not create a batch/v1 Job.
func (p *IptablesFaultProvider) createCleanupJob(ctx context.Context, targetPod *corev1.Pod, command string) error {
	if p.config.RESTConfig == nil {
		Logf("[WARNING]", "iptables emergency cleanup skipped for pod %s/%s: RESTConfig is nil", targetPod.Namespace, targetPod.Name)
		return nil
	}
	script := strings.TrimRight(command, "\n\t\r ")
	if err := p.waitIptablesContainerReady(ctx, targetPod); err != nil {
		return fmt.Errorf("wait for iptables pod %s/%s before cleanup exec: %w", targetPod.Namespace, targetPod.Name, err)
	}
	Logf("[DEBUG]", "iptables cleanup: exec into %s/%s", targetPod.Namespace, targetPod.Name)
	return p.streamExec(ctx, targetPod, script)
}
