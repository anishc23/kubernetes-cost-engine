package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
)

// Configuration validation is load-bearing safety logic: it is what stops a
// dangerous or unworkable configuration from reaching a running cluster. These
// tests exercise the cases that matter rather than the happy path alone.

func TestDefaultConfigIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the shipped default must be valid: %v", err)
	}
}

// The default policy must round-trip into a valid engine policy, or the binary
// would fail at startup with its own defaults.
func TestDefaultPolicyConverts(t *testing.T) {
	p, err := Default().Policy.ToPolicy()
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	if p.CPUStrategy != "p99" || p.MemoryStrategy != "max" {
		t.Errorf("default strategies = %s/%s, want p99/max (these are experimental results; "+
			"see research/results.md)", p.CPUStrategy, p.MemoryStrategy)
	}
	if p.MaxCPUBurstiness != 20 {
		t.Errorf("default CPU burstiness threshold = %v, want 20", p.MaxCPUBurstiness)
	}
	if !p.OOMProtection {
		t.Error("OOM protection must be on by default")
	}
}

// Mutation must be off by default. This is the single most consequential default
// in the project.
func TestApplyIsOffByDefault(t *testing.T) {
	if Default().Analysis.Apply {
		t.Fatal("analysis.apply must default to false: applying recommendations restarts pods")
	}
	if Default().Analysis.ApplyMaxDecreaseFraction >= 1 {
		t.Error("apply_max_decrease_fraction must bound how far a single applied change may cut")
	}
}

func TestValidationRejectsBadConfigurations(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string // a substring the error should contain
	}{
		{"no prometheus address", func(c *Config) { c.Prometheus.Address = "" }, "prometheus.address"},
		{"prometheus address without scheme", func(c *Config) { c.Prometheus.Address = "prometheus:9090" }, "http://"},
		{"zero step", func(c *Config) { c.Prometheus.Step = 0 }, "step"},
		{"zero interval", func(c *Config) { c.Analysis.Interval = 0 }, "interval"},
		{"zero concurrency", func(c *Config) { c.Analysis.Concurrency = 0 }, "concurrency"},
		{"apply fraction at 1", func(c *Config) { c.Analysis.ApplyMaxDecreaseFraction = 1.0 }, "apply_max_decrease_fraction"},
		{"unknown cpu strategy", func(c *Config) { c.Policy.CPUStrategy = "p42" }, "p42"},
		{"cpu safety below 1", func(c *Config) { c.Policy.CPUSafetyFactor = 0.9 }, "cpu_safety_factor"},
		{"memory safety below 1", func(c *Config) { c.Policy.MemorySafetyFactor = 0.5 }, "memory_safety_factor"},
		{"unknown percentile method", func(c *Config) { c.Policy.PercentileMethod = "magic" }, "percentile_method"},
		{"bad cpu floor", func(c *Config) { c.Policy.CPUFloor = "not-a-quantity" }, "cpu_floor"},
		{"bad memory floor", func(c *Config) { c.Policy.MemoryFloor = "12XiB" }, "memory_floor"},
		{"unknown instance type", func(c *Config) { c.Cost.InstanceType = "nonexistent.9xlarge" }, "instance_type"},
	}
	for _, c := range cases {
		cfg := Default()
		c.mut(&cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: expected a validation error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error should mention %q, got: %v", c.name, c.want, err)
		}
	}
}

// A configuration that can never produce a recommendation is a misconfiguration,
// and the operator should learn that at startup rather than from a wave of
// INSUFFICIENT_DATA hours later.
func TestRejectsUnsatisfiableSampleRequirement(t *testing.T) {
	cfg := Default()
	cfg.Prometheus.Step = humanize.Duration(time.Minute)
	cfg.Policy.MinSamples = 60 // needs 60 minutes
	cfg.Policy.ObservationWindow = humanize.Duration(30 * time.Minute)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected rejection: 60 samples at a 1m step cannot fit in a 30m window")
	}
	if !strings.Contains(err.Error(), "INSUFFICIENT_DATA") {
		t.Errorf("the error should explain the consequence, got: %v", err)
	}
}

// Validation must report every problem at once. Fixing a config file should not
// require a sequence of restart-and-retry cycles.
func TestValidationReportsAllProblemsAtOnce(t *testing.T) {
	cfg := Default()
	cfg.Prometheus.Address = ""
	cfg.Analysis.Concurrency = 0
	cfg.Policy.CPUSafetyFactor = 0.5
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"prometheus.address", "concurrency", "cpu_safety_factor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should report %q alongside the others; got:\n%s", want, msg)
		}
	}
}

