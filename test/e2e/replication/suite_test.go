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

// See docs/testing/ginkgo-flow-guide.md for detailed Ginkgo v2 flow diagrams and documentation.

package replication

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

// Cluster names for full-DR mode (when DR1_CONTEXT and DR2_CONTEXT are both set).
const (
	ClusterDR1 = "dr1"
	ClusterDR2 = "dr2"
)

var (
	cfg                            *rest.Config
	k8sClient                      client.Client
	k8sClientDR1                   client.Client
	k8sClientDR2                   client.Client
	useExistingCluster             bool
	dr1Context                     string
	dr2Context                     string
	networkFenceSupportCached      *bool  // cached result of NetworkFence capability detection (nil = not yet checked)
	networkFenceSupportProvisioner string // provisioner used for cached check
	sigChan                        chan os.Signal
	cleanupNamespaces              []string // track test namespaces for forced-termination cleanup
)

func init() {
	// Setup signal handler for graceful cleanup on forced termination (SIGTERM, SIGINT)
	// This catches termination signals before process is killed
	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigChan
		Logf("[SIGNAL]", "received %s - initiating cleanup of %d namespaces", sig, len(cleanupNamespaces))
		performEmergencyCleanup()
		os.Exit(130) // Standard exit code for SIGINT
	}()
}

func TestReplicationE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Replication E2E Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	Logf("[SETUP]", "checking USE_EXISTING_CLUSTER")
	useExistingCluster = os.Getenv("USE_EXISTING_CLUSTER") == "true"
	if !useExistingCluster {
		Skip("Replication E2E suite requires USE_EXISTING_CLUSTER=true. Use make test-replication-e2e or hack/run-replication-e2e.sh")
	}

	Logf("[SETUP]", "loading kubeconfig (KUBECONFIG=%s)", os.Getenv("KUBECONFIG"))
	Logf("[SETUP]", "registering replication and csiaddons APIs in scheme")
	err := replicationv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	err = csiaddonsv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	dr1Context = os.Getenv("DR1_CONTEXT")
	dr2Context = os.Getenv("DR2_CONTEXT")
	fullDR := dr1Context != "" && dr2Context != ""

	if fullDR {
		Logf("[SETUP]", "full-DR mode DR1_CONTEXT=%q DR2_CONTEXT=%q", dr1Context, dr2Context)
		cfg, err = restConfigForContext(dr1Context)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		cfgDR2, err := restConfigForContext(dr2Context)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgDR2).NotTo(BeNil())
		Logf("[SETUP]", "creating Kubernetes client for DR1")
		k8sClientDR1, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		Logf("[SETUP]", "creating Kubernetes client for DR2")
		k8sClientDR2, err = client.New(cfgDR2, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		k8sClient = k8sClientDR1
	} else {
		cfg, err = restConfig()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Logf("[SETUP]", "creating Kubernetes client (single cluster)")
		k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient).NotTo(BeNil())
	}

	Logf("[SETUP]", "ready")

	// Detect NetworkFence support once at suite level for efficiency
	// This is done after client creation so we can query the cluster
	Logf("[SETUP]", "detecting NetworkFence support")
	testEnv := GetTestEnv()
	isSupported := HasNetworkFenceSupport(context.Background(), k8sClient, testEnv.Provisioner)
	networkFenceSupportCached = &isSupported
	networkFenceSupportProvisioner = testEnv.Provisioner
	Logf("[SETUP]", "NetworkFence support = %v (provisioner=%q)", isSupported, testEnv.Provisioner)
})

// ReportAfterEach logs each spec's completion for progress visibility in logs.
var _ = ReportAfterEach(func(report types.SpecReport) {
	stateStr := report.State.String()
	dur := report.RunTime.Round(time.Millisecond)
	Logf("[SPEC]", "%s -> %s (%s)", report.FullText(), stateStr, dur)
})

