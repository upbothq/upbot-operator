package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1alpha1 "github.com/upbothq/operator/api/v1alpha1"
)

const (
	sourceLabel             = "upbot.app/source"
	sourceIngressWatcher    = "ingress-watcher"
	sourceIngressAnnotation = "upbot.app/source-ingress"
	sourceHostAnnotation    = "upbot.app/source-host"
	// Longest Kubernetes object name
	maxObjectName = 253
)

type IngressWatcherReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Interval string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.upbot.app,resources=monitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile keeps one Monitor per host of the Ingress: it creates the missing ones, updates the ones that
// changed and deletes the ones whose host is gone, or all of them when the Ingress is deleted or opts out.
func (r *IngressWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Reconciling IngressWatcher")

	var monitors monitoringv1alpha1.MonitorList
	if err := r.List(ctx, &monitors, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	owned, others := splitMonitors(monitors.Items, req.NamespacedName)

	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		if apierrors.IsNotFound(err) {
			// The owner reference lets the garbage collector delete them too, this is quicker
			return ctrl.Result{}, r.deleteMonitors(ctx, owned, "Ingress deleted")
		}
		return ctrl.Result{}, err
	}

	if monitoringDisabled(&ingress) {
		return ctrl.Result{}, r.deleteMonitors(ctx, owned, "monitoring disabled by the upbot.app/monitor annotation")
	}

	hosts := monitoredHosts(&ingress)
	if len(hosts) == 0 {
		// Nothing to monitor until the Ingress changes: no error, it would be retried for nothing
		logger.Info("Ingress has no host to monitor (no rules, no host or only wildcard hosts), skipping")
		return ctrl.Result{}, r.deleteMonitors(ctx, owned, "no host to monitor")
	}

	// Monitors created by hand already check these URLs: don't monitor them twice
	manualTargets := map[string]bool{}
	for _, monitor := range others {
		manualTargets[monitor.Spec.Target] = true
	}

	takenNames := map[string]bool{}
	for _, monitor := range monitors.Items {
		takenNames[monitor.Name] = true
	}

	interval := r.intervalFor(ctx, &ingress)
	byHost := map[string]*monitoringv1alpha1.Monitor{}
	for i := range owned {
		if host := monitorHost(&owned[i]); host != "" {
			byHost[host] = &owned[i]
		}
	}

	wanted := map[string]bool{}
	for _, host := range hosts {
		target := targetForHost(&ingress, host)
		if manualTargets[target] {
			logger.Info("A Monitor created by hand already checks this URL, skipping", "target", target)
			continue
		}
		wanted[host] = true

		if existing, ok := byHost[host]; ok {
			if err := r.updateMonitor(ctx, existing, host, target, interval); err != nil {
				return ctrl.Result{}, err
			}
			continue
		}

		name := monitorNameFor(ingress.Name, host, takenNames)
		takenNames[name] = true
		if err := r.createMonitor(ctx, &ingress, name, host, target, interval); err != nil {
			return ctrl.Result{}, err
		}
	}

	var stale []monitoringv1alpha1.Monitor
	for _, monitor := range owned {
		if !wanted[monitorHost(&monitor)] {
			stale = append(stale, monitor)
		}
	}
	return ctrl.Result{}, r.deleteMonitors(ctx, stale, "host no longer in the Ingress")
}

// splitMonitors separates the Monitors the watcher created for this Ingress from the others
func splitMonitors(monitors []monitoringv1alpha1.Monitor, ingress client.ObjectKey) (owned, others []monitoringv1alpha1.Monitor) {
	source := fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name)
	for _, monitor := range monitors {
		switch {
		case monitor.Labels[sourceLabel] != sourceIngressWatcher:
			others = append(others, monitor)
		case monitor.Annotations[sourceIngressAnnotation] == source:
			owned = append(owned, monitor)
		}
	}
	return owned, others
}

// monitoredHosts lists the hosts of the Ingress rules once each, in order. A wildcard host has no URL to check.
func monitoredHosts(ingress *networkingv1.Ingress) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, rule := range ingress.Spec.Rules {
		host := strings.ToLower(rule.Host)
		if host == "" || strings.HasPrefix(host, "*") || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

// monitorHost is the host a watcher Monitor checks. Monitors from before one Monitor per host have no
// annotation: their target gives it.
func monitorHost(monitor *monitoringv1alpha1.Monitor) string {
	if host := monitor.Annotations[sourceHostAnnotation]; host != "" {
		return host
	}
	if parsed, err := url.Parse(monitor.Spec.Target); err == nil {
		return strings.ToLower(parsed.Hostname())
	}
	return ""
}

// monitorNameFor keeps the Ingress name for its first host, so an Ingress with one host still has a Monitor
// of the same name, and adds a hash of the host for the others
func monitorNameFor(ingressName, host string, taken map[string]bool) string {
	if !taken[ingressName] {
		return ingressName
	}
	sum := sha256.Sum256([]byte(host))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	prefix := ingressName
	if len(prefix)+len(suffix) > maxObjectName {
		prefix = strings.TrimRight(prefix[:maxObjectName-len(suffix)], "-.")
	}
	return prefix + suffix
}

func (r *IngressWatcherReconciler) createMonitor(ctx context.Context, ingress *networkingv1.Ingress, name, host, target, interval string) error {
	logger := log.FromContext(ctx)

	monitor := &monitoringv1alpha1.Monitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ingress.Namespace,
			Annotations: map[string]string{
				"upbot.app/auto-generated": "true",
				sourceIngressAnnotation:    fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name),
				sourceHostAnnotation:       host,
			},
			Labels: map[string]string{
				sourceLabel:             sourceIngressWatcher,
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
		return err
	}

	if err := r.Create(ctx, monitor); err != nil {
		logger.Error(err, "Failed to create Monitor", "monitor", name)
		return err
	}

	logger.Info("Created Monitor for Ingress host", "monitor", name, "target", target)
	return nil
}

