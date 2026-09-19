// Command workload-gen produces controlled CPU and memory load inside a
// Kubernetes cluster.
//
// # Why a real load generator is needed
//
// The simulator in internal/simulator generates demand traces analytically,
// which gives exact ground truth but tells us nothing about whether the
// production collection path works: whether the Prometheus queries match the
// right series, whether cAdvisor's working-set metric behaves as assumed,
// whether the pod-name regexes resolve correctly. This binary closes that gap.
// It runs in a real pod, burns real CPU and allocates real memory to a
// configured schedule, and so produces metrics through the same cAdvisor ->
// Prometheus -> optimizer path that production uses.
//
// The two are complementary and neither substitutes for the other: the simulator
// provides ground truth without production fidelity, and this generator provides
// production fidelity without exact ground truth (the cluster interferes with
// what it achieves). research/experimental_setup.md states which results come
// from which.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "workload-gen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mode          = flag.String("mode", "stable-cpu", "workload class: stable-cpu, bursty-cpu, periodic-cpu, spiky-cpu, stable-memory, growing-memory, bursty-memory, sawtooth-memory, mixed, idle")
		baselineCPU   = flag.Int("baseline-cpu-milli", 200, "baseline CPU demand in millicores")
		burstCPU      = flag.Int("burst-cpu-milli", 1000, "burst CPU demand in millicores")
		baselineMem   = flag.Int("baseline-memory-mib", 256, "baseline resident memory in MiB")
		peakMem       = flag.Int("peak-memory-mib", 512, "peak resident memory in MiB")
		burstDuration = flag.Duration("burst-duration", 30*time.Second, "burst duration")
		burstInterval = flag.Duration("burst-interval", 5*time.Minute, "mean interval between bursts")
		cycleLength   = flag.Duration("cycle-length", 30*time.Minute, "cycle length for periodic and sawtooth classes")
		growthFactor  = flag.Float64("growth-factor", 2.0, "total memory growth over the run for growing-memory")
		runFor        = flag.Duration("duration", 0, "stop after this long (0 runs until terminated)")
		seed          = flag.Int64("seed", 1, "random seed for burst arrival")
		addr          = flag.String("addr", ":8081", "address for the health endpoint")
	)
	flag.Parse()

	cfg := loadConfig{
		mode:          *mode,
		baselineCPU:   float64(*baselineCPU),
		burstCPU:      float64(*burstCPU),
		baselineMem:   int64(*baselineMem) << 20,
		peakMem:       int64(*peakMem) << 20,
		burstDuration: *burstDuration,
		burstInterval: *burstInterval,
		cycleLength:   *cycleLength,
		growthFactor:  *growthFactor,
		runFor:        *runFor,
		rng:           rand.New(rand.NewSource(*seed)),
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.runFor)
		defer cancel()
	}

	gen := &generator{cfg: cfg, started: time.Now()}

	// A health endpoint lets Kubernetes probes work and gives the e2e test a way
	// to confirm the generator is actually running before it starts trusting the
	// metrics.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","mode":%q,"elapsed_seconds":%.0f,"target_cpu_milli":%.0f,"resident_mib":%d}`+"\n",
			cfg.mode, time.Since(gen.started).Seconds(),
			gen.targetCPU.Load(), gen.residentMiB.Load())
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()

	fmt.Printf("workload-gen: mode=%s baseline=%.0fm burst=%.0fm memory=%dMiB..%dMiB\n",
		cfg.mode, cfg.baselineCPU, cfg.burstCPU, cfg.baselineMem>>20, cfg.peakMem>>20)

	return gen.run(ctx)
}

type loadConfig struct {
	mode          string
	baselineCPU   float64
	burstCPU      float64
	baselineMem   int64
	peakMem       int64
	burstDuration time.Duration
	burstInterval time.Duration
	cycleLength   time.Duration
	growthFactor  float64
	runFor        time.Duration
	rng           *rand.Rand
}

