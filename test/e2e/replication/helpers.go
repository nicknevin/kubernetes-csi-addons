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

package replication

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega" //nolint:ST1001 // dot-import is standard for Gomega matchers in Ginkgo tests
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

const (
	replicationSecretName      = "replication-secret"
	replicationParameterPrefix = "replication.storage.openshift.io/"
	secretNameKey              = replicationParameterPrefix + "replication-secret-name"
	secretNamespaceKey         = replicationParameterPrefix + "replication-secret-namespace"
	pvcDataSourceKind          = "PersistentVolumeClaim"
	defaultStorageClass        = "rook-ceph-block"
	defaultProvisioner         = "rook-ceph.rbd.csi.ceph.com"
	pollInterval               = 2 * time.Second
	defaultReplicationPollSec  = 300
	pvcBindTimeout             = 120 * time.Second
	cleanupWaitTimeout         = 45 * time.Second
	quickErrorTimeout          = 30 * time.Second // for WaitForVolumeReplicationErrorQuick (validation/parameter errors)

	// mirrorImageReadyDelay is the time to wait for rbd-mirror to create the mirror image on the
	// secondary cluster after primary replication is enabled. Used by CreateSecondaryPVCFromPrimary.
	mirrorImageReadyDelay = 15 * time.Second

	// Finalizer names must match internal/controller/replication.storage/finalizers.go
	volumeReplicationFinalizer = "replication.storage.openshift.io"
	pvcReplicationFinalizer    = "replication.storage.openshift.io/pvc-protection"

	// NetworkFenceClass parameter keys (must match internal/controller/csiaddons/networkfenceclass_controller.go)
	networkFenceParamPrefix   = "csiaddons.openshift.io/"
	networkFenceSecretNameKey = networkFenceParamPrefix + "networkfence-secret-name"
	networkFenceSecretNsKey   = networkFenceParamPrefix + "networkfence-secret-namespace"
	networkFencePollTimeout   = 120 * time.Second // for WaitForNetworkFenceResult
	fenceCIDRProbeTimeout     = 30 * time.Second  // wait for CSIAddonsNode CIDRs before skipping L1-E-003
	// FENCE_TARGET_SERVICES lists "namespace/service" whose backends resolve for single-cluster iptables discovery
	// (comma-separated). Example: "rook-ceph/rook-ceph-active-mons"
	fenceTargetServicesEnv = "FENCE_TARGET_SERVICES"
	// FENCE_PEER_SERVICES lists "namespace/service" resolved on the **peer** cluster in full-DR iptables tests
	// (same format as FENCE_TARGET_SERVICES). If unset, FENCE_TARGET_SERVICES is reused for peer lookup keys.
	fencePeerServicesEnv = "FENCE_PEER_SERVICES"
	// FENCE_AUTO_ENDPOINT_NAMESPACES limits auto-discovery of Endpoints (comma-separated). Default: rook-ceph,csi-addons-system
	fenceAutoEndpointNamespacesEnv = "FENCE_AUTO_ENDPOINT_NAMESPACES"
	fenceAutoDiscoveryMaxCIDRs     = 32

	// NetworkFence/NetworkFenceClass finalizers (must match internal/controller/csiaddons/networkfence*.go)
	networkFenceFinalizer      = "csiaddons.openshift.io/network-fence"
	networkFenceClassFinalizer = "csiaddons.openshift.io/csiaddonsnode"
)

// MirroringMode is the replication mirroring mode (snapshot or journal).
type MirroringMode string

const (
	MirroringModeSnapshot MirroringMode = "snapshot"
	MirroringModeJournal  MirroringMode = "journal"
)

// TestEnv holds configuration for the e2e replication tests.
type TestEnv struct {
	StorageClass          string
	Provisioner           string
	ReplicationSecretName string // if set with ReplicationSecretNamespace, use existing secret
	ReplicationSecretNs   string
	// Full DR: when DR1_CONTEXT and DR2_CONTEXT are both set, tests can create resources on both clusters.
	DR1Context string
	DR2Context string
	FullDR     bool
}

// getReplicationPollTimeout returns the timeout for waiting on VolumeReplication conditions.
// REPLICATION_POLL_TIMEOUT (seconds) overrides the default (300s). Used for Replicating=True and similar.
func getReplicationPollTimeout() time.Duration {
	s := os.Getenv("REPLICATION_POLL_TIMEOUT")
	if s == "" {
		return defaultReplicationPollSec * time.Second
	}
	sec, err := strconv.Atoi(s)
	if err != nil || sec <= 0 {
		return defaultReplicationPollSec * time.Second
	}
	return time.Duration(sec) * time.Second
}

// GetTestEnv returns TestEnv from environment or defaults.
func GetTestEnv() TestEnv {
	sc := os.Getenv("STORAGE_CLASS")
	if sc == "" {
		sc = defaultStorageClass
	}
	provisioner := os.Getenv("CSI_PROVISIONER")
	if provisioner == "" {
		provisioner = defaultProvisioner
	}
	dr1 := os.Getenv("DR1_CONTEXT")
	dr2 := os.Getenv("DR2_CONTEXT")
	return TestEnv{
		StorageClass:          sc,
		Provisioner:           provisioner,
		ReplicationSecretName: os.Getenv("REPLICATION_SECRET_NAME"),
		ReplicationSecretNs:   os.Getenv("REPLICATION_SECRET_NAMESPACE"),
		DR1Context:            dr1,
		DR2Context:            dr2,
		FullDR:                dr1 != "" && dr2 != "",
	}
}

// Logf is a unified logging wrapper that prefixes all log lines with ISO 8601 timestamp.
// Format: YYYY-MM-DD HH:MM:SS.mmm [prefix] message
// Usage examples:
//
//	Logf("[CLEANUP]", "Starting deletion: %s/%s", ns, name)
//	Logf("[DEBUG]", "State: %s", state)
//	Logf("[INFO]", "Operation completed")
func Logf(prefix, format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "%s %s %s\n", ts, prefix, msg)
}

// UniqueNamespace returns a unique namespace name for e2e tests.
func UniqueNamespace() string {
	return fmt.Sprintf("e2e-replication-%s", uuid.NewUUID()[:8])
}

// SkipIfNotFullDR skips the current spec when DR1_CONTEXT and DR2_CONTEXT are not both set.
// It logs the skip reason to GinkgoWriter so it appears in the test output.
// Use for tests that require two clusters (e.g. L1-DIS-002, Full DR specs).
func SkipIfNotFullDR(testID, description string) {
	env := GetTestEnv()
	if !env.FullDR {
		Logf(fmt.Sprintf("[%s]", testID), "Skipping: %s (DR1_CONTEXT and DR2_CONTEXT must both be set)", description)
		ginkgo.Skip(fmt.Sprintf("%s requires DR1_CONTEXT and DR2_CONTEXT to be set", testID))
	}
}

