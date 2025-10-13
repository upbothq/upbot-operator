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
			// Return and don't requeue
			logger.Info("Ingress resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get Ingress")
		return ctrl.Result{}, err
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

	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) createMonitorFromIngress(ctx context.Context, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Creating Monitor for Ingress", "ingress", ingress.Name, "namespace", ingress.Namespace)

	target, err := r.getTargetFromIngress(ingress)
	if err != nil {
		logger.Error(err, "Failed to get target from Ingress", "ingress", ingress.Name, "namespace", ingress.Namespace)
		return ctrl.Result{}, err
	}

	interval := r.Interval
	if interval == "" {
		interval = "30" // default fallback
	}

	monitor := &monitoringv1alpha1.Monitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingress.Name,
			Namespace: ingress.Namespace,
			Annotations: map[string]string{
				"upbot.app/auto-generated": "true",
			},
			Labels: map[string]string{
				"upbot.app/source": "ingress-watcher",
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

	return fmt.Sprintf("%s://%s", scheme, rule.Host), nil
}

func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Owns(&monitoringv1alpha1.Monitor{}).
		Named("ingresswatcher").
		Complete(r)
}
