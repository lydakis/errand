# Shared hierarchy-validation evidence

Decision and safety condition: [HIERARCHY_VALIDATION.md](../../HIERARCHY_VALIDATION.md).
Baseline: `dd9fb846bd600eeb4e0e984926bd8e4da54c7328`.
Measured on 2026-09-16, native APFS/Darwin arm64 and Btrfs/Linux amd64,
Go 1.27.1, GOMAXPROCS=2. Each report records its own toolchains and mount.

## Campaigns and source versions

| Directory | Runtime candidate | Scope |
|---|---|---|
| `apfs`, `btrfs` | Initial cumulative predicate | 168 command samples and 1,368 complete preparation samples per host |
| `btrfs-controls` | Same initial predicate | 144 samples, same-binary A/A/B, microbenchmarks at 1s and fetch at 5 iterations |
| `btrfs-deferred` | Initial predicate plus isolated second-pass alternative | 108 samples, microbenchmarks at 300ms |
| `apfs-mismatch`, `btrfs-mismatch` | Initial predicate plus isolated mismatch-flag alternative | 108 samples per host, microbenchmarks at 300ms |
| `apfs-final`, `btrfs-final` | Original final mismatch-flag diff | 168 command samples and 72 dense preparation samples per host |
| `apfs-alternatives`, `btrfs-alternatives` | Predicate, preallocation, combined validation | 216 samples per host; all six orders twice |
| `apfs-boundaries`, `btrfs-boundaries` | Predicate, preallocation, independent entry-only validation | 432 samples per host, including deletion boundaries |
| `apfs-callers`, `btrfs-callers` | Selected predicate plus entry-only validation against the published baseline | 216 samples per host; same-binary A/A/B, all six orders twice |

Do not pool campaigns. [RESULTS.md](RESULTS.md) retains the original comparison,
[CONTROLS.md](CONTROLS.md) the focused diagnostics, and
[FINAL.md](FINAL.md) the original predicate candidate. The entry-only follow-up
has separate alternative, boundary and caller-control reports. Ratios are medians of matched
pairs, rather than ratios of separately reported medians. Pair ranges are
observed extrema, not confidence intervals.

The original campaigns include repeated-file and dispersed 10K histories at
depths 0/8/31, fully crossing six depth orders with six storage-mode orders.
The original final candidate repeats actual callers, all six direct update cases and the
50K dense append/compaction cases; it does not repeat the sparse history matrix.
Commands run against native loopback servers. Timings exclude fixture setup,
shell startup and cross-host network delivery. Preparation uses fresh processes
with warm filesystem caches. No timing sample uses a profiler.

Each directory retains `report.json`, `candidate-inputs.tar.gz`, native
filesystem/toolchain information, raw per-run logs, and an Errand `job.txt`.
The primary/final runs also retain ordinary Go test, race, vet and Python logs.
Follow-ups retain native Go test/race/vet logs. Python evidence tests run locally;
their current result is recorded in [followup-verification.json](followup-verification.json).
`raw-logs.tar.gz` contains individual samples, build logs, setup and drain logs
not stored separately. Reports record source, harness, archive and binary hashes.
The baseline input archive is retained once at this directory's root.

The initial test's parent-type case was strengthened after primary measurement
to leave a descendant untouched. `post-measurement-test.patch` records that
test-only change; primary host `final-test.txt` files verify it natively with
the race detector. Later controls and the final run use the stronger test. The
benchmark body did not change. Final Go/Python inputs were checked against their
archives after retrieval, before the subsequent review follow-up. `verification.json`
records that historical check; follow-up campaigns retain their own inputs.

## Reproduction

Start with a disposable checkout of `dd9fb846bd600eeb4e0e984926bd8e4da54c7328`,
overlay the desired campaign's `candidate-inputs.tar.gz`, and copy the retained
evidence directory to `docs/benchmarks/hierarchy-validation/`. The post-review
Python evidence tests use its reports, archives and renderer modules as fixtures. Using a base checkout
also supplies unchanged non-Go/Python inputs such as `.github/workflows/release.yml`,
which the Python suite reads but these source archives do not include.