func (c loadConfig) validate() error {
	switch c.mode {
	case "stable-cpu", "bursty-cpu", "periodic-cpu", "spiky-cpu",
		"stable-memory", "growing-memory", "bursty-memory", "sawtooth-memory", "mixed", "idle":
	default:
		return fmt.Errorf("unknown mode %q", c.mode)
	}
	if c.baselineCPU < 0 || c.burstCPU < 0 {
		return fmt.Errorf("CPU demand must be non-negative")
	}
	if c.baselineMem < 0 || c.peakMem < 0 {
		return fmt.Errorf("memory must be non-negative")
	}
	if c.burstInterval <= 0 || c.burstDuration <= 0 {
		return fmt.Errorf("burst duration and interval must be positive")
	}
	return nil
}

type generator struct {
	cfg     loadConfig
	started time.Time

	// targetCPU and residentMiB are published for the health endpoint.
	targetCPU   atomic.Value // float64
	residentMiB atomic.Int64

	// ballast holds the allocated memory. It is a slice of slices so that memory
	// can be released in chunks for the sawtooth class.
	mu      sync.Mutex
	ballast [][]byte
}

const (
	// controlInterval is how often the target load is recomputed. One second is
	// short enough to render a 30-second burst faithfully and long enough that
	// the control loop's own CPU cost is negligible.
	controlInterval = time.Second
	// ballastChunk is the allocation granularity. 4 MiB chunks keep the number of
	// live objects small while allowing reasonably fine control of the resident
	// set.
	ballastChunk = 4 << 20
)

func (g *generator) run(ctx context.Context) error {
	var wg sync.WaitGroup
	// One CPU worker per available core, so that a burst target above one core is
	// actually achievable. Each worker duty-cycles independently against a shared
	// target.
	workers := runtime.NumCPU()
	g.targetCPU.Store(0.0)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.burnLoop(ctx, workers)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.controlLoop(ctx)
	}()

	wg.Wait()
	return nil
}

// controlLoop recomputes the demand targets on a fixed cadence.
//
// It mirrors the generative model in internal/simulator so that a class named
// "bursty-cpu" here has the same shape as the class of that name in the
// simulator. Keeping the two aligned is what allows the e2e run to be read as a
// fidelity check on the simulated results rather than an unrelated experiment.
func (g *generator) controlLoop(ctx context.Context) {
	t := time.NewTicker(controlInterval)
	defer t.Stop()

	nextBurst := g.nextBurstDelay()
	burstUntil := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			elapsed := now.Sub(g.started)

			// Burst scheduling, shared by the burst-driven classes.
			inBurst := now.Before(burstUntil)
			if !inBurst && elapsed >= nextBurst {
				burstUntil = now.Add(g.cfg.burstDuration)
				nextBurst = elapsed + g.nextBurstDelay()
				inBurst = true
			}

			cpu, mem := g.targetsAt(elapsed, inBurst)
			g.targetCPU.Store(cpu)
			g.setResident(mem)
		}
	}
}

func (g *generator) nextBurstDelay() time.Duration {
	// Exponential arrivals, so bursts are irregular in the way real traffic is
	// rather than landing on a fixed grid that a percentile could learn.
	return time.Duration(g.cfg.rng.ExpFloat64() * float64(g.cfg.burstInterval))
}

