// Package helm_test verifies properties of the Helm chart by rendering it.
//
// These are not cosmetic checks. Each one encodes a security or safety property
// that would be invisible in a code review of the templates but is exactly what
// an operator relies on: that installing the chart cannot mutate the cluster
// unless two independent settings are both enabled, that the service account
// cannot read secrets, and that a dangerous policy value fails at render time
// rather than at runtime.
package helm_test

import (
	"os/exec"
	"strings"
	"testing"
)

const chartPath = "../../helm/k8s-cost-optimizer"

// render runs `helm template` and returns the manifest, or the error output.
func render(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"template", "test-release", chartPath}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	return string(out), err
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed; skipping chart tests")
	}
}

func TestChartRendersWithDefaults(t *testing.T) {
	requireHelm(t)
	out, err := render(t)
	if err != nil {
		t.Fatalf("default render failed: %v\n%s", err, out)
	}
	for _, kind := range []string{
		"kind: Deployment", "kind: Service", "kind: ServiceAccount",
		"kind: ConfigMap", "kind: ClusterRole", "kind: ClusterRoleBinding",
	} {
		if !strings.Contains(out, kind) {
			t.Errorf("default render is missing %s", kind)
		}
	}
}

// The default install must be read-only. If this regresses, installing the chart
// would silently grant the ability to restart every workload in the cluster.
func TestDefaultInstallGrantsNoWriteAccess(t *testing.T) {
	requireHelm(t)
	out, err := render(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	rules := clusterRoleSection(out)
	for _, verb := range []string{"patch", "update", "create", "delete", "deletecollection"} {
		if strings.Contains(rules, `"`+verb+`"`) {
			t.Errorf("the default ClusterRole grants %q; the default install must be read-only", verb)
		}
	}
	if !strings.Contains(out, "apply: false") {
		t.Error("the rendered config must have apply disabled by default")
	}
}

// Mutation requires two independent settings. Making them one would mean a single
// typo could enable cluster writes.
func TestApplyModeRequiresBothSettings(t *testing.T) {
	requireHelm(t)

	out, err := render(t, "--set", "analysis.apply=true")
	if err == nil {
		t.Error("analysis.apply=true without rbac.allowApply=true should fail to render")
	}
	if !strings.Contains(out, "rbac.allowApply") {
		t.Errorf("the failure should name the missing setting; got:\n%s", out)
	}

	out2, err := render(t, "--set", "analysis.apply=true", "--set", "rbac.allowApply=true")
	if err != nil {
		t.Fatalf("apply mode with RBAC should render: %v\n%s", err, out2)
	}
	if !strings.Contains(clusterRoleSection(out2), `"patch"`) {
		t.Error("apply mode should grant patch on pod-template owners")
	}
}

// Even in apply mode, the optimizer must never be able to delete a workload. It
// has no reason to, and withholding the verb means a bug cannot escalate into
// deleting one.
func TestDeleteVerbIsNeverGranted(t *testing.T) {
	requireHelm(t)
	out, err := render(t, "--set", "rbac.allowApply=true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(clusterRoleSection(out), `"delete"`) {
		t.Error("the ClusterRole grants delete; the optimizer must never be able to remove a workload")
	}
}

// A cost-analysis tool with secret access is a credential-theft target for no
// benefit.
func TestNoSecretOrConfigMapAccess(t *testing.T) {
	requireHelm(t)
	out, err := render(t, "--set", "rbac.allowApply=true")
	if err != nil {
		t.Fatal(err)
	}
	rules := clusterRoleSection(out)
	for _, res := range []string{"- secrets", "- configmaps"} {
		if strings.Contains(rules, res) {
			t.Errorf("the ClusterRole grants access to %s, which the optimizer never needs", strings.TrimPrefix(res, "- "))
		}
	}
}

// A safety factor below 1.0 recommends less than the statistic identified as the
// safe envelope. It must fail at render time, not silently at runtime.
func TestUnsafeSafetyFactorsAreRejected(t *testing.T) {
	requireHelm(t)
	for _, key := range []string{"policy.cpuSafetyFactor", "policy.memorySafetyFactor"} {
		out, err := render(t, "--set", key+"=0.8")
		if err == nil {
			t.Errorf("%s=0.8 should fail to render", key)
		}
		if !strings.Contains(out, "1.0") {
			t.Errorf("%s: the error should explain the constraint; got:\n%s", key, out)
		}
	}
}

func TestMissingPrometheusAddressIsRejected(t *testing.T) {
	requireHelm(t)
	out, err := render(t, "--set", "prometheus.address=")
	if err == nil {
		t.Errorf("an empty prometheus.address should fail to render:\n%s", out)
	}
}

// The pod must run non-root with a read-only root filesystem and no capabilities.
func TestSecurityContextIsHardened(t *testing.T) {
	requireHelm(t)
	out, err := render(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"- ALL", // dropped capabilities
		"type: RuntimeDefault",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered pod spec is missing %q", want)
		}
	}
	if strings.Contains(out, "privileged: true") {
		t.Error("the pod must never be privileged")
	}
}