// ReportAfterSuite writes a detailed summary of the run to GinkgoWriter (and thus to the log file).
var _ = ReportAfterSuite("Replication E2E detailed summary", func(report types.Report) {
	const sep = "================================================================================"
	_, _ = fmt.Fprintf(GinkgoWriter, "\n%s\n", sep)
	_, _ = fmt.Fprintf(GinkgoWriter, "REPLICATION E2E SUITE — DETAILED SUMMARY\n")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", sep)
	_, _ = fmt.Fprintf(GinkgoWriter, "Suite: %s\n", report.SuiteDescription)
	_, _ = fmt.Fprintf(GinkgoWriter, "Path:  %s\n", report.SuitePath)
	_, _ = fmt.Fprintf(GinkgoWriter, "Result: %s\n", resultString(report.SuiteSucceeded))
	_, _ = fmt.Fprintf(GinkgoWriter, "Started:  %s\n", report.StartTime.Format(time.RFC3339))
	_, _ = fmt.Fprintf(GinkgoWriter, "Finished: %s\n", report.EndTime.Format(time.RFC3339))
	_, _ = fmt.Fprintf(GinkgoWriter, "Duration: %s\n", report.RunTime.Round(time.Millisecond))
	_, _ = fmt.Fprintf(GinkgoWriter, "Pre-run: total specs=%d, specs to run=%d\n", report.PreRunStats.TotalSpecs, report.PreRunStats.SpecsThatWillRun)
	if len(report.SpecialSuiteFailureReasons) > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "Special failure reasons:\n")
		for _, r := range report.SpecialSuiteFailureReasons {
			_, _ = fmt.Fprintf(GinkgoWriter, "  - %s\n", r)
		}
	}
	// Collect specs by state, filter out lifecycle hooks and internal specs
	var passedSpecs, failedSpecs, skippedSpecs []*types.SpecReport
	for i, spec := range report.SpecReports {
		fullText := strings.TrimSpace(spec.FullText())
		// Skip:
		// - specs with empty FullText (internal setup/capability checks)
		// - BeforeSuite/AfterSuite/BeforeEach/AfterEach (lifecycle hooks)
		if fullText == "" ||
			strings.Contains(fullText, "[BeforeSuite]") ||
			strings.Contains(fullText, "[AfterSuite]") ||
			strings.Contains(fullText, "[BeforeEach]") ||
			strings.Contains(fullText, "[AfterEach]") ||
			strings.HasPrefix(fullText, "TOP-LEVEL") {
			continue
		}
		specPtr := &report.SpecReports[i]
		switch spec.State {
		case types.SpecStatePassed:
			passedSpecs = append(passedSpecs, specPtr)
		case types.SpecStateSkipped, types.SpecStatePending:
			skippedSpecs = append(skippedSpecs, specPtr)
		default:
			// All failure states
			if spec.Failed() {
				failedSpecs = append(failedSpecs, specPtr)
			}
		}
	}

	// Print failed tests first
	if len(failedSpecs) > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- FAILED (%d) ---\n", len(failedSpecs))
		for i, spec := range failedSpecs {
			dur := spec.RunTime.Round(time.Millisecond)
			_, _ = fmt.Fprintf(GinkgoWriter, "%d. [%s] %s (duration: %s)\n", i+1, spec.State.String(), spec.FullText(), dur)
			if spec.Failure.Message != "" {
				msg := strings.TrimSpace(spec.Failure.Message)
				if len(msg) > 500 {
					msg = msg[:500] + "..."
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "   Failure: %s\n", strings.ReplaceAll(msg, "\n", "\n   "))
			}
		}
	}

	// Print passed tests
	if len(passedSpecs) > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- PASSED (%d) ---\n", len(passedSpecs))
		for i, spec := range passedSpecs {
			dur := spec.RunTime.Round(time.Millisecond)
			_, _ = fmt.Fprintf(GinkgoWriter, "%d. %s (duration: %s)\n", i+1, spec.FullText(), dur)
		}
	}

	// Print skipped/pending tests
	if len(skippedSpecs) > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- SKIPPED/PENDING (%d) ---\n", len(skippedSpecs))
		for i, spec := range skippedSpecs {
			_, _ = fmt.Fprintf(GinkgoWriter, "%d. [%s] %s\n", i+1, spec.State.String(), spec.FullText())
		}
	}

	// Count pending separately from skipped
	var pendingCount int
	for _, spec := range skippedSpecs {
		if spec.State == types.SpecStatePending {
			pendingCount++
		}
	}
	skippedCount := len(skippedSpecs) - pendingCount
	passed := len(passedSpecs)
	failed := len(failedSpecs)
	skipped := skippedCount
	pending := pendingCount
	_, _ = fmt.Fprintf(GinkgoWriter, "\n--- Summary ---\n")
	_, _ = fmt.Fprintf(GinkgoWriter, "Passed: %d | Failed: %d | Skipped: %d | Pending: %d | Total: %d\n", passed, failed, skipped, pending, passed+failed+skipped+pending)
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", sep)
})

func resultString(succeeded bool) string {
	if succeeded {
		return "SUCCESS"
	}
	return "FAILED"
}

// restConfig builds a rest.Config using the same rules as kubectl: KUBECONFIG
// env var if set, otherwise the default kubeconfig path (~/.kube/config).
// This works with minikube profiles and any context selected via KUBECONFIG
// or the default config.
func restConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
}

