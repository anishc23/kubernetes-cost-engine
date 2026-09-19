// Package kube discovers Kubernetes workloads and their declared resources.
//
// Discovery is deliberately controller-centric rather than pod-centric. A
// resource request lives in a pod template, so a recommendation that is not
// expressed against a controller cannot be applied: telling an operator to
// change one pod's request is advice about an object that will be replaced on
// the next rollout.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

// Discoverer enumerates workloads. It is an interface so the API layer and the
// tests can run against a fake cluster.
type Discoverer interface {
	Discover(ctx context.Context) ([]model.Workload, error)
}

// Options configures discovery.
type Options struct {
	// Namespaces restricts discovery. Empty means all namespaces the service
	// account can list.
	Namespaces []string
	// ExcludeNamespaces filters out namespaces even when they are visible.
	// System namespaces are excluded by default: right-sizing kube-system is a
	// different and considerably riskier problem than right-sizing application
	// workloads, and the engine's heuristics are not calibrated for it.
	ExcludeNamespaces []string
	// LabelSelector narrows discovery to labelled workloads.
	LabelSelector string
	// IncludeDaemonSets controls whether DaemonSets are analysed. They are
	// excluded by default because a DaemonSet's request is multiplied across
	// every node, so the cost consequences of a change differ in kind, and
	// because node-agent workloads are frequently the ones whose degradation is
	// least tolerable.
	IncludeDaemonSets bool
}

// DefaultOptions returns the conservative discovery defaults.
func DefaultOptions() Options {
	return Options{
		ExcludeNamespaces: []string{"kube-system", "kube-public", "kube-node-lease"},
		IncludeDaemonSets: false,
	}
}

// Client discovers workloads from a live cluster.
type Client struct {
	cs      kubernetes.Interface
	opts    Options
	log     *slog.Logger
	metrics *metrics.Metrics
}

// NewClient builds a discoverer from a kubeconfig path, or from the in-cluster
// service account when kubeconfig is empty.
func NewClient(kubeconfig string, opts Options, log *slog.Logger, m *metrics.Metrics) (*Client, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	// Client-side rate limits are raised above the defaults because a discovery
	// pass issues a burst of list calls at once and then goes quiet for the whole
	// analysis interval; the default 5 QPS would serialise that burst for no
	// benefit. They remain bounded so a bug cannot flood the API server.
	cfg.QPS = 50
	cfg.Burst = 100
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return NewClientWithInterface(cs, opts, log, m), nil
}

// NewClientWithInterface builds a discoverer from an existing clientset, which
// is how the tests inject a fake.
func NewClientWithInterface(cs kubernetes.Interface, opts Options, log *slog.Logger, m *metrics.Metrics) *Client {
	return &Client{cs: cs, opts: opts, log: log, metrics: m}
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to the default kubeconfig loading rules so the CLI works
		// from a developer laptop without a flag.
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
		cfg, err2 := cc.ClientConfig()
		if err2 != nil {
			return nil, fmt.Errorf("no in-cluster config (%v) and no usable kubeconfig: %w", err, err2)
		}
		return cfg, nil
	}
	return cfg, nil
}

// Discover enumerates workloads and their declared resources.
//
// Pods are listed once and indexed by owner rather than listed per workload,
// because the pod list is the expensive call and issuing one per Deployment
// would make discovery O(workloads) API requests on a cluster where one request
// suffices.
func (c *Client) Discover(ctx context.Context) ([]model.Workload, error) {
	namespaces, err := c.targetNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Workload
	for _, ns := range namespaces {
		ws, err := c.discoverNamespace(ctx, ns)
		if err != nil {
			// One unreadable namespace must not abort the whole pass: partial
			// results with a logged error are more useful than none, and RBAC
			// commonly grants access to a subset.
			c.log.Error("namespace discovery failed", "namespace", ns, "error", err)
			if c.metrics != nil {
				c.metrics.KubeAPIErrors.WithLabelValues("discover_namespace").Inc()
			}
			continue
		}
		out = append(out, ws...)
	}
	if c.metrics != nil {
		c.metrics.WorkloadsDiscovered.Set(float64(len(out)))
	}
	return out, nil
}

func (c *Client) targetNamespaces(ctx context.Context) ([]string, error) {
	if len(c.opts.Namespaces) > 0 {
		return c.filterExcluded(c.opts.Namespaces), nil
	}
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		if c.metrics != nil {
			c.metrics.KubeAPIErrors.WithLabelValues("list_namespaces").Inc()
		}
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	return c.filterExcluded(names), nil
}

func (c *Client) filterExcluded(names []string) []string {
	excluded := map[string]bool{}
	for _, e := range c.opts.ExcludeNamespaces {
		excluded[e] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !excluded[n] {
			out = append(out, n)
		}
	}
	return out
}

