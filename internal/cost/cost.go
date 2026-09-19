// Package cost estimates the infrastructure cost of reserved Kubernetes
// capacity.
//
// # What this package computes, precisely
//
// It computes an *allocation-based cost estimate*: the price of the CPU and
// memory a workload reserves, at a configured per-unit rate, for a configured
// number of hours. It is not a cloud bill and does not attempt to be one.
//
// The gap between the two is large and worth stating plainly, because a cost
// tool that overstates its own accuracy is worse than one that reports a wider
// but honest number:
//
//   - Nodes are billed whole. Reducing one pod's request frees capacity on a node
//     that is still running and still charged. A reduction becomes a real saving
//     only when it lets the cluster autoscaler remove a node, or lets a pending
//     pod schedule without adding one. Allocation-based estimates therefore
//     describe an upper bound on achievable savings.
//   - Bin packing is lossy. A node with 4 spare cores and 1 spare GiB cannot host
//     a pod needing 1 core and 4 GiB. Real clusters strand capacity in ways a
//     per-workload model cannot see.
//   - Commitments, Savings Plans, Spot and sustained-use discounts change the
//     effective rate by large factors, and depend on cluster-wide commitments
//     rather than per-workload usage.
//   - Managed control planes, storage, networking and egress are not modelled.
//
// Given that, the package derives per-unit rates from node instance pricing by
// splitting an instance's hourly price between its CPU and memory in a stated
// proportion, so that the rate at least reflects the shape of a real machine
// rather than an invented number. The split itself is an assumption, documented
// in docs/cost-model.md and varied in the sensitivity analysis.
package cost

import (
	"fmt"
	"sort"

	"github.com/anishc23/k8s-cost-optimizer/internal/model"
)

// HoursPerMonth is the convention used for monthly projections: 730 hours, the
// mean month length (8760/12). Cloud providers quote monthly prices on this
// basis, so using it makes the estimate comparable to a published price list.
const HoursPerMonth = 730.0

// Model is a pricing model expressed as per-unit hourly rates.
type Model struct {
	// Name identifies the model in output, so a reader always knows which
	// pricing assumption produced a number.
	Name string `json:"name"`
	// CPUHourUSD is the price of one reserved CPU core for one hour.
	CPUHourUSD float64 `json:"cpu_hour_usd"`
	// MemoryGiBHourUSD is the price of one reserved GiB for one hour.
	MemoryGiBHourUSD float64 `json:"memory_gib_hour_usd"`
	// Provider and Region are metadata recorded with results.
	Provider string `json:"provider"`
	Region   string `json:"region"`
	// Source documents where the rates came from, so that a stale price list is
	// visible rather than silently authoritative.
	Source string `json:"source"`
}

// Validate rejects models that would produce meaningless estimates.
func (m Model) Validate() error {
	switch {
	case m.CPUHourUSD < 0:
		return fmt.Errorf("cost model %q: cpu_hour_usd is negative", m.Name)
	case m.MemoryGiBHourUSD < 0:
		return fmt.Errorf("cost model %q: memory_gib_hour_usd is negative", m.Name)
	case m.CPUHourUSD == 0 && m.MemoryGiBHourUSD == 0:
		return fmt.Errorf("cost model %q: both rates are zero, every estimate would be zero", m.Name)
	}
	return nil
}

// MonthlyUSD prices a reserved allocation for one replica for one month.
func (m Model) MonthlyUSD(cpu model.Millicores, mem model.Bytes) float64 {
	return m.HourlyUSD(cpu, mem) * HoursPerMonth
}

// HourlyUSD prices a reserved allocation for one replica for one hour.
func (m Model) HourlyUSD(cpu model.Millicores, mem model.Bytes) float64 {
	return cpu.Cores()*m.CPUHourUSD + mem.Gibibytes()*m.MemoryGiBHourUSD
}

// InstanceType is a node shape with a published hourly price. Rates are derived
// from these so that the per-unit prices reflect real machine proportions.
type InstanceType struct {
	Name      string  `json:"name"`
	VCPU      float64 `json:"vcpu"`
	MemoryGiB float64 `json:"memory_gib"`
	HourlyUSD float64 `json:"hourly_usd"`
	Provider  string  `json:"provider"`
	Region    string  `json:"region"`
	Source    string  `json:"source"`
}

