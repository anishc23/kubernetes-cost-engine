# Contributing

Thanks for looking. This is a research project with a production-shaped
implementation, and that shapes what contributions are most useful.

## What is most valuable

**Evidence that contradicts the findings.** The results here come from synthetic
workloads, which is the project's largest limitation. If you run this against real
workloads and the strategy rankings differ, that is the single most useful thing
you could report. Open an issue with the workload characteristics — burstiness,
duty cycle, growth — and what you observed.

**Production traces.** Replaying the engine against public traces (Google Borg,
Alibaba, Azure) would address the principal threat to validity. The engine
consumes a generic `model.Series`, so this is mostly a data-ingestion exercise.

**Real-cluster experience.** Cases where a recommendation was wrong, or a safety
gate fired when it should not have, or the opposite.

**Additional workload classes**, if they exercise a failure mode the existing ten
do not.

## Ground rules for changes

### Experimental findings are load-bearing

Several defaults are experimental results, not preferences, and tests assert them
with messages naming the finding they encode. If you change the default CPU
strategy away from p99, a test fails and tells you which result you are
contradicting.

**That is intentional.** Changing such a default requires new evidence: an
experiment configuration, a run, and an update to `research/results.md`. "It seems
better" is not sufficient, because the current value was arrived at by discovering
that "it seems better" had been wrong.

### Safety defaults do not move

- Mutation stays off by default, behind two independent settings.
- Only requests are patched, never limits.
- `BLOCKED` and `INSUFFICIENT_DATA` are never applied.
- No `delete` verb, no secret access in RBAC.

Chart tests enforce the last two. If you have a reason to change one of these,
open an issue first.

### The experiment framework contains no right-sizing logic

`internal/experiment` expands a matrix, invokes the production engine, and records
results. If right-sizing logic appeared there, the experiments would be measuring
code that is not deployed, and the findings would stop being statements about the
shipped system.

### Gates may never go below `min(proposed, current)`

Asserted at runtime and in tests. A gate that violated it would turn a safety
mechanism into a risk.

## Before opening a pull request

```bash
make verify        # fmt, vet, tests, chart lint and render
make experiment-smoke   # ~1s check that the experiment pipeline still runs
```

If you touched the engine, the simulator, or anything a finding rests on:

```bash
make research      # regenerate all experiments and figures
git diff --stat experiments/results analysis/figures
```

A change to results is not automatically a problem — but it should be intentional,
and it belongs in the pull request description.

## Style

**Comments explain why, not what.** The code says what it does. A comment should
say why it is done that way, especially where the obvious alternative was
rejected. Look at `internal/recommender/gates.go` or
`internal/simulator/replay.go` for the register.

**Tests state the property they protect.** A test named `TestFoo` that asserts
`x == 3` is hard to maintain because nobody knows whether 3 still matters. Prefer
names and failure messages that say what breaks if the assertion fails.

**Errors are actionable.** An error message should tell the reader what to do. The
configuration validator reports every problem at once for the same reason: a
sequence of restart-and-retry cycles is a poor use of someone's afternoon.

## Reporting bugs

Include: what you expected, what happened, the recommendation JSON if relevant
(it carries the statistics, strategy, gates and reason), and your policy
configuration from `GET /api/v1/policy`. Those three usually contain the answer.

## Security

Open a GitHub security advisory rather than a public issue. See
[docs/security.md](docs/security.md).

## Licence

Contributions are accepted under the Apache 2.0 licence.
