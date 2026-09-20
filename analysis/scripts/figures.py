#!/usr/bin/env python3
"""Generate every figure and table in the technical report.

Run: python analysis/scripts/figures.py

Each figure is produced by one function, from one experiment's records, with the
caption written alongside. Nothing is hand-edited: the report references these
files, so a change to the data changes the report's figures on the next run.
"""

from __future__ import annotations

import sys
import pathlib
import numpy as np
import pandas as pd

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import common  # noqa: E402

plt = common.setup_matplotlib()


# --- Figure 1 & 2: requested versus observed ---------------------------------

def fig_requested_vs_observed(main: pd.DataFrame):
    """Declared requests against true demand, per workload class.

    This is the RQ1 figure: it shows how much reserved capacity is never used, and
    that the answer depends heavily on which demand statistic is taken as the
    reference. Plotted on a log scale because the over-provisioning factors span
    two orders of magnitude, and a linear axis would compress every class but the
    idle one into a single band.
    """
    base = main.drop_duplicates(["workload_class", "seed"])
    for resource, cols, unit, scale, fname in [
        ("CPU", ["declared_cpu_milli", "true_cpu_mean_milli", "true_cpu_p99_milli", "true_cpu_max_milli"],
         "millicores", 1.0, "fig01_requested_vs_observed_cpu"),
        ("Memory", ["declared_memory_bytes", "true_memory_mean_bytes", "true_memory_p99_bytes", "true_memory_max_bytes"],
         "MiB", common.MI, "fig02_requested_vs_observed_memory"),
    ]:
        g = base.groupby("workload_class", observed=True)[cols].median() / scale
        classes = list(g.index)
        x = np.arange(len(classes))
        width = 0.2

        fig, ax = plt.subplots(figsize=(9, 4.2))
        labels = ["declared request", "true mean demand", "true p99 demand", "true max demand"]
        for i, (col, label) in enumerate(zip(cols, labels)):
            ax.bar(x + (i - 1.5) * width, g[col], width, label=label,
                   color=common.PALETTE[i], edgecolor="white", linewidth=0.4)
        ax.set_yscale("log")
        ax.set_xticks(x)
        ax.set_xticklabels(classes, rotation=30, ha="right")
        ax.set_ylabel(f"{resource} ({unit}, log scale)")
        ax.set_title(f"Declared {resource} request versus true demand, by workload class")
        ax.legend(ncol=2)

        # Annotate the over-provisioning factor against true max, which is the
        # number an operator would think of as "waste".
        ratio = g[cols[0]] / g[cols[3]]
        for xi, r in zip(x, ratio):
            ax.annotate(f"{r:.1f}x", (xi, g[cols[0]].iloc[xi]), textcoords="offset points",
                        xytext=(0, 4), ha="center", fontsize=7, color="#444444")

        common.save(fig, fname, f"""
Declared {resource.lower()} request against true demand for each workload class, median over
10 trace realisations, log scale. Annotations give the ratio of the declared request to
true maximum demand. The spread — from {ratio.min():.1f}x to {ratio.max():.1f}x — shows that
"how much waste exists" has no single answer even across a controlled set of workloads, and
that the reference statistic chosen changes the answer substantially: the spiky-cpu class is
{(g[cols[0]]/g[cols[2]]).loc['spiky-cpu']:.0f}x its p99 demand but only
{ratio.loc['spiky-cpu']:.1f}x its maximum.
""")


# --- Figure 3: savings by strategy -------------------------------------------

