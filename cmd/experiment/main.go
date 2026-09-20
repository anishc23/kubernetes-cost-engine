// Command experiment runs the controlled right-sizing studies described in
// research/methodology.md and writes machine-readable results.
//
// The experiments drive the same recommendation engine the production controller
// uses. Only the data source differs: synthetic traces with known ground truth
// rather than Prometheus. That is what makes the findings statements about the
// deployed system rather than about a separate research prototype.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/anishc23/k8s-cost-optimizer/internal/experiment"
	"github.com/anishc23/k8s-cost-optimizer/internal/logging"
	"github.com/anishc23/k8s-cost-optimizer/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "experiment: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to an experiment configuration file (required)")
		outDir      = flag.String("out", "experiments/results", "directory to write results into")
		concurrency = flag.Int("concurrency", 0, "parallel conditions (default: number of CPUs)")
		dryRun      = flag.Bool("dry-run", false, "report the size of the experiment matrix and exit")
		archive     = flag.Bool("archive-json", false, "also write the full JSON archive (large; not version-controlled)")
		logLevel    = flag.String("log-level", "info", "log level")
	)
	flag.Parse()

	if *configPath == "" {
		flag.Usage()
		return fmt.Errorf("-config is required")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	log := logging.New(*logLevel, "text")

	runner, err := experiment.NewRunner(cfg, log)
	if err != nil {
		return err
	}
	if *concurrency > 0 {
		runner.Concurrency = *concurrency
	}
	runner.ToolVersion = version.Version
	runner.GitCommit, runner.GitDirty = gitState()

	if *dryRun {
		fmt.Printf("experiment:   %s\n", cfg.Name)
		fmt.Printf("description:  %s\n", cfg.Description)
		fmt.Printf("conditions:   %d\n", cfg.Conditions())
		fmt.Printf("config hash:  %s\n", experiment.ConfigHash(cfg))
		fmt.Printf("trace length: %s at %s resolution (%d samples per trace)\n",
			cfg.TraceDuration, cfg.Step, int(cfg.TraceDuration/cfg.Step)+1)
		fmt.Printf("held out:     %s evaluation horizon\n", cfg.EvaluationHorizon)
		return nil
	}

	if runner.GitDirty {
		// A dirty run is allowed but marked: its results cannot be reproduced
		// from the recorded commit alone, and a reader deserves to know that.
		log.Warn("the working tree has uncommitted changes; results will be marked git_dirty=true and are not reproducible from the recorded commit")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	res, err := runner.Run(ctx)
	if err != nil {
		return err
	}

	base := filepath.Join(*outDir, cfg.Name)
	// The gzipped CSV and the provenance file are the committed evidence: together
	// they are everything needed to check a figure or reproduce a run, at a few
	// megabytes. The full JSON archive repeats the same records with provenance
	// attached and is an order of magnitude larger, so it is written for local use
	// and left out of version control (see .gitignore).
	if err := experiment.WriteCSV(res, base+".csv.gz"); err != nil {
		return err
	}
	if err := experiment.WriteProvenance(res, base+".provenance.json"); err != nil {
		return err
	}
	if *archive {
		if err := experiment.WriteJSON(res, base+".json"); err != nil {
			return err
		}
	}

	fmt.Printf("\n%s: %d records in %s\n", cfg.Name, len(res.Records), time.Since(start).Round(time.Millisecond))
	fmt.Printf("  %s.csv.gz\n  %s.provenance.json\n", base, base)
	if *archive {
		fmt.Printf("  %s.json\n", base)
	}
	return nil
}

func loadConfig(path string) (experiment.Config, error) {
	var cfg experiment.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// gitState records the commit and whether the tree is dirty.
//
// Failures are non-fatal: an experiment run from a source tarball outside a
// repository should still produce results, marked with an unknown commit, rather
// than refusing to run.
func gitState() (commit string, dirty bool) {
	commit = "unknown"
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		dirty = len(strings.TrimSpace(string(out))) > 0
	}
	return commit, dirty
}