// CPUCostShare is the fraction of an instance's price attributed to CPU, with
// the remainder attributed to memory.
//
// There is no ground truth for this split: a cloud provider sells a machine, not
// separable CPU and memory. 0.70 is used because across the general-purpose
// instance families the marginal price of a vCPU is consistently several times
// that of a GiB, and a 70/30 split reproduces published per-unit rates of
// CPU-optimised versus memory-optimised families to within roughly a factor of
// two. That is a weak justification and is treated as one: the sensitivity
// analysis varies this parameter, and docs/cost-model.md reports how much the
// savings ranking depends on it.
const CPUCostShare = 0.70

// DeriveModel splits an instance price into per-unit rates.
func DeriveModel(it InstanceType, cpuShare float64) (Model, error) {
	if it.VCPU <= 0 || it.MemoryGiB <= 0 {
		return Model{}, fmt.Errorf("instance %q: vcpu and memory must be positive", it.Name)
	}
	if it.HourlyUSD <= 0 {
		return Model{}, fmt.Errorf("instance %q: hourly price must be positive", it.Name)
	}
	if cpuShare <= 0 || cpuShare >= 1 {
		return Model{}, fmt.Errorf("cpu share %.2f must be in (0,1)", cpuShare)
	}
	return Model{
		Name:             fmt.Sprintf("%s/%s/%s", it.Provider, it.Region, it.Name),
		CPUHourUSD:       it.HourlyUSD * cpuShare / it.VCPU,
		MemoryGiBHourUSD: it.HourlyUSD * (1 - cpuShare) / it.MemoryGiB,
		Provider:         it.Provider,
		Region:           it.Region,
		Source: fmt.Sprintf("derived from %s on-demand price $%.4f/h (%.0f vCPU, %.0f GiB) with a %.0f/%.0f CPU/memory split; %s",
			it.Name, it.HourlyUSD, it.VCPU, it.MemoryGiB, cpuShare*100, (1-cpuShare)*100, it.Source),
	}, nil
}

// Catalog is the built-in instance price list.
//
// Prices are on-demand Linux list prices, recorded with the date they were
// captured. They are shipped as a convenience and a reproducibility anchor, not
// as a live price feed: a deployment that cares about accuracy should configure
// its own rates. Because they are dated, a reader can tell how stale they are —
// which an undated hard-coded number never permits.
func Catalog() []InstanceType {
	const src = "provider public on-demand price list, captured 2025-09-01"
	return []InstanceType{
		{Name: "m5.large", VCPU: 2, MemoryGiB: 8, HourlyUSD: 0.096, Provider: "aws", Region: "us-east-1", Source: src},
		{Name: "m5.xlarge", VCPU: 4, MemoryGiB: 16, HourlyUSD: 0.192, Provider: "aws", Region: "us-east-1", Source: src},
		{Name: "m5.2xlarge", VCPU: 8, MemoryGiB: 32, HourlyUSD: 0.384, Provider: "aws", Region: "us-east-1", Source: src},
		{Name: "c5.2xlarge", VCPU: 8, MemoryGiB: 16, HourlyUSD: 0.340, Provider: "aws", Region: "us-east-1", Source: src},
		{Name: "r5.2xlarge", VCPU: 8, MemoryGiB: 64, HourlyUSD: 0.504, Provider: "aws", Region: "us-east-1", Source: src},
		{Name: "e2-standard-4", VCPU: 4, MemoryGiB: 16, HourlyUSD: 0.134, Provider: "gcp", Region: "us-central1", Source: src},
		{Name: "n2-standard-8", VCPU: 8, MemoryGiB: 32, HourlyUSD: 0.388, Provider: "gcp", Region: "us-central1", Source: src},
		{Name: "Standard_D4s_v5", VCPU: 4, MemoryGiB: 16, HourlyUSD: 0.192, Provider: "azure", Region: "eastus", Source: src},
	}
}

// InstanceByName looks up a catalog entry.
func InstanceByName(name string) (InstanceType, error) {
	for _, it := range Catalog() {
		if it.Name == name {
			return it, nil
		}
	}
	names := make([]string, 0, len(Catalog()))
	for _, it := range Catalog() {
		names = append(names, it.Name)
	}
	sort.Strings(names)
	return InstanceType{}, fmt.Errorf("unknown instance type %q (known: %v)", name, names)
}

// DefaultModel is the shipped pricing model: an m5.xlarge in us-east-1.
//
// A general-purpose 4 vCPU / 16 GiB instance is chosen because it is the most
// common node shape in small and mid-sized clusters, so the derived rates are
// representative of the environment the tool is most likely to run in.
func DefaultModel() Model {
	it, err := InstanceByName("m5.xlarge")
	if err != nil {
		panic(err) // the catalog is a compile-time constant
	}
	m, err := DeriveModel(it, CPUCostShare)
	if err != nil {
		panic(err)
	}
	return m
}