def fig_savings_by_strategy(main: pd.DataFrame):
    """Savings by strategy, with the raw and constrained objectives side by side.

    The two panels make the central point of the evaluation visible: ranking
    strategies by savings alone gives a different — and misleading — answer from
    ranking them by savings that actually met the reliability constraint.
    """
    sub = main[main.cpu_safety_factor == 1.15]
    long = common.agg(sub, ["cpu_strategy"], ["savings_fraction", "feasible_savings"])
    strategies = [s for s in common.STRATEGY_ORDER if s in set(long["cpu_strategy"])]

    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(9.5, 4), sharey=True)
    for ax, metric, title, color in [
        (ax1, "savings_fraction", "Reported savings", common.COLOR_NEUTRAL),
        (ax2, "feasible_savings", "Savings meeting the reliability constraint", common.COLOR_SAFE),
    ]:
        w = common.wide(long, "cpu_strategy", metric).reindex(strategies)
        x = np.arange(len(strategies))
        ax.bar(x, w["point"] * 100, color=color, edgecolor="white", linewidth=0.5)
        ax.errorbar(x, w["point"] * 100,
                    yerr=[(w["point"] - w["low"]) * 100, (w["high"] - w["point"]) * 100],
                    fmt="none", ecolor="#333333", capsize=3, linewidth=0.8)
        ax.set_xticks(x)
        ax.set_xticklabels(strategies)
        ax.set_title(title)
        ax.set_xlabel("CPU strategy")
    ax1.set_ylabel("savings (% of allocation-based cost)")
    fig.suptitle("Savings by CPU strategy at a 1.15x safety factor, all workload classes pooled", y=1.02)

    raw = common.wide(long, "cpu_strategy", "savings_fraction").reindex(strategies)["point"]
    feas = common.wide(long, "cpu_strategy", "feasible_savings").reindex(strategies)["point"]
    common.save(fig, "fig03_savings_by_strategy", f"""
Median savings by CPU strategy at a 1.15x safety factor, with 95% percentile-bootstrap
intervals over 10 trace realisations per condition. Left: savings as reported. Right:
savings counted only where the condition met the reliability constraint (no OOMKills and
at most 1% of demanded CPU work unserved).

The two panels rank the strategies differently. On reported savings, the mean and p50
policies lead ({raw.get('mean', float('nan'))*100:.0f}% and {raw.get('p50', float('nan'))*100:.0f}%);
under the constraint they fall to {feas.get('mean', float('nan'))*100:.0f}% and
{feas.get('p50', float('nan'))*100:.0f}% because much of what they saved was paid for with
degradation. Reporting savings without a reliability constraint therefore does not merely
overstate the benefit, it reverses the ordering.
""")


# --- Figure 4: recommendation error by workload class ------------------------

def fig_error_by_class(main: pd.DataFrame):
    """Signed relative error against ground truth, per class and strategy.

    Zero is the ideal; negative means the recommendation was below the demand it
    should have covered. The CPU panel is measured against true p99 demand and the
    memory panel against true maximum, because those are the operating targets the
    two resources actually have.
    """
    sub = main[(main.cpu_safety_factor == 1.15) & (main.memory_safety_factor == 1.15)]
    fig, axes = plt.subplots(2, 1, figsize=(9.5, 7), sharex=True)

    for ax, (strat_col, err_col, title, ref) in zip(axes, [
        ("cpu_strategy", "cpu_relative_error", "CPU", "true p99 demand"),
        ("memory_strategy", "memory_relative_error", "Memory", "true maximum demand"),
    ]):
        strategies = [s for s in common.STRATEGY_ORDER if s in set(sub[strat_col]) and s != "current"]
        classes = [c for c in common.CLASS_ORDER if c in set(sub["workload_class"])]
        colors = common.strategy_colors(strategies)
        x = np.arange(len(classes))
        width = 0.8 / max(1, len(strategies))
        for i, s in enumerate(strategies):
            sel = sub[sub[strat_col] == s]
            med = sel.groupby("workload_class", observed=True)[err_col].median().reindex(classes)
            ax.bar(x + (i - (len(strategies) - 1) / 2) * width, med * 100, width,
                   label=s, color=colors[s], edgecolor="white", linewidth=0.3)
        ax.axhline(0, color="#000000", linewidth=0.8)
        ax.set_ylabel(f"error vs {ref} (%)")
        ax.set_title(f"{title}: signed relative error by workload class")
        ax.set_yscale("symlog", linthresh=10)
        if ax is axes[0]:
            ax.legend(ncol=6, loc="upper left")
    axes[-1].set_xticks(x)
    axes[-1].set_xticklabels(classes, rotation=30, ha="right")

    common.save(fig, "fig04_error_by_workload_class", """
Signed relative error of the recommended request against ground-truth demand, median over
10 realisations, at a 1.15x safety factor. Zero is exactly sized; negative is
under-provisioned. CPU error is measured against true p99 demand and memory error against
true maximum demand, because those are the operating targets the two resources have: a CPU
request below p99 causes throttling, while a memory request below the maximum causes a kill.

The symmetric-log vertical axis is necessary because errors span from a few percent on the
stable classes to several thousand percent on the idle class, where percentile policies
produce targets far below a floor-constrained minimum.
""")


