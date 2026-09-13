# Snapshot index optimization follow-up, 2026-09-13

This follows the [first comparison](SNAPSHOT_INDEX_BENCHMARKS.md). It changes the
experimental snapshot implementations, not production push, fetch or watch.
The later [flat comparison](SNAPSHOT_REVIEW_FOLLOWUP.md) did not retain indexed
state through transfers. The [active plan](SNAPSHOT_ENGINE_PLAN.md) now tests that
integration with performance as the decision criterion.

## Changes

- Encode entry metadata directly before SHA256 hashing: a version byte,
  length-prefixed strings and fixed-width integers. This removes per-entry JSON
  encoding and normal-case temporary allocations. Both the partitioned index
  and tree use it. The current wire manifest and its identity are unchanged.
- Allocate cold tree nodes in blocks of at most 256. New snapshots still copy
  only their changed search paths; they share immutable unchanged subtrees.
- Compare structural changes by splitting at absent keys. Heap ordering proves
  which root is absent from the other tree; sorted recursion preserves output
  order and subtree hashes avoid scanning unchanged content. There is no longer
  a flatten-and-compare fallback for root changes.
- Prepare independent subtree hashes with a bounded worker pool, then hash their
  ancestors. It respects `GOMAXPROCS`, caps at four workers, and stays serial for
  trees below 512 entries. The measured configuration allows two processors.
  This spends available CPU parallelism to reduce cold-build wall time.

The tests compare updates and diffs with an independent map oracle, include
structural changes without shared pointers, and verify long metadata, ownership,
deletion to empty and deterministic construction with one, two and four available
processors. Local race and static checks passed.

## Comparison method

Each version uses the same expanded test and benchmark harness: 1K, 10K and 100K
entries, ten operations and three representations. Five rounds alternate version
order on each runner. There are 900 samples per runner per comparison. Compilation,
fixture generation, and contract tests are outside benchmark timing. Both versions
use Go 1.27.1, `GOMAXPROCS=2`, `CGO_ENABLED=0` and a 150 ms benchmark target.

The flat representation is unchanged and acts as a control for run-to-run
variation. Five microbenchmark repetitions are exploratory evidence, not reliable
tail estimates or confidence intervals. Some large cases run only one or a few
iterations per sample. Raw reports retain every sample and its round number.

The [intermediate serial comparison](benchmarks/2026-09-13-snapshot-index-serial-followup.json)
records the first three changes. It exposed the remaining Cabal cold-build cost:
15.93 ms for a 10K-entry tree versus 9.41 ms for flat. Some 1K Cabal tree medians
were also slower, including 100-entry edits and full export. Those observations
remain in the evidence; they motivated retaining the full matrix for the final
parallel-build comparison.

These are in-memory metadata measurements. APFS on Mac mini and Btrfs on Cabal
describe the hosts; neither filesystem is exercised inside the timed operations.
The results do not establish a change in transfer, fsync, inventory, browser
reload latency or parity with rsync/Mutagen.

## Final comparison

Five-sample medians at 10,000 entries. Times are milliseconds, except the explicit microsecond rows. The baseline is the original prototype; the candidate contains all four changes.

| Host | Operation | Original tree | Candidate tree |
|---|---|---:|---:|
| mac-mini | Cold build (ms) | 5.094 | 1.770 |
| mac-mini | Rebuild + compare (ms) | 5.088 | 1.765 |
| mac-mini | One-file edit (µs) | 2.692 | 2.364 |
| mac-mini | Delete highest-priority path (µs) | 350.659 | 2.984 |
| mac-mini | Compare 100 deletions (µs) | 72.490 | 21.440 |
| mac-mini | Compare disjoint snapshots (ms) | 1.439 | 0.868 |
| mac-mini | One edit + full export (ms) | 2.308 | 2.263 |
| cabal | Cold build (ms) | 24.625 | 10.419 |
| cabal | Rebuild + compare (ms) | 25.148 | 10.346 |
| cabal | One-file edit (µs) | 14.098 | 13.809 |
| cabal | Delete highest-priority path (µs) | 1300.843 | 20.884 |
| cabal | Compare 100 deletions (µs) | 239.065 | 100.704 |
| cabal | Compare disjoint snapshots (ms) | 5.579 | 3.729 |
| cabal | One edit + full export (ms) | 9.873 | 11.600 |

