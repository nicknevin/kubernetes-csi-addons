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

package helpers

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
	return fmt.Sprintf("e2e-test-%s", uuid.NewUUID()[:8])
}

// CreateNamespace creates a namespace with the given name.
func CreateNamespace(ctx context.Context, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := c.Create(ctx, ns)
	Expect(err).NotTo(HaveOccurred())
	Logf("[CREATE]", "Created namespace: %s", name)
	return ns
}

// DeleteNamespace deletes a namespace and ignores NotFound.
func DeleteNamespace(ctx context.Context, c client.Client, ns *corev1.Namespace) {
	if ns == nil {
		return
	}
	err := c.Delete(ctx, ns)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[ERROR]", "Failed to delete namespace %s: %v", ns.Name, err)
		Expect(err).NotTo(HaveOccurred())
	} else if err == nil {
		Logf("[DELETE]", "Deleted namespace: %s", ns.Name)
	}
}

// DeletePVC deletes a PVC and ignores NotFound.
func DeletePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) {
	if pvc == nil {
		return
	}
	err := c.Delete(ctx, pvc)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[ERROR]", "Failed to delete PVC %s/%s: %v", pvc.Namespace, pvc.Name, err)
		Expect(err).NotTo(HaveOccurred())
	} else if err == nil {
		Logf("[DELETE]", "Deleted PVC: %s/%s", pvc.Namespace, pvc.Name)
	}
}

// DeletePV deletes a PV and ignores NotFound.
func DeletePV(ctx context.Context, c client.Client, pv *corev1.PersistentVolume) {
	if pv == nil {
		return
	}
	err := c.Delete(ctx, pv)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[ERROR]", "Failed to delete PV %s: %v", pv.Name, err)
		Expect(err).NotTo(HaveOccurred())
	} else if err == nil {
		Logf("[DELETE]", "Deleted PV: %s", pv.Name)
	}
}

// CreateSecret creates a secret with the given data.
func CreateSecret(ctx context.Context, c client.Client, namespace, name string, data map[string][]byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	err := c.Create(ctx, secret)
	Expect(err).NotTo(HaveOccurred())
	Logf("[CREATE]", "Created secret: %s/%s", namespace, name)
	return secret
}

// DeleteSecret deletes a secret and ignores NotFound.
func DeleteSecret(ctx context.Context, c client.Client, secret *corev1.Secret) {
	if secret == nil {
		return
	}
	err := c.Delete(ctx, secret)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[ERROR]", "Failed to delete secret %s/%s: %v", secret.Namespace, secret.Name, err)
		Expect(err).NotTo(HaveOccurred())
	} else if err == nil {
		Logf("[DELETE]", "Deleted secret: %s/%s", secret.Namespace, secret.Name)
	}
}

// WaitForResourceDeletion waits for a resource to be deleted from the cluster.
// This is useful when resources have finalizers or take time to be cleaned up.
func WaitForResourceDeletion(ctx context.Context, c client.Client, obj client.Object, timeout time.Duration) {
	key := client.ObjectKeyFromObject(obj)
	Eventually(func() bool {
		err := c.Get(ctx, key, obj)
		return errors.IsNotFound(err)
	}, timeout, 2*time.Second).Should(BeTrue(),
		"Resource %s %s/%s should be deleted", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
}

// GenerateUniqueName generates a unique name with the given prefix for test resources.
func GenerateUniqueName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewUUID()[:8])
}

// LogResourceStatus logs the current status of a Kubernetes resource for debugging.
func LogResourceStatus(prefix string, obj client.Object, status interface{}) {
	Logf(prefix, "Resource %s/%s status: %+v", obj.GetNamespace(), obj.GetName(), status)
}

// CreateConfigMap creates a ConfigMap with the given data.
func CreateConfigMap(ctx context.Context, c client.Client, namespace, name string, data map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
	err := c.Create(ctx, cm)
	Expect(err).NotTo(HaveOccurred())
	Logf("[CREATE]", "Created ConfigMap: %s/%s", namespace, name)
	return cm
}

// DeleteConfigMap deletes a ConfigMap and ignores NotFound.
func DeleteConfigMap(ctx context.Context, c client.Client, cm *corev1.ConfigMap) {
	if cm == nil {
		return
	}
	err := c.Delete(ctx, cm)
	if err != nil && !errors.IsNotFound(err) {
		Logf("[ERROR]", "Failed to delete ConfigMap %s/%s: %v", cm.Namespace, cm.Name, err)
		Expect(err).NotTo(HaveOccurred())
	} else if err == nil {
		Logf("[DELETE]", "Deleted ConfigMap: %s/%s", cm.Namespace, cm.Name)
	}
}