# --- Figure 5 & 6: the safety-margin trade-off -------------------------------

def fig_margin_tradeoff(sens: pd.DataFrame):
    """Savings and failure rate as functions of the safety margin.

    The two resources are plotted separately and with different risk metrics,
    because their failure modes differ: CPU degrades continuously (a throttle
    fraction) while memory fails discretely (a kill or not).
    """
    # CPU panel
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(10, 4))
    for strat, color in zip(["p95", "p99"], [common.PALETTE[0], common.PALETTE[1]]):
        sel = sens[sens.cpu_strategy == strat]
        g = sel.groupby("cpu_safety_factor", observed=True).agg(
            sav=("savings_fraction", "median"),
            thr=("cpu_throttle_fraction", "median"),
            thr90=("cpu_throttle_fraction", lambda s: s.quantile(0.9)))
        ax1.plot(g.index, g["sav"] * 100, "o-", color=color, label=f"{strat} savings", markersize=4)
        ax2.plot(g.index, g["thr90"] * 100, "s--", color=color, label=f"{strat} p90 throttle", markersize=4)
    ax1.set_xlabel("CPU safety factor")
    ax1.set_ylabel("savings (%)")
    ax1.set_title("CPU: savings versus margin")
    ax1.legend()
    ax2.set_xlabel("CPU safety factor")
    ax2.set_ylabel("unserved CPU work, 90th pct (%)")
    ax2.set_title("CPU: residual degradation versus margin")
    ax2.legend()
    common.save(fig, "fig05_margin_vs_savings_cpu", """
CPU savings and residual degradation against the safety factor, medians over 5 realisations
per condition. The risk panel plots the 90th percentile of the unserved-work fraction rather
than its median, because the median is zero for most conditions and a metric that is zero
almost everywhere cannot show the shape of the tail that matters.

Increasing the margin buys progressively less: beyond roughly 1.2x the residual degradation
of each policy is nearly flat while savings continue to fall linearly. The flat portion is
not noise — it is demand that sits structurally above the percentile, which no multiplier
applied to that percentile can reach.
""")

    # Memory panel
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(10, 4))
    for strat, color in zip(["p95", "max"], [common.PALETTE[0], common.PALETTE[2]]):
        sel = sens[sens.memory_strategy == strat]
        g = sel.groupby("memory_safety_factor", observed=True).agg(
            sav=("savings_fraction", "median"), surv=("survived_without_oom", "mean"))
        ax1.plot(g.index, g["sav"] * 100, "o-", color=color, label=f"{strat} savings", markersize=4)
        ax2.plot(g.index, g["surv"] * 100, "s--", color=color, label=f"{strat} survival", markersize=4)
    ax1.set_xlabel("memory safety factor")
    ax1.set_ylabel("savings (%)")
    ax1.set_title("Memory: savings versus margin")
    ax1.legend()
    ax2.set_xlabel("memory safety factor")
    ax2.set_ylabel("conditions with zero OOMKills (%)")
    ax2.set_ylim(0, 105)
    ax2.axhline(100, color="#999999", linewidth=0.6, linestyle=":")
    ax2.set_title("Memory: survival versus margin")
    ax2.legend()

    p95_plateau = sens[(sens.memory_strategy == "p95") & (sens.memory_safety_factor >= 1.3)]["survived_without_oom"].mean()
    common.save(fig, "fig06_margin_vs_failure_memory", f"""
Memory savings and survival against the safety factor, over 5 realisations per condition.
Survival is the fraction of conditions that completed the held-out horizon with zero
OOMKills.

The max policy reaches full survival at a 1.2x margin. The p95 policy does not: it plateaus
at {p95_plateau*100:.0f}% and stays there even at a 2.0x margin. The reason is structural
rather than statistical — on the bursty-memory class the p95 of the working set is a small
fraction of its peak, so no multiplier applied to p95 reaches the peak. A safety margin
cannot repair a statistic that systematically excludes the event it needs to cover.
""")