// CreateNamespace creates a namespace with the given name.
func CreateNamespace(ctx context.Context, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := c.Create(ctx, ns)
	Expect(err).NotTo(HaveOccurred())
	return ns
}

// CreateSecret creates a secret for replication. For Ceph RBD the driver expects
// "userID" and "userKey" in the secret data; we include them so the driver does not
// return "missing ID field 'userID' in secrets". For real Ceph clusters use an
// existing secret via REPLICATION_SECRET_NAME and REPLICATION_SECRET_NAMESPACE instead.
func CreateSecret(ctx context.Context, c client.Client, namespace, name string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"userID":  []byte("admin"),
			"userKey": []byte("dummy"),
		},
	}
	err := c.Create(ctx, secret)
	Expect(err).NotTo(HaveOccurred())
	return secret
}

// ReplicationSecretRef returns (secretName, secretNamespace) for use in VolumeReplicationClass.
// If REPLICATION_SECRET_NAME and REPLICATION_SECRET_NAMESPACE are set, those are returned and no secret is created.
// Otherwise a secret with userID/userKey is created in the given namespace and (replicationSecretName, namespace) is returned.
func ReplicationSecretRef(ctx context.Context, c client.Client, env TestEnv, namespace string) (name, ns string) {
	if env.ReplicationSecretName != "" && env.ReplicationSecretNs != "" {
		return env.ReplicationSecretName, env.ReplicationSecretNs
	}
	CreateSecret(ctx, c, namespace, replicationSecretName)
	return replicationSecretName, namespace
}

// CreatePVC creates a PVC in the given namespace and waits for it to be Bound.
// If onPoll is non-nil, it is called after each poll with the current PVC so tests can log progress (e.g. phase=Pending).
func CreatePVC(ctx context.Context, c client.Client, namespace, name, storageClass string, size string, onPoll func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
	if size == "" {
		size = "1Gi"
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: mustParseQuantity(size),
				},
			},
		},
	}
	err := c.Create(ctx, pvc)
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pvc)
		if err != nil {
			return false
		}
		if onPoll != nil {
			onPoll(pvc)
		}
		return pvc.Status.Phase == corev1.ClaimBound
	}, pvcBindTimeout, pollInterval).Should(BeTrue(), "PVC %s/%s should become Bound", namespace, name)
	err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pvc)
	Expect(err).NotTo(HaveOccurred())
	return pvc
}

// CreateSecondaryPVCFromPrimary restores the secondary PVC from the primary for RBD mirroring.
// It backs up the PV and PVC from the primary cluster, strips claimRef from the PV, and applies
// both on the secondary cluster so the PVC binds to the mirror image created by rbd-mirror.
// Call this after the primary VR has reached Replicating. Waits mirrorImageReadyDelay for the
// rbd-mirror daemon to create the mirror image on the secondary cluster before creating PV/PVC.
// Returns (pvcDR2, pvDR2). The caller must delete the PV on cleanup (after the PVC).
func CreateSecondaryPVCFromPrimary(ctx context.Context, cPrimary, cSecondary client.Client, pvcPrimary *corev1.PersistentVolumeClaim, namespace, secondaryPVCName string, onPoll func(*corev1.PersistentVolumeClaim)) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	// Wait for rbd-mirror to create the mirror image on the secondary cluster
	time.Sleep(mirrorImageReadyDelay)

	// Refresh primary PVC to ensure we have VolumeName
	err := cPrimary.Get(ctx, client.ObjectKey{Namespace: pvcPrimary.Namespace, Name: pvcPrimary.Name}, pvcPrimary)
	Expect(err).NotTo(HaveOccurred())
	Expect(pvcPrimary.Spec.VolumeName).NotTo(BeEmpty(), "primary PVC must be bound (have volumeName)")

	// Get the PV from the primary cluster
	pvPrimary := &corev1.PersistentVolume{}
	err = cPrimary.Get(ctx, client.ObjectKey{Name: pvcPrimary.Spec.VolumeName}, pvPrimary)
	Expect(err).NotTo(HaveOccurred())
	fmt.Printf("[CreateSecondaryPVCFromPrimary] Primary PV CSI.VolumeHandle=%s\n", pvPrimary.Spec.CSI.VolumeHandle)

	// Create PV for secondary: copy spec, remove claimRef, new name
	pvSecondaryName := "pv-" + secondaryPVCName + "-" + string(uuid.NewUUID())[:8]
	pvSecondary := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvSecondaryName,
		},
		Spec: *pvPrimary.Spec.DeepCopy(),
	}
	pvSecondary.Spec.ClaimRef = nil
	fmt.Printf("[CreateSecondaryPVCFromPrimary] Secondary PV CSI.VolumeHandle=%s (should point to mirrored image)\n", pvSecondary.Spec.CSI.VolumeHandle)

	err = cSecondary.Create(ctx, pvSecondary)
	Expect(err).NotTo(HaveOccurred())

	// Create PVC for secondary: same spec as primary, volumeName to bind to our PV
	storageClass := pvcPrimary.Spec.StorageClassName
	if storageClass != nil && *storageClass == "" {
		storageClass = nil
	}
	pvcSecondary := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secondaryPVCName,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pvcPrimary.Spec.AccessModes,
			Resources:        pvcPrimary.Spec.Resources,
			StorageClassName: storageClass,
			VolumeName:       pvSecondaryName,
		},
	}
	err = cSecondary.Create(ctx, pvcSecondary)
	Expect(err).NotTo(HaveOccurred())

	Eventually(func() bool {
		err := cSecondary.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secondaryPVCName}, pvcSecondary)
		if err != nil {
			return false
		}
		if onPoll != nil {
			onPoll(pvcSecondary)
		}
		return pvcSecondary.Status.Phase == corev1.ClaimBound
	}, pvcBindTimeout, pollInterval).Should(BeTrue(), "secondary PVC %s/%s should become Bound", namespace, secondaryPVCName)
	err = cSecondary.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secondaryPVCName}, pvcSecondary)
	Expect(err).NotTo(HaveOccurred())
	return pvcSecondary, pvSecondary
}

