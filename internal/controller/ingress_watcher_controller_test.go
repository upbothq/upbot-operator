/*
Copyright 2025.

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

package controller

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/upbothq/upbot-operator/api/v1alpha1"
)

var _ = Describe("Ingress Watcher", func() {
	const namespace = "default"

	var (
		ctx        context.Context
		reconciler *IngressWatcherReconciler
		key        = types.NamespacedName{Name: "web", Namespace: namespace}
	)

	rule := func(host string) networkingv1.IngressRule {
		return networkingv1.IngressRule{Host: host}
	}

	reconcileOnce := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	// Targets of the Monitors in the namespace, by name
	monitorTargets := func() map[string]string {
		GinkgoHelper()
		var list monitoringv1alpha1.MonitorList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(namespace))).To(Succeed())
		targets := map[string]string{}
		for _, m := range list.Items {
			if m.DeletionTimestamp.IsZero() {
				targets[m.Name] = m.Spec.Target
			}
		}
		return targets
	}

	sortedValues := func(m map[string]string) []string {
		values := []string{}
		for _, v := range m {
			values = append(values, v)
		}
		sort.Strings(values)
		return values
	}

	createIngress := func(rules ...networkingv1.IngressRule) *networkingv1.Ingress {
		GinkgoHelper()
		ingress := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace},
			Spec: networkingv1.IngressSpec{
				TLS:   []networkingv1.IngressTLS{{Hosts: []string{"api.example.com"}}},
				Rules: rules,
			},
		}
		Expect(k8sClient.Create(ctx, ingress)).To(Succeed())
		return ingress
	}

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &IngressWatcherReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Interval: "60"}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace}}))).To(Succeed())
		// No Monitor controller runs here, so nothing holds a finalizer
		Expect(k8sClient.DeleteAllOf(ctx, &monitoringv1alpha1.Monitor{}, client.InNamespace(namespace))).To(Succeed())
	})

	It("creates one Monitor per host, skipping wildcards, and keeps the Ingress name for the first one", func() {
		createIngress(rule("api.example.com"), rule("*.example.com"), rule("www.example.com"), rule("api.example.com"))
		reconcileOnce()

		targets := monitorTargets()
		Expect(targets).To(HaveLen(2))
		Expect(targets).To(HaveKeyWithValue("web", "https://api.example.com"))
		Expect(sortedValues(targets)).To(Equal([]string{"http://www.example.com", "https://api.example.com"}))

		// A second pass changes nothing
		reconcileOnce()
		Expect(monitorTargets()).To(Equal(targets))
	})

	It("deletes the Monitor of a host removed from the Ingress", func() {
		ingress := createIngress(rule("api.example.com"), rule("www.example.com"))
		reconcileOnce()
		Expect(monitorTargets()).To(HaveLen(2))

		ingress.Spec.Rules = []networkingv1.IngressRule{rule("api.example.com")}
		Expect(k8sClient.Update(ctx, ingress)).To(Succeed())
		reconcileOnce()

		Expect(monitorTargets()).To(Equal(map[string]string{"web": "https://api.example.com"}))
	})

	It("skips an Ingress without a host instead of failing", func() {
		createIngress(rule(""), rule("*.example.com"))
		reconcileOnce()
		Expect(monitorTargets()).To(BeEmpty())
	})

	It("does not monitor a URL a Monitor created by hand already checks", func() {
		Expect(k8sClient.Create(ctx, &monitoringv1alpha1.Monitor{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace},
			Spec:       monitoringv1alpha1.MonitorSpec{Type: "http", Target: "https://api.example.com", Interval: "30"},
		})).To(Succeed())

		createIngress(rule("api.example.com"), rule("www.example.com"))
		reconcileOnce()

		targets := monitorTargets()
		Expect(targets).To(HaveLen(2))
		Expect(targets).To(HaveKeyWithValue("web", "https://api.example.com"))
		Expect(sortedValues(targets)).To(Equal([]string{"http://www.example.com", "https://api.example.com"}))
	})

	It("adopts a Monitor created before one Monitor per host", func() {
		ingress := createIngress(rule("api.example.com"))
		legacy := &monitoringv1alpha1.Monitor{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "web",
				Namespace:   namespace,
				Labels:      map[string]string{sourceLabel: sourceIngressWatcher},
				Annotations: map[string]string{sourceIngressAnnotation: "default/web"},
			},
			Spec: monitoringv1alpha1.MonitorSpec{Type: "http", Target: "https://api.example.com", Interval: "60"},
		}
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())

		ingress.Spec.Rules = append(ingress.Spec.Rules, rule("www.example.com"))
		Expect(k8sClient.Update(ctx, ingress)).To(Succeed())
		reconcileOnce()

		Expect(monitorTargets()).To(HaveLen(2))
		adopted := &monitoringv1alpha1.Monitor{}
		Expect(k8sClient.Get(ctx, key, adopted)).To(Succeed())
		Expect(adopted.UID).To(Equal(legacy.UID))
		Expect(adopted.Annotations).To(HaveKeyWithValue(sourceHostAnnotation, "api.example.com"))
	})

	It("deletes its Monitors when the Ingress is deleted", func() {
		ingress := createIngress(rule("api.example.com"), rule("www.example.com"))
		reconcileOnce()
		Expect(monitorTargets()).To(HaveLen(2))

		Expect(k8sClient.Delete(ctx, ingress)).To(Succeed())
		reconcileOnce()
		Expect(monitorTargets()).To(BeEmpty())
	})
})
