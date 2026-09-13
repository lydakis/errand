# Snapshot review follow-up: historical flat experiment

This report preserves an experiment, not the current architecture decision. Its
flat selection was withdrawn: memory and code size were the wrong decision
criteria, and the pipeline discarded indexed state before comparison. The active
experiment carries Merkle snapshots across those boundaries and compares against
a flat control under identical integration. See [the plan](SNAPSHOT_ENGINE_PLAN.md).

The shared metadata boundary remains. The flat candidate used one owned sorted
inventory instead of a hybrid slice and persistent Merkle tree. An update finds
edit positions by binary search, bulk-copies unchanged ranges, and validates
replacements against the final hierarchy. It does not rescan all entries for
validation. No-op updates preserve wire identity; actual deletion to empty uses
the checkpoint's canonical nil encoding.

This removes the production tree, node hashes, parallel hashing pool, height
fallback and representation dispatch. The production snapshot implementation is
269 lines, down from roughly 750. Sorted validation, linear comparison, subtree
selection, cached JSON identity, constant-time byte totals and the watch hash-cache
merge remain. The separate prototypes are frozen comparison material, not runtime
dependencies or interchangeable user options.

## Findings addressed

- Empty-state identity no longer depends on an update branch. Regression tests
  cover no-op identity, deletion to empty, and randomized structural updates
  against full hierarchy validation and an independent map oracle.
- Subtree selection collects explicit entries directly. Ancestors above each
  change root are selected once, avoiding repeated searches for every descendant.
- Source reconstruction assembles metadata in one private helper, then validates
  once at the checkpoint or owned-snapshot boundary. Both use the sorted archive
  validator and a shared source-path rule, including Git and transaction paths.
  Snapshot expansion no longer runs two full archive validations back to back.
- The benchmark harness inspects the fixture directory's actual filesystem.
  It no longer assumes arbitrary `--output` shares the checkout's volume.

## Representation comparison

The corrected hybrid and flat candidate contain the same reconstruction,
ancestor-selection and hash-cache changes. Three alternating rounds on each
runner isolate representation cost. Watch preparation uses five iterations per
sample; memory runs in separate processes with one iteration and explicit GC.
Fixtures were measured as APFS on Mac mini and Btrfs on Cabal, with GOMAXPROCS=2.
The [raw comparison](benchmarks/2026-09-13-snapshot-review-ablation.json) retains
the exact probe, source and binary digests, all samples, and command outputs.

| 10K-entry operation | Mac mini: hybrid → flat | Cabal: hybrid → flat |
|---|---:|---:|
| First edit preparation | 31.55 → 32.87 ms | 5.47 → 5.76 ms |
| Retained edit preparation | 27.86 → 28.01 ms | 5.31 → 5.61 ms |
| Full reconciliation | 181.42 → 179.23 ms | 200.57 → 197.76 ms |
| Prepared retained heap | 3.84 → 1.60 MB | 3.84 → 1.60 MB |
| After one edit, retained heap | 3.04 → 1.60 MB | 3.04 → 1.60 MB |

Flat preparation retains 58% less heap before the first edit and 47% less after
an edit. At 10K, warm preparation is about 0.15 ms slower on Mini and 0.30 ms
slower on Cabal. These short comparisons do not establish tail latency. This comparison does not justify selecting flat for a speed-first objective.
It does not evaluate retained indexed transfer comparison.

The memory diagnostic now includes the actual prepared state, which held both
the slice and tree in the hybrid. It also measures an updated snapshot and a
single survivor after mass deletion. These are heap-allocation differences after
GC, not RSS limits or a long-running watch soak.

## Full before/after check

The final comparison uses the reviewed production code as baseline and the flat
candidate with all four fixes. The candidate passed the full Go suite, vet and the focused
manifest/snapshot/changes/archive race checks on both hosts. The preserved flat candidate Go source
inventory matches the measured candidate exactly. Three alternating rounds retain
120 samples per host in the [final data](benchmarks/2026-09-13-snapshot-review-final.json),
along with the exact harness, build hashes and full validation logs. Each command
sample averages three operations; metadata uses a 150 ms target and watch
preparation uses ten iterations. Values below are median milliseconds.