# --- Figure 7: observation window -------------------------------------------

def fig_observation_window(win: pd.DataFrame):
    """Savings, feasibility and recommendation stability against window length."""
    fig, axes = plt.subplots(1, 3, figsize=(11.5, 3.8))
    strategies = ["p95", "p99", "max"]
    colors = common.strategy_colors(strategies)

    for strat in strategies:
        sel = win[win.cpu_strategy == strat]
        g = sel.groupby("observation_window_hours", observed=True).agg(
            sav=("savings_fraction", "median"),
            feas=("feasible", "mean"),
            vol=("stability_median_abs_log2_ratio", "median"))
        axes[0].plot(g.index, g["sav"] * 100, "o-", color=colors[strat], label=strat, markersize=4)
        axes[1].plot(g.index, g["feas"] * 100, "o-", color=colors[strat], label=strat, markersize=4)
        axes[2].plot(g.index, g["vol"], "o-", color=colors[strat], label=strat, markersize=4)

    for ax, ylabel, title in [
        (axes[0], "savings (%)", "Savings"),
        (axes[1], "conditions meeting the constraint (%)", "Reliability"),
        (axes[2], "median |log2(r_t / r_{t-1})|", "Run-to-run volatility"),
    ]:
        ax.set_xscale("log")
        ax.set_xticks([1, 6, 24, 72])
        ax.set_xticklabels(["1h", "6h", "24h", "72h"])
        ax.set_xlabel("observation window")
        ax.set_ylabel(ylabel)
        ax.set_title(title)
        ax.legend()

    g95 = win[win.cpu_strategy == "max"].groupby("observation_window_hours", observed=True)
    sav_short = g95["savings_fraction"].median().loc[1] * 100
    sav_long = g95["savings_fraction"].median().loc[72] * 100
    vol_short = win[win.cpu_strategy == "p95"].groupby("observation_window_hours", observed=True)["stability_median_abs_log2_ratio"].median().loc[1]

    common.save(fig, "fig07_observation_window", f"""
Effect of observation-window length, with every window scored on the same held-out 24-hour
horizon so the comparison isolates history length rather than comparing different futures.
Volatility is the median absolute base-2 log ratio between consecutive recommendations over
eight scheduled recomputations; a value of 1.0 would mean the typical recomputation doubled
or halved the request.

Window length acts on the three axes in different directions. Savings under the max policy
*fall* with a longer window ({sav_short:.0f}% at 1h to {sav_long:.0f}% at 72h), because a
longer history contains rarer and taller peaks and so a maximum-based target grows.
Reliability improves monotonically. Volatility falls by more than an order of magnitude
(p95: {vol_short:.3f} at 1h to effectively zero at 72h). A short window therefore does not
simply trade accuracy for freshness: it produces a recommender that is simultaneously more
aggressive, less reliable, and operationally unstable.
""")


# --- Figure 8: CPU versus memory strategy comparison ------------------------

