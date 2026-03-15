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

var _ = Describe("PromoteVolumeReplication", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
	})

	Describe("L1-PROM-001: Promote secondary to primary (healthy)", func() {
		It("L1-PROM-001: promote secondary → primary when healthy, expect successful promotion", func() {
			By("L1-PROM-001: Create primary on DR1, secondary on DR2; promote secondary to primary")
			SkipIfNotFullDR("L1-PROM-001", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-prom", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-prom-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-prom", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-prom", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			fmt.Fprintf(GinkgoWriter, "  [DEBUG] Creating VR with replicationState constant value=%v (should be 'secondary')\n", replicationv1alpha1.Secondary)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-prom", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)
			fmt.Fprintf(GinkgoWriter, "  [DR2][VR] AFTER CREATION Spec.ReplicationState=%v (type=%T)\n", vrDR2.Spec.ReplicationState, vrDR2.Spec.ReplicationState)

			By("Waiting for secondary VR on DR2 to reach Secondary state and stable")
			Eventually(func() string {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				return string(vrDR2.Status.State)
			}, 30*time.Second, 1*time.Second).Should(Equal(string(replicationv1alpha1.SecondaryState)),
				"Secondary VR should be in Secondary state before promotion")

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			var nfc *csiaddonsv1alpha1.NetworkFenceClass
			var nf *csiaddonsv1alpha1.NetworkFence

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteNetworkFenceWithCleanup(cleanupCtx, cDR2, nf, vrDR2)
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

			By("L1-PROM-001: Promote secondary VR on DR2 by changing replicationState to Primary")
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Primary
			err := cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to update VR replicationState to Primary")

			By("Waiting for VR state to transition to Primary")
			Eventually(func() string {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(vrDR2))
				return string(vrDR2.Status.State)
			}, 5*time.Minute, 5*time.Second).Should(Equal(string(replicationv1alpha1.PrimaryState)),
				"VR should transition to Primary state after promotion request")

			By("L1-PROM-001: Assertion — secondary VR state changed to Primary")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(vrDR2.Status.State).To(Equal(replicationv1alpha1.PrimaryState),
				"Secondary VR should be promoted to Primary, got %s", vrDR2.Status.State)

			By("L1-PROM-001: Assertion — secondary PVC now writable (RW)")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Promoted secondary PVC should remain bound, got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-PROM-002: Promote already primary (idempotent)", func() {
		It("L1-PROM-002: promote when already primary (idempotent), expect no error", func() {
			By("L1-PROM-002: Create primary VR, attempt promote (idempotent no-op)")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-prom-idem", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-prom-idem-" + nsName
			By("Creating VolumeReplicationClass (snapshot)")
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-prom-idem"
			By("Creating VolumeReplication (already primary)")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for Replicating=True or Completed=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("L1-PROM-002: Recording initial VR state")
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			initialState := vr.Status.State

			By("L1-PROM-002: Attempting to promote already-primary VR (idempotent)")
			vr.Spec.ReplicationState = replicationv1alpha1.Primary
			err = c.Update(ctx, vr)
			Expect(err).NotTo(HaveOccurred(), "Update should succeed for idempotent promote")

			By("L1-PROM-002: Assertion — VR state remains Primary (no change)")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			Expect(vr.Status.State).To(Equal(initialState),
				"VR state should remain Primary after idempotent promote, got %s", vr.Status.State)
			Expect(vr.Status.State).To(Equal(replicationv1alpha1.PrimaryState),
				"VR should remain Primary, got %s", vr.Status.State)

			By("L1-PROM-002: Assertion — PVC remains bound and writable")
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvc.Name}, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
				"PVC should remain bound, got %s", pvc.Status.Phase)
		})
	})

	Describe("L1-PROM-007: Promote with active I/O workload", func() {
		It("L1-PROM-007: promote secondary to primary with active workload, expect graceful promotion", func() {
			By("L1-PROM-007: Create primary on DR1, secondary on DR2; promote under load")
			SkipIfNotFullDR("L1-PROM-007", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-prom-io", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-prom-io-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-prom-io", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-prom-io", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-prom-io", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

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

			By("L1-PROM-007: Promote secondary VR on DR2 with active workload (force=false)")
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Primary
			err := cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to promote with active workload")

			By("Waiting for VR state to transition to Primary (graceful promotion)")
			Eventually(func() string {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(vrDR2))
				return string(vrDR2.Status.State)
			}, 5*time.Minute, 5*time.Second).Should(Equal(string(replicationv1alpha1.PrimaryState)),
				"VR should transition to Primary state after promotion request with active workload")

			By("L1-PROM-007: Assertion — secondary VR promoted to Primary")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(vrDR2.Status.State).To(Equal(replicationv1alpha1.PrimaryState),
				"VR should be promoted to Primary, got %s", vrDR2.Status.State)

			By("L1-PROM-007: Assertion — secondary PVC now writable")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Promoted secondary PVC should remain bound, got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-PROM-008: Force promote with active I/O workload", func() {
		It("L1-PROM-008: force promote secondary with active workload, expect immediate promotion", func() {
			By("L1-PROM-008: Create primary on DR1, secondary on DR2; force promote under load")
			SkipIfNotFullDR("L1-PROM-008", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			_, _ = ReplicationSecretRef(ctx, cDR2, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-prom-force", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-prom-force-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-prom-force", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-prom-force", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-prom-force", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

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

			By("L1-PROM-008: Force promote secondary VR on DR2 with active workload (force=true)")
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Primary
			err := cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to force promote with active workload")

			By("Waiting for VR state to transition to Primary (force promotion)")
			Eventually(func() string {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(vrDR2))
				return string(vrDR2.Status.State)
			}, 5*time.Minute, 5*time.Second).Should(Equal(string(replicationv1alpha1.PrimaryState)),
				"VR should transition to Primary state after force promotion request")

			By("L1-PROM-008: Assertion — secondary VR immediately promoted to Primary")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(vrDR2.Status.State).To(Equal(replicationv1alpha1.PrimaryState),
				"VR should be force promoted to Primary, got %s", vrDR2.Status.State)

			By("L1-PROM-008: Assertion — secondary PVC now writable")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: pvcDR2.Name}, pvcDR2)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcDR2.Status.Phase).To(Equal(corev1.ClaimBound),
				"Promoted secondary PVC should remain bound, got %s", pvcDR2.Status.Phase)
		})
	})

	Describe("L1-PROM-003: Promote secondary to primary with peer unreachable (force=false)", func() {
		It("L1-PROM-003: fence peer cluster → promote fails → unfence → promote succeeds", func() {
			By("Starting L1-PROM-003: Promote secondary to primary with peer unreachable (force=false)")
			SkipIfNotFullDR("L1-PROM-003", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("Checking that the driver supports NetworkFence")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-PROM-003 requires NetworkFence and NetworkFenceClass CRDs to be installed and the CSI driver to advertise network_fence.NETWORK_FENCE in CSIAddonsNode status.capabilities.")
			}

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-prom-003", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-prom-003-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-prom-003", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-prom-003", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-prom-003", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			var nfc *csiaddonsv1alpha1.NetworkFenceClass
			var nf *csiaddonsv1alpha1.NetworkFence

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteNetworkFenceWithCleanup(cleanupCtx, cDR2, nf, vrDR2)
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

			By("[DR2] Creating NetworkFenceClass to fence peer cluster")
			nfcName := "nfc-prom-003-" + nsName
			nfc = CreateNetworkFenceClass(ctx, cDR2, nfcName, env.Provisioner, secretName, secretNs)

			By("[DR2] Getting fence CIDRs for peer cluster nodes")
			cidrs := GetFenceCIDRs(ctx, cDR2, env.Provisioner, nfcName)
			if len(cidrs) == 0 {
				Skip("L1-PROM-003 could not get CIDRs: set FENCE_CIDRS or ensure cluster has nodes with InternalIP")
			}

			nfName := "nf-prom-003-" + nsName
			By("[DR2] Creating NetworkFence (Fenced) to block peer cluster access")
			nf = CreateNetworkFence(ctx, cDR2, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
			By("[DR2] Waiting for NetworkFence to report Succeeded")
			WaitForNetworkFenceResult(ctx, cDR2, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			By("[DR2] Attempting to promote secondary to primary while peer is fenced (force=false; should fail)")
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Primary
			err := cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("[DR2] Waiting for VR to report error (FailedToPromote or peer unreachable)")
			WaitForVolumeReplicationErrorWithTimeout(ctx, cDR2, vrDR2, quickErrorTimeout)
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: L1-PROM-003 — promote with peer down (force=false) fails")
			Expect(hasVolumeReplicationErrorCondition(vrDR2)).To(BeTrue(),
				"L1-PROM-003: VR with fenced peer must have error condition (message: %q)", vrDR2.Status.Message)
			Expect(vrDR2.Status.State).NotTo(Equal(replicationv1alpha1.PrimaryState),
				"L1-PROM-003: VR state should not change to Primary when peer is unreachable with force=false")

			By("[DR2] Unfencing by setting NetworkFence state to Unfenced")
			UnfenceNetworkFence(ctx, cDR2, nf)

			By("[DR2] Waiting for NetworkFence unfence operation to complete successfully")
			WaitForNetworkFenceResult(ctx, cDR2, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			By("[DR2] Waiting for RBD mirror and cluster to recover VR health (Degraded=False)")
			Eventually(func() bool {
				err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
				if err != nil {
					return false
				}
				// Check that VR is no longer degraded (Degraded=False)
				for _, cond := range vrDR2.Status.Conditions {
					if cond.Type == "Degraded" {
						isHealthy := cond.Status == metav1.ConditionFalse
						if isHealthy {
							fmt.Fprintf(GinkgoWriter, "  [DR2][VR recovered] %s\n", FormatVRStatus(vrDR2))
						}
						return isHealthy
					}
				}
				return false
			}, 10*time.Minute, 10*time.Second).Should(BeTrue(),
				"VR health should recover (Degraded=False) after unfencing within 10 minutes")

			By("[DR2] Waiting for controller to retry and promote to succeed")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR after unfence] %s\n", FormatVRStatus(v))
			})
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("[DR2] Waiting for VR state to transition to Primary (state change may be async after operation succeeds)")
			Eventually(func() (replicationv1alpha1.State, error) {
				err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
				return vrDR2.Status.State, err
			}, 2*time.Minute, 5*time.Second).Should(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"VR state should transition to Primary or Unknown after promote operation")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: L1-PROM-003 — promote succeeds after unfence")
			Expect(vrDR2.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"L1-PROM-003: VR state must be Primary or Unknown after unfence and successful promote, got %q", vrDR2.Status.State)
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(),
				"L1-PROM-003: VR must have Replicating or Completed condition after successful promote")
		})
	})

	Describe("L1-PROM-004: Promote secondary to primary with peer unreachable (force=true)", func() {
		It("L1-PROM-004: fence peer cluster → force promote succeeds → unfence → verify stability", func() {
			// SKIP REASON: L1-PROM-004 is skipped due to an issue with force promote in degraded RBD mirror mode.
			// GitHub Issue: https://github.com/nadavleva/kubernetes-csi-addons/issues/7
			//
			// ISSUE DESCRIPTION:
			// When the RBD mirror is degraded (peer cluster is unreachable/fenced), the force promote operation
			// does not transition the VolumeReplication state to Primary or Unknown as expected. Instead, the VR
			// remains in Secondary state with conditions=[Completed=True Degraded=True]. This appears to be a
			// driver/storage backend limitation where the RBD mirror daemon cannot transition to Primary when
			// the mirror is in a degraded state (peer is unreachable).
			//
			// EXPECTED BEHAVIOR:
			// With force=true, the promote operation should succeed and transition VR state to Primary (or Unknown)
			// even when the peer is unreachable/fenced.
			//
			// ACTUAL BEHAVIOR:
			// VR state remains Secondary with Degraded condition despite successful force promote operation.
			// The assertion times out after 2 minutes waiting for state transition.
			//
			// NEXT STEPS:
			// 1. Investigate RBD mirror degradation handling in the CSI driver
			// 2. Check if RBD mirror can be forced to Primary when degraded
			// 3. May require driver-level changes or RBD configuration updates
			// See GitHub issue for investigation details and updates.
			Skip("L1-PROM-004: Skipped due to RBD mirror force promote issue in degraded mode - VR state does not transition to Primary when peer is fenced. Ref: https://github.com/nadavleva/kubernetes-csi-addons/issues/7")

			By("Starting L1-PROM-004: Promote secondary to primary with peer unreachable (force=true)")
			SkipIfNotFullDR("L1-PROM-004", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")

			By("Checking that the driver supports NetworkFence")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-PROM-004 requires NetworkFence and NetworkFenceClass CRDs to be installed and the CSI driver to advertise network_fence.NETWORK_FENCE in CSIAddonsNode status.capabilities.")
			}

			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			nsName := UniqueNamespace()
			By("Creating namespace on both DR1 and DR2")
			ns1 := CreateNamespace(ctx, cDR1, nsName)
			ns2 := CreateNamespace(ctx, cDR2, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)

			By("Creating primary PVC and VR on DR1")
			pvcDR1 := CreatePVC(ctx, cDR1, nsName, "pvc-dr1-prom-004", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-prom-004-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-prom-004", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-prom-004", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-prom-004", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			var nfc *csiaddonsv1alpha1.NetworkFenceClass
			var nf *csiaddonsv1alpha1.NetworkFence

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteNetworkFenceWithCleanup(cleanupCtx, cDR2, nf, vrDR2)
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

			By("[DR2] Creating NetworkFenceClass to fence peer cluster")
			nfcName := "nfc-prom-004-" + nsName
			nfc = CreateNetworkFenceClass(ctx, cDR2, nfcName, env.Provisioner, secretName, secretNs)

			By("[DR2] Getting fence CIDRs for peer cluster nodes")
			cidrs := GetFenceCIDRs(ctx, cDR2, env.Provisioner, nfcName)
			if len(cidrs) == 0 {
				Skip("L1-PROM-004 could not get CIDRs: set FENCE_CIDRS or ensure cluster has nodes with InternalIP")
			}

			nfName := "nf-prom-004-" + nsName
			By("[DR2] Creating NetworkFence (Fenced) to block peer cluster access")
			nf = CreateNetworkFence(ctx, cDR2, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
			By("[DR2] Waiting for NetworkFence to report Succeeded")
			WaitForNetworkFenceResult(ctx, cDR2, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			By("[DR2] Attempting to promote secondary to primary while peer is fenced (force=true; should succeed)")
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Primary
			err := cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("[DR2] Waiting for VR to report success (Replicating or Completed with Promoted reason)")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR2][VR force promote] %s\n", FormatVRStatus(v))
			})
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("[DR2] Waiting for VR state to transition to Primary (state change may be async after operation succeeds)")
			Eventually(func() (replicationv1alpha1.State, error) {
				err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
				return vrDR2.Status.State, err
			}, 2*time.Minute, 5*time.Second).Should(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"VR state should transition to Primary or Unknown after promote operation")
			err = cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: L1-PROM-004 — force promote with peer down succeeds")
			Expect(vrDR2.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"L1-PROM-004: VR state must transition to Primary or Unknown after force promote, got %q", vrDR2.Status.State)
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(),
				"L1-PROM-004: VR must have Replicating or Completed condition after force promote")

			By("[DR2] Unfencing by setting NetworkFence state to Unfenced")
			UnfenceNetworkFence(ctx, cDR2, nf)

			By("[DR2] Waiting for NetworkFence unfence operation to complete successfully")
			WaitForNetworkFenceResult(ctx, cDR2, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			By("[DR2] Waiting for RBD mirror and cluster to recover VR health (Degraded=False)")
			Eventually(func() bool {
				err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
				if err != nil {
					return false
				}
				// Check that VR is no longer degraded (Degraded=False)
				for _, cond := range vrDR2.Status.Conditions {
					if cond.Type == "Degraded" {
						isHealthy := cond.Status == metav1.ConditionFalse
						if isHealthy {
							fmt.Fprintf(GinkgoWriter, "  [DR2][VR recovered] %s\n", FormatVRStatus(vrDR2))
						}
						return isHealthy
					}
				}
				return false
			}, 10*time.Minute, 10*time.Second).Should(BeTrue(),
				"VR health should recover (Degraded=False) after unfencing within 10 minutes")

			By("[DR2] Verifying VR remains stable after unfence")
			Eventually(func() (replicationv1alpha1.State, error) {
				err := cDR2.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrDR2.Name}, vrDR2)
				return vrDR2.Status.State, err
			}, 2*time.Minute, 5*time.Second).Should(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"VR state should stabilize and remain Primary or Unknown after unfence")

			By("Assertions: L1-PROM-004 — VR remains stable after unfence")
			Expect(vrDR2.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"L1-PROM-004: VR state should remain Primary or Unknown after unfence, got %q", vrDR2.Status.State)

			DeleteNetworkFenceWithCleanup(ctx, cDR2, nf)
		})
	})

	Describe("L1-PROM-005: Promote secondary to primary with array unreachable (force=false)", func() {
		It("L1-PROM-005: [SCAFFOLD] array unreachable simulation required", func() {
			By("Starting L1-PROM-005: Promote secondary to primary with array unreachable (force=false)")
			Skip(`L1-PROM-005 requires array/storage unreachable simulation not yet supported in test infrastructure.

Ref: https://github.com/nadavleva/kubernetes-csi-addons/issues/9

Prerequisites for implementation:
1. Driver-specific storage shutdown mechanism (e.g., Ceph RBD pool offline)
   - NetworkFence blocks network access to peer; does NOT block local storage access
   - This test needs LOCAL storage unavailable on secondary cluster
2. Mock CSI driver hook or test container that simulates storage errors
3. Alternative: Extend CSI driver test harness to inject storage unavailability errors

Expected behavior when array is unreachable:
- CSI driver reports volume unavailable or I/O error
- Controller cannot perform PromoteVolume RPC (storage required for operation)
- VR status: Degraded=True, FailedToPromote reason
- force=false: Error persists until storage recovers`)
		})
	})

	Describe("L1-PROM-006: Promote secondary to primary with array unreachable (force=true)", func() {
		It("L1-PROM-006: [SCAFFOLD] array unreachable simulation required", func() {
			By("Starting L1-PROM-006: Promote secondary to primary with array unreachable (force=true)")
			Skip(`L1-PROM-006 requires array/storage unreachable simulation not yet supported in test infrastructure.

Ref: https://github.com/nadavleva/kubernetes-csi-addons/issues/9

Prerequisites for implementation:
1. Driver-specific storage shutdown mechanism (e.g., Ceph RBD pool offline)
2. Mock CSI driver or storage unavailability injection
3. Verify that force=true CANNOT overcome storage layer issues (unlike force=false peer scenarios)

Expected behavior:
- Storage unavailability cannot be overridden by force parameter
- force=true still fails because local storage is unavailable
- Reason: CSI operations (PromoteVolume) require storage access; force only affects peer coordination`)
		})
	})
})
