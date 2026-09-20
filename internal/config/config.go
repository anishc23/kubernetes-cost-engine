// Package config loads and validates the optimizer's runtime configuration.
//
// Configuration is loaded from a YAML file, then overridden by flags. Every
// field has a documented default, and validation runs before any component is
// constructed so that a misconfiguration fails at startup rather than producing
// silently wrong recommendations hours later.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/kube"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
	"github.com/anishc23/k8s-cost-optimizer/internal/stats"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
)

// Config is the complete runtime configuration.
type Config struct {
	Log        LogConfig        `json:"log"`
	Server     ServerConfig     `json:"server"`
	Kubernetes KubernetesConfig `json:"kubernetes"`
	Prometheus PrometheusConfig `json:"prometheus"`
	Policy     PolicyConfig     `json:"policy"`
	Cost       CostConfig       `json:"cost"`
	Analysis   AnalysisConfig   `json:"analysis"`
}

type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type ServerConfig struct {
	// Addr is the API listen address.
	Addr string `json:"addr"`
	// MetricsAddr serves /metrics. It is separate from the API address so that
	// the API can be exposed to users while metrics stay on an internal port.
	MetricsAddr string `json:"metrics_addr"`
	// ReadTimeout and WriteTimeout bound request handling. They are set rather
	// than left at Go's zero value (no timeout), because an unbounded server is
	// trivially held open by a slow client.
	ReadTimeout  humanize.Duration `json:"read_timeout"`
	WriteTimeout humanize.Duration `json:"write_timeout"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout humanize.Duration `json:"shutdown_timeout"`
}

type KubernetesConfig struct {
	Kubeconfig        string   `json:"kubeconfig"`
	Namespaces        []string `json:"namespaces"`
	ExcludeNamespaces []string `json:"exclude_namespaces"`
	LabelSelector     string   `json:"label_selector"`
	IncludeDaemonSets bool     `json:"include_daemonsets"`
}

type PrometheusConfig struct {
	Address         string            `json:"address"`
	Timeout         humanize.Duration `json:"timeout"`
	Step            humanize.Duration `json:"step"`
	BearerTokenFile string            `json:"bearer_token_file"`
}

// PolicyConfig mirrors recommender.Policy in YAML-friendly form.
type PolicyConfig struct {
	CPUStrategy        string  `json:"cpu_strategy"`
	MemoryStrategy     string  `json:"memory_strategy"`
	CPUSafetyFactor    float64 `json:"cpu_safety_factor"`
	MemorySafetyFactor float64 `json:"memory_safety_factor"`
	PercentileMethod   string  `json:"percentile_method"`

	ObservationWindow humanize.Duration `json:"observation_window"`

	MinSamples  int               `json:"min_samples"`
	MinDuration humanize.Duration `json:"min_duration"`
	MinCoverage float64           `json:"min_coverage"`

	OOMProtection        bool              `json:"oom_protection"`
	OOMRequireEvidence   bool              `json:"oom_require_evidence"`
	OOMLookbackRelevance humanize.Duration `json:"oom_lookback_relevance"`

	UsageExceedsRequestPercentile string `json:"usage_exceeds_request_percentile"`

	CPUFloor    string `json:"cpu_floor"`
	MemoryFloor string `json:"memory_floor"`

	MaxCPUBurstiness    float64 `json:"max_cpu_burstiness"`
	MaxMemoryBurstiness float64 `json:"max_memory_burstiness"`

	MinRelativeChange float64 `json:"min_relative_change"`
}

type CostConfig struct {
	// InstanceType derives rates from the built-in price catalog.
	InstanceType string `json:"instance_type"`
	// CPUCostShare overrides the CPU/memory price split.
	CPUCostShare float64 `json:"cpu_cost_share"`
	// Explicit rates override the derived ones entirely, for deployments that
	// know their actual effective price.
	CPUHourUSD       float64 `json:"cpu_hour_usd"`
	MemoryGiBHourUSD float64 `json:"memory_gib_hour_usd"`
	Provider         string  `json:"provider"`
	Region           string  `json:"region"`
}

type AnalysisConfig struct {
	// Interval is how often a full analysis cycle runs.
	Interval humanize.Duration `json:"interval"`
	// Concurrency bounds simultaneous Prometheus queries. It exists because
	// discovery can find thousands of workloads and issuing one query per
	// workload at once would overwhelm Prometheus long before it overwhelmed the
	// optimizer.
	Concurrency int `json:"concurrency"`
	// Apply enables writing recommendations back to the cluster.
	//
	// This defaults to false and must stay that way. The tool's advice can cause
	// outages if applied without review, so mutation is opt-in, per deployment,
	// by an operator who has read docs/security.md.
	Apply bool `json:"apply"`
	// ApplyMaxDecreaseFraction bounds how much a single applied change may reduce
	// a request, even when --apply is on. A recommendation to cut a request by
	// 95% may be correct, but applying it unattended is not a risk the tool
	// should take on an operator's behalf without an explicit ceiling.
	ApplyMaxDecreaseFraction float64 `json:"apply_max_decrease_fraction"`
}

// Default returns the shipped configuration.
func Default() Config {
	p := recommender.DefaultPolicy()
	return Config{
		Log: LogConfig{Level: "info", Format: "json"},
		Server: ServerConfig{
			Addr:            ":8080",
			MetricsAddr:     ":9090",
			ReadTimeout:     humanize.Duration(15 * time.Second),
			WriteTimeout:    humanize.Duration(60 * time.Second),
			ShutdownTimeout: humanize.Duration(20 * time.Second),
		},
		Kubernetes: KubernetesConfig{
			ExcludeNamespaces: kube.DefaultOptions().ExcludeNamespaces,
			IncludeDaemonSets: false,
		},
		Prometheus: PrometheusConfig{
			Address: "http://prometheus-server.monitoring.svc.cluster.local",
			Timeout: humanize.Duration(30 * time.Second),
			Step:    humanize.Duration(time.Minute),
		},
		Policy: PolicyConfig{
			CPUStrategy:                   p.CPUStrategy,
			MemoryStrategy:                p.MemoryStrategy,
			CPUSafetyFactor:               p.CPUSafetyFactor,
			MemorySafetyFactor:            p.MemorySafetyFactor,
			PercentileMethod:              string(p.PercentileMethod),
			ObservationWindow:             humanize.Duration(p.ObservationWindow),
			MinSamples:                    p.MinSamples,
			MinDuration:                   humanize.Duration(p.MinDuration),
			MinCoverage:                   p.MinCoverage,
			OOMProtection:                 p.OOMProtection,
			OOMRequireEvidence:            p.OOMRequireEvidence,
			OOMLookbackRelevance:          humanize.Duration(p.OOMLookbackRelevance),
			UsageExceedsRequestPercentile: p.UsageExceedsRequestPercentile,
			CPUFloor:                      "10m",
			MemoryFloor:                   "32Mi",
			// Carried over from the engine default rather than omitted. The
			// instability gate is the mechanism that makes the shipped CPU policy
			// safe on spiky workloads; dropping it here would silently disable that
			// protection for any deployment configured from a file rather than
			// through the Helm chart. TestDefaultPolicyConverts pins it.
			MaxCPUBurstiness:    p.MaxCPUBurstiness,
			MaxMemoryBurstiness: p.MaxMemoryBurstiness,
			MinRelativeChange:   p.MinRelativeChange,
		},
		Cost: CostConfig{InstanceType: "m5.xlarge", CPUCostShare: cost.CPUCostShare},
		Analysis: AnalysisConfig{
			Interval:                 humanize.Duration(15 * time.Minute),
			Concurrency:              8,
			Apply:                    false,
			ApplyMaxDecreaseFraction: 0.5,
		},
	}
}

// Load reads a YAML config file, merging it over the defaults. An empty path
// returns the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	// Unmarshalling over the defaults means an omitted field keeps its default
	// rather than becoming a zero value, which for a duration or a safety factor
	// would be actively dangerous.
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration and returns all problems at once, so that
// fixing a config file does not require a sequence of restart-and-retry cycles.
func (c Config) Validate() error {
	var problems []string

	if c.Prometheus.Address == "" {
		problems = append(problems, "prometheus.address is required")
	} else if !strings.HasPrefix(c.Prometheus.Address, "http://") && !strings.HasPrefix(c.Prometheus.Address, "https://") {
		problems = append(problems, fmt.Sprintf("prometheus.address %q must start with http:// or https://", c.Prometheus.Address))
	}
	if c.Prometheus.Step.D() <= 0 {
		problems = append(problems, "prometheus.step must be positive")
	}
	if c.Analysis.Interval.D() <= 0 {
		problems = append(problems, "analysis.interval must be positive")
	}
	if c.Analysis.Concurrency < 1 {
		problems = append(problems, "analysis.concurrency must be at least 1")
	}
	if c.Analysis.ApplyMaxDecreaseFraction < 0 || c.Analysis.ApplyMaxDecreaseFraction >= 1 {
		problems = append(problems, "analysis.apply_max_decrease_fraction must be in [0,1)")
	}
	if _, err := c.Policy.ToPolicy(); err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := c.CostModel(); err != nil {
		problems = append(problems, err.Error())
	}
	// A sampling step longer than the minimum observation duration can never
	// produce enough samples, so every workload would report INSUFFICIENT_DATA.
	if c.Prometheus.Step.D() > 0 && c.Policy.MinSamples > 0 {
		needed := time.Duration(c.Policy.MinSamples) * c.Prometheus.Step.D()
		if needed > c.Policy.ObservationWindow.D() {
			problems = append(problems, fmt.Sprintf(
				"policy.min_samples (%d) at prometheus.step (%s) needs %s of history, "+
					"but policy.observation_window is only %s: every workload would report INSUFFICIENT_DATA",
				c.Policy.MinSamples, c.Prometheus.Step, needed, c.Policy.ObservationWindow))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ToPolicy converts the configuration into a validated engine policy.
func (p PolicyConfig) ToPolicy() (recommender.Policy, error) {
	out := recommender.DefaultPolicy()
	out.CPUStrategy = p.CPUStrategy
	out.MemoryStrategy = p.MemoryStrategy
	out.CPUSafetyFactor = p.CPUSafetyFactor
	out.MemorySafetyFactor = p.MemorySafetyFactor
	out.ObservationWindow = p.ObservationWindow.D()
	out.MinSamples = p.MinSamples
	out.MinDuration = p.MinDuration.D()
	out.MinCoverage = p.MinCoverage
	out.OOMProtection = p.OOMProtection
	out.OOMRequireEvidence = p.OOMRequireEvidence
	out.OOMLookbackRelevance = p.OOMLookbackRelevance.D()
	out.UsageExceedsRequestPercentile = p.UsageExceedsRequestPercentile
	out.MaxCPUBurstiness = p.MaxCPUBurstiness
	out.MaxMemoryBurstiness = p.MaxMemoryBurstiness
	out.MinRelativeChange = p.MinRelativeChange

	switch p.PercentileMethod {
	case "", string(stats.LinearInterpolation):
		out.PercentileMethod = stats.LinearInterpolation
	case string(stats.NearestRank):
		out.PercentileMethod = stats.NearestRank
	default:
		return out, fmt.Errorf("policy.percentile_method %q must be %q or %q",
			p.PercentileMethod, stats.LinearInterpolation, stats.NearestRank)
	}

	if p.CPUFloor != "" {
		v, err := parseCPUFloor(p.CPUFloor)
		if err != nil {
			return out, err
		}
		out.CPUFloorMilli = v
	}
	if p.MemoryFloor != "" {
		v, err := parseMemoryFloor(p.MemoryFloor)
		if err != nil {
			return out, err
		}
		out.MemoryFloorBytes = v
	}

	reg := recommender.NewRegistry(out.PercentileMethod)
	if err := out.Validate(reg); err != nil {
		return out, err
	}
	return out, nil
}

// CostModel resolves the configured pricing model.
//
// Explicit rates take precedence over a derived instance price, because an
// operator who has entered their actual effective rate knows something the
// catalog does not.
func (c Config) CostModel() (cost.Model, error) {
	if c.Cost.CPUHourUSD > 0 || c.Cost.MemoryGiBHourUSD > 0 {
		m := cost.Model{
			Name:             "configured",
			CPUHourUSD:       c.Cost.CPUHourUSD,
			MemoryGiBHourUSD: c.Cost.MemoryGiBHourUSD,
			Provider:         c.Cost.Provider,
			Region:           c.Cost.Region,
			Source:           "explicitly configured per-unit rates",
		}
		return m, m.Validate()
	}
	name := c.Cost.InstanceType
	if name == "" {
		name = "m5.xlarge"
	}
	it, err := cost.InstanceByName(name)
	if err != nil {
		return cost.Model{}, fmt.Errorf("cost.instance_type: %w", err)
	}
	share := c.Cost.CPUCostShare
	if share == 0 {
		share = cost.CPUCostShare
	}
	return cost.DeriveModel(it, share)
}

// KubeOptions converts the Kubernetes section into discovery options.
func (c Config) KubeOptions() kube.Options {
	return kube.Options{
		Namespaces:        c.Kubernetes.Namespaces,
		ExcludeNamespaces: c.Kubernetes.ExcludeNamespaces,
		LabelSelector:     c.Kubernetes.LabelSelector,
		IncludeDaemonSets: c.Kubernetes.IncludeDaemonSets,
	}
}