// --- loading --------------------------------------------------------------

func TestLoadEmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.CPUStrategy != Default().Policy.CPUStrategy {
		t.Error("an empty path should return the defaults unchanged")
	}
}

func TestLoadMissingFileIsAnError(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

// Omitted fields must keep their defaults rather than becoming zero values. For
// a duration or a safety factor, a silent zero would be actively dangerous.
func TestPartialConfigKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
prometheus:
  address: http://prom:9090
policy:
  cpu_strategy: p95
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.CPUStrategy != "p95" {
		t.Errorf("the specified field did not load: %q", cfg.Policy.CPUStrategy)
	}
	if cfg.Policy.MemoryStrategy != "max" {
		t.Errorf("an omitted field lost its default: memory_strategy = %q", cfg.Policy.MemoryStrategy)
	}
	if cfg.Policy.MemorySafetyFactor != 1.25 {
		t.Errorf("an omitted safety factor became %v rather than keeping its default",
			cfg.Policy.MemorySafetyFactor)
	}
	if cfg.Analysis.Interval.D() == 0 {
		t.Error("an omitted duration became zero, which would spin the analysis loop")
	}
	if cfg.Analysis.Apply {
		t.Error("apply must stay false when the file does not mention it")
	}
}

// Durations must be written the way people write them.
func TestDurationsLoadFromHumanStrings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
prometheus:
  address: http://prom:9090
  step: 30s
policy:
  observation_window: 72h
  min_duration: 90m
analysis:
  interval: 5m
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Prometheus.Step.D(); got != 30*time.Second {
		t.Errorf("step = %v", got)
	}
	if got := cfg.Policy.ObservationWindow.D(); got != 72*time.Hour {
		t.Errorf("observation_window = %v", got)
	}
	if got := cfg.Policy.MinDuration.D(); got != 90*time.Minute {
		t.Errorf("min_duration = %v", got)
	}
	if got := cfg.Analysis.Interval.D(); got != 5*time.Minute {
		t.Errorf("interval = %v", got)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("policy:\n  cpu_safety_factor: [not, a, number]\n"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("expected a parse error")
	}
}

// --- cost model -----------------------------------------------------------

// An operator who states their actual effective rate knows something the catalog
// does not, so explicit rates must win.
func TestExplicitRatesOverrideTheInstanceCatalog(t *testing.T) {
	cfg := Default()
	cfg.Cost.CPUHourUSD = 0.021
	cfg.Cost.MemoryGiBHourUSD = 0.0028
	m, err := cfg.CostModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUHourUSD != 0.021 || m.MemoryGiBHourUSD != 0.0028 {
		t.Errorf("explicit rates were not used: %+v", m)
	}
	if m.Name != "configured" {
		t.Errorf("model name = %q, want it to identify the rates as configured", m.Name)
	}
}

func TestCostModelDerivesFromInstance(t *testing.T) {
	cfg := Default()
	cfg.Cost.InstanceType = "c5.2xlarge"
	m, err := cfg.CostModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUHourUSD <= 0 || m.MemoryGiBHourUSD <= 0 {
		t.Errorf("derived rates should be positive: %+v", m)
	}
	// The source must be recorded so a stale price list is visible.
	if !strings.Contains(m.Source, "c5.2xlarge") {
		t.Errorf("the model should document its derivation, got %q", m.Source)
	}
}

// --- discovery options ----------------------------------------------------

func TestKubeOptionsCarryTheConservativeDefaults(t *testing.T) {
	opts := Default().KubeOptions()
	if opts.IncludeDaemonSets {
		t.Error("DaemonSets must be excluded by default")
	}
	found := map[string]bool{}
	for _, ns := range opts.ExcludeNamespaces {
		found[ns] = true
	}
	for _, ns := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		if !found[ns] {
			t.Errorf("%s must be excluded by default", ns)
		}
	}
}

func TestPercentileMethodAccepted(t *testing.T) {
	for _, m := range []string{"", string(stats.LinearInterpolation), string(stats.NearestRank)} {
		cfg := Default()
		cfg.Policy.PercentileMethod = m
		if _, err := cfg.Policy.ToPolicy(); err != nil {
			t.Errorf("percentile method %q should be accepted: %v", m, err)
		}
	}
}