def fig_cpu_vs_memory(main: pd.DataFrame):
    """Heatmaps of the per-class risk of each strategy, side by side.

    This is the figure that carries the project's central claim. If the best CPU
    policy and the best memory policy were the same, the two panels would show the
    same pattern; the extent to which they differ is the extent to which treating
    the resources identically is wrong.
    """
    sub = main[(main.cpu_safety_factor == 1.25) & (main.memory_safety_factor == 1.25)]
    classes = [c for c in common.CLASS_ORDER if c in set(sub["workload_class"])]
    strategies = [s for s in common.STRATEGY_ORDER if s in set(sub["cpu_strategy"]) and s != "current"]

    cpu = sub.pivot_table(index="workload_class", columns="cpu_strategy",
                          values="cpu_throttle_fraction", aggfunc="median",
                          observed=True).reindex(index=classes, columns=strategies)
    mem = sub.pivot_table(index="workload_class", columns="memory_strategy",
                          values="survived_without_oom", aggfunc="mean",
                          observed=True).reindex(index=classes, columns=strategies)
    # Express memory as a failure rate so both panels read "darker is worse".
    memfail = 1.0 - mem

    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(10.5, 4.6))
    for ax, data, title, cbar_label in [
        (ax1, cpu, "CPU: unserved work fraction", "fraction of demanded CPU work unserved"),
        (ax2, memfail, "Memory: OOM failure rate", "fraction of conditions with an OOMKill"),
    ]:
        im = ax.imshow(data.to_numpy(), aspect="auto", cmap="YlOrRd",
                       vmin=0, vmax=max(0.05, np.nanmax(data.to_numpy())))
        ax.set_xticks(range(len(strategies)))
        ax.set_xticklabels(strategies)
        ax.set_yticks(range(len(classes)))
        ax.set_yticklabels(classes)
        ax.set_title(title)
        ax.grid(False)
        for i in range(len(classes)):
            for j in range(len(strategies)):
                v = data.to_numpy()[i, j]
                if np.isfinite(v):
                    ax.text(j, i, f"{v:.2f}" if v >= 0.005 else "0",
                            ha="center", va="center", fontsize=7,
                            color="white" if v > 0.5 * np.nanmax(data.to_numpy()) else "#222222")
        fig.colorbar(im, ax=ax, label=cbar_label, fraction=0.046)
    ax2.set_yticklabels([])
    fig.suptitle("Risk by strategy and workload class at a 1.25x safety factor", y=1.0)

    common.save(fig, "fig08_cpu_vs_memory_strategy", """
Per-class risk of each strategy at a 1.25x safety factor, with the two resources side by
side and both panels oriented so that darker is worse. CPU risk is the fraction of demanded
work left unserved; memory risk is the fraction of conditions that suffered at least one
OOMKill.

The two panels do not share a pattern, which is the evidence behind the engine's
resource-specific design. On CPU, p90 and p95 are safe on most classes and fail only where
peaks are short and rare. On memory, the same percentiles fail completely on bursty-memory
while p99 and max succeed everywhere. A single policy applied to both resources would
either accept memory kills in order to capture CPU savings, or forgo CPU savings in order to
be safe on memory.
""")


# --- Figure 9: the Pareto frontier -----------------------------------------

