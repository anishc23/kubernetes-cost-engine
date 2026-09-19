package kube

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](v T) *T { return &v }

func deployment(ns, name string, replicas int32, cpu, mem string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      "app",
						Resources: resourceListFor(cpu, mem),
					}},
				},
			},
		},
	}
}

func pod(ns, name, ownerKind, ownerName string, restarts int32, lastTerm *corev1.ContainerStateTerminated) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Name: ownerName}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:                 "app",
				RestartCount:         restarts,
				LastTerminationState: corev1.ContainerState{Terminated: lastTerm},
			}},
		},
	}
}

func TestDiscoverReadsDeclaredRequests(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		deployment("app", "api", 3, "2", "4Gi"),
	)
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	ws, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("discovered %d workloads, want 1", len(ws))
	}
	w := ws[0]
	if w.Key() != "app/api" || w.Kind != model.KindDeployment || w.Replicas != 3 {
		t.Errorf("unexpected workload: %+v", w)
	}
	d := w.Containers[0].Declared
	if d.CPURequest != 2000 {
		t.Errorf("CPU request = %v, want 2000m", d.CPURequest)
	}
	if d.MemoryRequest != model.Bytes(4*model.BytesPerGi) {
		t.Errorf("memory request = %v, want 4Gi", d.MemoryRequest)
	}
}

// A nil replica count means one, not zero: treating it as zero would zero out
// the cost of every workload that omits the field.
func TestNilReplicasMeansOne(t *testing.T) {
	d := deployment("app", "api", 1, "1", "1Gi")
	d.Spec.Replicas = nil
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}, d)
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	ws, _ := c.Discover(context.Background())
	if ws[0].Replicas != 1 {
		t.Errorf("replicas = %d, want 1 for an unset field", ws[0].Replicas)
	}
}

// System namespaces must be excluded by default: right-sizing kube-system is a
// different and riskier problem than right-sizing application workloads.
func TestSystemNamespacesExcludedByDefault(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		deployment("kube-system", "coredns", 2, "100m", "70Mi"),
		deployment("app", "api", 1, "1", "1Gi"),
	)
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	ws, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Namespace == "kube-system" {
			t.Errorf("kube-system workload %s should have been excluded", w.Key())
		}
	}
	if len(ws) != 1 {
		t.Errorf("discovered %d workloads, want 1", len(ws))
	}
}

func TestExplicitNamespacesRestrictDiscovery(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		deployment("a", "x", 1, "1", "1Gi"),
		deployment("b", "y", 1, "1", "1Gi"),
	)
	opts := DefaultOptions()
	opts.Namespaces = []string{"a"}
	c := NewClientWithInterface(cs, opts, quiet(), nil)
	ws, _ := c.Discover(context.Background())
	if len(ws) != 1 || ws[0].Namespace != "a" {
		t.Errorf("expected only namespace a, got %+v", ws)
	}
}

// DaemonSets are excluded by default because their request is multiplied across
// every node and their degradation is often least tolerable.
func TestDaemonSetsExcludedByDefault(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "agent"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Resources: resourceListFor("100m", "128Mi"),
			}}},
		}},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 5},
	}
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}, ds)

	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	ws, _ := c.Discover(context.Background())
	if len(ws) != 0 {
		t.Errorf("DaemonSets should be excluded by default, got %+v", ws)
	}

	opts := DefaultOptions()
	opts.IncludeDaemonSets = true
	c2 := NewClientWithInterface(cs, opts, quiet(), nil)
	ws2, _ := c2.Discover(context.Background())
	if len(ws2) != 1 {
		t.Fatalf("expected the DaemonSet when enabled, got %d workloads", len(ws2))
	}
	// Its "replica" count is the number of nodes it runs on: that is what
	// determines its cost.
	if ws2[0].Replicas != 5 {
		t.Errorf("DaemonSet replicas = %d, want the 5 scheduled nodes", ws2[0].Replicas)
	}
}

// --- owner resolution -----------------------------------------------------

// The Pod -> ReplicaSet -> Deployment chain is the case that matters: the pod
// cannot see the Deployment, so the name is recovered from the ReplicaSet's
// pod-template-hash suffix.
func TestResolveOwnerWalksReplicaSetToDeployment(t *testing.T) {
	cases := []struct {
		rsName string
		want   string
	}{
		{"api-7d4b8c9f5d", "api"},
		{"checkout-service-5f9b7c8d6", "checkout-service"},
		{"web-abc123", "web"},
	}
	for _, c := range cases {
		p := pod("app", c.rsName+"-x2k9p", "ReplicaSet", c.rsName, 0, nil)
		got, ok := resolveOwner(p)
		if !ok {
			t.Errorf("%s: owner not resolved", c.rsName)
			continue
		}
		if got != c.want {
			t.Errorf("%s: resolved to %q, want %q", c.rsName, got, c.want)
		}
	}
}

