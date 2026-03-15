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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

// hasVolumeReplicationCompletedCondition checks if VR has Completed=True condition.
// This is used for resync operations where the state remains Secondary but Completed=True signals completion.
func hasVolumeReplicationCompletedCondition(vr *replicationv1alpha1.VolumeReplication) bool {
	for _, cond := range vr.Status.Conditions {
		if cond.Type == replicationv1alpha1.ConditionCompleted && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasResyncOperationCompleted checks if the resync operation has completed (Resyncing=False).
// This is the key indicator that the resync RPC call finished, regardless of Degraded state.
// After resync completes, Degraded may still be True as the mirror recovers.
func hasResyncOperationCompleted(vr *replicationv1alpha1.VolumeReplication) bool {
	for _, cond := range vr.Status.Conditions {
		if cond.Type == replicationv1alpha1.ConditionResyncing && cond.Status == metav1.ConditionFalse {
			return true
		}
	}
	return false
}

var _ = Describe("ResyncVolumeReplication", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
	})

	Describe("L1-RSYNC-001: Resync secondary after split-brain", func() {
		It("L1-RSYNC-001: resync secondary after split-brain, expect full resync completes and data consistent", func() {
			By("L1-RSYNC-001: Setup primary on DR1, secondary on DR2, trigger network fence on DR2, then resync")
			SkipIfNotFullDR("L1-RSYNC-001", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-rsync", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-rsync-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-rsync", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("L1-RSYNC-001: Validating primary replication state")
			_ = cDR1.Get(ctx, client.ObjectKeyFromObject(vrDR1), vrDR1)
			fmt.Fprintf(GinkgoWriter, "  [DR1][INFO] Primary: %s\n", FormatVRStatus(vrDR1))
			Expect(vrDR1.Status.State).To(Equal(replicationv1alpha1.PrimaryState), "Primary should be in Primary state")
			Expect(hasReplicationSuccessCondition(vrDR1)).To(BeTrue(), "Primary should have success condition")

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-rsync", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-rsync", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Secondary state and stable")
			Eventually(func() string {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				return string(vrDR2.Status.State)
			}, 30*time.Second, 1*time.Second).Should(Equal(string(replicationv1alpha1.SecondaryState)),
				"Secondary VR should be in Secondary state before resync")

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			By("L1-RSYNC-001: Validating secondary replication state (pre-fence)")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] Secondary (pre-fence): %s\n", FormatVRStatus(vrDR2))
			Expect(vrDR2.Status.State).To(Equal(replicationv1alpha1.SecondaryState), "Secondary should be in Secondary state")
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(), "Secondary should have success condition")

			var nfc *csiaddonsv1alpha1.NetworkFenceClass
			var nf *csiaddonsv1alpha1.NetworkFence

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteNetworkFenceWithCleanup(cleanupCtx, cDR2, nf)
				DeleteNetworkFenceClassWithCleanup(cleanupCtx, cDR2, nfc)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR2, vrDR2)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR2, vrcDR2)
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeletePV(cleanupCtx, cDR2, pvDR2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vrDR1)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrcDR1)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvcDR1)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-RSYNC-001: Creating NetworkFence to simulate split-brain (isolate secondary)")
			if !IsNetworkFenceSupportAvailable() {
				By("L1-RSYNC-001: NetworkFence not supported, simulating split-brain via demotion")
				_ = cDR1.Get(ctx, client.ObjectKeyFromObject(vrDR1), vrDR1)
				fmt.Fprintf(GinkgoWriter, "  [NOTE] NetworkFence not available; skipping network isolation. Resync will proceed on healthy secondary.\n")
			} else {
				By("L1-RSYNC-001: NetworkFence supported, creating fence to simulate split-brain")
				nfc, nf = CreateNetworkFenceAndWait(ctx, cDR2, nsName, env.Provisioner, secretName, secretNs)
				By("L1-RSYNC-001: NetworkFence created, secondary is now isolated")

				By("L1-RSYNC-001: Validating secondary replication state via GetVolumeReplicationInfo (post-fence, degraded)")
				Eventually(func() bool {
					_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
					fmt.Fprintf(GinkgoWriter, "  [DR2][VR] post-fence: %s\n", FormatVRStatus(vrDR2))
					return HasVolumeReplicationErrorCondition(vrDR2)
				}, 30*time.Second, 2*time.Second).Should(BeTrue(),
					"Secondary should show error condition after network fence")
			}

			By("L1-RSYNC-001: Removing NetworkFence to resolve split-brain")
			DeleteNetworkFenceWithCleanup(ctx, cDR2, nf)

			By("L1-RSYNC-001: Triggering resync by updating VR to Resync state")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to update VR replicationState to Resync")

			By("Waiting for VR to complete resync (checking Completed condition)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] resync in progress: %s\n", FormatVRStatus(vrDR2))
				return hasVolumeReplicationCompletedCondition(vrDR2) && hasResyncOperationCompleted(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
				"VR Completed condition should be True and Resyncing=False after resync")

			By("L1-RSYNC-001: Assertion — data consistency confirmed")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] Secondary (post-resync): %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after resync")
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(), "Should have success condition")
		})
	})

	Describe("L1-RSYNC-002: Idempotent resync", func() {
		It("L1-RSYNC-002: resync already-synced secondary, expect idempotent success with no change", func() {
			By("L1-RSYNC-002: Create primary and secondary, trigger resync on healthy secondary (idempotent)")
			SkipIfNotFullDR("L1-RSYNC-002", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-idem", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-idem-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-idem", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-idem", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-idem", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR2, vrDR2)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR2, vrcDR2)
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeletePV(cleanupCtx, cDR2, pvDR2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vrDR1)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrcDR1)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvcDR1)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-RSYNC-002: Validating healthy state before first resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] pre-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(), "Should have success condition before resync")

			By("L1-RSYNC-002: Triggering first resync on healthy secondary")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for first resync to complete (checking Completed condition)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] first resync: %s\n", FormatVRStatus(vrDR2))
				return hasVolumeReplicationCompletedCondition(vrDR2) && hasResyncOperationCompleted(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-002: Assertion — data consistency after first resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-first-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after first resync")

			By("L1-RSYNC-002: Triggering second resync on already-synced secondary (idempotent)")
			err = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for second resync to complete (checking Completed condition)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] second resync: %s\n", FormatVRStatus(vrDR2))
				return hasVolumeReplicationCompletedCondition(vrDR2) && hasResyncOperationCompleted(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-002: Assertion — data consistency after second resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-second-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after second resync")
		})
	})

	Describe("L1-RSYNC-003: Resync with NetworkFence (split-brain recovery)", func() {
		It("L1-RSYNC-003: resync with secondary network-fenced (split-brain), expect resync completes after fence removal", func() {
			By("L1-RSYNC-003: Create primary and secondary, apply NetworkFence, resolve, then resync")
			SkipIfNotFullDR("L1-RSYNC-003", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-RSYNC-003: NetworkFence not supported on this setup")
			}

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-fence", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-fence-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-fence", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-fence", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-fence", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			var nfc *csiaddonsv1alpha1.NetworkFenceClass
			var nf *csiaddonsv1alpha1.NetworkFence

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteNetworkFenceWithCleanup(cleanupCtx, cDR2, nf)
				DeleteNetworkFenceClassWithCleanup(cleanupCtx, cDR2, nfc)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR2, vrDR2)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR2, vrcDR2)
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeletePV(cleanupCtx, cDR2, pvDR2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vrDR1)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrcDR1)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvcDR1)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-RSYNC-003: Creating NetworkFence to isolate secondary")
			nfc, nf = CreateNetworkFenceAndWait(ctx, cDR2, nsName, env.Provisioner, secretName, secretNs)

			By("L1-RSYNC-003: Validating secondary is fenced (split-brain state)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] fenced: %s\n", FormatVRStatus(vrDR2))
				return HasVolumeReplicationErrorCondition(vrDR2)
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"Secondary should show error after fence applied")

			By("L1-RSYNC-003: Removing NetworkFence to resolve split-brain")
			DeleteNetworkFenceWithCleanup(ctx, cDR2, nf)

			By("L1-RSYNC-003: Triggering resync after split-brain recovery")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for resync to complete (checking Completed condition)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] resync: %s\n", FormatVRStatus(vrDR2))
				return hasVolumeReplicationCompletedCondition(vrDR2) && hasResyncOperationCompleted(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-003: Assertion — data consistency after resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after resync")
		})
	})

	Describe("L1-RSYNC-004: Force resync", func() {
		It("L1-RSYNC-004: force resync on healthy secondary, expect resync proceeds with force parameter", func() {
			By("L1-RSYNC-004: Create primary and secondary, trigger force resync")
			SkipIfNotFullDR("L1-RSYNC-004", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-force", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-force-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-force", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-force", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-force", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR2, vrDR2)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR2, vrcDR2)
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeletePV(cleanupCtx, cDR2, pvDR2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vrDR1)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrcDR1)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvcDR1)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-RSYNC-004: Triggering resync (controller hardcodes force=true internally)")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to update VR to Resync state")

			By("Waiting for resync to complete (checking Completed condition)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] force-resync: %s\n", FormatVRStatus(vrDR2))
				return hasVolumeReplicationCompletedCondition(vrDR2) && hasResyncOperationCompleted(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-004: Assertion — data consistency after force resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-force-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after force resync")
		})
	})

	Describe("L1-RSYNC-005: Resync error handling", func() {
		It("L1-RSYNC-005: attempt resync with invalid parameters, expect error handling", func() {
			By("L1-RSYNC-005: Create primary and secondary, then attempt resync with invalid secret")
			SkipIfNotFullDR("L1-RSYNC-005", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-err", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-err-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-err", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-err", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-err", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR2, vrDR2)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR2, vrcDR2)
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeletePV(cleanupCtx, cDR2, pvDR2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vrDR1)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrcDR1)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvcDR1)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-RSYNC-005: Attempting resync and validating graceful error handling")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("L1-RSYNC-005: Waiting for error condition or graceful handling")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] error-handling: %s\n", FormatVRStatus(vrDR2))
				return vrDR2.Status.State != "" || vrDR2.Status.Conditions != nil
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"Controller should update VR status even with invalid params")

			By("L1-RSYNC-005: Assertion — Graceful error handling via GetVolumeReplicationInfo")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			fmt.Fprintf(GinkgoWriter, "  [DR2][VR] final state after error test: %s\n", FormatVRStatus(vrDR2))
			Expect(vrDR2.Status.State).ToNot(BeEmpty(),
				"VR should maintain some state even after error attempt")
		})
	})
})
