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
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

	"github.com/upbothq/upbot-go-sdk"
	monitoringv1alpha1 "github.com/upbothq/upbot-operator/api/v1alpha1"
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
	// ResyncPeriod is how often a synced monitor is read back from Upbot to undo changes made there.
	// Zero turns it off: the operator then only calls the API when the spec changes.
	ResyncPeriod time.Duration
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

	// This generation is already in Upbot: only the periodic resync calls the API
	if monitor.Status.ExternalID != "" &&
		monitor.Status.ObservedGeneration == monitor.Generation &&
		meta.IsStatusConditionTrue(monitor.Status.Conditions, conditionReady) {
		return r.resync(ctx, &monitor)
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
		markSynced(m)
	}); err != nil {
		// The next reconcile would create the monitor a second time: log the ID so it can be found
		logger.Error(err, "Created monitor in Upbot but failed to save its ID", "externalID", externalID)
		return ctrl.Result{}, err
	}

	logger.Info("Created monitor in Upbot", "externalID", externalID)
	return ctrl.Result{RequeueAfter: r.nextResync()}, nil
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
			return r.forgetDeletedMonitor(ctx, monitor)
		}

		return r.handleAPIError(ctx, monitor, "UpdateFailed", httpResp, err)
	}

	if err := r.patchStatus(ctx, monitor, markSynced); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Updated monitor in Upbot", "externalID", monitor.Status.ExternalID)
	return ctrl.Result{RequeueAfter: r.nextResync()}, nil
}

// nextResync is the ResyncPeriod plus up to 10% of jitter, so monitors synced together don't all call the API
// in the same second. Zero when the resync is off.
func (r *MonitorReconciler) nextResync() time.Duration {
	if r.ResyncPeriod <= 0 {
		return 0
	}
	return r.ResyncPeriod + rand.N(r.ResyncPeriod/10+1)
}

// markSynced records that this generation is in Upbot
func markSynced(m *monitoringv1alpha1.Monitor) {
	now := metav1.Now()
	m.Status.ObservedGeneration = m.Generation
	m.Status.LastSyncTime = &now
	setReadyCondition(m, metav1.ConditionTrue, "Synced", "The monitor is in Upbot")
}

// forgetDeletedMonitor clears the ID of a monitor deleted in Upbot, so the next reconcile creates it again
func (r *MonitorReconciler) forgetDeletedMonitor(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Monitor no longer exists in Upbot, creating it again", "externalID", monitor.Status.ExternalID)
	if err := r.patchStatus(ctx, monitor, func(m *monitoringv1alpha1.Monitor) {
		m.Status.ExternalID = ""
		m.Status.ObservedGeneration = 0
		m.Status.LastSyncTime = nil
		setReadyCondition(m, metav1.ConditionFalse, "NotFoundInUpbot", "The monitor was deleted in Upbot and will be created again")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// resync reads a synced monitor back from Upbot once per ResyncPeriod: it creates it again when it was deleted there
// and puts the spec back when it was edited there. Status writes in between make no API call.
func (r *MonitorReconciler) resync(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	if r.ResyncPeriod <= 0 {
		return ctrl.Result{}, nil
	}
	if last := monitor.Status.LastSyncTime; last != nil {
		if wait := r.ResyncPeriod - time.Since(last.Time); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	logger := logf.FromContext(ctx)
	remote, httpResp, err := r.getRemoteMonitor(ctx, monitor.Status.ExternalID)
	if err != nil {
		if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
			return r.forgetDeletedMonitor(ctx, monitor)
		}
		return r.handleAPIError(ctx, monitor, "ResyncFailed", httpResp, err)
	}

	if changed := remote.changedFields(monitor); len(changed) > 0 {
		logger.Info("Monitor was changed in Upbot, restoring the spec", "externalID", monitor.Status.ExternalID, "fields", changed)
		return r.handleUpdate(ctx, monitor)
	}

	if err := r.patchStatus(ctx, monitor, markSynced); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("Monitor in Upbot matches the spec", "externalID", monitor.Status.ExternalID)
	return ctrl.Result{RequeueAfter: r.nextResync()}, nil
}

// remoteMonitor holds the fields of a monitor in Upbot that the spec sets
type remoteMonitor struct {
	DisplayName string  `json:"display_name"`
	Type        string  `json:"type"`
	Target      *string `json:"target"`
	Interval    *int    `json:"interval"`
}

// changedFields lists the spec fields that differ in Upbot
func (remote *remoteMonitor) changedFields(monitor *monitoringv1alpha1.Monitor) []string {
	var changed []string
	if remote.DisplayName != upbotName(monitor) {
		changed = append(changed, "name")
	}
	if remote.Type != monitor.Spec.Type {
		changed = append(changed, "type")
	}
	if remote.Target == nil || *remote.Target != monitor.Spec.Target {
		changed = append(changed, "target")
	}
	if remote.Interval == nil || strconv.Itoa(*remote.Interval) != monitor.Spec.Interval {
		changed = append(changed, "interval")
	}
	return changed
}

// getRemoteMonitor reads a monitor from Upbot. The SDK has no method for GET /api/monitors/{id} yet,
// so it sends the request itself with the SDK's server, headers and HTTP client.
func (r *MonitorReconciler) getRemoteMonitor(ctx context.Context, id string) (*remoteMonitor, *http.Response, error) {
	cfg := r.ApiClient.GetConfig()
	base, err := cfg.ServerURLWithContext(ctx, "MonitorManagementAPIService.UpdateTheSpecifiedResourceInStorage")
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/monitors/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}
	for name, value := range cfg.DefaultHeader {
		req.Header.Set(name, value)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp, fmt.Errorf("GET /api/monitors/%s: %s: %s", id, resp.Status, body)
	}

	var payload struct {
		Data remoteMonitor `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp, fmt.Errorf("decoding the monitor from Upbot: %w", err)
	}
	return &payload.Data, resp, nil
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
