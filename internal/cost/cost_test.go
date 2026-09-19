package cost

import (
	"math"
	"testing"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

func approx(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v want %v", msg, got, want)
	}
}

// Deriving rates from an instance price must conserve the instance price: a
// fully packed node must cost exactly what the provider charges for it. If it
// did not, every estimate would carry a systematic error.
func TestDerivedRatesConserveInstancePrice(t *testing.T) {
	for _, it := range Catalog() {
		m, err := DeriveModel(it, CPUCostShare)
		if err != nil {
			t.Fatalf("%s: %v", it.Name, err)
		}
		full := m.HourlyUSD(
			model.Millicores(it.VCPU*1000),
			model.Bytes(it.MemoryGiB*float64(model.BytesPerGi)),
		)
		approx(t, full, it.HourlyUSD, 1e-9,
			"fully allocated "+it.Name+" should cost its list price")
	}
}

func TestDeriveModelRejectsBadInput(t *testing.T) {
	good := InstanceType{Name: "x", VCPU: 4, MemoryGiB: 16, HourlyUSD: 0.2}
	cases := []struct {
		name  string
		it    InstanceType
		share float64
	}{
		{"zero vcpu", InstanceType{Name: "x", VCPU: 0, MemoryGiB: 16, HourlyUSD: 0.2}, 0.7},
		{"zero memory", InstanceType{Name: "x", VCPU: 4, MemoryGiB: 0, HourlyUSD: 0.2}, 0.7},
		{"zero price", InstanceType{Name: "x", VCPU: 4, MemoryGiB: 16, HourlyUSD: 0}, 0.7},
		{"share 0", good, 0},
		{"share 1", good, 1},
		{"share negative", good, -0.5},
	}
	for _, c := range cases {
		if _, err := DeriveModel(c.it, c.share); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

func TestModelValidate(t *testing.T) {
	if err := DefaultModel().Validate(); err != nil {
		t.Errorf("default model should be valid: %v", err)
	}
	cases := []Model{
		{Name: "negative cpu", CPUHourUSD: -1, MemoryGiBHourUSD: 1},
		{Name: "negative mem", CPUHourUSD: 1, MemoryGiBHourUSD: -1},
		{Name: "all zero"},
	}
	for _, m := range cases {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", m.Name)
		}
	}
}

func TestMonthlyUsesSevenThirtyHours(t *testing.T) {
	m := Model{Name: "unit", CPUHourUSD: 1, MemoryGiBHourUSD: 0}
	approx(t, m.MonthlyUSD(1000, 0), 730, 1e-9, "one core-month at $1/h")
	m2 := Model{Name: "unit", CPUHourUSD: 0, MemoryGiBHourUSD: 1}
	approx(t, m2.MonthlyUSD(0, model.Bytes(model.BytesPerGi)), 730, 1e-9, "one GiB-month at $1/h")
}

func TestHourlyIsLinearInBothResources(t *testing.T) {
	m := Model{Name: "unit", CPUHourUSD: 0.03, MemoryGiBHourUSD: 0.004}
	a := m.HourlyUSD(1000, model.Bytes(model.BytesPerGi))
	b := m.HourlyUSD(2000, model.Bytes(2*model.BytesPerGi))
	approx(t, b, 2*a, 1e-12, "doubling both resources should double the price")
	approx(t, a, 0.034, 1e-12, "1 core + 1 GiB")
}

// Replica count must scale cost: the request is per container but the charge is
// per pod, so ignoring replicas would systematically mis-rank workloads.
func TestEstimateScalesWithReplicas(t *testing.T) {
	est, err := NewEstimator(DefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(replicas int32) model.WorkloadRecommendation {
		return model.WorkloadRecommendation{
			Replicas: replicas,
			Containers: []model.ContainerRecommendation{{
				Container: "app",
				CPU: model.ResourceRecommendation{
					Resource: model.ResourceCPU, Decision: model.DecisionDecrease,
					Current: 1000, Target: 500,
				},
				Memory: model.ResourceRecommendation{
					Resource: model.ResourceMemory, Decision: model.DecisionDecrease,
					Current: float64(2 * model.BytesPerGi), Target: float64(model.BytesPerGi),
				},
			}},
		}
	}
	one := mk(1)
	ten := mk(10)
	est.Estimate(&one)
	est.Estimate(&ten)
	approx(t, ten.Cost.CurrentMonthlyUSD, 10*one.Cost.CurrentMonthlyUSD, 1e-9, "current cost scales with replicas")
	approx(t, ten.Cost.MonthlySavingsUSD, 10*one.Cost.MonthlySavingsUSD, 1e-9, "savings scale with replicas")
	// The savings *fraction* must not change with replica count.
	approx(t, ten.Cost.SavingsFraction, one.Cost.SavingsFraction, 1e-12, "savings fraction is replica-invariant")
	approx(t, one.Cost.SavingsFraction, 0.5, 1e-9, "halving both resources halves the cost")
}

// A zero-replica workload still has a declared request that will be charged the
// moment it scales up; pricing it at zero would hide exactly the forgotten
// workloads the engine is most useful for.
func TestZeroReplicasPricedAsOne(t *testing.T) {
	est, _ := NewEstimator(DefaultModel())
	rec := model.WorkloadRecommendation{
		Replicas: 0,
		Containers: []model.ContainerRecommendation{{
			CPU:    model.ResourceRecommendation{Decision: model.DecisionNoChange, Current: 1000, Target: 1000},
			Memory: model.ResourceRecommendation{Decision: model.DecisionNoChange, Current: float64(model.BytesPerGi), Target: float64(model.BytesPerGi)},
		}},
	}
	est.Estimate(&rec)
	if rec.Cost.CurrentMonthlyUSD <= 0 {
		t.Error("a scaled-to-zero workload should still be priced at its declared request")
	}
}

// "We do not know" must never be reported as a saving.
func TestInsufficientDataIsPricedAtCurrent(t *testing.T) {
	est, _ := NewEstimator(DefaultModel())
	rec := model.WorkloadRecommendation{
		Replicas: 1,
		Containers: []model.ContainerRecommendation{{
			CPU: model.ResourceRecommendation{
				Decision: model.DecisionInsufficientData, Current: 2000, Target: 0,
			},
			Memory: model.ResourceRecommendation{
				Decision: model.DecisionInsufficientData, Current: float64(4 * model.BytesPerGi), Target: 0,
			},
		}},
	}
	est.Estimate(&rec)
	if rec.Cost.MonthlySavingsUSD != 0 {
		t.Errorf("INSUFFICIENT_DATA produced a saving of $%.2f", rec.Cost.MonthlySavingsUSD)
	}
	approx(t, rec.Cost.RecommendedMonthlyUSD, rec.Cost.CurrentMonthlyUSD, 1e-9,
		"unknown resources must be priced at their current request")
}

// A blocked recommendation holds the current value, so it must also yield no
// saving: this is the arithmetic that keeps the OOM gate honest in the cost
// report.
func TestBlockedRecommendationYieldsNoSaving(t *testing.T) {
	est, _ := NewEstimator(DefaultModel())
	rec := model.WorkloadRecommendation{
		Replicas: 3,
		Containers: []model.ContainerRecommendation{{
			CPU: model.ResourceRecommendation{Decision: model.DecisionDecrease, Current: 2000, Target: 500},
			Memory: model.ResourceRecommendation{
				Decision: model.DecisionBlocked,
				Current:  float64(4 * model.BytesPerGi), Target: float64(4 * model.BytesPerGi),
			},
		}},
	}
	est.Estimate(&rec)
	// The saving must come from CPU alone.
	m := DefaultModel()
	want := m.MonthlyUSD(1500, 0) * 3
	approx(t, rec.Cost.MonthlySavingsUSD, want, 1e-9, "blocked memory contributes no saving")
}

func TestSummarizeCountsDecisions(t *testing.T) {
	recs := []model.WorkloadRecommendation{
		{
			Replicas: 1,
			Cost:     model.CostEstimate{CurrentMonthlyUSD: 100, RecommendedMonthlyUSD: 60},
			Containers: []model.ContainerRecommendation{{
				CPU:    model.ResourceRecommendation{Decision: model.DecisionDecrease},
				Memory: model.ResourceRecommendation{Decision: model.DecisionBlocked},
			}},
		},
		{
			Replicas: 1,
			Cost:     model.CostEstimate{CurrentMonthlyUSD: 50, RecommendedMonthlyUSD: 50},
			Containers: []model.ContainerRecommendation{{
				CPU:    model.ResourceRecommendation{Decision: model.DecisionNoChange},
				Memory: model.ResourceRecommendation{Decision: model.DecisionInsufficientData},
			}},
		},
		{
			Replicas: 1,
			Cost:     model.CostEstimate{CurrentMonthlyUSD: 20, RecommendedMonthlyUSD: 30},
			Containers: []model.ContainerRecommendation{{
				CPU:    model.ResourceRecommendation{Decision: model.DecisionIncrease},
				Memory: model.ResourceRecommendation{Decision: model.DecisionIncrease},
			}},
		},
	}
	s := Summarize(recs, "test")
	if s.Workloads != 3 {
		t.Errorf("workloads = %d want 3", s.Workloads)
	}
	if s.Decreases != 1 || s.Increases != 2 || s.NoChanges != 1 || s.Blocked != 1 || s.InsufficientData != 1 {
		t.Errorf("decision counts wrong: %+v", s)
	}
	approx(t, s.CurrentMonthlyUSD, 170, 1e-9, "total current")
	approx(t, s.MonthlySavingsUSD, 30, 1e-9, "total savings")
	approx(t, s.SavingsFraction, 30.0/170.0, 1e-9, "savings fraction")
}

// An increase must show as a negative saving rather than being clipped: hiding
// the cost of a needed increase would make the engine look better than it is.
func TestIncreaseShowsAsNegativeSaving(t *testing.T) {
	est, _ := NewEstimator(DefaultModel())
	rec := model.WorkloadRecommendation{
		Replicas: 1,
		Containers: []model.ContainerRecommendation{{
			CPU:    model.ResourceRecommendation{Decision: model.DecisionIncrease, Current: 500, Target: 1500},
			Memory: model.ResourceRecommendation{Decision: model.DecisionNoChange, Current: float64(model.BytesPerGi), Target: float64(model.BytesPerGi)},
		}},
	}
	est.Estimate(&rec)
	if rec.Cost.MonthlySavingsUSD >= 0 {
		t.Errorf("an increase should produce a negative saving, got $%.2f", rec.Cost.MonthlySavingsUSD)
	}
}

func TestInstanceLookup(t *testing.T) {
	if _, err := InstanceByName("m5.xlarge"); err != nil {
		t.Errorf("m5.xlarge should be in the catalog: %v", err)
	}
	if _, err := InstanceByName("nonexistent.2xlarge"); err == nil {
		t.Error("expected an error for an unknown instance type")
	}
}

// Every catalog entry must carry a dated source, so that a reader can tell how
// stale the prices are.
func TestCatalogEntriesAreSourced(t *testing.T) {
	for _, it := range Catalog() {
		if it.Source == "" {
			t.Errorf("%s has no source attribution", it.Name)
		}
		if it.Provider == "" || it.Region == "" {
			t.Errorf("%s is missing provider/region metadata", it.Name)
		}
	}
	if DefaultModel().Source == "" {
		t.Error("the default model must document how its rates were derived")
	}
}

// The CPU cost share is an assumption, so the ordering it implies should be
// checked explicitly: a CPU-optimised instance must price CPU more cheaply per
// core than a memory-optimised one prices it, and vice versa for memory.
func TestDerivedRatesReflectInstanceShape(t *testing.T) {
	c5, _ := InstanceByName("c5.2xlarge") // 8 vCPU, 16 GiB
	r5, _ := InstanceByName("r5.2xlarge") // 8 vCPU, 64 GiB
	mc, _ := DeriveModel(c5, CPUCostShare)
	mr, _ := DeriveModel(r5, CPUCostShare)
	if mr.MemoryGiBHourUSD >= mc.MemoryGiBHourUSD {
		t.Errorf("memory-optimised r5 should price memory below c5: %.5f vs %.5f",
			mr.MemoryGiBHourUSD, mc.MemoryGiBHourUSD)
	}
}
