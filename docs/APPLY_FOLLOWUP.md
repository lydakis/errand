# Apply follow-up: scratch, multiple parents, and exchange

This follows committed P1 (`092e88a`), whose grouped protocol applied only to
existing regular-file replacements under one parent. Changes in this slice are
kept separate in frozen benchmark inputs:

- **baseline:** committed P1 plus the new workload benchmark.
- **scratch:** skip synchronization for disposable merged output; retain verified
  durable transaction staging. Also includes the Git operational-error fix,
  which these uncontested apply workloads do not exercise.
- **parents:** scratch plus grouping across verified existing parents.
- **exchange:** isolated exchange protocol on top of parents, not production.
- **pre-group:** historical `16d747d` plus the same workload fixture, without a
  group hook. Used only for the separate one-root comparison.

All source identities and immutable archives are under
[`benchmarks/apply-followup/inputs`](benchmarks/apply-followup/inputs).
The exchange patch and recovery argument are in
[`experiments/applyexchange`](../experiments/applyexchange/README.md).

## Production behavior and safety

Disposable merge output is copied without file or directory synchronization.
It is not recovery evidence: apply subsequently copies it into the transaction,
verifies it, and durably publishes staged values before installation. The durable
transaction copy and publication remain unchanged. This extends the existing
private-scratch distinction to merged output.

Grouping now admits regular-file replacements across existing parents. The
journal binds each item's own parent identity. Only one installation-parent
handle is open at a time, and the final installation barrier covers **every**
distinct parent. The backup-data/member/backup-parent ordering before each
replacement is unchanged. Journal validation rejects contradictory identities
for the same parent and incomplete/mixed grouped intent. Recovery continues to
use each item's bound parent and existing backup/staged-value evidence.

Single roots, creations, deletions, new parents, metadata-only or nonregular
changes, and final modes requiring temporary widening retain the reference
protocol. One such item still makes the entire transaction use that protocol.
This is measured and explicitly remains an eligibility limitation.

`ERRAND_TRACE_APPLY=1` opts into a diagnostic line on the process doing the apply:

```text
errand apply: strategy=grouped reason=eligible roots=128 parents=8
errand apply: strategy=reference reason=creation roots=128 parents=0
```

`parents` counts grouped parents, not a second inventory of reference parents.
The default output is unchanged. No source paths, content or environment values
are logged. For fetch, set it on the client; push application runs on the runner.

Git's operational failures with empty merge output now propagate the command
error and stderr, even if a wrapper exits in the conflict-count range. The
regression covers exit codes 1, 69 and 255 with and without conflict materialization.
Actual text conflicts continue through the existing merge tests.

## Measurement method

`scripts/benchmark_apply_followup.py` verifies frozen source identities, builds
separate binaries, rotates variant and workload order, and times one operation
per sample (`-benchtime=1x`). Two reference labels run the same binary to expose
execution-position variability. Fixture construction, output/mode checks and
historical retry checks are outside timing; apply includes durable receipt and
transaction cleanup. The driver retains each raw log and its hash.

Apply workloads change one byte per file: one 4 KiB file, one 1 MiB file, 128
4 KiB files in one/eight/128 existing parents, and 128-root mixed transactions
with one creation, deletion, new directory or restricted final mode. Changed
root count stays fixed in the mixed cases; creations/deletions necessarily differ
in before/after body totals. This is not the earlier fixed-1-MiB scaling fixture.
The watch control uses the existing 1,000-entry explicit-selection loopback
fixture, with one measured update per process. It is neither the upcoming
realistic Git/editor matrix nor a Mutagen comparison.

Production comparison: 20 rounds per tiny/large/watch case and four rounds per
batch case, four modes, on each native host. There is no separate attribution
build; watch retains its existing phase timers equally in every variant.
Native tests run before timings. Independent protocol experiments run only after
the production campaign on that host finishes, avoiding self-created benchmark
contention.

## Results and decisions

Retain the scratch policy, multi-parent grouping, and Git error correction for review.
The exchange protocol remains isolated. The measurements below describe the
frozen `parents` candidate. Subsequent review edits remove a dead durable-copy
wrapper, give merge scratch an explicit name, open publication parents directly
for accurate errors, and add a per-parent crash checkpoint. Those edits are
recorded by exact before/after production-file hashes in
`benchmarks/apply-followup/review-source.json`; the verifier rejects any other
source drift. They have not been rebenchmarked. Added coverage also exercises
recovery between parent publications and the replaced-directory diagnostic.

The main campaign completed 352 individual operations per host (704 total).
Values are operation medians; reductions below use median same-round ratios
against the two identical-code controls.