// DeletePV deletes a PV and ignores NotFound.
func DeletePV(ctx context.Context, c client.Client, pv *corev1.PersistentVolume) {
	if pv == nil {
		return
	}
	err := c.Delete(ctx, pv)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// FormatPVCStatus returns a one-line status for logging (e.g. phase=Pending).
func FormatPVCStatus(pvc *corev1.PersistentVolumeClaim) string {
	phase := string(pvc.Status.Phase)
	if phase == "" {
		phase = "<none>"
	}
	return fmt.Sprintf("%s/%s phase=%s", pvc.Namespace, pvc.Name, phase)
}

func mustParseQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// CreateVolumeReplicationClass creates a VolumeReplicationClass with the given mirroring mode and provisioner.
func CreateVolumeReplicationClass(ctx context.Context, c client.Client, name, provisioner, secretName, secretNamespace string, mode MirroringMode) *replicationv1alpha1.VolumeReplicationClass {
	return CreateVolumeReplicationClassWithParams(ctx, c, name, provisioner, secretName, secretNamespace, mode, nil)
}

// CreateVolumeReplicationClassWithParams creates a VolumeReplicationClass with the given mirroring mode and provisioner.
// paramOverrides, if non-nil, are merged into the base parameters (overrides take precedence).
// Use for negative tests (e.g. invalid schedulingInterval, mirroringMode) or optional params (e.g. schedulingStartTime).
func CreateVolumeReplicationClassWithParams(ctx context.Context, c client.Client, name, provisioner, secretName, secretNamespace string, mode MirroringMode, paramOverrides map[string]string) *replicationv1alpha1.VolumeReplicationClass {
	params := map[string]string{
		secretNameKey:      secretName,
		secretNamespaceKey: secretNamespace,
	}
	switch mode {
	case MirroringModeSnapshot:
		params["mirroringMode"] = "snapshot"
		params["schedulingInterval"] = "1m"
	case MirroringModeJournal:
		params["mirroringMode"] = "journal"
	default:
		params["mirroringMode"] = "snapshot"
		params["schedulingInterval"] = "1m"
	}
	for k, v := range paramOverrides {
		params[k] = v
	}
	vrc := &replicationv1alpha1.VolumeReplicationClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: replicationv1alpha1.VolumeReplicationClassSpec{
			Provisioner: provisioner,
			Parameters:  params,
		},
	}
	err := c.Create(ctx, vrc)
	Expect(err).NotTo(HaveOccurred())
	return vrc
}

// CreateVolumeReplication creates a VolumeReplication for the given PVC.
func CreateVolumeReplication(ctx context.Context, c client.Client, namespace, name, vrcName, pvcName string, state replicationv1alpha1.ReplicationState) *replicationv1alpha1.VolumeReplication {
	vr := &replicationv1alpha1.VolumeReplication{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: replicationv1alpha1.VolumeReplicationSpec{
			VolumeReplicationClass: vrcName,
			ReplicationState:       state,
			DataSource: corev1.TypedLocalObjectReference{
				APIGroup: nil,
				Kind:     pvcDataSourceKind,
				Name:     pvcName,
			},
		},
	}
	err := c.Create(ctx, vr)
	Expect(err).NotTo(HaveOccurred())
	return vr
}

// FormatVRStatus returns a one-line status summary for logging (no newline).
func FormatVRStatus(vr *replicationv1alpha1.VolumeReplication) string {
	state := string(vr.Status.State)
	if state == "" {
		state = "<none>"
	}
	condStr := ""
	for _, c := range vr.Status.Conditions {
		if condStr != "" {
			condStr += " "
		}
		condStr += c.Type + "=" + string(c.Status)
	}
	if condStr == "" {
		condStr = "<none>"
	}
	return fmt.Sprintf("%s/%s state=%s conditions=[%s] message=%q", vr.Namespace, vr.Name, state, condStr, vr.Status.Message)
}

// WaitForVolumeReplicationCondition waits until the VR has a condition matching the given type and status, or times out.
// If onPoll is non-nil, it is called after each poll with the current VR so tests can log progress.
func WaitForVolumeReplicationCondition(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication, conditionType string, status metav1.ConditionStatus, onPoll func(*replicationv1alpha1.VolumeReplication)) {
	key := client.ObjectKeyFromObject(vr)
	timeout := getReplicationPollTimeout()
	Eventually(func() bool {
		err := c.Get(ctx, key, vr)
		if err != nil {
			return false
		}
		if onPoll != nil {
			onPoll(vr)
		}
		for _, cond := range vr.Status.Conditions {
			if cond.Type == conditionType && cond.Status == status {
				return true
			}
		}
		return false
	}, timeout, pollInterval).Should(BeTrue(),
		"VolumeReplication %s/%s should get condition %s=%s (state=%s)", vr.Namespace, vr.Name, conditionType, status, vr.Status.State)
}

// hasReplicationSuccessCondition returns true when the VR indicates replication is enabled successfully:
// either Replicating=True or Completed=True (some controllers set Completed when replication is enabled).
func hasReplicationSuccessCondition(vr *replicationv1alpha1.VolumeReplication) bool {
	for _, cond := range vr.Status.Conditions {
		if cond.Status != metav1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case replicationv1alpha1.Replicating, replicationv1alpha1.ConditionCompleted:
			return true
		}
	}
	return false
}

// WaitForVolumeReplicationReplicatingOrCompleted waits until the VR has Replicating=True or Completed=True, or times out.
// Use this for "replication enabled" success: some controllers set Replicating, others set Completed when replication is on.
func WaitForVolumeReplicationReplicatingOrCompleted(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication, onPoll func(*replicationv1alpha1.VolumeReplication)) {
	key := client.ObjectKeyFromObject(vr)
	timeout := getReplicationPollTimeout()
	Eventually(func() bool {
		err := c.Get(ctx, key, vr)
		if err != nil {
			return false
		}
		if onPoll != nil {
			onPoll(vr)
		}
		return hasReplicationSuccessCondition(vr)
	}, timeout, pollInterval).Should(BeTrue(),
		"VolumeReplication %s/%s should get Replicating=True or Completed=True (state=%s)", vr.Namespace, vr.Name, vr.Status.State)
}

// hasVolumeReplicationErrorCondition returns true if the VR has an error (message set or a failure condition).
//
// Controller/driver behavior (see docs/testing/replication-e2e-suite.md): The csi-addons controller sets
// error/degraded state with Status==ConditionTrue (e.g. ConditionDegraded with Reason Error in setFailedPromotionCondition,
// setFailedDemotionCondition, etc.). It does not use ConditionFalse to signal "error"; ConditionFalse on
// ConditionCompleted with a failure Reason is set alongside ConditionDegraded=True. So we must check for
// ConditionDegraded with Status True, and ConditionCompleted False with failure Reasons, to detect real failures.
//
// This definition is important for L1-E-005 (idempotent second VR): when the controller does not set status on
// a duplicate VR (idempotent no-op), the test asserts "no error" via hasVolumeReplicationErrorCondition(vr2).
// We must not false-positive: a VR with no conditions or only initial state must not be treated as error.
// We must detect actual controller-set failures so tests that expect error (e.g. L1-INFO-008) can pass.
func hasVolumeReplicationErrorCondition(vr *replicationv1alpha1.VolumeReplication) bool {
	if vr.Status.Message != "" {
		return true
	}
	for _, cond := range vr.Status.Conditions {
		// Degraded with Status True indicates an error state (setFailedPromotionCondition, setFailedDemotionCondition, etc.).
		if cond.Status == metav1.ConditionTrue && cond.Type == replicationv1alpha1.ConditionDegraded {
			return true
		}
		// Completed False with a failure reason indicates promote/demote/resync failure.
		if cond.Status == metav1.ConditionFalse && cond.Type == replicationv1alpha1.ConditionCompleted &&
			(cond.Reason == replicationv1alpha1.FailedToPromote || cond.Reason == replicationv1alpha1.FailedToDemote || cond.Reason == replicationv1alpha1.FailedToResync) {
			return true
		}
	}
	return false
}