| Operation | Mac mini: before → after | Cabal: before → after |
|---|---:|---:|
| Fetch + apply, ephemeral | 115.22 → 116.52 | 105.44 → 95.83 |
| Fetch + apply, persistent | 260.50 → 244.26 | 248.70 → 262.40 |
| Metadata 1000 flat expand | 0.74 → 0.62 | 2.82 → 2.78 |
| Metadata 1000 flat prepare | 0.73 → 0.60 | 2.35 → 2.43 |
| Metadata 1000 nested expand | 1.05 → 0.84 | 4.16 → 3.14 |
| Metadata 1000 nested prepare | 0.85 → 0.74 | 3.05 → 2.97 |
| Metadata 10000 flat expand | 7.82 → 6.21 | 28.95 → 29.93 |
| Metadata 10000 flat prepare | 6.70 → 5.97 | 29.17 → 25.18 |
| Metadata 10000 nested expand | 10.35 → 9.83 | 37.64 → 33.05 |
| Metadata 10000 nested prepare | 8.49 → 7.59 | 31.21 → 30.71 |
| Push, 10K | 552.14 → 569.70 | 990.29 → 1349.25 |
| Watch cycle, 10K | 436.55 → 413.35 | 614.80 → 549.76 |
| Watch preparation 1000 first-edit | 30.91 → 27.55 | 4.48 → 4.28 |
| Watch preparation 1000 reconcile | 65.55 → 67.17 | 26.72 → 24.75 |
| Watch preparation 1000 retained-edit | 26.59 → 31.87 | 4.31 → 4.20 |
| Watch preparation 10000 first-edit | 27.48 → 29.10 | 5.34 → 5.42 |
| Watch preparation 10000 reconcile | 187.50 → 204.42 | 201.63 → 192.60 |
| Watch preparation 10000 retained-edit | 36.19 → 28.15 | 5.24 → 5.55 |
| Ephemeral submission, 1K | 1842.57 → 697.03 | 737.77 → 929.98 |
| Workspace creation, 1K | 1369.22 → 672.47 | 2000.21 → 1691.20 |

Metadata preparation/expansion generally improves, but the end-to-end matrix is
mixed. In particular, Cabal push and submission, and Mini's 1K retained watch
preparation, have slower medians. Five paired [focused controls](benchmarks/2026-09-13-snapshot-review-focused.json)
repeated these observations. Median paired candidate/baseline ratios were 0.944
for Mini retained preparation, 1.046 for Cabal push, and 1.045 for Cabal submission.
The large initial slowdowns were smaller in the repeat, but the Cabal paths did
not demonstrate a win. One candidate push still took 1.411 s. All samples remain
recorded; these results do not justify selecting the flat representation. The
large creation/submission gains on Mini are not attributed to the representation
alone. These are native-host loopback comparisons, not network or rsync/Mutagen
measurements.

## Scope and reproducibility

The [preserved baselines](benchmarks/2026-09-13-snapshot-review-baselines.tar.gz)
contain overlays for the reviewed uncommitted hybrid (`baseline`) and corrected
hybrid (`hybrid`). Start each from `git archive 9f3708a`, then copy its overlay
over that checkout. The reconstructed Go source inventories were verified against
the actual benchmark inputs. These variants are not identical to their parent
commit. The comparison script
is embedded in the raw data. Its prepared-state memory benchmark deliberately
supports the preserved hybrid API as well as the flat candidate.

Watch still exports a complete manifest to its caller. Transactional staging,
body verification, conflict handling and durable retries are unchanged. The
existing watch shutdown drains an in-flight push; this slice does not change
that cancellation contract. Source preparation honors supplied contexts, while
JSON root serialization remains uninterruptible within its before/after checks.
Direct materialization, shared initialization, persisted inventory, large-file
deltas and fresh cross-machine rsync/Mutagen comparisons remain later work.
