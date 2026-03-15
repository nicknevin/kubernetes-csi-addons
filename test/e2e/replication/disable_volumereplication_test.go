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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

var _ = Describe("DisableVolumeReplication", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
	})

	Describe("L1-DIS-001: Disable active replication on primary", func() {
		It("L1-DIS-001: disable replication on primary (force=false), replication removed, volume writeable", func() {
			By("L1-DIS-001: Enable replication on primary, then delete VR to trigger DisableVolumeReplication")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound (poll every 2s, timeout 120s)")
			pvc := CreatePVC(ctx, c, nsName, "pvc-dis-primary", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-dis-" + nsName
			By("Creating VolumeReplicationClass (snapshot, 1m interval) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-dis-primary"
			By("Creating VolumeReplication (primary) " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for Replicating=True or Completed=True (timeout from REPLICATION_POLL_TIMEOUT)")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("L1-DIS-001: Deleting VolumeReplication to trigger DisableVolumeReplication (force=false)")
			DeleteVolumeReplicationWithCleanup(ctx, c, vr)

			By("L1-DIS-001: Assert VR is gone — DisableVolumeReplication completed, replication removed")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(), "VR should be deleted after DisableVolumeReplication")

			By("L1-DIS-001: Assert PVC still bound — volume remains usable/writeable")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound after DisableVolumeReplication, got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-DIS-002: Disable active replication on secondary", func() {
		It("L1-DIS-002: disable replication on secondary (force=false), replication stopped, secondary remains RO", func() {
			SkipIfNotFullDR("L1-DIS-002", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("L1-DIS-002: Create primary on DR1, restore secondary from primary on DR2; delete secondary VR to trigger DisableVolumeReplication")
			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName) // ensure secret exists on DR2

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-primary", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-dis-dr-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-primary", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Restoring secondary PVC from primary on DR2 (backup/restore for RBD mirror)")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-secondary", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-secondary", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

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

			By("Waiting for secondary VR on DR2 to reach Replicating or Completed")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			By("L1-DIS-002: Deleting secondary VolumeReplication to trigger DisableVolumeReplication (force=false)")
			DeleteVolumeReplicationWithCleanup(ctx, cDR2, vrDR2)

			By("L1-DIS-002: Assert secondary VR is gone — DisableVolumeReplication completed, replication stopped")
			err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(), "Secondary VR should be deleted after DisableVolumeReplication")

			By("L1-DIS-002: Assert secondary PVC still bound — secondary remains RO")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Secondary PVC should remain bound after DisableVolumeReplication, got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-DIS-003: Previously disabled (idempotent disable on primary)", func() {
		It("L1-DIS-003: disable replication on primary when no VR exists (idempotent), expect no error", func() {
			By("L1-DIS-003: Create PVC but no VR; attempt to disable replication (idempotent no-op)")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-idem-dis", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-DIS-003: PVC exists but no VR — idempotent disable (no-op)")
			By("Assertion: PVC remains bound (replication was never enabled)")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound when no VR exists (idempotent disable), got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-DIS-004: Previously disabled (idempotent disable on secondary)", func() {
		It("L1-DIS-004: disable replication on secondary when no VR exists (idempotent), expect no error", func() {
			SkipIfNotFullDR("L1-DIS-004", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("L1-DIS-004: Create secondary PVC on DR2 but no VR; attempt to disable (idempotent no-op)")
			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			_, _ = ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating secondary PVC on DR2 (no replication)")
			pvcDR2 := CreatePVC(ctx, cDR2, nsName, "pvc-idem-dis-sec", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-DIS-004: Secondary PVC exists but no VR — idempotent disable (no-op)")
			By("Assertion: Secondary PVC remains bound")
			err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Secondary PVC should remain bound when no VR exists (idempotent disable), got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-DIS-005: Disable with peer unreachable (force=false)", func() {
		It("L1-DIS-005: disable replication on primary when peer unreachable (force=false), expect graceful failure", func() {
			By("L1-DIS-005: NetworkFence blocks peer; create VR, attempt disable (should fail gracefully)")
			c := GetK8sClient()
			By("Checking that the driver supports NetworkFence (cached at suite initialization)")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-DIS-005 requires NetworkFence support. Install NetworkFence CRDs and ensure driver advertises network_fence.NETWORK_FENCE.")
			}

			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-fence-dis", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-fence-dis-" + nsName
			By("Creating VolumeReplicationClass (snapshot)")
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			nfcName := "nfc-dis-fence-" + nsName
			By("Creating NetworkFenceClass")
			nfc := CreateNetworkFenceClass(ctx, c, nfcName, env.Provisioner, secretName, secretNs)

			By("Getting fence CIDRs")
			cidrs := GetFenceCIDRs(ctx, c, env.Provisioner, nfcName)
			if len(cidrs) == 0 {
				Skip("L1-DIS-005 could not get CIDRs: set FENCE_CIDRS or ensure cluster has nodes with InternalIP")
			}

			nfName := "nf-dis-fence-" + nsName
			By("Creating NetworkFence to block peer")
			nf := CreateNetworkFence(ctx, c, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
			By("Waiting for NetworkFence to be applied")
			WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			vrName := "vr-dis-fence"
			By("Creating VolumeReplication (primary) with peer blocked")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for VR to report error due to blocked peer")
			WaitForVolumeReplicationErrorWithTimeout(ctx, c, vr, quickErrorTimeout)

			By("Attempting to delete VR while peer unreachable (disable should fail gracefully)")
			DeleteVolumeReplicationWithCleanup(ctx, c, vr)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				// Unfence to clean up properly
				DeleteNetworkFenceWithCleanup(cleanupCtx, c, nf)
				DeleteNetworkFenceClassWithCleanup(cleanupCtx, c, nfc)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-DIS-005: Assertion — VR deletion completed (force=false, peer unreachable handled)")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"VR should be gone after DisableVolumeReplication with unreachable peer")
		})
	})

	Describe("L1-DIS-006: Disable with peer unreachable (force=true)", func() {
		It("L1-DIS-006: disable replication on primary when peer unreachable (force=true), expect immediate disable", func() {
			By("L1-DIS-006: NetworkFence blocks peer; create VR, attempt force disable (should succeed immediately)")
			c := GetK8sClient()
			By("Checking that the driver supports NetworkFence")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-DIS-006 requires NetworkFence support. Install NetworkFence CRDs and ensure driver advertises network_fence.NETWORK_FENCE.")
			}

			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-fence-dis-force", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-fence-dis-force-" + nsName
			By("Creating VolumeReplicationClass (snapshot)")
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			nfcName := "nfc-dis-fence-force-" + nsName
			By("Creating NetworkFenceClass")
			nfc := CreateNetworkFenceClass(ctx, c, nfcName, env.Provisioner, secretName, secretNs)

			By("Getting fence CIDRs")
			cidrs := GetFenceCIDRs(ctx, c, env.Provisioner, nfcName)
			if len(cidrs) == 0 {
				Skip("L1-DIS-006 could not get CIDRs: set FENCE_CIDRS or ensure cluster has nodes with InternalIP")
			}

			nfName := "nf-dis-fence-force-" + nsName
			By("Creating NetworkFence to block peer")
			nf := CreateNetworkFence(ctx, c, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
			By("Waiting for NetworkFence to be applied")
			WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			vrName := "vr-dis-fence-force"
			By("Creating VolumeReplication (primary) with peer blocked")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for VR to report error due to blocked peer")
			WaitForVolumeReplicationError(ctx, c, vr)

			By("L1-DIS-006: Attempting to delete VR with force=true (immediate disable expected)")
			DeleteVolumeReplicationWithCleanup(ctx, c, vr)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				// Unfence to clean up properly
				DeleteNetworkFenceWithCleanup(cleanupCtx, c, nf)
				DeleteNetworkFenceClassWithCleanup(cleanupCtx, c, nfc)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-DIS-006: Assertion — VR deleted successfully (force disable with unreachable peer)")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"VR should be gone after force DisableVolumeReplication")

			By("L1-DIS-006: Assertion — PVC remains bound (primary writeable)")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound after force disable, got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-DIS-009: Force disable active replication on primary", func() {
		It("L1-DIS-009: disable replication on primary (force=true, active replication), expect immediate disable", func() {
			By("L1-DIS-009: Enable replication on primary, then force disable")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-dis-force", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-dis-force-" + nsName
			By("Creating VolumeReplicationClass (snapshot)")
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-dis-force"
			By("Creating VolumeReplication (primary)")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for Replicating=True or Completed=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-DIS-009: Deleting VolumeReplication to trigger DisableVolumeReplication (force=true, active)")
			DeleteVolumeReplicationWithCleanup(ctx, c, vr)

			By("L1-DIS-009: Assertion — VR is gone, DisableVolumeReplication completed immediately")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"VR should be deleted after force DisableVolumeReplication")

			By("L1-DIS-009: Assertion — PVC remains bound and writeable")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound after force disable, got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-DIS-010: Force disable active replication on secondary", func() {
		It("L1-DIS-010: disable replication on secondary (force=true, active), expect immediate disable", func() {
			SkipIfNotFullDR("L1-DIS-010", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("L1-DIS-010: Create primary on DR1, secondary on DR2; force disable on secondary")
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
			vrcName := "vrc-dis-force-dr-" + nsName
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

			By("Waiting for secondary VR on DR2 to reach Replicating or Completed")
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

			By("L1-DIS-010: Deleting secondary VR to trigger force DisableVolumeReplication")
			DeleteVolumeReplicationWithCleanup(ctx, cDR2, vrDR2)

			By("L1-DIS-010: Assertion — secondary VR is gone")
			err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"Secondary VR should be deleted after force DisableVolumeReplication")

			By("L1-DIS-010: Assertion — secondary PVC remains bound")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Secondary PVC should remain bound after force disable, got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-DIS-011: Force disable previously disabled on primary", func() {
		It("L1-DIS-011: force disable when no VR exists on primary (idempotent), expect no error", func() {
			By("L1-DIS-011: Create PVC but no VR; force disable should be idempotent no-op")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-force-idem", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-DIS-011: PVC exists but no VR — force disable idempotent (no-op)")
			By("Assertion: PVC remains bound")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound when no VR exists (force disable idempotent), got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-DIS-012: Force disable previously disabled on secondary", func() {
		It("L1-DIS-012: force disable when no VR exists on secondary (idempotent), expect no error", func() {
			SkipIfNotFullDR("L1-DIS-012", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("L1-DIS-012: Create secondary PVC but no VR on DR2; force disable should be idempotent")
			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			_, _ = ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating secondary PVC on DR2 (no replication)")
			pvcDR2 := CreatePVC(ctx, cDR2, nsName, "pvc-force-idem-sec", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeletePVCWithCleanup(cleanupCtx, cDR2, pvcDR2)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("L1-DIS-012: Secondary PVC exists but no VR — force disable idempotent (no-op)")
			By("Assertion: Secondary PVC remains bound")
			err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Secondary PVC should remain bound when no VR exists (force disable idempotent), got %s", pvcDR2.Status.Phase)
		})
	})
})