<!-- apply-main:start -->
| Host / workload | Reference A / B | Scratch only | Candidate | Paired reduction vs A / B | Faster pairs vs A / B |
|---|---:|---:|---:|---:|---:|
| APFS / flat128 | 1424.5 / 1428.5 ms | 868.9 ms | 869.9 ms | 38.9% / 39.0% | 4/4 / 4/4 |
| APFS / parents8 | 6257.2 / 6234.6 ms | 5625.5 ms | 981.7 ms | 84.1% / 84.2% | 4/4 / 4/4 |
| APFS / parents128 | 6213.0 / 6295.0 ms | 5589.5 ms | 1498.4 ms | 75.8% / 76.2% | 4/4 / 4/4 |
| APFS / tiny | 104.9 / 105.4 ms | 101.6 ms | 102.4 ms | 3.7% / 3.2% | 19/20 / 14/20 |
| APFS / large | 123.5 / 125.8 ms | 120.5 ms | 118.9 ms | 4.8% / 4.4% | 17/20 / 17/20 |
| APFS / watch-small | 244.7 / 243.7 ms | 243.8 ms | 244.4 ms | 1.0% / 0.4% | 13/20 / 10/20 |
| BTRFS / flat128 | 3205.6 / 2849.9 ms | 3203.7 ms | 3009.9 ms | 8.7% / -5.6% | 2/4 / 1/4 |
| BTRFS / parents8 | 5694.8 / 6123.9 ms | 5044.6 ms | 2519.5 ms | 53.5% / 57.3% | 4/4 / 4/4 |
| BTRFS / parents128 | 5651.3 / 6182.6 ms | 5315.2 ms | 2980.1 ms | 47.9% / 51.9% | 4/4 / 4/4 |
| BTRFS / tiny | 84.0 / 81.6 ms | 80.1 ms | 79.6 ms | 5.3% / 1.9% | 14/20 / 12/20 |
| BTRFS / large | 126.9 / 127.9 ms | 124.4 ms | 123.6 ms | 2.1% / 4.0% | 12/20 / 14/20 |
| BTRFS / watch-small | 217.4 / 219.1 ms | 216.5 ms | 215.2 ms | 1.3% / 2.4% | 11/20 / 11/20 |
<!-- apply-main:end -->

Full mixed-case tables and raw observations: [APFS](benchmarks/apply-followup/apfs/summary.md),
[Btrfs](benchmarks/apply-followup/btrfs/summary.md).

The eight-parent and 128-parent candidates beat both controls in every batch
round on both hosts. APFS flat-batch improvement is attributable to scratch
synchronization removal; adding the parent map is near parity with that variant.
Btrfs flat results change sign with the control, so the four-round campaign
does not establish a flat-batch improvement or regression. A focused repeat
is recorded separately below.

Small-apply medians are lower in this campaign, but the Btrfs differences are
small relative to control variability and do not establish a reliable small-file
speedup. Watch remains effectively
unchanged in this fixture: approximately 244 ms on APFS and 215–219 ms on Btrfs.
There are occasional large observations in multiple Btrfs modes; these runs do
not establish production tail guarantees or universal non-regression.

Mixed creation/deletion/new-parent/restricted-mode cases still take roughly
5–6 seconds. Scratch savings do not eliminate their reference-protocol cost.
The remaining cliff is now measured rather than inferred from the flat fixture.

### Isolated exchange and historical one-root comparisons

The isolated exchange comparison uses the improved `parents` binary as its
reference, not original P1. Six rounds per host, all single-operation samples.

<!-- apply-exchange:start -->
| Host / workload | Reference A / B | Candidate | Paired reduction vs A / B | Faster pairs vs A / B |
|---|---:|---:|---:|---:|
| APFS / flat128 | 895.7 / 893.7 ms | 245.3 ms | 72.7% / 72.6% | 6/6 / 6/6 |
| APFS / parents8 | 953.9 / 970.1 ms | 312.4 ms | 67.3% / 67.6% | 6/6 / 6/6 |
| APFS / parents128 | 1489.8 / 1490.2 ms | 852.6 ms | 42.6% / 42.9% | 6/6 / 6/6 |
| BTRFS / flat128 | 2577.5 / 2721.6 ms | 2235.4 ms | 9.4% / 13.7% | 4/6 / 5/6 |
| BTRFS / parents8 | 2512.9 / 2533.8 ms | 2114.7 ms | 14.1% / 17.1% | 5/6 / 5/6 |
| BTRFS / parents128 | 2941.0 / 3272.8 ms | 2784.5 ms | 5.0% / 17.7% | 4/6 / 4/6 |
<!-- apply-exchange:end -->

[APFS exchange evidence](benchmarks/apply-followup/apfs-exchange/summary.md) and
[Btrfs exchange evidence](benchmarks/apply-followup/btrfs-exchange/summary.md).
Every APFS exchange observation beat both same-round controls. Btrfs median
ratios improve, but individual exchange observations lose against a control in
every workload; this is a smaller and less consistent benefit. The protocol is
not adopted: cross-directory persistence before the final parent barrier and
interrupted recovery need their separate review. The prototype validates the
performance opportunity without silently changing production recovery.