def fig_pareto(main: pd.DataFrame):
    """Savings against the rate of constraint violation, with the frontier marked.

    The risk axis is the fraction of conditions that failed the *full* reliability
    constraint, covering CPU degradation and memory kills together. An earlier
    version of this figure plotted memory failures alone, and consequently marked a
    configuration using CPU p50 as the "safest that still saves" — while that
    configuration leaves a third of demanded CPU work unserved on the bursty class.
    Plotting one resource's risk while summarising both resources' savings is a way
    to make an unsafe configuration look optimal.
    """
    g = main.groupby(["cpu_strategy", "memory_strategy", "cpu_safety_factor", "memory_safety_factor"],
                     observed=True).agg(
        sav=("savings_fraction", "median"),
        feas=("feasible", "mean"),
        oom=("survived_without_oom", lambda s: 1 - s.mean()),
        thr=("cpu_throttle_fraction", "median")).reset_index()
    g["infeasible"] = 1.0 - g["feas"]

    fig, ax = plt.subplots(figsize=(8, 5))
    sc = ax.scatter(g["infeasible"] * 100, g["sav"] * 100, c=g["oom"] * 100,
                    cmap="YlOrRd", s=20, alpha=0.85, edgecolors="none")
    fig.colorbar(sc, ax=ax, label="of which memory failures (% of conditions)")

    # Pareto frontier: for each risk level keep the best savings, then drop any
    # point that a lower-risk point already matches or beats.
    by_risk = g.groupby("infeasible", observed=True)["sav"].max().sort_index()
    frontier_x, frontier_y, best = [], [], -np.inf
    for risk, sav in by_risk.items():
        if sav > best:
            frontier_x.append(risk * 100)
            frontier_y.append(sav * 100)
            best = sav
    ax.step(frontier_x, frontier_y, where="post", color=common.COLOR_RISK,
            linewidth=1.5, label="Pareto frontier", zorder=3)
    ax.scatter(frontier_x, frontier_y, facecolors="none", edgecolors=common.COLOR_RISK,
               s=55, linewidths=1.2, zorder=4)

    # Annotate the configuration that was feasible on every single condition and
    # saved the most. This is the one an operator could deploy cluster-wide.
    allsafe = g[g["feas"] == 1.0]
    note = "no configuration was feasible on every condition"
    if len(allsafe):
        b = allsafe.loc[allsafe["sav"].idxmax()]
        note = (f"universally feasible best:\n"
                f"cpu {b['cpu_strategy']} x{b['cpu_safety_factor']:g}, "
                f"mem {b['memory_strategy']} x{b['memory_safety_factor']:g}\n"
                f"{b['sav']*100:.0f}% savings")
        ax.annotate(note, (0, b["sav"] * 100), textcoords="offset points",
                    xytext=(34, -78), fontsize=7.5, ha="left",
                    arrowprops=dict(arrowstyle="->", color="#444444", lw=0.8),
                    bbox=dict(boxstyle="round,pad=0.35", fc="white", ec="#999999", lw=0.5))

    ax.set_xlabel("conditions violating the reliability constraint (%)")
    ax.set_ylabel("median savings (%)")
    ax.set_title("Savings against reliability risk across all configurations")
    ax.legend(loc="lower right")

    best_safe = allsafe["sav"].max() * 100 if len(allsafe) else float("nan")
    best_any = g["sav"].max() * 100
    common.save(fig, "fig09_pareto_savings_vs_risk", f"""
Every (CPU strategy, memory strategy, CPU margin, memory margin) configuration, positioned
by its median savings across all workload classes and 10 realisations, against the fraction
of conditions that violated the reliability constraint. Colour separates out how much of
that risk was memory failure rather than CPU degradation. The step line connects the
configurations that no lower-risk configuration matches.

The frontier is steep at the safe end. The best configuration that was feasible on *every*
class and seed reaches {best_safe:.0f}% savings, against {best_any:.0f}% for the most
aggressive configuration overall — so the price of universal safety in this workload
population is roughly {best_any - best_safe:.0f} percentage points. Most of the space is
dominated, which is the practically useful content of the figure: the real choice is among
the handful of frontier points, not among all {len(g)} configurations.
""")


# --- Figure 10: the ablation -----------------------------------------------

