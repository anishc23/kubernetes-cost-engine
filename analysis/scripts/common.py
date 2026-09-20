"""Shared loading, aggregation and plotting helpers for the analysis pipeline.

Design notes
------------
Two choices here shape every figure and table downstream.

First, aggregation across seeds uses the **median** with a **bootstrap**
confidence interval rather than the mean with a normal-theory interval. The
per-condition distributions are not symmetric: savings fractions are bounded
above by 1, throttle fractions are bounded below by 0 and have a long right tail,
and OOM counts are zero-inflated. A mean with a t-interval assumes away exactly
the structure that matters, and would report intervals extending below zero for
quantities that cannot be negative. The bootstrap makes no distributional
assumption; it costs a few seconds of compute and buys interpretability.

Second, no significance tests are reported. With ten seeds per condition and
thousands of conditions, any p-value would be both underpowered per comparison
and hopelessly multiple-compared across them; and because the seeds are draws
from a generative model the analyst chose, a "significant" difference would be a
statement about the simulator's parameters rather than about workloads. Effect
sizes with intervals answer the question that can actually be answered: how large
is the difference, and how much does it move across realisations.
"""

from __future__ import annotations

import json
import pathlib
import numpy as np
import pandas as pd

RESULTS_DIR = pathlib.Path(__file__).resolve().parents[2] / "experiments" / "results"
FIGURES_DIR = pathlib.Path(__file__).resolve().parents[1] / "figures"
TABLES_DIR = pathlib.Path(__file__).resolve().parents[1] / "tables"

# Bytes per binary unit, for readable axis labels.
MI = 1024.0 ** 2
GI = 1024.0 ** 3

# Workload classes in a fixed order, so every figure orders them identically.
CLASS_ORDER = [
    "stable-cpu", "bursty-cpu", "periodic-cpu", "spiky-cpu",
    "stable-memory", "growing-memory", "bursty-memory", "sawtooth-memory",
    "mixed", "idle",
]

# Strategies from least to most conservative. The ordering is the axis along
# which the central comparison is read, so it is fixed here rather than left to
# alphabetical accident.
STRATEGY_ORDER = ["current", "mean", "p50", "p90", "p95", "p99", "max"]


def load(name: str) -> pd.DataFrame:
    """Load one experiment's records, with categorical ordering applied.

    Results are stored gzipped, which pandas handles by extension. A plain .csv is
    accepted too, so a freshly generated file works either way.
    """
    gz = RESULTS_DIR / f"{name}.csv.gz"
    plain = RESULTS_DIR / f"{name}.csv"
    path = gz if gz.exists() else plain
    if not path.exists():
        raise FileNotFoundError(
            f"Neither {gz} nor {plain} exists. Run `make experiments` (or "
            f"`go run ./cmd/experiment -config experiments/configs/{name}.yaml`) first."
        )
    df = pd.read_csv(path)
    df["workload_class"] = pd.Categorical(
        df["workload_class"],
        categories=[c for c in CLASS_ORDER if c in set(df["workload_class"])],
        ordered=True,
    )
    for col in ("cpu_strategy", "memory_strategy"):
        present = [s for s in STRATEGY_ORDER if s in set(df[col])]
        df[col] = pd.Categorical(df[col], categories=present, ordered=True)
    return df


def provenance(name: str) -> dict:
    """Load the provenance block for an experiment, for stamping figures."""
    path = RESULTS_DIR / f"{name}.provenance.json"
    if not path.exists():
        return {}
    with open(path) as f:
        return json.load(f)


def bootstrap_ci(values, statistic=np.median, n_resamples: int = 2000,
                 level: float = 0.95, seed: int = 12345):
    """Return (point estimate, low, high) for a statistic, by percentile bootstrap.

    Returns a zero-width interval for fewer than two observations rather than
    raising: some conditions legitimately have a single usable record, and a
    pipeline that crashed on them would force the analyst to special-case every
    call site.
    """
    v = np.asarray([x for x in values if x is not None and np.isfinite(x)], dtype=float)
    if v.size == 0:
        return np.nan, np.nan, np.nan
    point = float(statistic(v))
    if v.size < 2:
        return point, point, point
    rng = np.random.default_rng(seed)
    idx = rng.integers(0, v.size, size=(n_resamples, v.size))
    stats = statistic(v[idx], axis=1)
    alpha = (1.0 - level) / 2.0
    return point, float(np.quantile(stats, alpha)), float(np.quantile(stats, 1 - alpha))


def agg(df: pd.DataFrame, by, metrics, **kwargs) -> pd.DataFrame:
    """Aggregate metrics over seeds with medians and bootstrap intervals.

    The output is long-form: one row per (group, metric) with point, low, high and
    n. Long form is used because it plots and tabulates without reshaping, and
    because it makes the sample size behind every number explicit — an aggregate
    whose n is invisible is an aggregate that will eventually be over-read.
    """
    rows = []
    for keys, g in df.groupby(by, observed=True, sort=True):
        if not isinstance(keys, tuple):
            keys = (keys,)
        base = dict(zip(by if isinstance(by, list) else [by], keys))
        for m in metrics:
            point, lo, hi = bootstrap_ci(g[m].to_numpy(), **kwargs)
            rows.append({**base, "metric": m, "point": point, "low": lo,
                         "high": hi, "n": int(g[m].notna().sum())})
    return pd.DataFrame(rows)