The [24-round Btrfs historical check](benchmarks/apply-followup/btrfs-historical-small/summary.md)
compares **committed P1** (`baseline` in that report) against pre-P1 `16d747d`
(reference A/B). Tiny-file medians are 77.05 / 75.49 → 83.66 ms; 1 MiB-file
medians are 121.60 / 122.54 → 125.15 ms. Median paired overhead is
8.5% / 3.9% (6.46 / 2.93 ms) for tiny and 1.5% / 3.0% (1.76 / 3.64 ms)
for large. P1 is slower in 19/24 and 20/24 tiny-file pairs, and 18/24 and 17/24
large-file pairs. This supports a small P1 cost, with material noise and outliers,
rather than proving universal non-regression. It measures the whole P1 slice,
not an isolated syscall toggle. The stronger backup-data ordering is retained.
Do not subtract medians across this and the main campaign to claim a causal net
change from pre-P1 to the current candidate.

<!-- apply-historical:start -->
| Workload / variant | Reference A / B | Candidate | Paired reduction vs A / B | Faster pairs vs A / B |
|---|---:|---:|---:|---:|
| tiny / baseline | 77.05 / 75.49 ms | 83.66 ms | -8.4% / -3.9% | 5/24 / 4/24 |
| large / baseline | 121.60 / 122.54 ms | 125.15 ms | -1.5% / -3.0% | 6/24 / 7/24 |
<!-- apply-historical:end -->

The [12-round Btrfs flat repeat](benchmarks/apply-followup/btrfs-flat-repeat/summary.md)
was triggered by the first campaign's conflicting controls. P1 A/B medians are
3.122 / 3.194 s, scratch-only 2.622 s, and scratch+parents 2.809 s. Paired time
reductions are 20.7% / 15.9% for scratch and 12.8% / 11.4% for scratch+parents.
The latter wins 9/12 and 10/12 pairs. Retain the original four-round report
alongside this repeat; neither all-pair dominance nor precise small differences
between scratch and parents are established.

<!-- apply-repeat:start -->
| Workload / variant | Reference A / B | Candidate | Paired reduction vs A / B | Faster pairs vs A / B |
|---|---:|---:|---:|---:|
| flat128 / scratch | 3122.02 / 3194.25 ms | 2621.63 ms | 20.7% / 15.9% | 7/12 / 10/12 |
| flat128 / parents | 3122.02 / 3194.25 ms | 2809.05 ms | 12.8% / 11.4% | 9/12 / 10/12 |
<!-- apply-repeat:end -->

## Validation and retained evidence

Production package suites passed on APFS and Btrfs for `internal/changes`,
`internal/snapshot`, `internal/client`, `internal/daemon` and `cmd/errand`.
Focused race coverage passed for grouped application, synchronization and Git
merge handling. Multi-parent crash tests repeat the existing process-exit matrix
on files in different parents. The added every-parent publication test verifies
that failure of the last parent barrier rolls the transaction back before commit.

After the review edits, the full `internal/changes` suite and focused
`Test(Grouped|ApplySynchronization|MergeRegular)` race coverage passed again on
APFS and Btrfs. This includes the partial-parent-publication crash and accurate
replaced-directory error regressions. All 13 apply-related Python tests passed.
[Review validation](benchmarks/apply-followup/review-validation.json) records the
runner jobs, commands, results and source hashes. These are correctness checks,
not new performance measurements.

The isolated exchange candidate passed ordinary apply/merge/transfer regressions,
its dedicated crash, fallback, concurrent-edit and ambiguous-evidence tests, and
race checks on both native hosts. Reference-protocol ordering tests remain enabled
in production; the experimental run excludes assertions specific to the old
rename sequence. None of these are power-loss tests.

All six campaigns completed and cleaned their scratch directories: 704 main
observations, 108 exchange observations, 144 historical one-file observations,
and 48 focused Btrfs repeat observations: **1,004 individual samples**. Verified
source identities, raw-log hashes, sample membership/counts, execution positions
and regenerated summaries using `scripts/verify_apply_followup.py`. The verifier
also regenerates the marked decision tables above, including same-round
reductions and win counts, and checks the exact reviewed production source against
the frozen source plus recorded review edits. Native test logs, frozen inputs,
the exchange patch, commands, runner handles and checksums
are retained under [the evidence directory](benchmarks/apply-followup/runs.json).
The main driver is also frozen inside the `parents` inputs; subsequent runs used
the retained `driver.py`, whose only addition was `--cases` selection.

## Further work

Exchange stays isolated until its distinct recovery protocol has a separate
adoption review, including interrupted rollback and filesystem persistence.
That review must assert restoration or retention for every sibling after a
concurrent edit, rather than checking only the edited file. Also investigate two
inherited behaviors separately: restoring a later user deletion during rollback,
and accepting a content-identical replaced parent after its publication barrier.
Neither is established here as a new regression. Stronger classification of Git
wrappers that emit stdout remains separate from the empty-output correction.
Creations under existing parents are the next bounded eligibility extension;
deletions and newly created parents need their own recovery analysis. Compact
progress records remain gated on residual serialization cost. The broader next
roadmap item remains realistic Git/editor-save watch measurements (P2).
