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
	"sigs.k8s.io/controller-runtime/pkg/client"

	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

var _ = Describe("Full DR (two clusters)", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
		SkipIfNotFullDR("Full DR", "test requires two clusters")
	})

	Describe("Dual-cluster resource creation", func() {
		It("creates namespace on both DR1 and DR2, PVC and VR on DR1 only", func() {
			By("Full DR: creating same namespace on DR1 and DR2")
			nsName := UniqueNamespace()
			cDR1 := GetK8sClientForCluster(ClusterDR1)
			cDR2 := GetK8sClientForCluster(ClusterDR2)

			ns1 := CreateNamespace(ctx, cDR1, nsName)
			By("Namespace created on DR1 (" + DR1Context() + ")")
			ns2 := CreateNamespace(ctx, cDR2, nsName)
			By("Namespace created on DR2 (" + DR2Context() + ")")

			secretName, secretNs := ReplicationSecretRef(ctx, cDR1, env, nsName)
			By("Creating PVC and VR on DR1 only (primary)")
			pvc := CreatePVC(ctx, cDR1, nsName, "pvc-dr1", env.StorageClass, "1Gi", func(p *corev1.PersistentVolumeClaim) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][PVC] %s\n", FormatPVCStatus(p))
			})

			vrcName := "vrc-full-dr-" + nsName
			vrc := CreateVolumeReplicationClass(ctx, cDR1, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)
			vrName := "vr-dr1"
			vr := CreateVolumeReplication(ctx, cDR1, nsName, vrName, vrcName, pvc.Name, replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, cDR1, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, cDR1, vrc)
				DeletePVCWithCleanup(cleanupCtx, cDR1, pvc)
				DeleteNamespace(cleanupCtx, cDR1, ns1)
				DeleteNamespace(cleanupCtx, cDR2, ns2)
			})

			By("Waiting for Replicating=True or Completed=True on DR1")
			WaitForVolumeReplicationReplicatingOrCompleted(ctx, cDR1, vr, func(v *replicationv1alpha1.VolumeReplication) {
				fmt.Fprintf(GinkgoWriter, "  [DR1][VR] %s\n", FormatVRStatus(v))
			})
			err := cDR1.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("DR1 VR state: " + string(vr.Status.State))

			By("Listing VolumeReplications on DR2 (expect none or secondary VRs depending on setup)")
			vrListDR2 := &replicationv1alpha1.VolumeReplicationList{}
			err = cDR2.List(ctx, vrListDR2, client.InNamespace(nsName))
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "  [DR2] VR count in %s: %d\n", nsName, len(vrListDR2.Items))
		})
	})
})