// WaitForVolumeReplicationReplicatingOrCompletedUntil waits up to timeout for Replicating=True or Completed=True.
// Returns true if success was seen, false on timeout. Use for idempotent second VR: if false, assert no error.
func WaitForVolumeReplicationReplicatingOrCompletedUntil(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication, timeout time.Duration, onPoll func(*replicationv1alpha1.VolumeReplication)) bool {
	key := client.ObjectKeyFromObject(vr)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, vr)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		if onPoll != nil {
			onPoll(vr)
		}
		if hasReplicationSuccessCondition(vr) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// WaitForVolumeReplicationErrorWithTimeout waits until the VR has any error condition or message indicating failure.
// Uses hasVolumeReplicationErrorCondition, which is aligned with csi-addons controller behavior (error/degraded
// set with ConditionTrue; see docs/testing/replication-e2e-suite.md and the comment on hasVolumeReplicationErrorCondition).
// timeout can be getReplicationPollTimeout() for default, or quickErrorTimeout for validation/parameter errors.
func WaitForVolumeReplicationErrorWithTimeout(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication, timeout time.Duration) {
	key := client.ObjectKeyFromObject(vr)
	Eventually(func() bool {
		err := c.Get(ctx, key, vr)
		if err != nil {
			return false
		}
		return hasVolumeReplicationErrorCondition(vr)
	}, timeout, pollInterval).Should(BeTrue(),
		"VolumeReplication %s/%s should report an error", vr.Namespace, vr.Name)
}

// WaitForVolumeReplicationError waits until the VR has any error condition or message indicating failure.
// Uses the default poll timeout from REPLICATION_POLL_TIMEOUT (or 300s). For validation/parameter errors
// that manifest quickly, use WaitForVolumeReplicationErrorWithTimeout(ctx, c, vr, quickErrorTimeout) instead.
func WaitForVolumeReplicationError(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication) {
	WaitForVolumeReplicationErrorWithTimeout(ctx, c, vr, getReplicationPollTimeout())
}

// HasVolumeReplicationErrorCondition is an exported wrapper for hasVolumeReplicationErrorCondition.
// Used by test code to check if a VR has an error condition (aligned with csi-addons controller behavior).
func HasVolumeReplicationErrorCondition(vr *replicationv1alpha1.VolumeReplication) bool {
	return hasVolumeReplicationErrorCondition(vr)
}

// WaitForVolumeReplicationInfoWithStatus waits until GetVolumeReplicationInfo would report a specific status
// (e.g., "healthy", "degraded", "syncing"). This is called via VR status polling and formatting for logging.
// The status parameter is typically "healthy", "degraded", "syncing", "disconnected", or "error".
// This helper validates that GetVolumeReplicationInfo-related status transitions occur in the VR.
// Returns the VR when the expected status is achieved, or nil on timeout.
func WaitForVolumeReplicationInfoWithStatus(ctx context.Context, c client.Client, pvcName, nsName, expectedStatus string, onPoll func(*replicationv1alpha1.VolumeReplication)) *replicationv1alpha1.VolumeReplication {
	// Find the VR associated with this PVC
	vrList := &replicationv1alpha1.VolumeReplicationList{}
	err := c.List(ctx, vrList, client.InNamespace(nsName))
	if err != nil {
		return nil
	}

	for _, vr := range vrList.Items {
		if vr.Spec.DataSource.Name == pvcName {
			// Wait for the VR to reach the expected status
			timeout := getReplicationPollTimeout()
			key := client.ObjectKeyFromObject(&vr)
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				err := c.Get(ctx, key, &vr)
				if err != nil {
					time.Sleep(pollInterval)
					continue
				}
				if onPoll != nil {
					onPoll(&vr)
				}
				// Check for expected status based on VR state and conditions
				statusMatches := false
				switch expectedStatus {
				case "healthy":
					// Healthy: Replicating=True or Completed=True, no error
					statusMatches = (hasReplicationSuccessCondition(&vr)) && !hasVolumeReplicationErrorCondition(&vr)
				case "degraded":
					// Degraded: Has error condition or Degraded=True
					statusMatches = hasVolumeReplicationErrorCondition(&vr)
				case "syncing":
					// Syncing: Replicating=True (actively syncing)
					for _, cond := range vr.Status.Conditions {
						if cond.Type == replicationv1alpha1.ConditionReplicating && cond.Status == metav1.ConditionTrue {
							statusMatches = true
							break
						}
					}
				case "disconnected":
					// Disconnected: Degraded state with peer communication error
					statusMatches = hasVolumeReplicationErrorCondition(&vr)
				case "error":
					// Error: Any error condition
					statusMatches = hasVolumeReplicationErrorCondition(&vr)
				}
				if statusMatches {
					return &vr
				}
				time.Sleep(pollInterval)
			}
			// Timeout reached
			return nil
		}
	}
	return nil
}

// RemoveFinalizerFromVR patches the VR to remove the replication finalizer so it can be deleted.
// Use when the controller is unable to remove it (e.g. driver unreachable).
// removeFinalizerWithRetry removes a finalizer from an object with conflict retry logic.
// It retries up to 3 times if Update fails with 409 Conflict, refetching the object each time.
// The removeFn callback is responsible for checking and removing the finalizer from the object.
func removeFinalizerWithRetry(ctx context.Context, c client.Client, obj client.Object, finalizer, objType, namespace, name string, removeFn func()) {
	key := client.ObjectKeyFromObject(obj)
	err := c.Get(ctx, key, obj)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}

	// Check if finalizer exists before attempting removal
	if !containsString(obj.GetFinalizers(), finalizer) {
		return
	}

	removeFn()

	// Retry on conflict (409) - object may be modified concurrently by controller
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := c.Update(ctx, obj)
		if err == nil {
			return // Success
		}
		if errors.IsConflict(err) && attempt < maxRetries-1 {
			// Object was modified concurrently, refetch and retry
			Logf("[CLEANUP]", "Conflict updating %s finalizer (attempt %d/%d), refetching: %s/%s", objType, attempt+1, maxRetries, namespace, name)
			getErr := c.Get(ctx, key, obj)
			if getErr != nil {
				if errors.IsNotFound(getErr) {
					return // Object was deleted, no need to remove finalizer
				}
				Expect(getErr).NotTo(HaveOccurred())
			}
			// Check if finalizer is still present
			if !containsString(obj.GetFinalizers(), finalizer) {
				return // Already removed
			}
			// Remove finalizer again before retry
			removeFn()
			continue
		}
		// Non-conflict error or max retries reached
		Expect(err).NotTo(HaveOccurred())
	}
}

