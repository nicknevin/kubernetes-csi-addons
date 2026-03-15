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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
func RemoveFinalizerFromVR(ctx context.Context, c client.Client, vr *replicationv1alpha1.VolumeReplication) {
	if vr == nil {
		return
	}
	key := client.ObjectKeyFromObject(vr)
	err := c.Get(ctx, key, vr)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	if !containsString(vr.Finalizers, volumeReplicationFinalizer) {
		return
	}
	vr.Finalizers = removeString(vr.Finalizers, volumeReplicationFinalizer)
	Expect(c.Update(ctx, vr)).To(Succeed())
}

// RemoveFinalizerFromPVC patches the PVC to remove the replication finalizer so it can be deleted.
func RemoveFinalizerFromPVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	if pvc == nil {
		return
	}
	key := client.ObjectKeyFromObject(pvc)
	err := c.Get(ctx, key, pvc)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	if !containsString(pvc.Finalizers, pvcReplicationFinalizer) {
		return
	}
	pvc.Finalizers = removeString(pvc.Finalizers, pvcReplicationFinalizer)
	Expect(c.Update(ctx, pvc)).To(Succeed())
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

// GetFenceCIDRs returns CIDRs to use for NetworkFence. It first checks env FENCE_CIDRS (comma-separated).
// If unset, it waits up to fenceCIDRProbeTimeout for CSIAddonsNodes (matching provisioner) to have
// status.networkFenceClientStatus for networkFenceClassName. If still empty, falls back to node InternalIPs
// (useful when driver does not advertise GET_CLIENTS_TO_FENCE, e.g. CephFS). Returns nil only when all
// sources fail (caller should skip the test).
func GetFenceCIDRs(ctx context.Context, c client.Client, provisioner, networkFenceClassName string) []string {
	if s := os.Getenv("FENCE_CIDRS"); s != "" {
		var cidrs []string
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				cidrs = append(cidrs, part)
			}
		}
		if len(cidrs) > 0 {
			return cidrs
		}
	}
	deadline := time.Now().Add(fenceCIDRProbeTimeout)
	var cidrs []string
	for time.Now().Before(deadline) {
		list := &csiaddonsv1alpha1.CSIAddonsNodeList{}
		err := c.List(ctx, list)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		cidrs = nil
		for i := range list.Items {
			node := &list.Items[i]
			if node.Spec.Driver.Name != provisioner {
				continue
			}
			for _, nfcs := range node.Status.NetworkFenceClientStatus {
				if nfcs.NetworkFenceClassName != networkFenceClassName {
					continue
				}
				for _, detail := range nfcs.ClientDetails {
					cidrs = append(cidrs, detail.Cidrs...)
				}
			}
		}
		if len(cidrs) > 0 {
			return cidrs
		}
		time.Sleep(pollInterval)
	}
	// Fallback: use node InternalIPs when driver does not advertise GET_CLIENTS_TO_FENCE (e.g. CephFS)
	return GetNodeIPsForFencing(ctx, c)
}

// GetNodeIPsForFencing returns node InternalIPs as /32 CIDRs. Used as fallback when
// FENCE_CIDRS is unset and CSIAddonsNode networkFenceClientStatus is empty.
func GetNodeIPsForFencing(ctx context.Context, c client.Client) []string {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return nil
	}
	var cidrs []string
	for _, node := range nodeList.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
				cidrs = append(cidrs, addr.Address+"/32")
				break
			}
		}
	}
	return cidrs
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

	// Create NetworkFenceClass
	nfc := CreateNetworkFenceClass(ctx, c, nfcName, provisioner, secretName, secretNamespace)

	// Get CIDRs to fence
	cidrs := GetFenceCIDRs(ctx, c, provisioner, nfcName)
	if len(cidrs) == 0 {
		Expect(cidrs).NotTo(BeEmpty(), "Failed to get CIDRs for network fencing")
	}

	// Create NetworkFence with Fenced state
	nf := CreateNetworkFence(ctx, c, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)

	// Wait for fence to be applied
	WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

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
	key := client.ObjectKeyFromObject(nf)
	err := c.Get(ctx, key, nf)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	if !containsString(nf.Finalizers, networkFenceFinalizer) {
		return
	}
	nf.Finalizers = removeString(nf.Finalizers, networkFenceFinalizer)
	Expect(c.Update(ctx, nf)).To(Succeed())
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
		for _, vr := range vrs {
			if vr == nil {
				continue
			}
			Eventually(func() bool {
				err := c.Get(ctx, client.ObjectKeyFromObject(vr), vr)
				if err != nil {
					return false
				}
				// Check that VR is no longer degraded (Degraded=False)
				for _, cond := range vr.Status.Conditions {
					if cond.Type == "Degraded" {
						return cond.Status == metav1.ConditionFalse
					}
				}
				return false
			}, 200*time.Second, 10*time.Second).Should(BeTrue(),
				"VR %s/%s health should recover (Degraded=False) after unfencing", vr.Namespace, vr.Name)
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
	key := client.ObjectKeyFromObject(nfc)
	err := c.Get(ctx, key, nfc)
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	if !containsString(nfc.Finalizers, networkFenceClassFinalizer) {
		return
	}
	nfc.Finalizers = removeString(nfc.Finalizers, networkFenceClassFinalizer)
	Expect(c.Update(ctx, nfc)).To(Succeed())
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
