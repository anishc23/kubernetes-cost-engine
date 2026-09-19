package experiment

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// WriteJSON writes the full result, including provenance and the resolved
// configuration, as indented JSON.
//
// JSON is the archival format: it is self-describing, keeps provenance attached
// to the data, and survives schema changes. CSV is the analysis format. Both are
// written because the analysis layer is pandas and the archive needs to be
// readable without it.
func WriteJSON(res *Result, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create results directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	return nil
}

// csvColumns is the column order of the results CSV.
//
// The order is fixed explicitly rather than derived by reflection so that a
// change to a struct field's position cannot silently reorder a published data
// file. Adding a column is a deliberate edit here.
var csvColumns = []string{
	"experiment", "run_id", "workload_class", "workload_name", "seed",
	"cpu_strategy", "memory_strategy", "cpu_safety_factor", "memory_safety_factor",
	"observation_window_hours", "evaluation_horizon_hours", "step_seconds", "policy_id",
	"oom_protection_enabled", "usage_exceeds_enabled", "floors_enabled",
	"min_change_enabled", "rounding_enabled", "unified_strategy",
	"declared_cpu_milli", "declared_memory_bytes",
	"cpu_decision", "memory_decision", "cpu_risk", "memory_risk",
	"recommended_cpu_milli", "recommended_memory_bytes", "raw_cpu_milli", "raw_memory_bytes",
	"cpu_gates", "memory_gates",
	"observed_cpu_mean_milli", "observed_cpu_p95_milli", "observed_cpu_p99_milli",
	"observed_cpu_max_milli", "observed_cpu_samples",
	"observed_memory_mean_bytes", "observed_memory_p95_bytes",
	"observed_memory_p99_bytes", "observed_memory_max_bytes",
	"true_cpu_mean_milli", "true_cpu_p95_milli", "true_cpu_p99_milli", "true_cpu_max_milli",
	"true_memory_mean_bytes", "true_memory_p95_bytes", "true_memory_p99_bytes", "true_memory_max_bytes",
	"savings_fraction", "monthly_savings_usd", "current_monthly_usd",
	"cpu_utilization", "memory_utilization",
	"cpu_violation_rate", "cpu_throttle_fraction", "cpu_throttled_core_seconds",
	"memory_violation_rate", "oom_kills", "oom_kills_per_day", "survived_without_oom",
	"cpu_over_provision_ratio", "memory_over_provision_ratio",
	"cpu_relative_error", "memory_relative_error",
	"feasible", "feasible_savings",
	"stability_median_abs_log2_ratio", "stability_max_abs_log2_ratio",
	"stability_change_rate", "stability_range_ratio", "stability_recomputations",
}

// WriteCSV writes the records as a flat CSV for the Python analysis layer.
func WriteCSV(res *Result, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create results directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write(csvColumns); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for i := range res.Records {
		if err := w.Write(res.Records[i].csvRow()); err != nil {
			return fmt.Errorf("write record %d: %w", i, err)
		}
	}
	w.Flush()
	return w.Error()
}

func (r Record) csvRow() []string {
	// Floats are written with %g at full precision rather than a fixed number of
	// decimals: rounding here would quietly limit what the analysis can resolve,
	// and small quantities (throttle fractions near 1e-5) would collapse to zero.
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	b := func(v bool) string { return strconv.FormatBool(v) }

	var stMed, stMax, stChange, stRange string
	var stN string
	if r.Stability != nil {
		stMed = f(r.Stability.MedianAbsLog2Ratio)
		stMax = f(r.Stability.MaxAbsLog2Ratio)
		stChange = f(r.Stability.ChangeRate)
		stRange = f(r.Stability.RangeRatio)
		stN = strconv.Itoa(r.Stability.Recomputations)
	}

	return []string{
		r.Experiment, r.RunID, r.WorkloadClass, r.WorkloadName, strconv.FormatInt(r.Seed, 10),
		r.CPUStrategy, r.MemoryStrategy, f(r.CPUSafetyFactor), f(r.MemorySafetyFactor),
		f(r.ObservationWindowHours), f(r.EvaluationHorizonHours), f(r.StepSeconds), r.PolicyID,
		b(r.OOMProtectionEnabled), b(r.UsageExceedsEnabled), b(r.FloorsEnabled),
		b(r.MinChangeEnabled), b(r.RoundingEnabled), b(r.UnifiedStrategy),
		f(r.DeclaredCPUMilli), f(r.DeclaredMemoryBytes),
		r.CPUDecision, r.MemoryDecision, r.CPURisk, r.MemoryRisk,
		f(r.RecommendedCPUMilli), f(r.RecommendedMemoryBytes), f(r.RawCPUMilli), f(r.RawMemoryBytes),
		r.CPUGates, r.MemoryGates,
		f(r.ObservedCPUMeanMilli), f(r.ObservedCPUP95Milli), f(r.ObservedCPUP99Milli),
		f(r.ObservedCPUMaxMilli), strconv.Itoa(r.ObservedCPUSamples),
		f(r.ObservedMemMeanBytes), f(r.ObservedMemP95Bytes),
		f(r.ObservedMemP99Bytes), f(r.ObservedMemMaxBytes),
		f(r.TrueCPUMeanMilli), f(r.TrueCPUP95Milli), f(r.TrueCPUP99Milli), f(r.TrueCPUMaxMilli),
		f(r.TrueMemMeanBytes), f(r.TrueMemP95Bytes), f(r.TrueMemP99Bytes), f(r.TrueMemMaxBytes),
		f(r.SavingsFraction), f(r.MonthlySavingsUSD), f(r.CurrentMonthlyUSD),
		f(r.CPUUtilization), f(r.MemoryUtilization),
		f(r.CPUViolationRate), f(r.CPUThrottleFraction), f(r.CPUThrottledCoreSeconds),
		f(r.MemoryViolationRate), strconv.Itoa(r.OOMKills), f(r.OOMKillsPerDay), b(r.SurvivedWithoutOOM),
		f(r.CPUOverProvisionRatio), f(r.MemoryOverProvisionRatio),
		f(r.CPURelativeError), f(r.MemoryRelativeError),
		b(r.Feasible), f(r.FeasibleSavings),
		stMed, stMax, stChange, stRange, stN,
	}
}

// WriteProvenance writes a standalone provenance file next to the results, so
// that the run metadata is legible without parsing a large data file.
func WriteProvenance(res *Result, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		Provenance Provenance `json:"provenance"`
		Config     Config     `json:"config"`
	}{res.Provenance, res.Config}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
