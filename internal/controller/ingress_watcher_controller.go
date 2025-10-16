package controller

import (
	"fmt"

	monitoringv1alpha1 "github.com/upbothq/operator/api/v1alpha1"
	"golang.org/x/net/context"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type IngressWatcherReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Interval string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors,verbs=get;list;watch;create;update;patch;delete

func (r *IngressWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling IngressWatcher")

	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		if errors.IsNotFound(err) {
			// Ingress not found. Could have been deleted after reconcile request.
			// Check if there's an associated monitor that should be cleaned up
			logger.Info("Ingress resource not found, checking for associated monitor to clean up")
			return r.handleIngressDeletion(ctx, req.NamespacedName)
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get Ingress")
		return ctrl.Result{}, err
	}

	// Check if monitoring is disabled for this ingress
	if disabled, exists := ingress.Annotations["upbot.app/monitor"]; exists && (disabled == "false" || disabled == "disabled") {
		logger.Info("Monitoring disabled for this ingress via annotation", "ingress", ingress.Name)
		// Check if there's an existing monitor that should be cleaned up
		return r.handleMonitorCleanupForDisabledIngress(ctx, req.NamespacedName)
	}

	// Check if the Monitor already exists, if not create a new one

	monitorName := req.NamespacedName
	var existingMonitor monitoringv1alpha1.Monitor
	err := r.Get(ctx, monitorName, &existingMonitor)

	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Failed to get Monitor")
		return ctrl.Result{}, err
	}

	if errors.IsNotFound(err) {
		return r.createMonitorFromIngress(ctx, &ingress)
	}

	// Monitor exists, check if it needs to be updated
	return r.updateMonitorIfNeeded(ctx, &existingMonitor, &ingress)
}