// Estimator prices recommendations.
type Estimator struct {
	model Model
}

// NewEstimator validates the model up front so that callers holding an Estimator
// can rely on it producing meaningful numbers.
func NewEstimator(m Model) (*Estimator, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &Estimator{model: m}, nil
}

// Model returns the pricing model in use.
func (e *Estimator) Model() Model { return e.model }

// Estimate prices a workload recommendation across all of its containers and
// replicas.
//
// Replica count is included because the request is per container but the cost is
// incurred per pod: a 100m saving on a 50-replica deployment is a 5-core saving.
// Reporting per-container savings would systematically understate impact and
// would mis-rank workloads against each other.
//
// Containers whose decision carries no target (INSUFFICIENT_DATA) are priced at
// their current request, so that "we do not know" never appears as a saving.
func (e *Estimator) Estimate(rec *model.WorkloadRecommendation) {
	replicas := float64(rec.Replicas)
	if replicas < 1 {
		// A scaled-to-zero workload still has a declared request that will be
		// charged the moment it scales up; pricing it at zero would make the
		// engine's advice invisible for exactly the workloads most likely to be
		// forgotten. One replica is the honest floor.
		replicas = 1
	}
	var current, recommended float64
	for _, c := range rec.Containers {
		curCPU := model.Millicores(c.CPU.Current)
		curMem := model.Bytes(c.Memory.Current)
		current += e.model.MonthlyUSD(curCPU, curMem) * replicas

		recCPU, recMem := curCPU, curMem
		if c.CPU.Decision != model.DecisionInsufficientData {
			recCPU = model.Millicores(c.CPU.Target)
		}
		if c.Memory.Decision != model.DecisionInsufficientData {
			recMem = model.Bytes(c.Memory.Target)
		}
		recommended += e.model.MonthlyUSD(recCPU, recMem) * replicas
	}
	rec.Cost = model.CostEstimate{
		CurrentMonthlyUSD:     current,
		RecommendedMonthlyUSD: recommended,
		MonthlySavingsUSD:     current - recommended,
		Model:                 e.model.Name,
	}
	if current > 0 {
		rec.Cost.SavingsFraction = (current - recommended) / current
	}
}

// Summary aggregates cost across many workloads for the API and dashboards.
type Summary struct {
	Workloads             int     `json:"workloads"`
	CurrentMonthlyUSD     float64 `json:"current_monthly_usd"`
	RecommendedMonthlyUSD float64 `json:"recommended_monthly_usd"`
	MonthlySavingsUSD     float64 `json:"monthly_savings_usd"`
	SavingsFraction       float64 `json:"savings_fraction"`

	// Counts of each decision, so that a savings figure is always accompanied by
	// how many workloads the engine declined to act on. A large saving from three
	// workloads out of four hundred is a different claim from the same saving
	// across all of them.
	Decreases        int `json:"decreases"`
	Increases        int `json:"increases"`
	NoChanges        int `json:"no_changes"`
	Blocked          int `json:"blocked"`
	InsufficientData int `json:"insufficient_data"`

	Model string `json:"model"`
}

// Summarize aggregates a set of recommendations.
func Summarize(recs []model.WorkloadRecommendation, modelName string) Summary {
	s := Summary{Workloads: len(recs), Model: modelName}
	for _, r := range recs {
		s.CurrentMonthlyUSD += r.Cost.CurrentMonthlyUSD
		s.RecommendedMonthlyUSD += r.Cost.RecommendedMonthlyUSD
		for _, c := range r.Containers {
			for _, d := range []model.Decision{c.CPU.Decision, c.Memory.Decision} {
				switch d {
				case model.DecisionDecrease:
					s.Decreases++
				case model.DecisionIncrease:
					s.Increases++
				case model.DecisionNoChange:
					s.NoChanges++
				case model.DecisionBlocked:
					s.Blocked++
				case model.DecisionInsufficientData:
					s.InsufficientData++
				}
			}
		}
	}
	s.MonthlySavingsUSD = s.CurrentMonthlyUSD - s.RecommendedMonthlyUSD
	if s.CurrentMonthlyUSD > 0 {
		s.SavingsFraction = s.MonthlySavingsUSD / s.CurrentMonthlyUSD
	}
	return s
}