func RemoveFinalizerFromVR(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication) {
	if vr == nil {
		return
	}
	removeFinalizerWithRetry(ctx, c, vr, volumeReplicationFinalizer, "VolumeReplication", vr.Namespace, vr.Name, func() {
		vr.Finalizers = removeString(vr.Finalizers, volumeReplicationFinalizer)
	})
}

// RemoveFinalizerFromPVC patches the PVC to remove the replication finalizer so it can be deleted.
func RemoveFinalizerFromPVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	if pvc == nil {
		return
	}
	removeFinalizerWithRetry(ctx, c, pvc, pvcReplicationFinalizer, "PersistentVolumeClaim", pvc.Namespace, pvc.Name, func() {
		pvc.Finalizers = removeString(pvc.Finalizers, pvcReplicationFinalizer)
	})
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// DeleteVolumeReplicationWithCleanup deletes the VR by removing its finalizer first,
// then triggering deletion. This prevents the 45-second timeout when finalizers block deletion.
func DeleteVolumeReplicationWithCleanup(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication) {
	if vr == nil {
		return
	}
	key := client.ObjectKeyFromObject(vr)
	startTime := time.Now()
	Logf("[CLEANUP]", "Starting VolumeReplication deletion: %s/%s", vr.Namespace, vr.Name)

	// Remove finalizer first to avoid 45-second deletion timeout
	err := c.Get(ctx, key, vr)
	if err == nil && containsString(vr.Finalizers, volumeReplicationFinalizer) {
		Logf("[CLEANUP]", "Removing finalizer from VolumeReplication: %s/%s", vr.Namespace, vr.Name)
		RemoveFinalizerFromVR(ctx, c, vr)
	}

	// Now delete the resource (should succeed quickly without finalizer)
	_ = c.Delete(ctx, vr)
	deadline := time.Now().Add(cleanupWaitTimeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, vr)
		if errors.IsNotFound(err) {
			elapsed := time.Since(startTime)
			Logf("[CLEANUP]", "VolumeReplication deleted successfully: %s/%s (took %v)", vr.Namespace, vr.Name, elapsed)
			return
		}
		time.Sleep(pollInterval)
	}
	// Still present after timeout (unexpected since we removed finalizer)
	elapsed := time.Since(startTime)
	Logf("[CLEANUP]", "WARNING: VolumeReplication still present after %v timeout (total time: %v): %s/%s", cleanupWaitTimeout, elapsed, vr.Namespace, vr.Name)
}

// DeletePVCWithCleanup deletes the PVC by removing its finalizer first,
// then triggering deletion. This prevents the 45-second timeout when finalizers block deletion.
func DeletePVCWithCleanup(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	if pvc == nil {
		return
	}
	key := client.ObjectKeyFromObject(pvc)
	startTime := time.Now()
	Logf("[CLEANUP]", "Starting PersistentVolumeClaim deletion: %s/%s", pvc.Namespace, pvc.Name)

	// Remove finalizer first to avoid 45-second deletion timeout
	err := c.Get(ctx, key, pvc)
	if err == nil && containsString(pvc.Finalizers, pvcReplicationFinalizer) {
		Logf("[CLEANUP]", "Removing finalizer from PersistentVolumeClaim: %s/%s", pvc.Namespace, pvc.Name)
		RemoveFinalizerFromPVC(ctx, c, pvc)
	}

	// Now delete the resource (should succeed quickly without finalizer)
	_ = c.Delete(ctx, pvc)
	deadline := time.Now().Add(cleanupWaitTimeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, pvc)
		if errors.IsNotFound(err) {
			elapsed := time.Since(startTime)
			Logf("[CLEANUP]", "PersistentVolumeClaim deleted successfully: %s/%s (took %v)", pvc.Namespace, pvc.Name, elapsed)
			return
		}
		time.Sleep(pollInterval)
	}
	// Still present after timeout (unexpected since we removed finalizer)
	elapsed := time.Since(startTime)
	Logf("[CLEANUP]", "WARNING: PersistentVolumeClaim still present after %v timeout (total time: %v): %s/%s", cleanupWaitTimeout, elapsed, pvc.Namespace, pvc.Name)
}

// DeleteVolumeReplication deletes a VR and ignores NotFound.
func DeleteVolumeReplication(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication) {
	err := c.Delete(ctx, vr)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// DeleteVolumeReplicationClass deletes a VRC and ignores NotFound.
func DeleteVolumeReplicationClass(ctx context.Context, c client.Client, vrc *replicationv1alpha1.VolumeReplicationClass) {
	err := c.Delete(ctx, vrc)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	// VRC may be deleted immediately or have finalizers - don't wait for async deletion
	// The cleanup script will handle any remaining VRCs after test suite
}

// DeleteVolumeReplicationClassWithCleanup deletes a VRC and waits for it to be gone (up to 45s).
// This ensures VRCs don't accumulate when they have finalizers.
func DeleteVolumeReplicationClassWithCleanup(ctx context.Context, c client.Client, vrc *replicationv1alpha1.VolumeReplicationClass) {
	if vrc == nil {
		return
	}
	key := client.ObjectKeyFromObject(vrc)
	startTime := time.Now()
	Logf("[CLEANUP]", "Starting VolumeReplicationClass deletion: %s/%s", vrc.Namespace, vrc.Name)
	_ = c.Delete(ctx, vrc)
	deadline := time.Now().Add(cleanupWaitTimeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, vrc)
		if errors.IsNotFound(err) {
			elapsed := time.Since(startTime)
			Logf("[CLEANUP]", "VolumeReplicationClass deleted successfully: %s/%s (took %v)", vrc.Namespace, vrc.Name, elapsed)
			return
		}
		time.Sleep(pollInterval)
	}
	// VRC still present after timeout; log warning but continue
	// (VRC may have finalizers that take longer or be stuck)
	elapsed := time.Since(startTime)
	Logf("[CLEANUP]", "WARNING: VolumeReplicationClass %s/%s still present after %v timeout (total elapsed: %v)", vrc.Namespace, vrc.Name, cleanupWaitTimeout, elapsed)
}

// DeletePVC deletes a PVC and ignores NotFound.
func DeletePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	err := c.Delete(ctx, pvc)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// DeleteNamespace deletes a namespace and ignores NotFound.
func DeleteNamespace(ctx context.Context, c client.Client, ns *corev1.Namespace) {
	err := c.Delete(ctx, ns)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// networkFenceCapability is the capability string advertised by CSIAddonsNode when the driver
// supports NetworkFence (matches identity.Capability_NetworkFence_NETWORK_FENCE).
const networkFenceCapability = "network_fence.NETWORK_FENCE"

// HasNetworkFenceSupport returns true if (1) NetworkFence and NetworkFenceClass CRDs are installed,
// and (2) at least one CSIAddonsNode for the given provisioner advertises network_fence.NETWORK_FENCE.
// Use before L1-E-003 to skip when the driver does not support fencing.
func HasNetworkFenceSupport(ctx context.Context, c client.Client, provisioner string) bool {
	nfList := &csiaddonsv1alpha1.NetworkFenceList{}
	if err := c.List(ctx, nfList); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false
		}
		return false
	}
	nfcList := &csiaddonsv1alpha1.NetworkFenceClassList{}
	if err := c.List(ctx, nfcList); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false
		}
		return false
	}
	// Check driver advertises NETWORK_FENCE capability (CSIAddonsNode status.capabilities)
	list := &csiaddonsv1alpha1.CSIAddonsNodeList{}
	if err := c.List(ctx, list); err != nil {
		return false
	}
	for i := range list.Items {
		node := &list.Items[i]
		if node.Spec.Driver.Name != provisioner {
			continue
		}
		if node.Status.State != csiaddonsv1alpha1.CSIAddonsNodeStateConnected {
			continue
		}
		for _, cap := range node.Status.Capabilities {
			if cap == networkFenceCapability {
				return true
			}
		}
	}
	return false
}