// A config change must roll the pod. Without the checksum annotation, a helm
// upgrade that only alters values would leave the running pod on the old config.
func TestConfigChangeRollsThePod(t *testing.T) {
	requireHelm(t)
	a, err := render(t)
	if err != nil {
		t.Fatal(err)
	}
	b, err := render(t, "--set", "policy.cpuSafetyFactor=1.4")
	if err != nil {
		t.Fatal(err)
	}
	sumA, sumB := extract(a, "checksum/config:"), extract(b, "checksum/config:")
	if sumA == "" {
		t.Fatal("the Deployment has no checksum/config annotation")
	}
	if sumA == sumB {
		t.Error("changing the policy did not change the config checksum, so an upgrade would not roll the pod")
	}
}

// The shipped policy defaults are experimental results (see research/results.md).
// A change to them should be a deliberate act accompanied by new evidence.
func TestChartDefaultsMatchTheEvidenceBasedPolicy(t *testing.T) {
	requireHelm(t)
	out, err := render(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cpu_strategy: "p99"`,
		`memory_strategy: "max"`,
		"cpu_safety_factor: 1.25",
		"memory_safety_factor: 1.25",
		"max_cpu_burstiness: 20",
		"oom_protection: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("chart default %q is missing; the defaults are experimental results, "+
				"and changing one requires new evidence (see research/results.md)", want)
		}
	}
}

func TestOptionalResourcesRenderWhenEnabled(t *testing.T) {
	requireHelm(t)
	out, err := render(t,
		"--set", "serviceMonitor.enabled=true",
		"--set", "grafanaDashboard.enabled=true")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kind: ServiceMonitor") {
		t.Error("ServiceMonitor did not render when enabled")
	}
	if !strings.Contains(out, "k8s-cost-optimizer.json") {
		t.Error("the Grafana dashboard ConfigMap did not render when enabled")
	}
}

// clusterRoleSection returns only the ClusterRole part of a manifest, so that a
// match on a word elsewhere (imagePullSecrets, a comment) cannot produce a false
// positive in the RBAC assertions.
func clusterRoleSection(manifest string) string {
	var b strings.Builder
	in := false
	for _, doc := range strings.Split(manifest, "\n---") {
		if strings.Contains(doc, "kind: ClusterRole\n") && !strings.Contains(doc, "kind: ClusterRoleBinding") {
			in = true
		} else {
			in = false
		}
		if in {
			// Strip comments: the templates explain what is deliberately absent,
			// and those explanations must not be mistaken for grants.
			for _, line := range strings.Split(doc, "\n") {
				if t := strings.TrimSpace(line); !strings.HasPrefix(t, "#") {
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

func extract(manifest, key string) string {
	for _, line := range strings.Split(manifest, "\n") {
		if strings.Contains(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), key))
		}
	}
	return ""
}
