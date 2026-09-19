// Command koctl is the command-line interface to the cost optimization engine.
//
// It runs the same analysis the in-cluster controller runs, against the user's
// current kubeconfig context, and prints the result as a table or JSON. This
// matters for adoption: an operator can evaluate the tool's advice from their
// laptop before deciding whether to deploy it, and can diff its output between
// policy configurations without running a server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/api"
	"github.com/anishc23/k8s-cost-optimizer/internal/config"
	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/kube"
	"github.com/anishc23/k8s-cost-optimizer/internal/logging"
	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/model"
	"github.com/anishc23/k8s-cost-optimizer/internal/promapi"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
	"github.com/anishc23/k8s-cost-optimizer/internal/version"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
	"github.com/anishc23/k8s-cost-optimizer/pkg/quantity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "koctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `koctl - Kubernetes resource right-sizing

Usage:
  koctl recommend [flags]   analyse workloads and print recommendations
  koctl workloads [flags]   list discovered workloads and their declared requests
  koctl pricing             show the built-in instance price catalog
  koctl version             print version information

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Examples:
  koctl recommend --prometheus-address http://localhost:9090
  koctl recommend --namespace production --output json
  koctl recommend --cpu-strategy p99 --cpu-safety-factor 1.3
`)
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to a YAML configuration file")
		kubeconfig  = flag.String("kubeconfig", "", "path to a kubeconfig file (default: the usual resolution rules)")
		promAddr    = flag.String("prometheus-address", "", "Prometheus base URL")
		namespace   = flag.String("namespace", "", "restrict analysis to one namespace")
		window      = flag.Duration("observation-window", 0, "observation window")
		output      = flag.String("output", "table", "output format: table or json")
		cpuStrategy = flag.String("cpu-strategy", "", "CPU strategy: mean, p50, p90, p95, p99, max, current")
		memStrategy = flag.String("memory-strategy", "", "memory strategy: mean, p50, p90, p95, p99, max, current")
		cpuSafety   = flag.Float64("cpu-safety-factor", 0, "CPU safety factor (>= 1.0)")
		memSafety   = flag.Float64("memory-safety-factor", 0, "memory safety factor (>= 1.0)")
		instance    = flag.String("instance-type", "", "instance type used to derive cost rates")
		minSavings  = flag.Float64("min-savings", 0, "only show workloads saving at least this much per month")
		logLevel    = flag.String("log-level", "warn", "log level")
	)
	flag.Usage = usage
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		usage()
		return fmt.Errorf("a subcommand is required")
	}

	switch cmd {
	case "version":
		fmt.Printf("koctl %s (commit %s, built %s, %s)\n",
			version.Version, version.Commit, version.BuildDate, version.GoVersion())
		return nil
	case "pricing":
		return printPricing()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *kubeconfig != "" {
		cfg.Kubernetes.Kubeconfig = *kubeconfig
	}
	if *promAddr != "" {
		cfg.Prometheus.Address = *promAddr
	}
	if *namespace != "" {
		cfg.Kubernetes.Namespaces = []string{*namespace}
	}
	if *window > 0 {
		cfg.Policy.ObservationWindow = humanize.Duration(*window)
	}
	if *cpuStrategy != "" {
		cfg.Policy.CPUStrategy = *cpuStrategy
	}
	if *memStrategy != "" {
		cfg.Policy.MemoryStrategy = *memStrategy
	}
	if *cpuSafety > 0 {
		cfg.Policy.CPUSafetyFactor = *cpuSafety
	}
	if *memSafety > 0 {
		cfg.Policy.MemorySafetyFactor = *memSafety
	}
	if *instance != "" {
		cfg.Cost.InstanceType = *instance
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := logging.New(*logLevel, "text")
	svc, err := buildService(cfg, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := svc.Analyze(ctx); err != nil {
		return err
	}
	snap := svc.Snapshot()

	switch cmd {
	case "recommend":
		if *output == "json" {
			return printJSON(snap)
		}
		return printRecommendationTable(snap, *minSavings)
	case "workloads":
		if *output == "json" {
			return printJSON(snap.Workloads)
		}
		return printWorkloadTable(snap)
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func buildService(cfg config.Config, log *logAdapter) (*api.Service, error) {
	m := metrics.New()
	policy, err := cfg.Policy.ToPolicy()
	if err != nil {
		return nil, err
	}
	engine, err := recommender.NewEngine(policy)
	if err != nil {
		return nil, err
	}
	costModel, err := cfg.CostModel()
	if err != nil {
		return nil, err
	}
	estimator, err := cost.NewEstimator(costModel)
	if err != nil {
		return nil, err
	}
	discoverer, err := kube.NewClient(cfg.Kubernetes.Kubeconfig, cfg.KubeOptions(), log, m)
	if err != nil {
		return nil, err
	}
	promClient, err := promapi.NewClient(promapi.Config{
		Address:         cfg.Prometheus.Address,
		Timeout:         cfg.Prometheus.Timeout.D(),
		Step:            cfg.Prometheus.Step.D(),
		BearerTokenFile: cfg.Prometheus.BearerTokenFile,
	}, log, m)
	if err != nil {
		return nil, err
	}
	return api.NewService(discoverer, promClient, engine, estimator,
		api.Options{Concurrency: cfg.Analysis.Concurrency, QueryStep: cfg.Prometheus.Step.D()}, log, m), nil
}

func printRecommendationTable(snap api.Snapshot, minSavings float64) error {
	recs := make([]model.WorkloadRecommendation, 0, len(snap.Recommendations))
	for _, r := range snap.Recommendations {
		if r.Cost.MonthlySavingsUSD >= minSavings {
			recs = append(recs, r)
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		return recs[i].Cost.MonthlySavingsUSD > recs[j].Cost.MonthlySavingsUSD
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tWORKLOAD\tCONTAINER\tCPU NOW\tCPU REC\tCPU\tMEM NOW\tMEM REC\tMEM\tRISK\t$/MO SAVED")
	for _, r := range recs {
		for _, c := range r.Containers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\n",
				r.Namespace, r.Name, c.Container,
				quantity.CPUString(model.Millicores(c.CPU.Current)), targetStr(c.CPU, true), short(c.CPU.Decision),
				quantity.MemoryString(model.Bytes(c.Memory.Current)), targetStr(c.Memory, false), short(c.Memory.Decision),
				worstRisk(c), r.Cost.MonthlySavingsUSD)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	s := snap.Summary
	fmt.Printf("\nAllocation-based estimate over %d workloads (policy %s)\n", s.Workloads, snap.PolicyID)
	fmt.Printf("  current:     $%.2f/month\n", s.CurrentMonthlyUSD)
	fmt.Printf("  recommended: $%.2f/month\n", s.RecommendedMonthlyUSD)
	fmt.Printf("  savings:     $%.2f/month (%.1f%%)\n", s.MonthlySavingsUSD, s.SavingsFraction*100)
	fmt.Printf("  decisions:   %d decrease, %d increase, %d no change, %d blocked, %d insufficient data\n",
		s.Decreases, s.Increases, s.NoChanges, s.Blocked, s.InsufficientData)
	// The caveat is printed every time, not hidden in documentation: a savings
	// figure read as a cloud bill is the most likely way this tool misleads.
	fmt.Printf("\nThis prices reserved capacity, not cloud invoices. A saving is realised only when\n" +
		"the freed capacity lets the cluster run fewer nodes. See docs/cost-model.md.\n")
	if len(snap.Errors) > 0 {
		fmt.Printf("\n%d workloads could not be analysed:\n", len(snap.Errors))
		for _, e := range snap.Errors {
			fmt.Printf("  %s: %s\n", e.Workload, e.Error)
		}
	}
	// Blocked and insufficient-data reasons are worth surfacing: they are the
	// cases where an operator most wants to know why the tool declined.
	printWithheld(recs)
	return nil
}

func printWithheld(recs []model.WorkloadRecommendation) {
	type row struct{ key, resource, reason string }
	var rows []row
	for _, r := range recs {
		for _, c := range r.Containers {
			for _, adv := range []struct {
				res string
				rec model.ResourceRecommendation
			}{{"cpu", c.CPU}, {"memory", c.Memory}} {
				if adv.rec.Decision == model.DecisionBlocked || adv.rec.Decision == model.DecisionInsufficientData {
					rows = append(rows, row{r.Key() + "/" + c.Container, adv.res, adv.rec.Reason})
				}
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Printf("\nWithheld recommendations (%d):\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  %s [%s]: %s\n", r.key, r.resource, r.reason)
	}
}

func printWorkloadTable(snap api.Snapshot) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tWORKLOAD\tKIND\tREPLICAS\tCONTAINER\tCPU REQ\tMEM REQ\tRESTARTS\tOOM")
	for _, wl := range snap.Workloads {
		for _, c := range wl.Containers {
			ev, _ := wl.EvidenceFor(c.Name)
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\t%d\n",
				wl.Namespace, wl.Name, wl.Kind, wl.Replicas, c.Name,
				quantity.CPUString(c.Declared.CPURequest),
				quantity.MemoryString(c.Declared.MemoryRequest),
				ev.Restarts, ev.OOMKills)
		}
	}
	return w.Flush()
}

func printPricing() error {
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE\tPROVIDER\tREGION\tVCPU\tGIB\t$/HOUR\t$/CORE-HOUR\t$/GIB-HOUR")
	for _, it := range cost.Catalog() {
		m, err := cost.DeriveModel(it, cost.CPUCostShare)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.0f\t%.0f\t%.4f\t%.5f\t%.5f\n",
			it.Name, it.Provider, it.Region, it.VCPU, it.MemoryGiB, it.HourlyUSD,
			m.CPUHourUSD, m.MemoryGiBHourUSD)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nPer-unit rates are derived by splitting the instance price %.0f/%.0f between CPU and memory.\n",
		cost.CPUCostShare*100, (1-cost.CPUCostShare)*100)
	fmt.Printf("That split is an assumption, not a published figure; see docs/cost-model.md.\n")
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func targetStr(r model.ResourceRecommendation, isCPU bool) string {
	if r.Decision == model.DecisionInsufficientData {
		return "-"
	}
	if isCPU {
		return quantity.CPUString(model.Millicores(r.Target))
	}
	return quantity.MemoryString(model.Bytes(r.Target))
}

func short(d model.Decision) string {
	switch d {
	case model.DecisionDecrease:
		return "down"
	case model.DecisionIncrease:
		return "up"
	case model.DecisionNoChange:
		return "keep"
	case model.DecisionBlocked:
		return "BLOCKED"
	case model.DecisionInsufficientData:
		return "no data"
	default:
		return string(d)
	}
}

// worstRisk reports the highest risk across a container's resources, because
// that is the one that determines whether applying the change is safe.
func worstRisk(c model.ContainerRecommendation) string {
	rank := map[model.RiskLevel]int{
		model.RiskLow: 0, model.RiskUnknown: 1, model.RiskModerate: 2, model.RiskHigh: 3,
	}
	worst := c.CPU.Risk
	if rank[c.Memory.Risk] > rank[worst] {
		worst = c.Memory.Risk
	}
	return string(worst)
}
