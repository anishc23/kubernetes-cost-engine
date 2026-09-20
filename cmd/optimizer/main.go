// Command optimizer runs the Kubernetes cost optimization engine: it discovers
// workloads, collects their usage history from Prometheus, generates
// right-sizing recommendations, and serves them over a REST API.
//
// The controller and the API server run in one process. This is a deliberate
// choice against splitting them: the API serves a snapshot the controller
// produces, so separating them would require a shared store and a
// synchronisation protocol to solve a problem that does not exist at this scale.
// A modular monolith with clean package boundaries is the right shape here, and
// docs/architecture.md records what would have to change for that to stop being
// true.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anishc23/k8s-cost-optimizer/internal/api"
	"github.com/anishc23/k8s-cost-optimizer/internal/config"
	"github.com/anishc23/k8s-cost-optimizer/internal/cost"
	"github.com/anishc23/k8s-cost-optimizer/internal/kube"
	"github.com/anishc23/k8s-cost-optimizer/internal/logging"
	"github.com/anishc23/k8s-cost-optimizer/internal/metrics"
	"github.com/anishc23/k8s-cost-optimizer/internal/promapi"
	"github.com/anishc23/k8s-cost-optimizer/internal/recommender"
	"github.com/anishc23/k8s-cost-optimizer/internal/version"
	"github.com/anishc23/k8s-cost-optimizer/pkg/humanize"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "optimizer: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to a YAML configuration file")
		promAddr    = flag.String("prometheus-address", "", "Prometheus base URL (overrides the config file)")
		addr        = flag.String("addr", "", "API listen address (overrides the config file)")
		metricsAddr = flag.String("metrics-addr", "", "metrics listen address (overrides the config file)")
		interval    = flag.Duration("interval", 0, "analysis interval (overrides the config file)")
		window      = flag.Duration("observation-window", 0, "observation window (overrides the config file)")
		showVersion = flag.Bool("version", false, "print version information and exit")
		printConfig = flag.Bool("print-config", false, "print the resolved configuration and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("optimizer %s (commit %s, built %s, %s)\n",
			version.Version, version.Commit, version.BuildDate, version.GoVersion())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Flags override the file, which overrides the defaults.
	if *promAddr != "" {
		cfg.Prometheus.Address = *promAddr
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	if *metricsAddr != "" {
		cfg.Server.MetricsAddr = *metricsAddr
	}
	if *interval > 0 {
		cfg.Analysis.Interval = humanize.Duration(*interval)
	}
	if *window > 0 {
		cfg.Policy.ObservationWindow = humanize.Duration(*window)
	}

	// Validation runs before any component is built, so a misconfiguration fails
	// in the first second rather than producing wrong recommendations later.
	if err := cfg.Validate(); err != nil {
		return err
	}

	if *printConfig {
		return printResolvedConfig(cfg)
	}

	log := logging.New(cfg.Log.Level, cfg.Log.Format)
	log.Info("starting optimizer",
		"version", version.Version, "commit", version.Commit,
		"prometheus", cfg.Prometheus.Address,
		"observation_window", cfg.Policy.ObservationWindow.String(),
		"analysis_interval", cfg.Analysis.Interval.String(),
		"apply", cfg.Analysis.Apply)

	if cfg.Analysis.Apply {
		// Applying recommendations mutates production workloads and restarts
		// their pods. It is opt-in, and when it is on the operator should see
		// that in the logs without having to go looking.
		log.Warn("apply mode is ENABLED: recommendations will be written back to the cluster, restarting pods",
			"max_decrease_fraction", cfg.Analysis.ApplyMaxDecreaseFraction)
	}

	m := metrics.New()

	policy, err := cfg.Policy.ToPolicy()
	if err != nil {
		return err
	}
	engine, err := recommender.NewEngine(policy)
	if err != nil {
		return err
	}
	log.Info("recommendation policy loaded",
		"policy_id", policy.PolicyID(),
		"cpu", fmt.Sprintf("%s x%.2f", policy.CPUStrategy, policy.CPUSafetyFactor),
		"memory", fmt.Sprintf("%s x%.2f", policy.MemoryStrategy, policy.MemorySafetyFactor))

	costModel, err := cfg.CostModel()
	if err != nil {
		return err
	}
	estimator, err := cost.NewEstimator(costModel)
	if err != nil {
		return err
	}
	log.Info("cost model loaded", "model", costModel.Name, "source", costModel.Source)

	discoverer, err := kube.NewClient(cfg.Kubernetes.Kubeconfig, cfg.KubeOptions(), log, m)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}

	promClient, err := promapi.NewClient(promapi.Config{
		Address:         cfg.Prometheus.Address,
		Timeout:         cfg.Prometheus.Timeout.D(),
		Step:            cfg.Prometheus.Step.D(),
		BearerTokenFile: cfg.Prometheus.BearerTokenFile,
	}, log, m)
	if err != nil {
		return fmt.Errorf("prometheus: %w", err)
	}

	svc := api.NewService(discoverer, promClient, engine, estimator,
		api.Options{Concurrency: cfg.Analysis.Concurrency, QueryStep: cfg.Prometheus.Step.D()}, log, m)

	server := api.NewServer(svc, log, m, []api.NamedCheck{
		{Name: "prometheus", Check: promClient.Healthy},
		{Name: "kubernetes", Check: discoverer.Healthy},
	}, api.BuildInfo{
		Version: version.Version, Commit: version.Commit,
		BuildDate: version.BuildDate, GoVersion: version.GoVersion(),
	})

	// SIGTERM is what Kubernetes sends on pod termination; handling it allows
	// in-flight requests to finish instead of being severed mid-response.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	apiSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout.D(),
		WriteTimeout: cfg.Server.WriteTimeout.D(),
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", server.MetricsHandler())
	metricsSrv := &http.Server{
		Addr:         cfg.Server.MetricsAddr,
		Handler:      metricsMux,
		ReadTimeout:  cfg.Server.ReadTimeout.D(),
		WriteTimeout: cfg.Server.WriteTimeout.D(),
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("api listening", "addr", cfg.Server.Addr)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api server: %w", err)
		}
	}()
	go func() {
		log.Info("metrics listening", "addr", cfg.Server.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	go svc.Run(ctx, cfg.Analysis.Interval.D())

	select {
	case err := <-errCh:
		stop()
		shutdown(apiSrv, metricsSrv, cfg.Server.ShutdownTimeout.D())
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
		shutdown(apiSrv, metricsSrv, cfg.Server.ShutdownTimeout.D())
		log.Info("optimizer stopped")
		return nil
	}
}

func shutdown(servers ...any) {
	// The final argument is the timeout; the preceding ones are servers.
	timeout := 20 * time.Second
	if len(servers) > 0 {
		if d, ok := servers[len(servers)-1].(time.Duration); ok {
			timeout = d
			servers = servers[:len(servers)-1]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, s := range servers {
		if srv, ok := s.(*http.Server); ok {
			_ = srv.Shutdown(ctx)
		}
	}
}

func printResolvedConfig(cfg config.Config) error {
	enc := newYAMLEncoder(os.Stdout)
	return enc.Encode(cfg)
}