def fig_ablation(main: pd.DataFrame, uni: pd.DataFrame, nogates: pd.DataFrame,
                 oom_on: pd.DataFrame, oom_off: pd.DataFrame):
    """Ablation results: what each component of the engine actually contributes."""
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(10.5, 4.2))

    # Panel 1: unified versus resource-specific, as an achievable frontier.
    for label, df, keys, color in [
        ("resource-specific", main,
         ["cpu_strategy", "memory_strategy", "cpu_safety_factor", "memory_safety_factor"], common.COLOR_SAFE),
        ("unified (cpu = memory)", uni, ["cpu_strategy", "cpu_safety_factor"], common.COLOR_RISK),
    ]:
        g = df.groupby(keys, observed=True).agg(
            sav=("savings_fraction", "median"), feas=("feasible", "mean")).reset_index()
        ax1.scatter(g["feas"] * 100, g["sav"] * 100, s=22, alpha=0.75, label=label, color=color,
                    edgecolors="none")
    ax1.set_xlabel("conditions meeting the constraint (%)")
    ax1.set_ylabel("savings (%)")
    ax1.set_title("Ablation C: one shared strategy versus two")
    ax1.legend(loc="upper right")

    # Panel 2: the OOM gate on the population where it can act.
    labels = ["gate enabled", "gate disabled"]
    oom_means = [oom_on["oom_kills"].mean(), oom_off["oom_kills"].mean()]
    sav_means = [oom_on["savings_fraction"].median() * 100, oom_off["savings_fraction"].median() * 100]
    x = np.arange(2)
    ax2b = ax2.twinx()
    ax2.bar(x - 0.18, oom_means, 0.34, color=common.COLOR_RISK, label="OOMKills per condition")
    ax2b.bar(x + 0.18, sav_means, 0.34, color=common.COLOR_NEUTRAL, label="savings (%)")
    ax2.set_xticks(x)
    ax2.set_xticklabels(labels)
    ax2.set_ylabel("OOMKill episodes per condition")
    ax2b.set_ylabel("savings (%)")
    ax2b.grid(False)
    ax2.set_title("Ablation A: OOM protection on already-failing workloads")
    h1, l1 = ax2.get_legend_handles_labels()
    h2, l2 = ax2b.get_legend_handles_labels()
    ax2.legend(h1 + h2, l1 + l2, loc="upper center")

    blocked = (oom_on["memory_decision"] == "BLOCKED").mean() * 100
    common.save(fig, "fig10_ablation", f"""
Ablation results.

Left: the achievable savings/reliability space when CPU and memory may use different
strategies against when they are forced to share one. The resource-specific space extends
further along both axes, and in particular contains fully-feasible configurations that the
unified space does not reach.

Right: OOM protection evaluated on workloads deployed below their true peak demand, so that
they are already being killed and their observed working-set series is censored at the limit.
On this population the gate fires on {blocked:.0f}% of conditions and reduces kill episodes
from {oom_means[1]:.0f} to {oom_means[0]:.0f} per condition, at no cost in savings. On the
over-provisioned population of the main experiment the gate never fires at all, because there
is no OOM history for it to act on — a result reported in full in research/results.md,
because the first version of this ablation was incapable of observing the component it was
meant to test.
""")


def fig_decision_mix(main: pd.DataFrame):
    """What the engine actually decides, per class.

    Included because a savings figure alone hides the shape of the advice: an
    engine that recommends nothing for half the cluster is a different tool from
    one that acts everywhere, even at the same total saving.
    """
    sub = main[(main.cpu_safety_factor == 1.15) & (main.cpu_strategy == "p95") & (main.memory_strategy == "max")]
    decisions = ["DECREASE", "NO_CHANGE", "INCREASE", "BLOCKED", "INSUFFICIENT_DATA"]
    colors = {"DECREASE": common.COLOR_SAFE, "NO_CHANGE": "#999999", "INCREASE": common.PALETTE[4],
              "BLOCKED": common.COLOR_RISK, "INSUFFICIENT_DATA": "#666666"}

    fig, axes = plt.subplots(1, 2, figsize=(10.5, 4))
    for ax, col, title in [(axes[0], "cpu_decision", "CPU"), (axes[1], "memory_decision", "Memory")]:
        tab = (sub.groupby(["workload_class", col], observed=True).size()
               .unstack(fill_value=0).reindex(columns=decisions, fill_value=0))
        tab = tab.div(tab.sum(axis=1), axis=0) * 100
        bottom = np.zeros(len(tab))
        for d in decisions:
            ax.bar(range(len(tab)), tab[d], bottom=bottom, label=d, color=colors[d],
                   edgecolor="white", linewidth=0.3)
            bottom += tab[d].to_numpy()
        ax.set_xticks(range(len(tab)))
        ax.set_xticklabels(tab.index, rotation=35, ha="right")
        ax.set_ylabel("share of conditions (%)")
        ax.set_title(f"{title} decisions")
    axes[0].legend(ncol=2, fontsize=7)
    fig.suptitle("Engine decisions by workload class (CPU p95 x1.15, memory max x1.15)", y=1.02)

    common.save(fig, "fig11_decision_mix", """
Distribution of engine decisions per workload class under the shipped policy. A savings
total hides this structure: the engine declines to act on some classes entirely, and on the
under-provisioned cases it recommends an increase, which costs money and is the correct
advice. An evaluation that reported only the cost reduction would score the increases as a
failure.
""")