// CreateNetworkFenceClass creates a NetworkFenceClass with the given provisioner and secret ref.
// The secret is used by the CSI driver for fence/unfence operations. Use the same secret as
// replication (e.g. rook-csi-rbd-provisioner) when the driver supports both.
//
// For Ceph CSI (Rook), clusterID is required. It is taken from FENCE_CLUSTER_ID env var, or
// inferred as secretNamespace when the provisioner contains "ceph" (Rook uses namespace as clusterID).
func CreateNetworkFenceClass(ctx context.Context, c client.Client, name, provisioner, secretName, secretNamespace string) *csiaddonsv1alpha1.NetworkFenceClass {
	params := map[string]string{
		networkFenceSecretNameKey: secretName,
		networkFenceSecretNsKey:   secretNamespace,
	}
	// Ceph CSI requires clusterID for network fencing. Use FENCE_CLUSTER_ID or infer from secret namespace.
	if clusterID := os.Getenv("FENCE_CLUSTER_ID"); clusterID != "" {
		params["clusterID"] = clusterID
	} else if strings.Contains(provisioner, "ceph") && secretNamespace != "" {
		params["clusterID"] = secretNamespace
	}
	nfc := &csiaddonsv1alpha1.NetworkFenceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: csiaddonsv1alpha1.NetworkFenceClassSpec{
			Provisioner: provisioner,
			Parameters:  params,
		},
	}
	err := c.Create(ctx, nfc)
	Expect(err).NotTo(HaveOccurred())
	return nfc
}