func (r *IngressWatcherReconciler) updateMonitor(ctx context.Context, monitor *monitoringv1alpha1.Monitor, host, target, interval string) error {
	if monitor.Spec.Target == target && monitor.Spec.Interval == interval && monitor.Spec.Type == "http" &&
		monitor.Annotations[sourceHostAnnotation] == host {
		return nil
	}

	monitor.Spec.Target = target
	monitor.Spec.Interval = interval
	monitor.Spec.Type = "http"
	if monitor.Annotations == nil {
		monitor.Annotations = map[string]string{}
	}
	monitor.Annotations[sourceHostAnnotation] = host

	if err := r.Update(ctx, monitor); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update Monitor", "monitor", monitor.Name)
		return err
	}

	log.FromContext(ctx).Info("Updated Monitor for Ingress host", "monitor", monitor.Name, "target", target, "interval", interval)
	return nil
}

func (r *IngressWatcherReconciler) deleteMonitors(ctx context.Context, monitors []monitoringv1alpha1.Monitor, reason string) error {
	for i := range monitors {
		if err := r.Delete(ctx, &monitors[i]); client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Error(err, "Failed to delete Monitor", "monitor", monitors[i].Name)
			return err
		}
		log.FromContext(ctx).Info("Deleted Monitor", "monitor", monitors[i].Name, "reason", reason)
	}
	return nil
}

// monitoringDisabled reads the upbot.app/monitor annotation: any false boolean ("false", "0", "False", "f"...)
// or "disabled" turns monitoring off, anything else leaves it on
func monitoringDisabled(ingress *networkingv1.Ingress) bool {
	value := strings.TrimSpace(ingress.Annotations["upbot.app/monitor"])
	if strings.EqualFold(value, "disabled") {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && !enabled
}

// hostHasTLS tells if one of the Ingress TLS entries covers the host. An entry without hosts uses the
// controller's default certificate for every host, a wildcard covers one label ("*.example.com" covers
// "api.example.com" only).
func hostHasTLS(ingress *networkingv1.Ingress, host string) bool {
	for _, tls := range ingress.Spec.TLS {
		if len(tls.Hosts) == 0 {
			return true
		}
		for _, tlsHost := range tls.Hosts {
			if strings.EqualFold(tlsHost, host) {
				return true
			}
			if suffix, ok := strings.CutPrefix(tlsHost, "*."); ok {
				label, rest, found := strings.Cut(host, ".")
				if found && label != "" && strings.EqualFold(rest, suffix) {
					return true
				}
			}
		}
	}
	return false
}

// validIntervals are the intervals the Monitor CRD and the Upbot API accept, in seconds
var validIntervals = map[string]bool{"30": true, "60": true, "120": true, "300": true, "600": true}

// intervalFor returns the upbot.app/interval annotation, else the --ingress-watcher-interval flag, else 30.
// An invalid value is skipped with a log line: the Monitor CRD would refuse it and the Ingress would never get a monitor.
func (r *IngressWatcherReconciler) intervalFor(ctx context.Context, ingress *networkingv1.Ingress) string {
	logger := log.FromContext(ctx)

	if custom := ingress.Annotations["upbot.app/interval"]; custom != "" {
		if validIntervals[custom] {
			return custom
		}
		logger.Info("Ignoring invalid upbot.app/interval annotation, allowed values are 30, 60, 120, 300 and 600", "ingress", ingress.Name, "interval", custom)
	}

	if validIntervals[r.Interval] {
		return r.Interval
	}
	if r.Interval != "" {
		logger.Info("Ignoring invalid --ingress-watcher-interval, allowed values are 30, 60, 120, 300 and 600", "interval", r.Interval)
	}

	return "30"
}

// targetForHost builds the URL to check: https when a TLS entry covers the host, then the upbot.app/path annotation
func targetForHost(ingress *networkingv1.Ingress, host string) string {
	scheme := "http"
	if hostHasTLS(ingress, host) {
		scheme = "https"
	}

	target := fmt.Sprintf("%s://%s", scheme, host)

	if customPath := ingress.Annotations["upbot.app/path"]; customPath != "" {
		// Leading slash added, trailing slash removed unless the path is just "/"
		if customPath[0] != '/' {
			customPath = "/" + customPath
		}
		if len(customPath) > 1 && customPath[len(customPath)-1] == '/' {
			customPath = customPath[:len(customPath)-1]
		}
		target += customPath
	}

	return target
}

func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Owns(&monitoringv1alpha1.Monitor{}).
		Named("ingresswatcher").
		Complete(r)
}