def wide(long_df: pd.DataFrame, by, metric: str) -> pd.DataFrame:
    """Pick one metric out of a long-form aggregate as a wide table."""
    sub = long_df[long_df["metric"] == metric]
    return sub.set_index(by)[["point", "low", "high", "n"]]


def feasible_rate(df: pd.DataFrame, by) -> pd.DataFrame:
    """Fraction of conditions meeting the reliability constraint, with a Wilson interval.

    A Wilson interval is used rather than the normal approximation because the
    rates of interest are often at or near 0 and 1, where the normal interval
    produces bounds outside [0,1] and is known to under-cover.
    """
    rows = []
    for keys, g in df.groupby(by, observed=True, sort=True):
        if not isinstance(keys, tuple):
            keys = (keys,)
        base = dict(zip(by if isinstance(by, list) else [by], keys))
        n = len(g)
        k = int(g["feasible"].sum())
        p = k / n if n else np.nan
        lo, hi = wilson(k, n)
        rows.append({**base, "feasible_rate": p, "low": lo, "high": hi, "n": n, "successes": k})
    return pd.DataFrame(rows)


def wilson(k: int, n: int, z: float = 1.959964):
    """Wilson score interval for a binomial proportion."""
    if n == 0:
        return np.nan, np.nan
    p = k / n
    denom = 1 + z * z / n
    centre = (p + z * z / (2 * n)) / denom
    half = z * np.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / denom
    return max(0.0, centre - half), min(1.0, centre + half)


# --- plotting ---------------------------------------------------------------

def setup_matplotlib():
    """Configure matplotlib once, for consistent, legible figures."""
    import matplotlib
    matplotlib.use("Agg")  # no display in CI or over SSH
    import matplotlib.pyplot as plt
    plt.rcParams.update({
        "figure.figsize": (8, 4.5),
        "figure.dpi": 130,
        "savefig.dpi": 200,
        "savefig.bbox": "tight",
        "font.size": 9,
        "axes.titlesize": 10,
        "axes.labelsize": 9,
        "axes.grid": True,
        "grid.alpha": 0.25,
        "grid.linewidth": 0.5,
        "axes.spines.top": False,
        "axes.spines.right": False,
        "legend.frameon": False,
        "legend.fontsize": 8,
    })
    return plt


# A colour-blind-safe qualitative palette (Okabe-Ito). Chosen because the
# strategy comparison is the main visual claim in the results, and a reader who
# cannot distinguish the series cannot check the claim.
PALETTE = [
    "#0072B2", "#D55E00", "#009E73", "#CC79A7",
    "#E69F00", "#56B4E9", "#F0E442", "#000000",
]

# Semantic colours used consistently across figures.
COLOR_SAFE = "#009E73"
COLOR_RISK = "#D55E00"
COLOR_NEUTRAL = "#0072B2"


# Explicit colour per strategy rather than positional assignment from PALETTE.
# Assigning by index put the pale yellow (#F0E442) on `max`, which is one of the
# two most important series in every figure and was close to invisible on a white
# ground. The map below keeps the Okabe-Ito hues but orders them by contrast so
# that the conservative strategies, which carry the headline results, are the most
# legible.
STRATEGY_COLORS = {
    "current": "#666666",   # a baseline: deliberately muted
    "mean":    "#E69F00",   # amber
    "p50":     "#F0E442",   # pale yellow, the least important series
    "p90":     "#CC79A7",   # pink
    "p95":     "#0072B2",   # blue
    "p99":     "#009E73",   # green
    "max":     "#D55E00",   # vermillion
}


def strategy_colors(strategies):
    """Stable colour per strategy, so a strategy is the same colour everywhere."""
    present = set(strategies)
    return {s: c for s, c in STRATEGY_COLORS.items() if s in present}


def save(fig, name: str, caption: str = "") -> pathlib.Path:
    """Save a figure and write its caption beside it.

    Captions are written to disk rather than embedded in the image so that the
    report can include them as text and they remain searchable and diffable.
    """
    FIGURES_DIR.mkdir(parents=True, exist_ok=True)
    path = FIGURES_DIR / f"{name}.png"
    fig.savefig(path)
    if caption:
        (FIGURES_DIR / f"{name}.txt").write_text(caption.strip() + "\n")
    import matplotlib.pyplot as plt
    plt.close(fig)
    return path


def write_table(df: pd.DataFrame, name: str, float_fmt: str = "%.4f") -> pathlib.Path:
    """Write a table as CSV for the report to reference."""
    TABLES_DIR.mkdir(parents=True, exist_ok=True)
    path = TABLES_DIR / f"{name}.csv"
    df.to_csv(path, float_format=float_fmt)
    return path


def fmt_ci(point, low, high, pct: bool = False, digits: int = 1) -> str:
    """Render a point estimate with its interval, for inclusion in markdown tables."""
    if not np.isfinite(point):
        return "n/a"
    scale = 100.0 if pct else 1.0
    suffix = "%" if pct else ""
    return (f"{point*scale:.{digits}f}{suffix} "
            f"[{low*scale:.{digits}f}, {high*scale:.{digits}f}]")