// restConfigForContext returns rest.Config for the given context name (same kubeconfig, different context).
func restConfigForContext(contextName string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
}

// GetK8sClient returns the primary Kubernetes client (DR1 in full-DR mode, else the default cluster).
func GetK8sClient() client.Client {
	return k8sClient
}

// GetK8sClientForCluster returns the client for the given cluster name when running in full-DR mode
// (DR1_CONTEXT and DR2_CONTEXT both set). Cluster name must be ClusterDR1 or ClusterDR2.
// If not in full-DR mode, returns the single shared client for any cluster name.
func GetK8sClientForCluster(clusterName string) client.Client {
	if k8sClientDR1 != nil && k8sClientDR2 != nil {
		switch clusterName {
		case ClusterDR1:
			return k8sClientDR1
		case ClusterDR2:
			return k8sClientDR2
		}
	}
	return k8sClient
}

// IsFullDRMode returns true when DR1_CONTEXT and DR2_CONTEXT are both set.
func IsFullDRMode() bool {
	return dr1Context != "" && dr2Context != ""
}

// DR1Context returns the DR1 context name (empty if not set).
func DR1Context() string { return dr1Context }

// performEmergencyCleanup attempts to clean up test namespaces when test execution is forcefully terminated.
// Called on SIGTERM/SIGINT signals. Note: this runs in a goroutine and may not complete if process is killed.
func performEmergencyCleanup() {
	if k8sClient == nil {
		Logf("[CLEANUP]", "k8s client not initialized, skipping emergency cleanup")
		return
	}

	if len(cleanupNamespaces) == 0 {
		Logf("[CLEANUP]", "no test namespaces to cleanup")
		return
	}

	// Use timeout context for cleanup (10 seconds per namespace)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(cleanupNamespaces)*10)*time.Second)
	defer cancel()

	Logf("[CLEANUP]", "emergency cleanup started for %d namespaces", len(cleanupNamespaces))
	for _, ns := range cleanupNamespaces {
		nsObj := &corev1.Namespace{}
		nsKey := client.ObjectKey{Name: ns}

		// Check if namespace exists
		if err := k8sClient.Get(ctx, nsKey, nsObj); err != nil {
			Logf("[CLEANUP]", "namespace %q not found or error: %v", ns, err)
			continue
		}

		// Delete namespace (cascading delete removes all resources)
		Logf("[CLEANUP]", "deleting test namespace %q (cascade)", ns)
		if err := k8sClient.Delete(ctx, nsObj); err != nil {
			Logf("[CLEANUP]", "error deleting namespace %q: %v (continuing)", ns, err)
			continue
		}

		// Don't wait for deletion - just trigger it
		Logf("[CLEANUP]", "namespace %q deletion triggered", ns)
	}

	Logf("[CLEANUP]", "emergency cleanup triggered, exiting")
}

// RegisterTestNamespace tracks a namespace for emergency cleanup on forced termination.
// Called automatically by CreateNamespaceWithCleanup() wrapper.
func RegisterTestNamespace(namespace string) {
	cleanupNamespaces = append(cleanupNamespaces, namespace)
	Logf("[CLEANUP]", "registered namespace %q for emergency cleanup (total: %d)", namespace, len(cleanupNamespaces))
}

// CreateNamespaceWithCleanup creates a namespace using the helper and automatically registers it for emergency cleanup.
// This wrapper ensures all test namespaces are tracked for forced-termination cleanup.
func CreateNamespaceWithCleanup(ctx context.Context, c client.Client, name string) *corev1.Namespace {
	ns := CreateNamespace(ctx, c, name)
	RegisterTestNamespace(ns.Name)
	return ns
}

// DR2Context returns the DR2 context name (empty if not set).
func DR2Context() string { return dr2Context }

// IsNetworkFenceSupportAvailable returns the cached result of NetworkFence capability detection.
// The detection is performed once at BeforeSuite initialization and reused for all tests.
// Returns true if NetworkFence and NetworkFenceClass CRDs are installed and the CSI driver
// advertises network_fence.NETWORK_FENCE capability. Used by tests to skip NetworkFence-dependent
// scenarios gracefully.
//
// Note: The cached result is specific to the provisioner detected at BeforeSuite time.
// If the provisioner changes during test execution, the cached result may not be accurate.
// For multi-provisioner test suites, call HasNetworkFenceSupport directly for each provisioner.
func IsNetworkFenceSupportAvailable() bool {
	if networkFenceSupportCached == nil {
		// Should not happen if BeforeSuite ran correctly, but return false as safe default
		return false
	}
	return *networkFenceSupportCached
}
