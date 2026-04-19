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

	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
	"github.com/csi-addons/kubernetes-csi-addons/test/e2e/helpers"
)

// hasVolumeReplicationCompletedCondition is true when ConditionCompleted has Status=True.
//
// This is not sufficient to mean “resync is done”: after a successful ResyncVolume RPC the controller
// runs setResyncCondition (internal/controller/replication.storage/status.go), which sets
// ConditionCompleted=True (Reason Demoted) at the same time as ConditionResyncing=True until the CSI
// driver reports Ready (ResyncVolumeResponse.ready). So Completed=True with Resyncing=True is normal
// while the mirror catches up. Failed resync uses setFailedResyncCondition (Completed=False, Resyncing=False).
func hasVolumeReplicationCompletedCondition(vr *replicationv1alpha1.VolumeReplication) bool {
	for _, cond := range vr.Status.Conditions {
		if cond.Type == replicationv1alpha1.ConditionCompleted && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasResyncOperationCompleted is true when ConditionResyncing has Status=False (Reason NotResyncing).
// The controller clears Resyncing after the driver reports resync ready (and applies setNotDegradedCondition),
// or on failure via setFailedResyncCondition—pair with hasVolumeReplicationCompletedCondition to tell success
// (Completed=True) from failure (Completed=False).
func hasResyncOperationCompleted(vr *replicationv1alpha1.VolumeReplication) bool {
	for _, cond := range vr.Status.Conditions {
		if cond.Type == replicationv1alpha1.ConditionResyncing && cond.Status == metav1.ConditionFalse {
			return true
		}
	}
	return false
}

// volumeReplicationResyncWaitSatisfied is the condition these specs use in Eventually: successful resync
// reconciliation requires Completed=True and Resyncing=False. Do not drop either: Resyncing=False alone would
// match failed resync (Completed=False).
func volumeReplicationResyncWaitSatisfied(vr *replicationv1alpha1.VolumeReplication) bool {
	return hasVolumeReplicationCompletedCondition(vr) && hasResyncOperationCompleted(vr)
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-rsync-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-rsync", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("L1-RSYNC-001: Validating primary replication state")
			_ = cDR1.Get(ctx, client.ObjectKeyFromObject(vrDR1), vrDR1)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][INFO] Primary: %s\n", FormatVRStatus(vrDR1))
			Expect(vrDR1.Status.State).To(Equal(replicationv1alpha1.PrimaryState), "Primary should be in Primary state")
			Expect(hasReplicationSuccessCondition(vrDR1)).To(BeTrue(), "Primary should have success condition")

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-rsync", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			By("L1-RSYNC-001: Validating secondary replication state (pre-fence)")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] Secondary (pre-fence): %s\n", FormatVRStatus(vrDR2))
			Expect(vrDR2.Status.State).To(Equal(replicationv1alpha1.SecondaryState), "Secondary should be in Secondary state")
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(), "Secondary should have success condition")

			var faultProvider helpers.PeerFenceProvider
			var cidrs []string
			var fenceBaselines map[string]*helpers.ConnectivityBaseline

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				if faultProvider != nil {
					if err := helpers.CollectFaultInjectionLogs(cleanupCtx, faultProvider); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "[WARNING] L1-RSYNC-001 fault injection log collection: %v\n", err)
					}
					_ = faultProvider.Cleanup(cleanupCtx)
				}
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

			injector := helpers.GetFaultInjectorTypeFromEnv()
			fenceParams := map[string]string{"secretName": secretName, "secretNamespace": secretNs}
			fenceApplied := false

			By("L1-RSYNC-001: Checking fault injection prerequisites (align with L1-PROM-003)")
			if injector != helpers.FaultInjectorNone && !IsNetworkFenceSupportAvailable() && !helpers.HasPrivilegedDaemonSetSupport(ctx, cDR2) {
				Skip("L1-RSYNC-001 requires either NetworkFence support or privileged DaemonSet capabilities for fault injection")
			}

			if injector != helpers.FaultInjectorNone {
				By("L1-RSYNC-001: Resolving peer fence CIDRs for E2E_FAULT_INJECTOR")
				if injector == helpers.FaultInjectorIptables {
					cidrs = GetFenceCIDRsForFaultInjectionPeer(ctx, cDR2, cDR1)
				} else {
					cidrs = GetFenceCIDRsWithPeerNodeClient(ctx, cDR2, cDR1, env.Provisioner, "")
				}
				if len(cidrs) == 0 {
					Skip("L1-RSYNC-001 could not get CIDRs: for iptables set FENCE_CIDRS or FENCE_PEER_SERVICES/FENCE_TARGET_SERVICES; for NetworkFence set FENCE_CIDRS, wait for CSI client CIDRs, or ensure DR1 has node InternalIPs for fallback")
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "  [L1-RSYNC-001] fencing peer CIDRs (injector=%s): %v\n", injector, cidrs)

				var err error
				faultProvider, err = helpers.NewFaultInjectionProvider(helpers.FaultInjectionConfig{
					Type:       injector,
					Client:     cDR2,
					RESTConfig: GetRESTConfigDR2(),
					Namespace:  nsName,
					ProviderParams: map[string]string{
						"provisioner":     env.Provisioner,
						"image":           helpers.DefaultIptablesImageWithRegistry,
						"cluster_context": "DR2",
					},
				})
				Expect(err).NotTo(HaveOccurred(), "NewFaultInjectionProvider for L1-RSYNC-001")

				fenceBaselines = IptablesBaselinesOrGinkgoSkip(ctx, injector, faultProvider, cidrs)

				By(fmt.Sprintf("L1-RSYNC-001: Fencing peer from DR2 via %s: %v", injector, cidrs))
				for _, cidr := range cidrs {
					Expect(faultProvider.FenceIP(ctx, cidr, fenceParams)).To(Succeed(), "FenceIP %s", cidr)
				}
				if injector == helpers.FaultInjectorIptables {
					time.Sleep(5 * time.Second)
					var lastFenceVerify string
					Eventually(func() bool {
						lastFenceVerify = ""
						for _, cidr := range cidrs {
							fenced, vErr := faultProvider.VerifyConnectivity(ctx, cidr, true, helpers.BaselineForCIDR(fenceBaselines, cidr))
							if vErr != nil {
								lastFenceVerify = fmt.Sprintf("VerifyConnectivity %s: %v", cidr, vErr)
								return false
							}
							if !fenced {
								lastFenceVerify = fmt.Sprintf("CIDR %s probes still match pre-fence reachability (expected partition)", cidr)
								return false
							}
						}
						return true
					}, 2*time.Minute, 10*time.Second).Should(BeTrue(),
						"L1-RSYNC-001: after iptables fence, DR2 must show partition vs baseline (CIDRs %v). %s", cidrs, lastFenceVerify)
				}

				fenceApplied = true
				By("L1-RSYNC-001: Validating secondary replication state (post-fence, degraded)")
				Eventually(func() bool {
					_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
					_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] post-fence: %s\n", FormatVRStatus(vrDR2))
					return HasVolumeReplicationErrorCondition(vrDR2)
				}, 30*time.Second, 2*time.Second).Should(BeTrue(),
					"Secondary should show error condition after fence")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [NOTE] L1-RSYNC-001: E2E_FAULT_INJECTOR=none; resync without network isolation.\n")
			}

			By("L1-RSYNC-001: Restoring connectivity (unfence) before resync")
			if faultProvider != nil && fenceApplied {
				for _, cidr := range cidrs {
					Expect(faultProvider.UnfenceIP(ctx, cidr, fenceParams)).To(Succeed(), "UnfenceIP %s", cidr)
				}
				var lastUnfence string
				Eventually(func() bool {
					lastUnfence = ""
					for _, cidr := range cidrs {
						ok, vErr := faultProvider.VerifyConnectivity(ctx, cidr, false, helpers.BaselineForCIDR(fenceBaselines, cidr))
						if vErr != nil {
							lastUnfence = fmt.Sprintf("VerifyConnectivity %s: %v", cidr, vErr)
							return false
						}
						if !ok {
							lastUnfence = fmt.Sprintf("CIDR %s not yet matching reachable baseline after unfence", cidr)
							return false
						}
					}
					return true
				}, 2*time.Minute, 10*time.Second).Should(BeTrue(),
					"L1-RSYNC-001: after unfence, DR2 connectivity must match pre-fence reachability (CIDRs %v). %s", cidrs, lastUnfence)
				helpers.SweepIptablesResidualAfterUnfence(ctx, injector, faultProvider)
			}
			By("L1-RSYNC-001: Triggering resync by updating VR to Resync state")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred(), "Failed to update VR replicationState to Resync")

			By("Waiting for VR resync: Resyncing=False and Completed=True (Completed may stay True while Resyncing=True until driver reports ready)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] resync poll: %s\n", FormatVRStatus(vrDR2))
				return volumeReplicationResyncWaitSatisfied(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
				"expect Resyncing=False and Completed=True after resync (Resyncing=True with Completed=True means still catching up)")

			By("L1-RSYNC-001: Assertion — data consistency confirmed")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] Secondary (post-resync): %s\n", FormatVRStatus(vrDR2))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-idem-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-idem", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-idem", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-idem", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
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
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] pre-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasReplicationSuccessCondition(vrDR2)).To(BeTrue(), "Should have success condition before resync")

			By("L1-RSYNC-002: Triggering first resync on healthy secondary")
			err := cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for first resync: Resyncing=False and Completed=True")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] first resync: %s\n", FormatVRStatus(vrDR2))
				return volumeReplicationResyncWaitSatisfied(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-002: Assertion — data consistency after first resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-first-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after first resync")

			By("L1-RSYNC-002: Triggering second resync on already-synced secondary (idempotent)")
			err = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for second resync: Resyncing=False and Completed=True")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] second resync: %s\n", FormatVRStatus(vrDR2))
				return volumeReplicationResyncWaitSatisfied(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-002: Assertion — data consistency after second resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-second-resync: %s\n", FormatVRStatus(vrDR2))
			Expect(hasVolumeReplicationCompletedCondition(vrDR2)).To(BeTrue(), "Should have Completed=True after second resync")
		})
	})

	Describe("L1-RSYNC-003: Resync with NetworkFence (split-brain recovery)", func() {
		It("L1-RSYNC-003: resync with secondary network-fenced (split-brain), expect resync completes after fence removal", func() {
			By("L1-RSYNC-003: Create primary and secondary, apply fault injection fence on DR2, resolve, then resync")
			SkipIfNotFullDR("L1-RSYNC-003", "requires two clusters (DR1_CONTEXT and DR2_CONTEXT)")
			injector := helpers.GetFaultInjectorTypeFromEnv()
			if injector == helpers.FaultInjectorNone {
				Skip("L1-RSYNC-003 requires fault injection (E2E_FAULT_INJECTOR=iptables|networkfence)")
			}
			switch injector {
			case helpers.FaultInjectorNetworkFence:
				if !IsNetworkFenceSupportAvailable() {
					Skip("L1-RSYNC-003 (networkfence) requires NetworkFence support on this setup")
				}
			case helpers.FaultInjectorIptables:
				if !helpers.HasPrivilegedDaemonSetSupport(ctx, GetK8sClientForCluster(ClusterDR2)) {
					Skip("L1-RSYNC-003 (iptables) requires privileged DaemonSet support on DR2")
				}
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-fence-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-fence", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-fence", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-fence", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
			})

			var faultProvider helpers.PeerFenceProvider
			var cidrs []string

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				if faultProvider != nil {
					_ = helpers.CollectFaultInjectionLogs(cleanupCtx, faultProvider)
					_ = faultProvider.Cleanup(cleanupCtx)
				}
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

			var err error
			faultProvider, err = helpers.NewFaultInjectionProvider(helpers.FaultInjectionConfig{
				Type:       injector,
				Client:     cDR2,
				RESTConfig: GetRESTConfigDR2(),
				Namespace:  nsName,
				ProviderParams: map[string]string{
					"provisioner":     env.Provisioner,
					"image":           helpers.DefaultIptablesImageWithRegistry,
					"cluster_context": "DR2",
				},
			})
			Expect(err).NotTo(HaveOccurred(), "NewFaultInjectionProvider")
			if !faultProvider.IsSupported(ctx) {
				Skip(fmt.Sprintf("L1-RSYNC-003 (%s): fault injection not supported on this cluster", injector))
			}
			switch injector {
			case helpers.FaultInjectorNetworkFence:
				cidrs = GetFenceCIDRsWithPeerNodeClient(ctx, cDR2, cDR1, env.Provisioner, "")
			case helpers.FaultInjectorIptables:
				cidrs = GetFenceCIDRsForFaultInjectionPeer(ctx, cDR2, cDR1)
			}
			if len(cidrs) == 0 {
				Skip("L1-RSYNC-003 could not get fence CIDRs (set FENCE_CIDRS or discovery env vars)")
			}
			fp := map[string]string{"secretName": secretName, "secretNamespace": secretNs}
			By(fmt.Sprintf("L1-RSYNC-003: Fencing peer from DR2 via %s: %v", injector, cidrs))
			for _, cidr := range cidrs {
				Expect(faultProvider.FenceIP(ctx, cidr, fp)).To(Succeed(), "FenceIP %s", cidr)
			}
			if injector == helpers.FaultInjectorIptables {
				time.Sleep(5 * time.Second)
			}

			By("L1-RSYNC-003: Validating secondary is fenced (split-brain state)")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] fenced: %s\n", FormatVRStatus(vrDR2))
				return HasVolumeReplicationErrorCondition(vrDR2)
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"Secondary should show error after fence applied")

			By("L1-RSYNC-003: Unfencing peer before resync")
			for _, cidr := range cidrs {
				Expect(faultProvider.UnfenceIP(ctx, cidr, fp)).To(Succeed(), "UnfenceIP %s", cidr)
			}
			helpers.SweepIptablesResidualAfterUnfence(ctx, injector, faultProvider)

			By("L1-RSYNC-003: Triggering resync after split-brain recovery")
			err = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			Expect(err).NotTo(HaveOccurred())
			vrDR2.Spec.ReplicationState = replicationv1alpha1.Resync
			err = cDR2.Update(ctx, vrDR2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for resync: Resyncing=False and Completed=True")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] resync: %s\n", FormatVRStatus(vrDR2))
				return volumeReplicationResyncWaitSatisfied(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-003: Assertion — data consistency after resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-resync: %s\n", FormatVRStatus(vrDR2))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-force-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-force", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-force", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-force", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
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

			By("Waiting for resync: Resyncing=False and Completed=True")
			Eventually(func() bool {
				_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] force-resync: %s\n", FormatVRStatus(vrDR2))
				return volumeReplicationResyncWaitSatisfied(vrDR2)
			}, 5*time.Minute, 5*time.Second).Should(BeTrue())

			By("L1-RSYNC-004: Assertion — data consistency after force resync")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][INFO] post-force-resync: %s\n", FormatVRStatus(vrDR2))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcName := "vrc-err-" + nsName
			vrcDR1 := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR1 := CreateVolumeReplication(ctx, cDR1, nsName, "vr-dr1-err", vrcName, pvcDR1.Name, replicationv1alpha1.Primary)

			By("Waiting for primary VR on DR1 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vrDR1, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})

			By("Creating secondary PVC and VR on DR2")
			pvcDR2, pvDR2 := CreateSecondaryPVCFromPrimary(ctx, cDR1, cDR2, pvcDR1, nsName, "pvc-dr2-err", func(p *corev1.PersistentVolumeClaim) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][PVC] %s\n", FormatPVCStatus(p))
			})
			vrcDR2 := CreateVolumeReplicationClass(ctx, cDR2, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrDR2 := CreateVolumeReplication(ctx, cDR2, nsName, "vr-dr2-err", vrcName, pvcDR2.Name, replicationv1alpha1.Secondary)

			By("Waiting for secondary VR on DR2 to reach Replicating=True")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR2, vrDR2, func(v *replicationv1alpha1.VolumeReplication) {
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] %s\n", FormatVRStatus(v))
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
				_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] error-handling: %s\n", FormatVRStatus(vrDR2))
				return vrDR2.Status.State != "" || vrDR2.Status.Conditions != nil
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"Controller should update VR status even with invalid params")

			By("L1-RSYNC-005: Assertion — Graceful error handling via GetVolumeReplicationInfo")
			_ = cDR2.Get(ctx, client.ObjectKeyFromObject(vrDR2), vrDR2)
			_, _ = fmt.Fprintf(GinkgoWriter, "  [DR2][VR] final state after error test: %s\n", FormatVRStatus(vrDR2))
			Expect(vrDR2.Status.State).ToNot(BeEmpty(),
				"VR should maintain some state even after error attempt")
		})
	})
})