def table_summary(main: pd.DataFrame, win: pd.DataFrame):
    """Write the summary tables the report references."""
    sub = main[main.cpu_safety_factor == 1.15]
    t1 = common.agg(sub, ["cpu_strategy"],
                    ["savings_fraction", "feasible_savings", "cpu_utilization",
                     "cpu_throttle_fraction", "cpu_violation_rate"])
    common.write_table(t1.set_index(["cpu_strategy", "metric"]), "table01_cpu_strategies")

    t2 = main.pivot_table(index="workload_class", columns="memory_strategy",
                          values="survived_without_oom", aggfunc="mean", observed=True)
    common.write_table(t2, "table02_memory_survival_by_class")

    t3 = main.pivot_table(index="workload_class", columns="cpu_strategy",
                          values="cpu_throttle_fraction", aggfunc="median", observed=True)
    common.write_table(t3, "table03_cpu_throttle_by_class")

    t4 = win.groupby(["observation_window_hours", "cpu_strategy"], observed=True).agg(
        savings=("savings_fraction", "median"),
        feasible=("feasible", "mean"),
        volatility=("stability_median_abs_log2_ratio", "median"),
        range_ratio=("stability_range_ratio", "median")).reset_index()
    common.write_table(t4.set_index(["observation_window_hours", "cpu_strategy"]), "table04_observation_window")

    # The operator-facing table: per class, the cheapest configuration that was
    # feasible on every seed.
    g = main.groupby(["workload_class", "cpu_strategy", "memory_strategy",
                      "cpu_safety_factor", "memory_safety_factor"], observed=True).agg(
        feas=("feasible", "mean"), sav=("savings_fraction", "median")).reset_index()
    safe = g[g["feas"] == 1.0]
    best = safe.loc[safe.groupby("workload_class", observed=True)["sav"].idxmax()]
    common.write_table(best.set_index("workload_class"), "table05_best_feasible_per_class")
    return best


def main_entry():
    print("loading experiment records...")
    main = common.load("main")
    win = common.load("windows")
    sens = common.load("sensitivity")
    uni = common.load("ablation_unified_strategy")
    nogates = common.load("ablation_no_gates")
    oom_on = common.load("oom_recovery")
    oom_off = common.load("oom_recovery_no_gate")

    print(f"  main={len(main)} windows={len(win)} sensitivity={len(sens)} "
          f"unified={len(uni)} no_gates={len(nogates)} oom={len(oom_on)}+{len(oom_off)}")

    print("generating figures...")
    fig_requested_vs_observed(main)
    fig_savings_by_strategy(main)
    fig_error_by_class(main)
    fig_margin_tradeoff(sens)
    fig_observation_window(win)
    fig_cpu_vs_memory(main)
    fig_pareto(main)
    fig_ablation(main, uni, nogates, oom_on, oom_off)
    fig_decision_mix(main)

    print("writing tables...")
    best = table_summary(main, win)

    figs = sorted(common.FIGURES_DIR.glob("*.png"))
    tables = sorted(common.TABLES_DIR.glob("*.csv"))
    print(f"\n{len(figs)} figures -> {common.FIGURES_DIR}")
    for f in figs:
        print(f"  {f.name}")
    print(f"\n{len(tables)} tables -> {common.TABLES_DIR}")
    for t in tables:
        print(f"  {t.name}")
    print("\nBest fully-feasible configuration per workload class:")
    print(best[["workload_class", "cpu_strategy", "memory_strategy",
                "cpu_safety_factor", "memory_safety_factor", "sav"]].to_string(index=False))


if __name__ == "__main__":
    main_entry()
