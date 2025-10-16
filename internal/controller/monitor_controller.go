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
	"net/http"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1alpha1 "github.com/upbothq/operator/api/v1alpha1"
	"github.com/upbothq/upbot-go-sdk"
)

const monitorFinalizer = "monitoring.upbot.app/finalizer"

// MonitorReconciler reconciles a Monitor object
type MonitorReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	ApiClient *upbot.APIClient
}

// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Monitor object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *MonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Reconciling Monitor", "name", req.NamespacedName)

	var monitor monitoringv1alpha1.Monitor
	if err := r.Get(ctx, req.NamespacedName, &monitor); err != nil {
		// The Monitor resource may have been deleted after the reconcile request.
		// In this case, we don't need to requeue the request.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !monitor.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &monitor)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(&monitor, monitorFinalizer) {
		controllerutil.AddFinalizer(&monitor, monitorFinalizer)
		if err := r.Update(ctx, &monitor); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Added finalizer to monitor")
		return ctrl.Result{}, nil
	}

	return r.handleCreateOrUpdate(ctx, &monitor)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1alpha1.Monitor{}).
		Named("monitor").
		Complete(r)
}

func (r *MonitorReconciler) handleCreateOrUpdate(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Check if monitor already exists in Upbot (has ExternalID)
	if monitor.Status.ExternalID != "" {
		logger.Info("Monitor already exists in Upbot", "externalID", monitor.Status.ExternalID)
		return r.handleUpdate(ctx, monitor)
	}

	// Monitor doesn't exist in Upbot, create it
	logger.Info("Creating monitor in Upbot", "name", monitor.Name)

	val := int32(0)
	newMonitor := upbot.StoreANewlyCreatedResourceInStorageRequest{
		Name:       &monitor.Name,
		Type:       monitor.Spec.Type,
		Target:     &monitor.Spec.Target,
		Interval:   &monitor.Spec.Interval,
		RetryCount: *upbot.NewNullableInt32(&val),
	}

	// Call API to create monitor
	req := r.ApiClient.MonitorManagementAPI.StoreANewlyCreatedResourceInStorage(ctx)
	resp, _, err := req.StoreANewlyCreatedResourceInStorageRequest(newMonitor).Execute()
	if err != nil {
		logger.Error(err, "Failed to create monitor in Upbot")
		return ctrl.Result{}, err
	}

	// Update the status with the external ID
	if resp != nil && resp.Id != nil {
		monitor.Status.ExternalID = *resp.Id
		if err := r.Status().Update(ctx, monitor); err != nil {
			logger.Error(err, "Failed to update Monitor status with external ID")
			return ctrl.Result{}, err
		}
		logger.Info("Created monitor in Upbot and updated status", "externalID", *resp.Id)
	}

	return ctrl.Result{}, nil
}

func (r *MonitorReconciler) handleUpdate(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Perform optimistic update since there's no direct "get specific monitor" method in the SDK
	logger.Info("Updating monitor in Upbot", "externalID", monitor.Status.ExternalID)

	val := int32(0)
	updateRequest := upbot.UpdateTheSpecifiedResourceInStorageRequest{
		Name:       &monitor.Name,
		Type:       &monitor.Spec.Type,
		Target:     *upbot.NewNullableString(&monitor.Spec.Target),
		Interval:   &monitor.Spec.Interval,
		RetryCount: *upbot.NewNullableInt32(&val),
	}

	req := r.ApiClient.MonitorManagementAPI.UpdateTheSpecifiedResourceInStorage(ctx, monitor.Status.ExternalID)
	_, err := req.UpdateTheSpecifiedResourceInStorageRequest(updateRequest).Execute()
	if err != nil {
		logger.Error(err, "Failed to update monitor in Upbot", "externalID", monitor.Status.ExternalID)

		// Check if monitor was deleted externally by trying to parse the error
		// This is a simplified approach - in production you might want more robust error handling
		if httpErr, ok := err.(*upbot.GenericOpenAPIError); ok {
			// If we can't update, it might be because the monitor was deleted externally
			// For now, we'll log the error and continue
			logger.Info("Update failed, monitor might have been deleted externally", "error", httpErr.Error())
			// Optionally clear the external ID and recreate:
			// monitor.Status.ExternalID = ""
			// return ctrl.Result{Requeue: true}, r.Status().Update(ctx, monitor)
		}

		return ctrl.Result{}, err
	}

	logger.Info("Successfully updated monitor in Upbot", "externalID", monitor.Status.ExternalID)
	return ctrl.Result{}, nil
}

func (r *MonitorReconciler) handleDeletion(ctx context.Context, monitor *monitoringv1alpha1.Monitor) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Check if our finalizer is present
	if !controllerutil.ContainsFinalizer(monitor, monitorFinalizer) {
		logger.Info("Finalizer not found, nothing to do")
		return ctrl.Result{}, nil
	}

	// Delete from external system if ExternalID exists
	if monitor.Status.ExternalID != "" {
		logger.Info("Deleting monitor from Upbot", "externalID", monitor.Status.ExternalID)

		_, httpResp, err := r.ApiClient.MonitorManagementAPI.DeleteASpecificMonitor(ctx, monitor.Status.ExternalID).Execute()
		if err != nil {
			// Check if it's a 404 error (monitor already deleted)
			if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
				logger.Info("Monitor already deleted in Upbot", "externalID", monitor.Status.ExternalID)
			} else {
				logger.Error(err, "Failed to delete monitor in Upbot", "externalID", monitor.Status.ExternalID)
				return ctrl.Result{}, err
			}
		} else {
			logger.Info("Successfully deleted monitor from Upbot", "externalID", monitor.Status.ExternalID)
		}
	}

	// Remove our finalizer to allow the object to be deleted
	controllerutil.RemoveFinalizer(monitor, monitorFinalizer)
	if err := r.Update(ctx, monitor); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("Removed finalizer, monitor will be deleted")
	return ctrl.Result{}, nil
}
