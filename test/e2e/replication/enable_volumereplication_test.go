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
	"sigs.k8s.io/controller-runtime/pkg/client"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

var _ = Describe("EnableVolumeReplication", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
	})

	Describe("L1-E-001: Enable snapshot mode", func() {
		It("L1-E-001 + L1-INFO-001: enable snapshot mode then get replication info (2 test cases)", func() {
			By("Test case 1: EnableVolumeReplication (L1-E-001) — enable snapshot mode")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound (poll every 2s, timeout 120s)")
			pvc := CreatePVC(ctx, c, nsName, "pvc-rep", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-snapshot-" + nsName
			By("Creating VolumeReplicationClass (snapshot, 1m interval) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-snapshot"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
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

			By("Assertions: EnableVolumeReplication (L1-E-001) — VR state after enable")
			Expect(vr.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"EnableVolumeReplication: VR state must be Primary or Unknown after successful enable, got %q", vr.Status.State)

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-001) — replication info present")
			Expect(vr.Status.Conditions).NotTo(BeEmpty(),
				"GetVolumeReplicationInfo: VR status conditions must be set for healthy replication (conditions: %v)", vr.Status.Conditions)
		})
	})

	Describe("L1-E-002: Enable journal mode", func() {
		It("L1-E-002 + L1-INFO-001: enable journal mode then get replication info (2 test cases)", func() {
			By("Test case 1: EnableVolumeReplication (L1-E-002) — enable journal mode")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound (poll every 2s, timeout 120s)")
			pvc := CreatePVC(ctx, c, nsName, "pvc-journal", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-journal-" + nsName
			By("Creating VolumeReplicationClass (journal mode) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeJournal)

			vrName := "vr-journal"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for Replicating=True or Completed=True (journal may take longer than snapshot)")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: EnableVolumeReplication (L1-E-002) — VR state after enable")
			Expect(vr.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"EnableVolumeReplication: VR state must be Primary or Unknown after successful enable, got %q", vr.Status.State)

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-001) — replication info present")
			Expect(vr.Status.Conditions).NotTo(BeEmpty(),
				"GetVolumeReplicationInfo: VR status conditions must be set for healthy replication (conditions: %v)", vr.Status.Conditions)
		})
	})

	Describe("L1-E-003: Peer unreachable (NetworkFence)", func() {
		It("L1-E-003 + L1-INFO-005: fence node → EnableVolumeReplication fails and GetVolumeReplicationInfo shows error; unfence → EnableVolumeReplication succeeds and GetVolumeReplicationInfo shows healthy", func() {
			By("Test case 1: Block storage node via NetworkFence; create VR and expect EnableVolumeReplication to fail; assert GetVolumeReplicationInfo (L1-INFO-005) shows error")
			c := GetK8sClient()
			By("Checking that the driver supports NetworkFence (cached at suite initialization)")
			if !IsNetworkFenceSupportAvailable() {
				Skip("L1-E-003 requires NetworkFence and NetworkFenceClass CRDs to be installed and the CSI driver to advertise network_fence.NETWORK_FENCE in CSIAddonsNode status.capabilities. Install the CRDs and ensure the driver supports NetworkFence.")
			}
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)
			RegisterTestNamespace(ns.Name)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-fence", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-fence-" + nsName
			By("Creating VolumeReplicationClass (snapshot) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			nfcName := "nfc-fence-" + nsName
			By("Creating NetworkFenceClass " + nfcName + " (same provisioner and secret as VRC)")
			nfc := CreateNetworkFenceClass(ctx, c, nfcName, env.Provisioner, secretName, secretNs)

			By("Getting fence CIDRs (from FENCE_CIDRS env, CSIAddonsNode status, or node InternalIPs)")
			cidrs := GetFenceCIDRs(ctx, c, env.Provisioner, nfcName)
			if len(cidrs) == 0 {
				Skip("L1-E-003 could not get CIDRs: set FENCE_CIDRS (comma-separated, e.g. FENCE_CIDRS=192.168.122.164/32) or ensure cluster has nodes with InternalIP")
			}

			nfName := "nf-fence-" + nsName
			By("Creating NetworkFence (Fenced) to block node access " + nfName)
			nf := CreateNetworkFence(ctx, c, nfName, nfcName, cidrs, csiaddonsv1alpha1.Fenced)
			By("Waiting for NetworkFence to report Succeeded")
			WaitForNetworkFenceResult(ctx, c, nf, csiaddonsv1alpha1.FencingOperationResultSucceeded)

			vrName := "vr-fence"
			By("Creating VolumeReplication " + vrName + " while node is fenced (EnableVolumeReplication should fail)")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				// Unfence first so RBD mirroring can recover before VR/PVC cleanup.
				// Order matters: leaving fence active during VR delete leaves cluster in bad state.
				DeleteNetworkFenceWithCleanup(cleanupCtx, c, nf)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteNetworkFenceClassWithCleanup(cleanupCtx, c, nfc)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for VR to report error (peer unreachable)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-005) — peer unreachable returns error in VR status")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"GetVolumeReplicationInfo (L1-INFO-005): VR with fenced/peer unreachable must have error (message or degraded condition)")

			By("Unfencing by setting fenceState to Unfenced")
			UnfenceNetworkFence(ctx, c, nf)
			// Cleanup will delete NF (already unfenced); NFC deleted after

			By("Waiting for controller to retry and EnableVolumeReplication to succeed")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: EnableVolumeReplication (L1-E-003) — VR state after unfence and successful enable")
			Expect(vr.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"EnableVolumeReplication: VR state must be Primary or Unknown after unfence and successful enable, got %q", vr.Status.State)

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-001) — replication info present after unfence")
			Expect(vr.Status.Conditions).NotTo(BeEmpty(),
				"GetVolumeReplicationInfo: VR status conditions must be set for healthy replication after unfence (conditions: %v)", vr.Status.Conditions)
		})
	})

	Describe("L1-E-005: Idempotent enable", func() {
		It("L1-E-005 + L1-INFO-001: idempotent enable then get replication info (2 test cases)", func() {
			By("Test case 1: EnableVolumeReplication (L1-E-005) — idempotent enable; GetVolumeReplicationInfo (L1-INFO-001) on first VR")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound (poll every 2s, timeout 120s)")
			pvc := CreatePVC(ctx, c, nsName, "pvc-idem", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-idem-" + nsName
			By("Creating VolumeReplicationClass (snapshot, 1m interval) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-idem"
			By("Creating first VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for first VR Replicating=True or Completed=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: EnableVolumeReplication (L1-E-005) — first VR state after enable")
			Expect(vr.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"EnableVolumeReplication: VR state must be Primary or Unknown after successful enable, got %q", vr.Status.State)

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-001) — replication info present on first VR")
			Expect(vr.Status.Conditions).NotTo(BeEmpty(),
				"GetVolumeReplicationInfo: VR status conditions must be set for healthy replication (conditions: %v)", vr.Status.Conditions)

			// Test case 2: Create a second VR for the same PVC (same volume) - controller should treat as idempotent / no error
			vr2Name := "vr-idem-second"
			By("Creating second VolumeReplication " + vr2Name + " for same PVC")
			vr2 := CreateVolumeReplication(ctx, c, nsName, vr2Name, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr2)
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			// Second VR: many controllers never set status on a duplicate VR (idempotent no-op). Wait up to 20s for success.
			// If controller processes it, should reach Completed=True within seconds; if not, controller treats as idempotent no-op.
			By("Waiting for second VR Replicating=True or Completed=True (up to 20s); if none, require no error (idempotent)")
			gotSuccess := WaitForVolumeReplicationReplicatingOrCompletedUntil(ctx, c, vr2, 20*time.Second, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vr2Name}, vr2)
			Expect(err).NotTo(HaveOccurred())
			if gotSuccess {
				By("Assertions: EnableVolumeReplication (L1-E-005) — second VR state (idempotent success)")
				Expect(vr2.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
					"EnableVolumeReplication: second VR state must be Primary or Unknown when idempotent success, got %q", vr2.Status.State)
			} else {
				By("Second VR did not get success condition; asserting no error (idempotent no-op)")
				Expect(hasVolumeReplicationErrorCondition(vr2)).To(BeFalse(),
					"EnableVolumeReplication: second VR should have no error when controller does not set status (idempotent no-op)")
			}
		})
	})

	Describe("L1-E-004: Invalid schedulingInterval parameter", func() {
		It("L1-E-004 + L1-INFO-012: invalid schedulingInterval returns error; GetVolumeReplicationInfo returns error state", func() {
			By("Starting L1-E-004: Invalid schedulingInterval parameter")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-invalid-interval", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-invalid-interval-" + nsName
			By("Creating VolumeReplicationClass with invalid schedulingInterval=5x")
			vrc := CreateVolumeReplicationClassWithParams(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot, map[string]string{
				"schedulingInterval": "5x",
			})

			vrName := "vr-invalid-interval"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for error in VR status (gRPC InvalidArgument or driver error)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: L1-E-004 — invalid schedulingInterval returns error; L1-INFO-012 — GetVolumeReplicationInfo returns error state")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"L1-E-004/L1-INFO-012: VR with invalid schedulingInterval must report error (message: %q)", vr.Status.Message)
		})
	})

	Describe("L1-E-006: Secret reference missing/invalid", func() {
		It("L1-E-006 + L1-INFO-013: missing/invalid secret returns error; GetVolumeReplicationInfo returns error state", func() {
			By("Starting L1-E-006: Secret reference missing/invalid")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-bad-secret", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			// Use non-existent secret (not created in namespace)
			secretName := "nonexistent-replication-secret"
			secretNs := nsName
			vrcName := "vrc-bad-secret-" + nsName
			By("Creating VolumeReplicationClass with non-existent secret")

			vrc := CreateVolumeReplicationClassWithParams(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot, nil)

			vrName := "vr-bad-secret"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for error in VR status (FailedPrecondition or controller failed to get secret)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: L1-E-006 — missing/invalid secret returns error; L1-INFO-013 — GetVolumeReplicationInfo returns error state")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"L1-E-006/L1-INFO-013: VR with non-existent secret must report error (message: %q)", vr.Status.Message)
		})
	})

	Describe("L1-E-007: Invalid mirroringMode parameter", func() {
		It("L1-E-007 + L1-INFO-011: invalid mirroringMode returns error; GetVolumeReplicationInfo returns error state", func() {
			By("Starting L1-E-007: Invalid mirroringMode parameter")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-invalid-mode", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-invalid-mode-" + nsName
			By("Creating VolumeReplicationClass with invalid mirroringMode=invalid")
			vrc := CreateVolumeReplicationClassWithParams(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot, map[string]string{
				"mirroringMode": "invalid",
			})

			vrName := "vr-invalid-mode"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for error in VR status (gRPC InvalidArgument)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: L1-E-007 — invalid mirroringMode returns error; L1-INFO-011 — GetVolumeReplicationInfo returns error state")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"L1-E-007/L1-INFO-011: VR with invalid mirroringMode must report error (message: %q)", vr.Status.Message)
		})
	})

	Describe("L1-E-008: Future schedulingStartTime", func() {
		It("L1-E-008 + L1-INFO-001: future schedulingStartTime enables replication; GetVolumeReplicationInfo returns replication info", func() {
			By("Starting L1-E-008: Future schedulingStartTime")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-future-start", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			// schedulingStartTime in RFC3339 format, 30 seconds in the future
			futureTime := time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)
			vrcName := "vrc-future-start-" + nsName
			By("Creating VolumeReplicationClass with schedulingStartTime=" + futureTime)
			vrc := CreateVolumeReplicationClassWithParams(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot, map[string]string{
				"schedulingStartTime": futureTime,
			})

			vrName := "vr-future-start"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for Replicating=True or Completed=True (driver may ignore schedulingStartTime if unsupported)")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
			})
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: L1-E-008 — VR enabled with future schedulingStartTime; L1-INFO-001 — GetVolumeReplicationInfo returns replication info")
			Expect(vr.Status.State).To(Or(Equal(replicationv1alpha1.PrimaryState), Equal(replicationv1alpha1.UnknownState)),
				"L1-E-008: VR state must be Primary or Unknown after enable with future schedulingStartTime, got %q", vr.Status.State)
			Expect(hasReplicationSuccessCondition(vr)).To(BeTrue(),
				"L1-E-008: VR must have Replicating or Completed condition")
			Expect(vr.Status.Conditions).NotTo(BeEmpty(),
				"L1-INFO-001: GetVolumeReplicationInfo — VR status conditions must be set for healthy replication (conditions: %v)", vr.Status.Conditions)
		})
	})

	Describe("L1-E-009: Invalid schedulingStartTime format", func() {
		It("L1-E-009 + L1-INFO-014: invalid schedulingStartTime format returns error; GetVolumeReplicationInfo returns error state", func() {
			By("Starting L1-E-009: Invalid schedulingStartTime format")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-invalid-time", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-invalid-time-" + nsName
			By("Creating VolumeReplicationClass with invalid schedulingStartTime=invalid")
			vrc := CreateVolumeReplicationClassWithParams(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot, map[string]string{
				"schedulingStartTime": "invalid",
			})

			vrName := "vr-invalid-time"
			By("Creating VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for error in VR status (gRPC InvalidArgument)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: L1-E-009 — invalid schedulingStartTime format returns error; L1-INFO-014 — GetVolumeReplicationInfo returns error state")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"L1-E-009/L1-INFO-014: VR with invalid schedulingStartTime format must report error (message: %q)", vr.Status.Message)
		})
	})
})