// GetFenceCIDRsForFaultInjection returns CIDRs for the iptables fault injector only (not NetworkFence).
// Order: 1) FENCE_CIDRS, 2) service/backend IPs (Endpoints + EndpointSlice). Raw cluster node InternalIPs are
// never used as fence targets (avoids blocking the fencing node); use FENCE_CIDRS if you must target a node IP.
func GetFenceCIDRsForFaultInjection(ctx context.Context, c client.Client) []string {
	if cidrs := parseFenceCIDRSFromEnv(); len(cidrs) > 0 {
		Logf("[DEBUG]", "GetFenceCIDRsForFaultInjection: Using FENCE_CIDRS env: %v", cidrs)
		return cidrs
	}
	Logf("[INFO]", "GetFenceCIDRsForFaultInjection: resolving targets via GetNodeIPsForFencing (iptables: no raw node-IP fence targets)")
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
	if len(keys) == 0 {
		Logf("[WARN]", "GetFenceCIDRsForFaultInjectionPeer: set %s or %s (namespace/service list) or FENCE_CIDRS",
			fencePeerServicesEnv, fenceTargetServicesEnv)
		return nil
	}
	var merged []string
	for _, key := range keys {
		ips := collectServiceBackendIPs(ctx, peerClient, key)
		Logf("[INFO]", "peer fence: %s/%s backend IPs on peer cluster: %v", key.Namespace, key.Name, ips)
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
	Logf("[DEBUG]", "GetFenceCIDRs: FENCE_CIDRS not set, checking CSIAddonsNode for networkFenceClientStatus (provisioner=%s, class=%s)", provisioner, networkFenceClassName)
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

func parseFenceTargetServicesFromEnv() []client.ObjectKey {
	return parseFenceNamespaceServiceList(os.Getenv(fenceTargetServicesEnv), fenceTargetServicesEnv)
}

// parseFencePeerServicesFromEnv returns service keys for peer-cluster lookup: FENCE_PEER_SERVICES if set,
// otherwise the same keys as FENCE_TARGET_SERVICES.
func parseFencePeerServicesFromEnv() []client.ObjectKey {
	s := strings.TrimSpace(os.Getenv(fencePeerServicesEnv))
	if s != "" {
		return parseFenceNamespaceServiceList(s, fencePeerServicesEnv)
	}
	return parseFenceTargetServicesFromEnv()
}

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

func cidrsFromEndpointsObject(ctx context.Context, c client.Client, key client.ObjectKey) []string {
	ep := &corev1.Endpoints{} //nolint:SA1019 // v1 Endpoints still used alongside EndpointSlice for older clusters / Services
	if err := c.Get(ctx, key, ep); err != nil {
		Logf("[DEBUG]", "cidrsFromEndpointsObject: get Endpoints %s: %v", key, err)
		return nil
	}
	var ips []string
	for _, sub := range ep.Subsets {
		for _, a := range sub.Addresses {
			if a.IP != "" {
				ips = append(ips, a.IP)
			}
		}
	}
	return ips
}

// collectServiceBackendIPs merges addresses from v1 Endpoints and EndpointSlices labeled for the Service.
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
	for _, ip := range cidrsFromEndpointsObject(ctx, c, key) {
		add(ip)
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

func fenceCIDRsFromConfiguredTargetServices(ctx context.Context, c client.Client, nodeIPs map[string]struct{}) []string {
	keys := parseFenceTargetServicesFromEnv()
	if len(keys) == 0 {
		return nil
	}
	var merged []string
	for _, key := range keys {
		ips := collectServiceBackendIPs(ctx, c, key)
		Logf("[INFO]", "%s: service %s/%s backend IPs (Endpoints+EndpointSlice): %v", fenceTargetServicesEnv, key.Namespace, key.Name, ips)
		merged = append(merged, ips...)
	}
	out := filterEndpointIPsToCIDRs(merged, nodeIPs)
	if len(out) > 0 {
		Logf("[INFO]", "%s: fence CIDRs after excluding node InternalIPs: %v", fenceTargetServicesEnv, out)
	} else if len(merged) > 0 {
		Logf("[WARN]", "%s: all backend IPs matched node InternalIPs; nothing to fence from configured services", fenceTargetServicesEnv)
	}
	return out
}

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

func capFenceCIDRList(cidrs []string) []string {
	if len(cidrs) <= fenceAutoDiscoveryMaxCIDRs {
		return cidrs
	}
	Logf("[WARN]", "capping fence CIDR list from %d to %d (set FENCE_CIDRS to be explicit)", len(cidrs), fenceAutoDiscoveryMaxCIDRs)
	return append([]string(nil), cidrs[:fenceAutoDiscoveryMaxCIDRs]...)
}

// CreateNetworkFence creates a NetworkFence that blocks (Fenced) or unblocks (Unfenced) the given CIDRs.
func CreateNetworkFence(ctx context.Context, c client.Client, name, networkFenceClassName string, cidrs []string, fenceState csiaddonsv1alpha1.FenceState) *csiaddonsv1alpha1.NetworkFence {
	nf := &csiaddonsv1alpha1.NetworkFence{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: csiaddonsv1alpha1.NetworkFenceSpec{
			NetworkFenceClassName: networkFenceClassName,
			FenceState:            fenceState,
			Cidrs:                 cidrs,
		},
	}
	err := c.Create(ctx, nf)
	Expect(err).NotTo(HaveOccurred())
	return nf
}

// WaitForNetworkFenceResult waits until the NetworkFence status has the given Result or times out.
func WaitForNetworkFenceResult(ctx context.Context, c client.Client, nf *csiaddonsv1alpha1.NetworkFence, result csiaddonsv1alpha1.FencingOperationResult) {
	key := client.ObjectKeyFromObject(nf)
	Eventually(func() bool {
		err := c.Get(ctx, key, nf)
		if err != nil {
			return false
		}
		return nf.Status.Result == result
	}, networkFencePollTimeout, pollInterval).Should(BeTrue(),
		"NetworkFence %s should get status.result=%s (got %s)", nf.Name, result, nf.Status.Result)
}

// CreateNetworkFenceAndWait creates both a NetworkFenceClass and a NetworkFence, waits for the fence to complete.
// This is a convenience function for tests that need to simulate split-brain via network isolation.
// Returns the created NetworkFenceClass and NetworkFence pointers.
func CreateNetworkFenceAndWait(ctx context.Context, c client.Client, namespace, provisioner, secretName, secretNamespace string) (*csiaddonsv1alpha1.NetworkFenceClass, *csiaddonsv1alpha1.NetworkFence) {
	nfcName := "nfc-" + UniqueNamespace()
	nfName := "nf-" + UniqueNamespace()

	Logf("[DEBUG]", "CreateNetworkFenceAndWait: Creating NetworkFenceClass: %s", nfcName)
	// Create NetworkFenceClass
	nfc := CreateNetworkFenceClass(ctx, c, nfcName, provisioner, secretName, secretNamespace)
	Logf("[INFO]", "CreateNetworkFenceAndWait: NetworkFenceClass created: %s", nfcName)

	// Get CIDRs to fence
	Logf("[DEBUG]", "CreateNetworkFenceAndWait: Getting fence CIDRs for class: %s", nfcName)
	cidrs := GetFenceCIDRs(ctx, c, provisioner, nfcName)
	if len(cidrs) == 0 {
		Logf("[ERROR]", "CreateNetworkFenceAndWait: No CIDRs found for fencing! Cannot proceed with NetworkFence creation")
		Expect(cidrs).NotTo(BeEmpty(), "Failed to get CIDRs for network fencing")
	}
	Logf("[INFO]", "CreateNetworkFenceAndWait: Retrieved %d CIDRs for fencing: %v", len(cidrs), cidrs)

	// Create NetworkFence with Fenced state
	Logf("[DEBUG]", "CreateNetworkFenceAndWait: Creating NetworkFence: %s with CIDRs: %v", nfName, cidrs)
	nf := CreateNetworkFence(ctx, c, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
	Logf("[INFO]", "CreateNetworkFenceAndWait: NetworkFence created: %s", nfName)

	// Wait for fence to be applied
	Logf("[DEBUG]", "CreateNetworkFenceAndWait: Waiting for NetworkFence result (timeout=2 min)...")
	WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)
	Logf("[INFO]", "CreateNetworkFenceAndWait: NetworkFence %s result succeeded", nfName)

	return nfc, nf
}

// UnfenceNetworkFence sets fenceState to Unfenced to unblock the CIDRs. Deletion no longer
// triggers UnfenceClusterNetwork; the controller requires an explicit fenceState: Unfenced update.
func UnfenceNetworkFence(ctx context.Context, c client.Client, nf *csiaddonsv1alpha1.NetworkFence) {
	if nf == nil {
		return
	}
	key := client.ObjectKeyFromObject(nf)
	if err := c.Get(ctx, key, nf); err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	if nf.Spec.FenceState == csiaddonsv1alpha1.Unfenced {
		return // already unfenced
	}
	nf.Spec.FenceState = csiaddonsv1alpha1.Unfenced
	Expect(c.Update(ctx, nf)).To(Succeed())
	WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)
}

// DeleteNetworkFence deletes a NetworkFence. Does not perform unfence; use UnfenceNetworkFence first.
func DeleteNetworkFence(ctx context.Context, c client.Client, nf *csiaddonsv1alpha1.NetworkFence) {
	if nf == nil {
		return
	}
	err := c.Delete(ctx, nf)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// RemoveFinalizerFromNetworkFence patches the NetworkFence to remove its finalizer so it can be deleted.
func RemoveFinalizerFromNetworkFence(ctx context.Context, c client.Client, nf *csiaddonsv1alpha1.NetworkFence) {
	if nf == nil {
		return
	}
	removeFinalizerWithRetry(ctx, c, nf, networkFenceFinalizer, "NetworkFence", nf.Namespace, nf.Name, func() {
		nf.Finalizers = removeString(nf.Finalizers, networkFenceFinalizer)
	})
}

// logVRState logs comprehensive state information about a VolumeReplication resource in CRD format
func logVRState(vr *replicationv1alpha1.VolumeReplication, stageName string) {
	if vr == nil {
		return
	}

	// Format conditions as Condition=Status pairs
	conditionsStr := "NO CONDITIONS"
	if len(vr.Status.Conditions) > 0 {
		var condDetails []string
		for _, cond := range vr.Status.Conditions {
			condStr := fmt.Sprintf("%s=%s", cond.Type, cond.Status)
			condDetails = append(condDetails, condStr)
		}
		conditionsStr = strings.Join(condDetails, " ")
	}

	// Format message
	message := vr.Status.Message
	if message == "" {
		message = "-"
	}

	// Log data line in CRD table format:
	// NAMESPACE              NAME              STATE        CONDITIONS                               MESSAGE
	Logf("[INFO]", "%-30s %-30s %-15s %-40s %s",
		vr.Namespace, vr.Name, vr.Status.State, conditionsStr, message)
}

// DeleteNetworkFenceWithCleanup unfences the CIDRs (sets fenceState: Unfenced), then deletes the
// NetworkFence. Deletion no longer triggers UnfenceClusterNetwork; unfence must be explicit.
// If vrs is provided (non-nil), waits for each VR's Degraded condition to become False before deletion.
func DeleteNetworkFenceWithCleanup(ctx context.Context, c client.Client, nf *csiaddonsv1alpha1.NetworkFence, vrs ...*replicationv1alpha1.VolumeReplication) {
	if nf == nil {
		return
	}
	key := client.ObjectKeyFromObject(nf)
	if err := c.Get(ctx, key, nf); err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	// Unfence first: deletion no longer triggers UnfenceClusterNetwork
	if nf.Spec.FenceState == csiaddonsv1alpha1.Fenced {
		UnfenceNetworkFence(ctx, c, nf)
		// Wait for unfence operation to complete before deletion
		WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

		// If VRs provided, wait for them to recover (Degraded=False) instead of hardcoded sleep
		// After unfencing, RBD mirror needs time to detect connectivity and process state updates
		if len(vrs) > 0 {
			Logf("[INFO]", "NetworkFence unfenced successfully, allowing RBD mirror time to detect connectivity restoration")
			// Give RBD mirror a moment to process the unfence and detect connectivity is restored
			time.Sleep(3 * time.Second)

			Logf("[INFO]", "")
			Logf("[INFO]", "==== VR STATE BEFORE UNFENCING ====")
			Logf("[INFO]", "%-30s %-30s %-15s %-40s %s", "NAMESPACE", "NAME", "STATE", "CONDITIONS", "MESSAGE")
			for _, vr := range vrs {
				if vr == nil {
					continue
				}
				err := c.Get(ctx, client.ObjectKeyFromObject(vr), vr)
				if err != nil {
					Logf("[WARNING]", "Failed to fetch VR %s/%s before unfence: %v", vr.Namespace, vr.Name, err)
					continue
				}
				logVRState(vr, "PRE-UNFENCE")
			}

			Logf("[INFO]", "")
			Logf("[INFO]", "==== VR STATE IMMEDIATELY AFTER UNFENCING ====")
			Logf("[INFO]", "%-30s %-30s %-15s %-40s %s", "NAMESPACE", "NAME", "STATE", "CONDITIONS", "MESSAGE")
			for _, vr := range vrs {
				if vr == nil {
					continue
				}
				err := c.Get(ctx, client.ObjectKeyFromObject(vr), vr)
				if err != nil {
					Logf("[WARNING]", "Failed to fetch VR %s/%s immediately after unfence: %v", vr.Namespace, vr.Name, err)
					continue
				}
				logVRState(vr, "POST-UNFENCE-IMMEDIATE")
			}

			Logf("[INFO]", "Starting recovery verification: waiting for VR Degraded=False")

			firstPoll := true
			for _, vr := range vrs {
				if vr == nil {
					continue
				}
				pollCount := 0
				Eventually(func() bool {
					pollCount++
					err := c.Get(ctx, client.ObjectKeyFromObject(vr), vr)
					if err != nil {
						Logf("[DEBUG]", "[Poll %2d] Failed to fetch VR %s/%s: %v", pollCount, vr.Namespace, vr.Name, err)
						return false
					}

					// Log header only on first poll to avoid clutter
					if pollCount == 1 && firstPoll {
						Logf("[INFO]", "")
						Logf("[INFO]", "==== VR RECOVERY MONITORING (polling every 5s, timeout 300s) ====")
						Logf("[INFO]", "%-30s %-30s %-15s %-40s %s", "NAMESPACE", "NAME", "STATE", "CONDITIONS", "MESSAGE")
						firstPoll = false
					}

					// Log current state for debugging with all details
					logVRState(vr, fmt.Sprintf("RECOVERY-POLL-%d", pollCount))

					// VR is recovered when Degraded is False (or condition absent = true)
					for _, cond := range vr.Status.Conditions {
						if cond.Type == replicationv1alpha1.ConditionDegraded {
							if cond.Status == metav1.ConditionFalse {
								Logf("[INFO]", "✓ VR %s/%s HAS RECOVERED: Degraded=False", vr.Namespace, vr.Name)
								return true
							}
							// Still degraded, continue waiting
							return false
						}
					}
					// No Degraded condition means VR is healthy/recovered
					Logf("[INFO]", "✓ VR %s/%s HAS RECOVERED: Degraded condition absent", vr.Namespace, vr.Name)
					return true
				}, 300*time.Second, 5*time.Second).Should(BeTrue(),
					"VR %s/%s health should recover (Degraded=False) after unfencing", vr.Namespace, vr.Name)

				// Log final state after recovery
				err := c.Get(ctx, client.ObjectKeyFromObject(vr), vr)
				if err == nil {
					if firstPoll {
						Logf("[INFO]", "")
						Logf("[INFO]", "==== VR FINAL STATE (RECOVERED) ====")
						Logf("[INFO]", "%-30s %-30s %-15s %-40s %s", "NAMESPACE", "NAME", "STATE", "CONDITIONS", "MESSAGE")
						firstPoll = false
					}
					logVRState(vr, "FINAL-RECOVERED")
				}
			}
			Logf("[INFO]", "All VRs recovered after unfencing, proceeding with deletion")
		}
	}
	_ = c.Delete(ctx, nf)
	deadline := time.Now().Add(cleanupWaitTimeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, nf)
		if errors.IsNotFound(err) {
			return
		}
		time.Sleep(pollInterval)
	}
	if err := c.Get(ctx, key, nf); err == nil {
		RemoveFinalizerFromNetworkFence(ctx, c, nf)
	}
}

// DeleteNetworkFenceClass deletes a NetworkFenceClass.
func DeleteNetworkFenceClass(ctx context.Context, c client.Client, nfc *csiaddonsv1alpha1.NetworkFenceClass) {
	if nfc == nil {
		return
	}
	err := c.Delete(ctx, nfc)
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// RemoveFinalizerFromNetworkFenceClass patches the NetworkFenceClass to remove its finalizer.
func RemoveFinalizerFromNetworkFenceClass(ctx context.Context, c client.Client, nfc *csiaddonsv1alpha1.NetworkFenceClass) {
	if nfc == nil {
		return
	}
	removeFinalizerWithRetry(ctx, c, nfc, networkFenceClassFinalizer, "NetworkFenceClass", nfc.Namespace, nfc.Name, func() {
		nfc.Finalizers = removeString(nfc.Finalizers, networkFenceClassFinalizer)
	})
}

// DeleteNetworkFenceClassWithCleanup deletes the NetworkFenceClass, waits for it to be gone, and
// removes its finalizer if it is still present after the timeout.
func DeleteNetworkFenceClassWithCleanup(ctx context.Context, c client.Client, nfc *csiaddonsv1alpha1.NetworkFenceClass) {
	if nfc == nil {
		return
	}
	key := client.ObjectKeyFromObject(nfc)
	_ = c.Delete(ctx, nfc)
	deadline := time.Now().Add(cleanupWaitTimeout)
	for time.Now().Before(deadline) {
		err := c.Get(ctx, key, nfc)
		if errors.IsNotFound(err) {
			return
		}
		time.Sleep(pollInterval)
	}
	if err := c.Get(ctx, key, nfc); err == nil {
		RemoveFinalizerFromNetworkFenceClass(ctx, c, nfc)
	}
}