func (c *Client) discoverNamespace(ctx context.Context, ns string) ([]model.Workload, error) {
	opts := metav1.ListOptions{LabelSelector: c.opts.LabelSelector}

	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", ns, err)
	}
	evidence := buildEvidence(pods.Items)

	var out []model.Workload

	deployments, err := c.cs.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		out = append(out, c.fromPodTemplate(
			ns, d.Name, model.KindDeployment, replicasOf(d.Spec.Replicas),
			d.Labels, d.Spec.Template, evidence))
	}

	statefulSets, err := c.cs.AppsV1().StatefulSets(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		out = append(out, c.fromPodTemplate(
			ns, s.Name, model.KindStatefulSet, replicasOf(s.Spec.Replicas),
			s.Labels, s.Spec.Template, evidence))
	}

	if c.opts.IncludeDaemonSets {
		daemonSets, err := c.cs.AppsV1().DaemonSets(ns).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
		}
		for i := range daemonSets.Items {
			d := &daemonSets.Items[i]
			// A DaemonSet's replica count is the number of nodes it is scheduled
			// on, which is the number that determines its cost.
			out = append(out, c.fromPodTemplate(
				ns, d.Name, model.KindDaemonSet, d.Status.DesiredNumberScheduled,
				d.Labels, d.Spec.Template, evidence))
		}
	}

	cronJobs, err := c.cs.BatchV1().CronJobs(ns).List(ctx, opts)
	if err != nil {
		// CronJobs are optional: a cluster may not grant access to batch
		// resources, and that should not fail the whole namespace.
		c.log.Debug("listing cronjobs failed", "namespace", ns, "error", err)
	} else {
		for i := range cronJobs.Items {
			cj := &cronJobs.Items[i]
			out = append(out, c.fromPodTemplate(
				ns, cj.Name, model.KindCronJob, 1,
				cj.Labels, cj.Spec.JobTemplate.Spec.Template, evidence))
		}
	}
	return out, nil
}

func replicasOf(r *int32) int32 {
	if r == nil {
		// An unset replica count means the default of one, not zero.
		return 1
	}
	return *r
}

// fromPodTemplate converts a pod template into a workload with its declared
// resources and the reliability evidence gathered from its running pods.
func (c *Client) fromPodTemplate(
	ns, name string, kind model.WorkloadKind, replicas int32,
	labels map[string]string, tpl corev1.PodTemplateSpec,
	evidence map[string]map[string]model.RestartEvidence,
) model.Workload {
	w := model.Workload{
		Namespace: ns, Name: name, Kind: kind, Replicas: replicas, Labels: labels,
		Evidence: map[string]model.RestartEvidence{},
	}
	// Init containers are excluded: their requests affect scheduling (the pod's
	// effective request is the maximum of the init and sum of app requests) but
	// they run briefly at startup, and their usage series is almost entirely
	// empty. Sizing them from a usage percentile would produce confident advice
	// from no data.
	for _, ctr := range tpl.Spec.Containers {
		w.Containers = append(w.Containers, model.Container{
			Name:     ctr.Name,
			Declared: declaredFrom(ctr.Resources),
		})
		if byContainer, ok := evidence[podOwnerKey(ns, name)]; ok {
			if ev, ok := byContainer[ctr.Name]; ok {
				w.Evidence[ctr.Name] = ev
			}
		}
	}
	return w
}

func declaredFrom(r corev1.ResourceRequirements) model.ResourceRequests {
	out := model.ResourceRequests{}
	if q, ok := r.Requests[corev1.ResourceCPU]; ok {
		out.CPURequest = quantity.CPUFromQuantity(q)
	}
	if q, ok := r.Requests[corev1.ResourceMemory]; ok {
		out.MemoryRequest = quantity.MemoryFromQuantity(q)
	}
	if q, ok := r.Limits[corev1.ResourceCPU]; ok {
		v := quantity.CPUFromQuantity(q)
		out.CPULimit = &v
	}
	if q, ok := r.Limits[corev1.ResourceMemory]; ok {
		v := quantity.MemoryFromQuantity(q)
		out.MemoryLimit = &v
	}
	return out
}

// buildEvidence indexes restart and OOM history by workload and container.
//
// OOMKills are counted from the *last terminated* state of each container,
// which is where the kubelet records why a container was previously killed.
// Counting total restarts would conflate OOMKills with crashes, config errors
// and liveness-probe failures — and only the OOM subset is evidence about memory
// sizing.
func buildEvidence(pods []corev1.Pod) map[string]map[string]model.RestartEvidence {
	out := map[string]map[string]model.RestartEvidence{}
	for i := range pods {
		pod := &pods[i]
		owner, ok := resolveOwner(pod)
		if !ok {
			continue
		}
		key := podOwnerKey(pod.Namespace, owner)
		if out[key] == nil {
			out[key] = map[string]model.RestartEvidence{}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			ev := out[key][cs.Name]
			ev.Restarts += int(cs.RestartCount)
			if term := cs.LastTerminationState.Terminated; term != nil {
				if isOOM(term) {
					ev.OOMKills++
					t := term.FinishedAt.Time
					if ev.LastOOM == nil || t.After(*ev.LastOOM) {
						ev.LastOOM = &t
					}
				}
			}
			out[key][cs.Name] = ev
		}
	}
	return out
}