For the original full campaign:

```sh
python3 scripts/benchmark_hierarchy_validation.py --output dist/hierarchy-validation
```

For the original final candidate, use the `apfs-final` or `btrfs-final` campaign's archive and add `--focused`.
On Mac mini set `DEVELOPER_DIR=/Library/Developer/CommandLineTools`.
The driver uses a unique, owned scratch directory on the native filesystem,
removes it on exit, and kills/reaps process groups on timeout or interruption.

Diagnostic drivers are retained inside their own campaign archives at
`experiments/snapshotcheckpoint/hierarchy_controls.py`; they are not an additional
maintained production tool. From the matching archive, run it with an output
directory argument. The deferred campaign adds `--deferred-types --micro-only
--duration 300ms`; the mismatch campaigns add `--mismatch-types --micro-only
--duration 300ms`. Those alternatives are constructed only inside scratch and
their exact changes are retained in `deferred.patch` or `mismatch.patch`.

Regenerate tables without any benchmark run:

```sh
python3 summarize.py apfs/report.json btrfs/report.json
python3 summarize.py apfs-final/report.json btrfs-final/report.json
python3 summarize_controls.py btrfs-controls/report.json btrfs-deferred/report.json btrfs-mismatch/report.json apfs-mismatch/report.json
```

These renderers validate sample identity/counts, paired receipts and full crossed
ordering where applicable. `SHA256SUMS` covers every retained file except itself;
verify from this directory with `shasum -a 256 -c SHA256SUMS`.

## Review follow-up tooling

`evidence.py` supplies explicit integrity failures that remain active under
`python -O`. Both original renderers verify the candidate archive before
rendering. The main renderer also validates phase identities and durations and
prints dense Load/Index tables. The measured JSON/logs and source archives are
unchanged; updated Markdown tables are regenerated from them.

`CALLER_DIAGNOSTICS.md` is generated without a new measurement:

```sh
python3 summarize_callers.py apfs-final/report.json btrfs-final/report.json
```

It separates Go's calibration run from all final timed fetch iterations and
prints burst phases, including settle time and resample counts. No timed outliers
are dropped. Fetch is a negative control for this runtime change because its
apply paths do not invoke `Snapshot.Update`.

The bounded follow-up driver is `scripts/benchmark_hierarchy_followup.py`, with
`--mode alternatives`, `--mode boundaries`, or `--mode callers`, `--output DIR`, and 12 rounds by
default. Alternatives compare the original final candidate, replacement-slice
preallocation, and preallocation plus entry-only validation in scratch copies.
Boundary runs separate entry-only validation from preallocation and add single
updates, deletion-only and deletion-heavy batches. Each campaign's exact patches
define its variants; do not pool their samples.
Caller controls execute the exact final candidate and the same baseline binary
twice, using all six orders equally often. Reports retain exact variant patches,
source and binary identities, native tests/race/vet, timing budgets, raw logs,
phase metrics, and host load observations. Timing runs are unprofiled.

Alternative and boundary modes must be replayed from their corresponding pre-adoption source archives. Caller mode uses the selected entry-only source.

The selected caller controls and their complete timed fetch iterations are
generated with:

```sh
python3 summarize_followup.py apfs-callers/report.json btrfs-callers/report.json
python3 summarize_callers.py apfs-callers/report.json btrfs-callers/report.json
```

Outputs are [CALLER_CONTROLS.md](CALLER_CONTROLS.md) and
[FOLLOWUP_CALLER_DIAGNOSTICS.md](FOLLOWUP_CALLER_DIAGNOSTICS.md). The selected Go
and module files match both caller archives and the independently measured
entry-only boundary variant. The verification record also checks the pinned
baseline archive against Git and every retained raw sample against its report.