Cold tree construction now costs 1.77 ms versus 2.47 ms for flat on Mac mini, and 10.42 ms versus 9.73 ms for flat on Cabal in the same final comparison. At 100K entries, tree/flat medians are 17.92/23.83 ms and 101.76/97.62 ms respectively. This removes the large cold-build penalty, although the tree remains somewhat slower on Cabal.

At 10K entries, candidate construction allocates approximately 2.14 MiB in 54 allocations, versus 4.88 MiB in 40006 allocations for the original tree on Mac mini. These are per-build allocations, not retained-memory measurements.

Full export still costs milliseconds and is not consistently faster. The final broad comparison repeated slower 1K Cabal tree medians for 100-entry edits (1.016 → 1.360 ms) and full export (1.099 → 1.472 ms), and the 10K export median was also higher. The unchanged flat control varies too, but this is not grounds to silently discard the slower samples or claim no regressions.

[Final raw comparison](benchmarks/2026-09-13-snapshot-index-final-followup.json) retains all 1,800 samples. Both runners passed the full contract suite. Source digests match the local candidate and preserved baseline, and sample coverage was checked by operation, representation, size and round.

## Focused check of the slower Cabal samples

The broader runs did not provide a stable regression signal, so a temporary
probe isolated the 1K/10K batch-edit and export cases, with flat as a control.
Seven alternating rounds used a longer 500 ms target. The command was:

```sh
errand --on cabal --no-apply --artifact probe-results -- python3 scripts/probe_small_cases.py
```

The temporary probe and all 112 observations are retained in the
[focused report](benchmarks/2026-09-13-snapshot-index-small-probe.json). Its explicit
15% paired-median slowdown check did not fire:

| Tree operation | Original median ms | Candidate median ms | Median paired candidate/original ratio |
|---|---:|---:|---:|
| 1K, 100-entry edit | 1.143 | 1.212 | 1.031 |
| 1K, edit + full export | 1.193 | 1.204 | 0.964 |
| 10K, 100-entry edit | 1.307 | 1.213 | 0.931 |
| 10K, edit + full export | 10.608 | 10.426 | 0.997 |

The paired ratio is the median of ratios within each alternating round, so it
does not equal the ratio of the two marginal medians. This focused check did not
reproduce the larger slowdown. It does not establish its cause or prove that no
regression exists under the broad workload. No additional code was changed to
make this check pass; the broader slower observations remain in the reports.

## Decision and next integration

Keep all four changes. The tree now has cold construction near the flat reference
on Cabal and below it on Mac mini at 10K/100K entries, fast structural comparisons,
and substantially lower build allocation counts. The partitioned alternative
also benefits from the shared metadata encoder, but still needs full sorting for
ordered export and copies progressively larger partitions as the tree grows.

The next implementation step is a validated shared snapshot type used by
`PreparedTransferSource` and retained watch preparation. Carry that type through
reconciliation, with full export only where the current transport or durable
record requires it. Then replace the internal staging archive/extract cycle and
route initialization through the same verified materializer. Measure the actual
operations before claiming a user-visible gain.

These prototypes still require production input bounds, cancellation and retained
memory checks. Their representation-specific digest is not an adopted protocol.
Selection evidence, body verification, conflict policy and durable publication
retain their existing authority throughout integration.

## Reproduce

The original implementation is retained in the
[baseline archive](benchmarks/2026-09-13-snapshot-index-baseline.tar.gz), with the
expanded structural benchmark cases. Extract it into a fresh temporary directory,
then copy the current `experiments/snapshotindex/index_test.go` over the extracted
test file so both sides include the worker-independence contract. Run from the
current repository root:

```sh
python3 scripts/benchmark_snapshot_index.py \
  --output /tmp/snapshot-index-comparison \
  --baseline-source /tmp/extracted-snapshot-baseline \
  --revision "$(git rev-parse HEAD)"
```

Use a fresh output directory. Per-version reports record source files, source and
binary hashes, test output, sample counts and timings; the comparison also records
the alternating order. These are uncommitted prototypes based on parent revision
`9f3708a8a2b5ff7b9e7a84df3bd48809101e9d58`.
