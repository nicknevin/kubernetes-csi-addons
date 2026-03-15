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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	replicationv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

var _ = Describe("GetVolumeReplicationInfo", func() {
	var ctx context.Context
	var env TestEnv

	BeforeEach(func() {
		ctx = context.Background()
		env = GetTestEnv()
	})

	Describe("L1-INFO-008: Non-existent volume", func() {
		It("L1-INFO-008 query for non-existent volume returns error in VR status", func() {
			By("Starting L1-INFO-008: Non-existent volume")
			c := GetK8sClient()
			nsName := UniqueNamespace()
			By("Creating namespace " + nsName)
			ns := CreateNamespace(ctx, c, nsName)

			secretName, secretNs := ReplicationSecretRef(ctx, c, env, nsName)
			vrcName := "vrc-nonexist-" + nsName
			By("Creating VolumeReplicationClass " + vrcName)
			vrc := CreateVolumeReplicationClass(ctx, c, vrcName, env.Provisioner, secretName, secretNs, MirroringModeSnapshot)

			// VR with dataSource pointing to a PVC that does not exist
			vrName := "vr-nonexist"
			By("Creating VolumeReplication with non-existent PVC")
			vr := CreateVolumeReplication(ctx, c, nsName, vrName, vrcName, "pvc-does-not-exist", replicationv1alpha1.Primary)

			DeferCleanup(func() {
				cleanupCtx := context.Background()
				DeleteVolumeReplicationWithCleanup(cleanupCtx, c, vr)
				DeleteVolumeReplicationClassWithCleanup(cleanupCtx, c, vrc)
				DeleteNamespace(cleanupCtx, c, ns)
			})

			By("Waiting for error in VR status")
			WaitForVolumeReplicationError(ctx, c, vr)
			err := c.Get(ctx, client.ObjectKey{Namespace: nsName, Name: vrName}, vr)
			Expect(err).NotTo(HaveOccurred())
			By("Assertions: GetVolumeReplicationInfo (L1-INFO-008) — non-existent volume returns error in VR status")
			Expect(vr.Status.Message).NotTo(BeEmpty(),
				"GetVolumeReplicationInfo (L1-INFO-008): VR with non-existent volume must have error message in status (message: %q)", vr.Status.Message)
		})
	})
})
