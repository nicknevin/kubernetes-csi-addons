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

	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
	"github.com/csi-addons/kubernetes-csi-addons/test/e2e/helpers"
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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

	Describe("L1-E-003: Peer unreachable (NetworkFence or iptables)", func() {
		It("L1-E-003 + L1-INFO-005: fence node → EnableVolumeReplication fails and GetVolumeReplicationInfo shows error; unfence → EnableVolumeReplication succeeds and GetVolumeReplicationInfo shows healthy", func() {
			injector := helpers.GetFaultInjectorTypeFromEnv()
			Logf("[TEST]", "L1-E-003 fault injector: %s (E2E_FAULT_INJECTOR=iptables|networkfence|none; default iptables when unset)", injector)

			if injector == helpers.FaultInjectorNone {
				Skip("L1-E-003 requires fault injection (set E2E_FAULT_INJECTOR to iptables or networkfence)")
			}

			By("Test case 1: Block storage node via fault injection; create VR and expect EnableVolumeReplication to fail; assert GetVolumeReplicationInfo (L1-INFO-005) shows error")
			c := GetK8sClient()

			switch injector {
			case helpers.FaultInjectorNetworkFence:
				if !IsNetworkFenceSupportAvailable() {
					Skip("L1-E-003 (networkfence) requires NetworkFence and NetworkFenceClass CRDs and CSI driver network_fence capability in CSIAddonsNode.")
				}
			case helpers.FaultInjectorIptables:
				if !helpers.HasPrivilegedDaemonSetSupport(ctx, c) {
					Skip("L1-E-003 (iptables) requires privileged DaemonSet support for iptables fault injection.")
				}
			}

			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)
			RegisterTestNamespace(ns.Name)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			By("Creating PVC and waiting for Bound")
			pvc := CreatePVC(ctx, c, nsName, "pvc-fence", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-fence-" + nsName
			By("Creating VolumeReplicationClass (snapshot) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			faultProvider, err := helpers.NewFaultInjectionProvider(helpers.FaultInjectionConfig{
				Type:       injector,
				Client:     c,
				RESTConfig: GetRESTConfig(),
				Namespace:  nsName,
				ProviderParams: map[string]string{
					"provisioner": env.Provisioner,
					"image":       helpers.DefaultIptablesImageWithRegistry,
				},
			})
			Expect(err).NotTo(HaveOccurred(), "NewFaultInjectionProvider")
			if !faultProvider.IsSupported(ctx) {
				Skip(fmt.Sprintf("L1-E-003 (%s): fault injection provider not supported on this cluster", injector))
			}

			var cidrs []string
			switch injector {
			case helpers.FaultInjectorNetworkFence:
				By("Getting fence CIDRs (from FENCE_CIDRS env, CSIAddonsNode status for any class, or node InternalIPs)")
				cidrs = GetFenceCIDRs(ctx, c, env.Provisioner, "")
			case helpers.FaultInjectorIptables:
				By("Getting fence CIDRs for iptables (FENCE_CIDRS; full-DR: peer backends from FENCE_PEER_SERVICES or FENCE_TARGET_SERVICES on DR2)")
				if IsFullDRMode() {
					cidrs = GetFenceCIDRsForFaultInjectionPeer(ctx, GetK8sClientForCluster(ClusterDR1), GetK8sClientForCluster(ClusterDR2))
				} else {
					cidrs = GetFenceCIDRsForFaultInjection(ctx, c)
				}
			}
			if len(cidrs) == 0 {
				Skip("L1-E-003 could not get CIDRs: set FENCE_CIDRS, wait for CSI networkFenceClientStatus, or use FENCE_TARGET_SERVICES / node discovery per docs/testing/replication-e2e-suite.md")
			}

			fenceParams := map[string]string{
				"secretName":      secretName,
				"secretNamespace": secretNs,
			}
			By(fmt.Sprintf("Fencing peer CIDRs via %s: %v", injector, cidrs))
			for _, cidr := range cidrs {
				Expect(faultProvider.FenceIP(ctx, cidr, fenceParams)).To(Succeed(), "FenceIP %s", cidr)
			}
			if injector == helpers.FaultInjectorIptables {
				time.Sleep(5 * time.Second)
			}

			vrName := "vr-fence"
			By("Creating VolumeReplication " + vrName + " while node is fenced (EnableVolumeReplication should fail)")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				if faultProvider != nil {
					_ = helpers.CollectFaultInjectionLogs(cleanupCtx, faultProvider)
					_ = faultProvider.Cleanup(cleanupCtx)
				}
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeletePVCWithCleanup(cleanupCtx, c, pvc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for VR to report error (peer unreachable)")
			WaitForVolumeReplicationError(ctx, c, vr)
			err = c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())

			By("Assertions: GetVolumeReplicationInfo (L1-INFO-005) — peer unreachable returns error in VR status")
			Expect(hasVolumeReplicationErrorCondition(vr)).To(BeTrue(),
				"GetVolumeReplicationInfo (L1-INFO-005): VR with fenced/peer unreachable must have error (message or degraded condition)")

			By("Unfencing (NetworkFence CR or iptables rules via PeerFenceProvider)")
			for _, cidr := range cidrs {
				Expect(faultProvider.UnfenceIP(ctx, cidr, fenceParams)).To(Succeed(), "UnfenceIP %s", cidr)
			}

			By("Waiting for controller to retry and EnableVolumeReplication to succeed")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-idem-" + nsName
			By("Creating VolumeReplicationClass (snapshot, 1m interval) " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			vrName := "vr-idem"
			By("Creating first VolumeReplication " + vrName)
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			By("Waiting for first VR Replicating=True or Completed=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, c, vr, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [PVC] %s\n", FormatPVCStatus(p))
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
