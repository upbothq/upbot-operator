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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/upbothq/operator/api/v1alpha1"
	"github.com/upbothq/upbot-go-sdk"
)

const (
	monitorFinalizer = "monitoring.upbot.app/finalizer"
	conditionReady   = "Ready"

	// The API refused the monitor (invalid spec, plan limit, bad token): retrying soon gives the same answer,
	// a spec change reconciles right away anyway
	clientErrorRequeue = 10 * time.Minute
	// Default wait when the API rate limits without a Retry-After header
	rateLimitRequeue = time.Minute
	// Longest API error body kept in the Ready condition
	maxErrorMessage = 500
)

// MonitorReconciler reconciles a Monitor object
type MonitorReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	ApiClient *upbot.APIClient
}

// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors/finalizers,verbs=update

// Reconcile syncs a Monitor with its monitor in Upbot: it creates it, updates it when the spec changes
// and deletes it when the Monitor is deleted.
func (r *MonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.V(1).Info("Reconciling Monitor", "name", req.NamespacedName)

	var monitor monitoringv1alpha1.Monitor
	if err := r.Get(ctx, req.NamespacedName, &monitor); err != nil {
		// The Monitor resource may have been deleted after the reconcile request.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !monitor.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &monitor)
	}

	if !controllerutil.ContainsFinalizer(&monitor, monitorFinalizer) {
		controllerutil.AddFinalizer(&monitor, monitorFinalizer)
		if err := r.Update(ctx, &monitor); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Added finalizer to monitor")
		return ctrl.Result{}, nil
	}

	// This generation is already in Upbot: status writes, finalizer changes and resyncs make no API call
	if monitor.Status.ExternalID != "" &&
		monitor.Status.ObservedGeneration == monitor.Generation &&
		meta.IsStatusConditionTrue(monitor.Status.Conditions, conditionReady) {
		return ctrl.Result{}, nil
	}

	if monitor.Status.ExternalID != "" {
		return r.handleUpdate(ctx, &monitor)
	}

	return r.handleCreate(ctx, &monitor)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1alpha1.Monitor{}).
		Named("monitor").
		Complete(r)
}

// upbotName keeps Monitors with the same name in different namespaces apart in Upbot
func upbotName(monitor *monitoringv1alpha1.Monitor) string {
	return fmt.Sprintf("%s/%s", monitor.Namespace, monitor.Name)
}