func (r *IngressWatcherReconciler) createMonitorFromIngress(ctx context.Context, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Creating Monitor for Ingress", "ingress", ingress.Name, "namespace", ingress.Namespace)

	target, err := r.getTargetFromIngress(ingress)
	if err != nil {
		logger.Error(err, "Failed to get target from Ingress", "ingress", ingress.Name, "namespace", ingress.Namespace)
		return ctrl.Result{}, err
	}

	// Check for custom interval annotation first, then fall back to global setting
	interval := r.Interval
	if customInterval, exists := ingress.Annotations["upbot.app/interval"]; exists && customInterval != "" {
		interval = customInterval
	} else if interval == "" {
		interval = "30" // default fallback
	}

	monitor := &monitoringv1alpha1.Monitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingress.Name,
			Namespace: ingress.Namespace,
			Annotations: map[string]string{
				"upbot.app/auto-generated": "true",
				"upbot.app/source-ingress": fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name),
			},
			Labels: map[string]string{
				"upbot.app/source":      "ingress-watcher",
				"upbot.app/target-type": "http",
			},
		},
		Spec: monitoringv1alpha1.MonitorSpec{
			Type:     "http",
			Target:   target,
			Interval: interval,
		},
	}

	if err := ctrl.SetControllerReference(ingress, monitor, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference", "ingress", ingress.Name, "namespace", ingress.Namespace)
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, monitor); err != nil {
		logger.Error(err, "Failed to create Monitor", "monitor", monitor.Name, "namespace", monitor.Namespace)
		return ctrl.Result{}, err
	}
	logger.Info("Successfully created Monitor", "monitor", monitor.Name, "namespace", monitor.Namespace)

	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) updateMonitorIfNeeded(ctx context.Context, monitor *monitoringv1alpha1.Monitor, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	
	// Check if this monitor was created by the ingress watcher
	if monitor.Labels["upbot.app/source"] != "ingress-watcher" {
		logger.Info("Monitor not created by ingress watcher, skipping update", "monitor", monitor.Name)
		return ctrl.Result{}, nil
	}

	needsUpdate := false
	
	// Get the current target from ingress
	expectedTarget, err := r.getTargetFromIngress(ingress)
	if err != nil {
		logger.Error(err, "Failed to get target from Ingress", "ingress", ingress.Name)
		return ctrl.Result{}, err
	}
	
	// Get the expected interval (check for custom annotation first)
	expectedInterval := r.Interval
	if customInterval, exists := ingress.Annotations["upbot.app/interval"]; exists && customInterval != "" {
		expectedInterval = customInterval
	} else if expectedInterval == "" {
		expectedInterval = "30" // default fallback
	}
	
	// Check if target needs update
	if monitor.Spec.Target != expectedTarget {
		logger.Info("Target mismatch, updating monitor", "monitor", monitor.Name, "current", monitor.Spec.Target, "expected", expectedTarget)
		monitor.Spec.Target = expectedTarget
		needsUpdate = true
	}
	
	// Check if interval needs update
	if monitor.Spec.Interval != expectedInterval {
		logger.Info("Interval mismatch, updating monitor", "monitor", monitor.Name, "current", monitor.Spec.Interval, "expected", expectedInterval)
		monitor.Spec.Interval = expectedInterval
		needsUpdate = true
	}
	
	// Check if type needs update
	if monitor.Spec.Type != "http" {
		logger.Info("Type mismatch, updating monitor", "monitor", monitor.Name, "current", monitor.Spec.Type, "expected", "http")
		monitor.Spec.Type = "http"
		needsUpdate = true
	}
	
	if needsUpdate {
		if err := r.Update(ctx, monitor); err != nil {
			logger.Error(err, "Failed to update Monitor", "monitor", monitor.Name)
			return ctrl.Result{}, err
		}
		logger.Info("Successfully updated Monitor", "monitor", monitor.Name)
	} else {
		logger.Info("Monitor is up to date", "monitor", monitor.Name)
	}
	
	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) handleIngressDeletion(ctx context.Context, namespacedName client.ObjectKey) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	
	// Try to find the monitor associated with this ingress
	var monitor monitoringv1alpha1.Monitor
	err := r.Get(ctx, namespacedName, &monitor)
	
	if errors.IsNotFound(err) {
		// No monitor found, nothing to clean up
		logger.Info("No associated monitor found for deleted ingress", "ingress", namespacedName)
		return ctrl.Result{}, nil
	}
	
	if err != nil {
		logger.Error(err, "Failed to get monitor for deleted ingress", "ingress", namespacedName)
		return ctrl.Result{}, err
	}
	
	// Check if this monitor was created by the ingress watcher
	if monitor.Labels["upbot.app/source"] != "ingress-watcher" {
		logger.Info("Monitor not created by ingress watcher, not cleaning up", "monitor", monitor.Name)
		return ctrl.Result{}, nil
	}
	
	// Check if this monitor was created for this specific ingress
	expectedSourceAnnotation := fmt.Sprintf("%s/%s", namespacedName.Namespace, namespacedName.Name)
	if monitor.Annotations["upbot.app/source-ingress"] != expectedSourceAnnotation {
		logger.Info("Monitor not associated with this ingress, not cleaning up", "monitor", monitor.Name, "expected", expectedSourceAnnotation, "actual", monitor.Annotations["upbot.app/source-ingress"])
		return ctrl.Result{}, nil
	}
	
	// Delete the monitor
	logger.Info("Deleting monitor for deleted ingress", "monitor", monitor.Name, "ingress", namespacedName)
	if err := r.Delete(ctx, &monitor); err != nil {
		logger.Error(err, "Failed to delete monitor", "monitor", monitor.Name)
		return ctrl.Result{}, err
	}
	
	logger.Info("Successfully deleted monitor for deleted ingress", "monitor", monitor.Name, "ingress", namespacedName)
	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) handleMonitorCleanupForDisabledIngress(ctx context.Context, namespacedName client.ObjectKey) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	
	// Try to find the monitor associated with this ingress
	var monitor monitoringv1alpha1.Monitor
	err := r.Get(ctx, namespacedName, &monitor)
	
	if errors.IsNotFound(err) {
		// No monitor found, nothing to clean up
		logger.Info("No monitor found for disabled ingress", "ingress", namespacedName)
		return ctrl.Result{}, nil
	}
	
	if err != nil {
		logger.Error(err, "Failed to get monitor for disabled ingress", "ingress", namespacedName)
		return ctrl.Result{}, err
	}
	
	// Check if this monitor was created by the ingress watcher
	if monitor.Labels["upbot.app/source"] != "ingress-watcher" {
		logger.Info("Monitor not created by ingress watcher, not cleaning up", "monitor", monitor.Name)
		return ctrl.Result{}, nil
	}
	
	// Delete the monitor since monitoring is disabled
	logger.Info("Deleting monitor for disabled ingress", "monitor", monitor.Name, "ingress", namespacedName)
	if err := r.Delete(ctx, &monitor); err != nil {
		logger.Error(err, "Failed to delete monitor for disabled ingress", "monitor", monitor.Name)
		return ctrl.Result{}, err
	}
	
	logger.Info("Successfully deleted monitor for disabled ingress", "monitor", monitor.Name, "ingress", namespacedName)
	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) getTargetFromIngress(ingress *networkingv1.Ingress) (string, error) {
	if len(ingress.Spec.Rules) == 0 {
		return "", fmt.Errorf("no rules found in Ingress")
	}

	rule := ingress.Spec.Rules[0]
	if rule.Host == "" {
		return "", fmt.Errorf("no host found in Ingress rule")
	}

	scheme := "https"
	if len(ingress.Spec.TLS) == 0 {
		scheme = "http"
	}

	// Start with base URL
	target := fmt.Sprintf("%s://%s", scheme, rule.Host)
	
	// Check for custom path annotation
	if customPath, exists := ingress.Annotations["upbot.app/path"]; exists && customPath != "" {
		// Clean up the path - ensure it starts with / and handle trailing slashes
		if customPath[0] != '/' {
			customPath = "/" + customPath
		}
		// Remove trailing slash unless it's just "/"
		if len(customPath) > 1 && customPath[len(customPath)-1] == '/' {
			customPath = customPath[:len(customPath)-1]
		}
		target += customPath
	}

	return target, nil
}

func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Owns(&monitoringv1alpha1.Monitor{}).
		Named("ingresswatcher").
		Complete(r)
}