func TestResolveOwnerHandlesOtherKinds(t *testing.T) {
	if got, ok := resolveOwner(pod("app", "db-0", "StatefulSet", "db", 0, nil)); !ok || got != "db" {
		t.Errorf("StatefulSet owner = %q, %v", got, ok)
	}
	if got, ok := resolveOwner(pod("app", "agent-x1y2z", "DaemonSet", "agent", 0, nil)); !ok || got != "agent" {
		t.Errorf("DaemonSet owner = %q, %v", got, ok)
	}
	if got, ok := resolveOwner(pod("app", "backup-28374-abcde", "Job", "backup-28374", 0, nil)); !ok || got != "backup" {
		t.Errorf("CronJob-owned Job = %q, %v", got, ok)
	}
}

// A pod whose owner cannot be resolved must contribute no evidence, rather than
// evidence attributed to the wrong workload.
func TestUnresolvableOwnerContributesNoEvidence(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "standalone"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 9,
		}}},
	}
	ev := buildEvidence([]corev1.Pod{*p})
	if len(ev) != 0 {
		t.Errorf("an unowned pod contributed evidence: %+v", ev)
	}
	if _, ok := resolveOwner(p); ok {
		t.Error("an unowned pod should not resolve an owner")
	}
}

// --- OOM evidence ---------------------------------------------------------

// Only OOM restarts are evidence about memory sizing. Conflating them with
// crashes would block memory reductions for workloads whose problem is a bug.
func TestOOMKillsCountedSeparatelyFromRestarts(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-time.Hour))
	oom := pod("app", "api-abc-11111", "ReplicaSet", "api-7d4b8c9f5d", 3,
		&corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished})
	crash := pod("app", "api-abc-22222", "ReplicaSet", "api-7d4b8c9f5d", 5,
		&corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, FinishedAt: finished})

	ev := buildEvidence([]corev1.Pod{*oom, *crash})
	got := ev["app/api"]["app"]
	if got.Restarts != 8 {
		t.Errorf("restarts = %d, want 8 (3+5)", got.Restarts)
	}
	if got.OOMKills != 1 {
		t.Errorf("OOMKills = %d, want 1: a crash is not an OOM", got.OOMKills)
	}
	if got.LastOOM == nil {
		t.Error("expected a LastOOM timestamp")
	}
}

// Exit code 137 alone is not conclusive (any SIGKILL produces it), so it is
// only trusted when the reason is empty.
func TestExitCode137OnlyTrustedWithoutAReason(t *testing.T) {
	cases := []struct {
		reason string
		code   int32
		want   bool
	}{
		{"OOMKilled", 137, true},
		{"oomkilled", 137, true}, // case-insensitive
		{"", 137, true},          // runtime did not set a reason
		{"Error", 137, false},    // an explicit non-OOM reason wins
		{"Completed", 0, false},
		{"", 1, false},
	}
	for _, c := range cases {
		got := isOOM(&corev1.ContainerStateTerminated{Reason: c.reason, ExitCode: c.code})
		if got != c.want {
			t.Errorf("isOOM(reason=%q, code=%d) = %v, want %v", c.reason, c.code, got, c.want)
		}
	}
}

func TestEvidenceAttachedToDiscoveredWorkload(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		deployment("app", "api", 2, "1", "1Gi"),
		pod("app", "api-7d4b8c9f5d-11111", "ReplicaSet", "api-7d4b8c9f5d", 2,
			&corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished}),
	)
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	ws, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := ws[0].EvidenceFor("app")
	if !ok {
		t.Fatal("evidence was not attached to the discovered workload")
	}
	if ev.OOMKills != 1 {
		t.Errorf("OOMKills = %d, want 1", ev.OOMKills)
	}
}

// --- pod regex ------------------------------------------------------------

// The regex must be anchored and quoted, or one workload's recommendation would
// be computed from another's usage.
func TestPodRegexIsAnchoredAndDoesNotMatchSiblings(t *testing.T) {
	api := model.Workload{Name: "api", Kind: model.KindDeployment}
	re := regexp.MustCompile(PodRegexFor(api))

	if !re.MatchString("api-7d4b8c9f5d-x2k9p") {
		t.Error("should match its own Deployment pods")
	}
	// The critical case: a workload named "api" must not capture "api-gateway".
	if re.MatchString("api-gateway-7d4b8c9f5d-x2k9p") {
		t.Error("api must not match api-gateway pods")
	}
	if re.MatchString("other-api-7d4b8c9f5d-x2k9p") {
		t.Error("must not match a pod whose name merely contains the workload name")
	}
}

