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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/upbothq/upbot-go-sdk"
	monitoringv1alpha1 "github.com/upbothq/upbot-operator/api/v1alpha1"
)

type apiCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeUpbot records the API calls and answers each method with the status and body set for it
type fakeUpbot struct {
	mu        sync.Mutex
	calls     []apiCall
	responses map[string]fakeResponse
	server    *httptest.Server
}

type fakeResponse struct {
	status int
	body   string
}

func newFakeUpbot() *fakeUpbot {
	f := &fakeUpbot{responses: map[string]fakeResponse{
		http.MethodPost:   {http.StatusCreated, `{"id":"ext-1"}`},
		http.MethodPut:    {http.StatusOK, `{"data":{}}`},
		http.MethodDelete: {http.StatusNoContent, ``},
	}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		f.mu.Lock()
		f.calls = append(f.calls, apiCall{Method: req.Method, Path: req.URL.Path, Body: body})
		resp := f.responses[req.Method]
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	return f
}

func (f *fakeUpbot) respond(method string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[method] = fakeResponse{status, body}
}

func (f *fakeUpbot) takeCalls() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func (f *fakeUpbot) client() *upbot.APIClient {
	cfg := upbot.NewConfiguration()
	cfg.Servers = upbot.ServerConfigurations{{URL: f.server.URL}}
	return upbot.NewAPIClient(cfg)
}

var _ = Describe("Monitor Controller", func() {
	const resourceName = "test-resource"

	var (
		ctx        context.Context
		api        *fakeUpbot
		reconciler *MonitorReconciler
		key        = types.NamespacedName{Name: resourceName, Namespace: "default"}
	)

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	getMonitor := func() *monitoringv1alpha1.Monitor {
		GinkgoHelper()
		monitor := &monitoringv1alpha1.Monitor{}
		Expect(k8sClient.Get(ctx, key, monitor)).To(Succeed())
		return monitor
	}

	BeforeEach(func() {
		ctx = context.Background()
		api = newFakeUpbot()
		reconciler = &MonitorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ApiClient: api.client()}

		Expect(k8sClient.Create(ctx, &monitoringv1alpha1.Monitor{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
			Spec:       monitoringv1alpha1.MonitorSpec{Type: "http", Target: "https://example.com", Interval: "60"},
		})).To(Succeed())

		// The first reconcile only adds the finalizer
		reconcileOnce()
		Expect(api.takeCalls()).To(BeEmpty())
	})

	AfterEach(func() {
		monitor := &monitoringv1alpha1.Monitor{}
		if err := k8sClient.Get(ctx, key, monitor); err == nil {
			monitor.Finalizers = nil
			Expect(k8sClient.Update(ctx, monitor)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, monitor))).To(Succeed())
		}
		api.server.Close()
	})

	It("creates the monitor once, then makes no API call while the spec is unchanged", func() {
		reconcileOnce()

		calls := api.takeCalls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Method).To(Equal(http.MethodPost))
		Expect(calls[0].Body).To(HaveKeyWithValue("name", "default/test-resource"))

		monitor := getMonitor()
		Expect(monitor.Status.ExternalID).To(Equal("ext-1"))
		Expect(monitor.Status.ObservedGeneration).To(Equal(monitor.Generation))
		Expect(meta.IsStatusConditionTrue(monitor.Status.Conditions, conditionReady)).To(BeTrue())

		reconcileOnce()
		Expect(api.takeCalls()).To(BeEmpty())
	})

	It("updates the monitor when the spec changes, without overwriting its retry count", func() {
		reconcileOnce()
		api.takeCalls()

		monitor := getMonitor()
		monitor.Spec.Interval = "300"
		Expect(k8sClient.Update(ctx, monitor)).To(Succeed())
		reconcileOnce()

		calls := api.takeCalls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Method).To(Equal(http.MethodPut))
		Expect(calls[0].Path).To(Equal("/api/monitors/ext-1"))
		Expect(calls[0].Body).To(HaveKeyWithValue("interval", "300"))
		Expect(calls[0].Body).NotTo(HaveKey("retry_count"))
		Expect(getMonitor().Status.ObservedGeneration).To(Equal(getMonitor().Generation))
	})

	It("creates the monitor again when it was deleted in Upbot", func() {
		reconcileOnce()
		api.takeCalls()

		monitor := getMonitor()
		monitor.Spec.Target = "https://example.org"
		Expect(k8sClient.Update(ctx, monitor)).To(Succeed())

		api.respond(http.MethodPut, http.StatusNotFound, `{"message":"Not found"}`)
		api.respond(http.MethodPost, http.StatusCreated, `{"id":"ext-2"}`)
		reconcileOnce()
		Expect(getMonitor().Status.ExternalID).To(BeEmpty())

		reconcileOnce()
		Expect(getMonitor().Status.ExternalID).To(Equal("ext-2"))

		calls := api.takeCalls()
		Expect(calls).To(HaveLen(2))
		Expect(calls[0].Method).To(Equal(http.MethodPut))
		Expect(calls[1].Method).To(Equal(http.MethodPost))
	})

	It("shows an API rejection on the Ready condition and waits before trying again", func() {
		api.respond(http.MethodPost, http.StatusUnprocessableEntity, `{"message":"The interval is not allowed by your plan"}`)

		result := reconcileOnce()
		Expect(result.RequeueAfter).To(Equal(clientErrorRequeue))

		condition := meta.FindStatusCondition(getMonitor().Status.Conditions, conditionReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("CreateFailed"))
		Expect(condition.Message).To(ContainSubstring("not allowed by your plan"))
		Expect(getMonitor().Status.ExternalID).To(BeEmpty())
	})

	It("waits for Retry-After when the API rate limits", func() {
		api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(http.StatusTooManyRequests)
		})

		Expect(reconcileOnce().RequeueAfter).To(Equal(42 * time.Second))
	})

	It("deletes the monitor from Upbot before releasing the finalizer", func() {
		reconcileOnce()
		api.takeCalls()

		Expect(k8sClient.Delete(ctx, getMonitor())).To(Succeed())
		reconcileOnce()

		calls := api.takeCalls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Method).To(Equal(http.MethodDelete))
		Expect(calls[0].Path).To(Equal("/api/monitors/ext-1"))

		err := k8sClient.Get(ctx, key, &monitoringv1alpha1.Monitor{})
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})