func (g *generator) targetsAt(elapsed time.Duration, inBurst bool) (cpuMilli float64, memBytes int64) {
	c := g.cfg
	progress := 0.0
	if c.runFor > 0 {
		progress = math.Min(1, float64(elapsed)/float64(c.runFor))
	}
	cyclePhase := func() float64 {
		if c.cycleLength <= 0 {
			return 0
		}
		return 2 * math.Pi * math.Mod(elapsed.Seconds(), c.cycleLength.Seconds()) / c.cycleLength.Seconds()
	}

	switch c.mode {
	case "stable-cpu", "stable-memory":
		return c.baselineCPU, c.baselineMem
	case "bursty-cpu", "spiky-cpu":
		if inBurst {
			return c.burstCPU, c.baselineMem
		}
		return c.baselineCPU, c.baselineMem
	case "periodic-cpu":
		shape := 0.5 * (1 - math.Cos(cyclePhase()))
		return c.baselineCPU + (c.burstCPU-c.baselineCPU)*shape, c.baselineMem
	case "growing-memory":
		g := 1.0
		if c.growthFactor > 0 {
			g = 1 + (c.growthFactor-1)*progress
		}
		return c.baselineCPU, int64(float64(c.baselineMem) * g)
	case "bursty-memory":
		if inBurst {
			return c.baselineCPU, c.peakMem
		}
		return c.baselineCPU, c.baselineMem
	case "sawtooth-memory":
		cycle := c.cycleLength
		if cycle <= 0 {
			cycle = 30 * time.Minute
		}
		frac := math.Mod(elapsed.Seconds(), cycle.Seconds()) / cycle.Seconds()
		mem := int64(float64(c.baselineMem) + float64(c.peakMem-c.baselineMem)*frac)
		cpu := c.baselineCPU
		if frac > 0.95 {
			cpu = c.burstCPU // collection costs CPU
		}
		return cpu, mem
	case "mixed":
		load := 0.5 * (1 - math.Cos(cyclePhase()))
		if inBurst {
			load = math.Min(1, load+0.6)
		}
		return c.baselineCPU + (c.burstCPU-c.baselineCPU)*load,
			int64(float64(c.baselineMem) + float64(c.peakMem-c.baselineMem)*load*0.6)
	case "idle":
		return c.baselineCPU, c.baselineMem
	default:
		return c.baselineCPU, c.baselineMem
	}
}

// burnLoop duty-cycles a busy loop to approximate a share of the CPU target.
//
// Duty cycling is used rather than a tight spin because the target is a
// *fraction* of a core: spinning continuously would consume a whole core
// regardless of the target. Each worker takes an equal share of the target, so
// the aggregate across workers approximates the configured millicores.
func (g *generator) burnLoop(ctx context.Context, workers int) {
	const slice = 20 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		target, _ := g.targetCPU.Load().(float64)
		share := target / float64(workers) / 1000.0 // fraction of one core
		if share <= 0 {
			// Sleeping a whole slice rather than busy-waiting means an idle
			// generator genuinely idles, which matters because the idle class is
			// supposed to look idle in the metrics.
			time.Sleep(slice)
			continue
		}
		if share > 1 {
			share = 1
		}
		busy := time.Duration(float64(slice) * share)
		idle := slice - busy

		deadline := time.Now().Add(busy)
		// A trivial arithmetic loop the compiler cannot elide, because the result
		// escapes through the atomic store below.
		var x float64 = 1.0001
		for time.Now().Before(deadline) {
			for i := 0; i < 2000; i++ {
				x = x * 1.0000001
				if x > 1e300 {
					x = 1.0001
				}
			}
		}
		sink.Store(x)
		if idle > 0 {
			time.Sleep(idle)
		}
	}
}

// sink prevents the compiler from optimising the burn loop away.
var sink atomic.Value

// setResident grows or shrinks the resident set toward the target.
//
// The allocated bytes are written to, not merely allocated: an untouched Go
// allocation may not be backed by physical pages, so it would never appear in
// container_memory_working_set_bytes and the generator would silently produce no
// memory load at all.
func (g *generator) setResident(target int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	current := int64(len(g.ballast)) * ballastChunk
	for current < target {
		chunk := make([]byte, ballastChunk)
		// Touch one byte per 4 KiB page so the kernel actually backs it.
		for i := 0; i < len(chunk); i += 4096 {
			chunk[i] = byte(i)
		}
		g.ballast = append(g.ballast, chunk)
		current += ballastChunk
	}
	for current > target && len(g.ballast) > 0 {
		g.ballast[len(g.ballast)-1] = nil
		g.ballast = g.ballast[:len(g.ballast)-1]
		current -= ballastChunk
	}
	g.residentMiB.Store(current >> 20)
}