func (r *MonitorReconciler) handleCreate(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Creating monitor in Upbot", "name", monitor.Name)

	name := upbotName(monitor)
	retryCount := int32(0)
	newMonitor := upbot.StoreANewlyCreatedResourceInStorageRequest{
		Name:       &name,
		Type:       monitor.Spec.Type,
		Target:     &monitor.Spec.Target,
		Interval:   &monitor.Spec.Interval,
		RetryCount: *upbot.NewNullableInt32(&retryCount),
	}

	resp, httpResp, err := r.ApiClient.MonitorManagementAPI.StoreANewlyCreatedResourceInStorage(ctx).
		StoreANewlyCreatedResourceInStorageRequest(newMonitor).Execute()
	if err != nil {
		return r.handleAPIError(ctx, monitor, "CreateFailed", httpResp, err)
	}

	if resp == nil || resp.Id == nil || *resp.Id == "" {
		// Retrying could create a second monitor, so stop here until the spec changes
		err := errors.New("the Upbot API created the monitor without returning its ID")
		r.setReady(ctx, monitor, metav1.ConditionFalse, "InvalidResponse", err.Error())
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	externalID := *resp.Id
	if err := r.patchStatus(ctx, monitor, func(m *monitoringv1alpha1.Monitor) {
		m.Status.ExternalID = externalID
		m.Status.ObservedGeneration = m.Generation
		setReadyCondition(m, metav1.ConditionTrue, "Synced", "The monitor is in Upbot")
	}); err != nil {
		// The next reconcile would create the monitor a second time: log the ID so it can be found
		logger.Error(err, "Created monitor in Upbot but failed to save its ID", "externalID", externalID)
		return ctrl.Result{}, err
	}

	logger.Info("Created monitor in Upbot", "externalID", externalID)
	return ctrl.Result{}, nil
}

func (r *MonitorReconciler) handleUpdate(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Updating monitor in Upbot", "externalID", monitor.Status.ExternalID)

	// retry_count is left out: it isn't in the spec, so the value set in Upbot stays
	name := upbotName(monitor)
	updateRequest := upbot.UpdateTheSpecifiedResourceInStorageRequest{
		Name:     &name,
		Type:     &monitor.Spec.Type,
		Target:   *upbot.NewNullableString(&monitor.Spec.Target),
		Interval: &monitor.Spec.Interval,
	}

	httpResp, err := r.ApiClient.MonitorManagementAPI.UpdateTheSpecifiedResourceInStorage(ctx, monitor.Status.ExternalID).
		UpdateTheSpecifiedResourceInStorageRequest(updateRequest).Execute()
	if err != nil {
		if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
			// Deleted in Upbot: forget the ID so the next reconcile creates it again
			logger.Info("Monitor no longer exists in Upbot, creating it again", "externalID", monitor.Status.ExternalID)
			if err := r.patchStatus(ctx, monitor, func(m *monitoringv1alpha1.Monitor) {
				m.Status.ExternalID = ""
				m.Status.ObservedGeneration = 0
				setReadyCondition(m, metav1.ConditionFalse, "NotFoundInUpbot", "The monitor was deleted in Upbot and will be created again")
			}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		return r.handleAPIError(ctx, monitor, "UpdateFailed", httpResp, err)
	}

	if err := r.patchStatus(ctx, monitor, func(m *monitoringv1alpha1.Monitor) {
		m.Status.ObservedGeneration = m.Generation
		setReadyCondition(m, metav1.ConditionTrue, "Synced", "The monitor is in Upbot")
	}); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Updated monitor in Upbot", "externalID", monitor.Status.ExternalID)
	return ctrl.Result{}, nil
}

// handleAPIError shows the API error on the Ready condition and picks when to try again
func (r *MonitorReconciler) handleAPIError(ctx context.Context, monitor *monitoringv1alpha1.Monitor, reason string, httpResp *http.Response, err error) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Error(err, "Upbot API call failed", "reason", reason, "body", apiErrorBody(err))

	message := err.Error()
	if body := apiErrorBody(err); body != "" {
		message = fmt.Sprintf("%s: %s", message, body)
	}
	if len(message) > maxErrorMessage {
		message = message[:maxErrorMessage]
	}
	r.setReady(ctx, monitor, metav1.ConditionFalse, reason, message)

	// No response: network error, retry with the controller's backoff
	if httpResp == nil {
		return ctrl.Result{}, err
	}

	switch {
	case httpResp.StatusCode == http.StatusTooManyRequests:
		return ctrl.Result{RequeueAfter: retryAfter(httpResp)}, nil
	case httpResp.StatusCode >= 400 && httpResp.StatusCode < 500:
		return ctrl.Result{RequeueAfter: clientErrorRequeue}, nil
	default:
		return ctrl.Result{}, err
	}
}

func apiErrorBody(err error) string {
	var apiErr *upbot.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		return string(apiErr.Body())
	}
	return ""
}

func retryAfter(httpResp *http.Response) time.Duration {
	if seconds, err := strconv.Atoi(httpResp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return rateLimitRequeue
}

func setReadyCondition(monitor *monitoringv1alpha1.Monitor, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&monitor.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: monitor.Generation,
	})
}

// setReady writes the Ready condition, a failure is only logged since the caller already reports an error
func (r *MonitorReconciler) setReady(ctx context.Context, monitor *monitoringv1alpha1.Monitor, status metav1.ConditionStatus, reason, message string) {
	if err := r.patchStatus(ctx, monitor, func(m *monitoringv1alpha1.Monitor) {
		setReadyCondition(m, status, reason, message)
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update Monitor status")
	}
}

// patchStatus merge-patches the status without a resourceVersion, so a concurrent spec change can't make it
// conflict, and retries transient errors: losing the ID of a monitor just created would create it twice
func (r *MonitorReconciler) patchStatus(ctx context.Context, monitor *monitoringv1alpha1.Monitor, mutate func(*monitoringv1alpha1.Monitor)) error {
	return retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return !apierrors.IsNotFound(err)
	}, func() error {
		base := monitor.DeepCopy()
		mutate(monitor)
		return r.Status().Patch(ctx, monitor, client.MergeFrom(base))
	})
}

func (r *MonitorReconciler) handleDeletion(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(monitor, monitorFinalizer) {
		return ctrl.Result{}, nil
	}

	if monitor.Status.ExternalID != "" {
		logger.Info("Deleting monitor from Upbot", "externalID", monitor.Status.ExternalID)

		_, httpResp, err := r.ApiClient.MonitorManagementAPI.DeleteASpecificMonitor(ctx, monitor.Status.ExternalID).Execute()
		if err != nil {
			if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
				logger.Info("Monitor already deleted in Upbot", "externalID", monitor.Status.ExternalID)
			} else {
				return r.handleAPIError(ctx, monitor, "DeleteFailed", httpResp, err)
			}
		} else {
			logger.Info("Deleted monitor from Upbot", "externalID", monitor.Status.ExternalID)
		}
	}

	controllerutil.RemoveFinalizer(monitor, monitorFinalizer)
	if err := r.Update(ctx, monitor); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