func TestPodRegexPerWorkloadKind(t *testing.T) {
	cases := []struct {
		kind    model.WorkloadKind
		name    string
		match   string
		noMatch string
	}{
		{model.KindStatefulSet, "db", "db-0", "db-abc-x1y2z"},
		{model.KindStatefulSet, "db", "db-12", "dbx-0"},
		{model.KindDaemonSet, "agent", "agent-x1y2z", "agent-0"},
		{model.KindCronJob, "backup", "backup-28374-abcde", "backup-abcde"},
	}
	for _, c := range cases {
		re := regexp.MustCompile(PodRegexFor(model.Workload{Name: c.name, Kind: c.kind}))
		if !re.MatchString(c.match) {
			t.Errorf("%s %q should match %q", c.kind, c.name, c.match)
		}
		if re.MatchString(c.noMatch) {
			t.Errorf("%s %q should not match %q", c.kind, c.name, c.noMatch)
		}
	}
}

// Workload names may contain regex metacharacters in principle; quoting must
// prevent them from being interpreted.
func TestPodRegexQuotesMetacharacters(t *testing.T) {
	w := model.Workload{Name: "a.b", Kind: model.KindDeployment}
	re := regexp.MustCompile(PodRegexFor(w))
	if re.MatchString("axb-7d4b8c9f5d-x2k9p") {
		t.Error("the dot in the workload name was treated as a wildcard")
	}
}

// --- patching -------------------------------------------------------------

// BLOCKED and INSUFFICIENT_DATA are exactly the cases where the engine declined
// to make a claim; turning them into a cluster write would defeat the safety
// machinery.
func TestPatchOmitsBlockedAndUnknownDecisions(t *testing.T) {
	rec := model.WorkloadRecommendation{
		Namespace: "app", Name: "api", Kind: model.KindDeployment,
		Containers: []model.ContainerRecommendation{{
			Container: "app",
			CPU:       model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: 500},
			Memory:    model.ResourceRecommendation{Decision: model.DecisionBlocked, Target: 4e9},
		}},
	}
	b, err := buildResourcePatch(rec)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !contains(s, `"cpu":"500m"`) {
		t.Errorf("patch should set the CPU request: %s", s)
	}
	if contains(s, `"memory"`) {
		t.Errorf("patch must not touch a BLOCKED memory request: %s", s)
	}
}

func TestPatchIsNilWhenNothingActionable(t *testing.T) {
	rec := model.WorkloadRecommendation{
		Containers: []model.ContainerRecommendation{{
			Container: "app",
			CPU:       model.ResourceRecommendation{Decision: model.DecisionNoChange, Target: 1000},
			Memory:    model.ResourceRecommendation{Decision: model.DecisionInsufficientData},
		}},
	}
	b, err := buildResourcePatch(rec)
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Errorf("expected no patch, got %s", b)
	}
}

// Limits must never be patched: changing a memory limit alters when the kernel
// kills the process, and no usage series justifies that automatically.
func TestPatchNeverTouchesLimits(t *testing.T) {
	rec := model.WorkloadRecommendation{
		Containers: []model.ContainerRecommendation{{
			Container: "app",
			CPU:       model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: 500},
			Memory:    model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: float64(model.BytesPerGi)},
		}},
	}
	b, _ := buildResourcePatch(rec)
	if contains(string(b), "limits") {
		t.Errorf("patch must not contain limits: %s", b)
	}
	if !contains(string(b), `"requests"`) {
		t.Errorf("patch should set requests: %s", b)
	}
}

func TestApplyRecommendationPatchesDeployment(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		deployment("app", "api", 1, "2", "4Gi"),
	)
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	rec := model.WorkloadRecommendation{
		Namespace: "app", Name: "api", Kind: model.KindDeployment,
		Containers: []model.ContainerRecommendation{{
			Container: "app",
			CPU:       model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: 500},
			Memory:    model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: float64(model.BytesPerGi)},
		}},
	}
	if err := c.ApplyRecommendation(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := cs.AppsV1().Deployments("app").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reqs := got.Spec.Template.Spec.Containers[0].Resources.Requests
	if reqs.Cpu().MilliValue() != 500 {
		t.Errorf("CPU request = %v, want 500m", reqs.Cpu())
	}
	if reqs.Memory().Value() != model.BytesPerGi {
		t.Errorf("memory request = %v, want 1Gi", reqs.Memory())
	}
}

func TestApplyRejectsUnsupportedKind(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewClientWithInterface(cs, DefaultOptions(), quiet(), nil)
	rec := model.WorkloadRecommendation{
		Namespace: "app", Name: "job", Kind: model.KindCronJob,
		Containers: []model.ContainerRecommendation{{
			Container: "app",
			CPU:       model.ResourceRecommendation{Decision: model.DecisionDecrease, Target: 500},
		}},
	}
	if err := c.ApplyRecommendation(context.Background(), rec); err == nil {
		t.Error("expected an error for an unsupported workload kind")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