func isOOM(t *corev1.ContainerStateTerminated) bool {
	// Reason is the reliable signal; exit code 137 (128+SIGKILL) is checked as a
	// fallback because some runtimes report the kill without setting the reason.
	// 137 alone is not conclusive — any SIGKILL produces it — so it is only
	// trusted when the reason is empty.
	if strings.EqualFold(t.Reason, "OOMKilled") {
		return true
	}
	return t.Reason == "" && t.ExitCode == 137
}

// ownerPattern matches the generated pod-name suffixes that Kubernetes appends,
// used to resolve a pod back to its controller when the owner chain is
// incomplete.
var replicaSetSuffix = regexp.MustCompile(`-[a-z0-9]{6,10}$`)

// resolveOwner walks a pod's owner references to the top-level controller name.
//
// The important case is the two-level chain Pod -> ReplicaSet -> Deployment. The
// ReplicaSet's own owner reference is not visible from the pod, so the
// Deployment name is recovered by stripping the ReplicaSet's pod-template-hash
// suffix. That is a string operation on a name, which is fragile, so it is
// confined to this one function and the fallback is explicit: a pod whose owner
// cannot be resolved contributes no evidence rather than evidence attributed to
// the wrong workload.
func resolveOwner(pod *corev1.Pod) (string, bool) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			// ReplicaSet names are "<deployment>-<pod-template-hash>".
			if m := replicaSetSuffix.FindStringIndex(ref.Name); m != nil {
				return ref.Name[:m[0]], true
			}
			return ref.Name, true
		case "StatefulSet", "DaemonSet":
			return ref.Name, true
		case "Job":
			// Jobs created by a CronJob are "<cronjob>-<timestamp>".
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 {
				return ref.Name[:idx], true
			}
			return ref.Name, true
		}
	}
	return "", false
}

func podOwnerKey(ns, name string) string { return ns + "/" + name }

// PodRegexFor builds the pod-name regex used in Prometheus queries for a
// workload.
//
// The pattern is anchored and the workload name is quoted, so that a workload
// named "api" cannot match the pods of "api-gateway". Without the anchor, one
// workload's recommendation would be computed from another's usage.
func PodRegexFor(w model.Workload) string {
	name := regexp.QuoteMeta(w.Name)
	switch w.Kind {
	case model.KindStatefulSet:
		// StatefulSet pods are "<name>-<ordinal>".
		return fmt.Sprintf("^%s-[0-9]+$", name)
	case model.KindDaemonSet:
		return fmt.Sprintf("^%s-[a-z0-9]{5}$", name)
	case model.KindCronJob:
		return fmt.Sprintf("^%s-[0-9]+-[a-z0-9]{5}$", name)
	default:
		// Deployment pods are "<name>-<replicaset hash>-<pod hash>".
		return fmt.Sprintf("^%s-[a-z0-9]+-[a-z0-9]{5}$", name)
	}
}

// ApplyRecommendation patches a workload's resource requests.
//
// This is the only function in the project that writes to the cluster. It is
// never called unless the operator has explicitly enabled --apply, and it patches
// requests only, never limits: changing a memory limit is the single most
// dangerous edit this tool could make, and there is no evidence in the usage
// series that would justify doing it automatically.
func (c *Client) ApplyRecommendation(ctx context.Context, rec model.WorkloadRecommendation) error {
	patch, err := buildResourcePatch(rec)
	if err != nil {
		return err
	}
	if patch == nil {
		return nil // nothing actionable
	}
	switch rec.Kind {
	case model.KindDeployment:
		_, err = c.cs.AppsV1().Deployments(rec.Namespace).Patch(
			ctx, rec.Name, types_StrategicMergePatchType, patch, metav1.PatchOptions{})
	case model.KindStatefulSet:
		_, err = c.cs.AppsV1().StatefulSets(rec.Namespace).Patch(
			ctx, rec.Name, types_StrategicMergePatchType, patch, metav1.PatchOptions{})
	case model.KindDaemonSet:
		_, err = c.cs.AppsV1().DaemonSets(rec.Namespace).Patch(
			ctx, rec.Name, types_StrategicMergePatchType, patch, metav1.PatchOptions{})
	default:
		return fmt.Errorf("applying recommendations to %s is not supported", rec.Kind)
	}
	if err != nil {
		if c.metrics != nil {
			c.metrics.KubeAPIErrors.WithLabelValues("apply").Inc()
		}
		return fmt.Errorf("patch %s %s/%s: %w", rec.Kind, rec.Namespace, rec.Name, err)
	}
	c.log.Info("applied recommendation",
		"kind", rec.Kind, "namespace", rec.Namespace, "name", rec.Name)
	return nil
}

// Healthy verifies the cluster connection.
func (c *Client) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("kubernetes health check: %w", err)
	}
	return nil
}

var _ Discoverer = (*Client)(nil)

// Unused import guard for batchv1/appsv1 referenced only through the clientset.
var (
	_ = appsv1.Deployment{}
	_ = batchv1.CronJob{}
)
