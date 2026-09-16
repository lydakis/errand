# Observation-only journal comparison

This bounded step-3 experiment compares changed-record observation publication
with full observation replacement. Production push, fetch, watch, workspace
creation and job submission still do not use disk checkpoints.

## Decision

Retain the observation journal as an experiment. Do not enable it in production.
It substantially reduces edit publication bytes, but it does not establish a
consistent complete-preparation win across these workloads and filesystems.

For the 50K-file cumulative six-edit sequence (loaded journal depths 0–5),
median paired journal/replacement ratios are **0.986 on
APFS** (3/6 wins, range 0.835–1.102) and **0.935 on Btrfs** (5/6 wins, range
0.896–1.006), using the same-binary replacement control. The APFS effect is
unresolved; the Btrfs result is promising for sparse edits. Comparing only against
the frozen control would report larger gains, 6.3% and 7.0%, respectively. That
would conflate journal behavior with control/binary/run variation. The comparison
does not establish that the storage refactor itself caused those differences.

At 50K entries, unchanged journal restarts have paired ratios of 1.012 and 1.056,
and record compaction 0.985 and 1.068. Byte-triggered compaction is slower in all
six pairs on both hosts: **1.120 APFS and 1.145 Btrfs**. Recovery costs 1.125 and
1.256 relative to the controls' ordinary unchanged restarts, with the workload
asymmetry described below. Smaller and Git-selected cases are mixed. These six
rounds identify tradeoffs; small differences are not equivalence or universal
speedup claims.

The selected contiguous/Merkle engine, production caller behavior and production
fsync policy remain unchanged. Whole-file body transfer is a separate roadmap
step. No remote delivery improvement is claimed by these preparation timings.

## Implementations and guarantees

The control is observation replacement from frozen commit
`9d9fe5ab02a54b8e6ce48a2542a5441741019bfe`. A second control runs that same
replacement strategy in the candidate binary, controlling shared preparation and
dispatch. It does not isolate framing/codec overhead from journaling; a framed
observation replacement comparator could separate those effects in a later slice.

The new `observation-journal` probe stores a base of ordered source observations,
then appends changed observations and explicit deletions. Stamp-only changes are
included. It stores no serialized tree nodes or subtree hashes. Every process
reconstructs the selected adaptive index. This leaves the contiguous/Merkle
crossover unchanged.

Preparation delegates loading/publication to a storage strategy. Derived and
observation-only journals share the framed codec, locks, generation checks,
publication, byte limits and recovery implementation. The observation decoder
rejects derived-image fields. Existing `derived` and `journal` modes remain
available for reproducing prior experiments.

Each journal is bounded to 32 transactions, 8 MiB of journal frames and 64 MiB for
the complete file. Replacement uses one temporary file plus atomic rename.
Append checks the full loaded file's checksum under the writer lock; size-only
checks are not substituted. Busy caches are advisory misses or skipped writes.
Full replacement remains last-writer-wins. Interrupted or invalid suffixes recover
only the fully validated prefix and force a replacement on successful publication.

Replay validates every transaction through the shared manifest implementation,
then coalesces observation edits into one array merge. For an indexed inventory,
replaying the first delta can construct the retained tree during loading. A
base-only restart constructs it during index preparation. Consequently phase
shifts do not establish saved work; total preparation is the decision metric.

Fresh selection, exact native stat evidence, verified observation ownership,
source/selection checks and cancellation remain required. Checkpoint writes do
not fsync because the cache is expendable. Production data and receipt durability
are unchanged. Filesystem eligibility, private-directory ownership and handle
lifetime remain requirements before production integration.

## Measurement

`benchmark_observation_journal.py` builds the frozen control and candidate. Each
sample runs in a fresh Go process with `GOMAXPROCS=2`, on a native APFS or Btrfs
fixture. The driver rejects other filesystems. OS/page caches remain warm. Each
case uses six rounds covering all six execution orders of the three variants.
Tests, race checks and vet complete before timing; fixture mutations and `sync`
are outside the measured interval.

`Total` includes selection, loading and validation, stat/hash work, index
preparation, verification, publication and wire-root hashing. It excludes process
startup and JSON output. This is not a network, CLI-startup or application-ready
latency comparison.

Fixtures contain 1K files of 32 bytes, 10K of 4 KiB, 50K of 128 bytes, and a
Git-selected 10K/4-KiB tree. Each has misses, unchanged restarts, one-file edits,
1,000-file batches, record-triggered compaction and interrupted-suffix recovery.
The 50K fixture also exercises actual byte-triggered compaction. There are 450
measured samples per host.

- Ordinary edit and batch cases seed once per scenario, then accumulate six
  transactions. Their timed loaded depths are **0, 1, 2, 3, 4, 5**; journal
  execution positions are **3, 2, 1, 1, 2, 3**. These summarize one shallow growing
  sequence, not repeated fixed-depth trials, steady-state cost, or a full
  append/compaction lifecycle. History depth and execution order are coupled.
- Record compaction replays 32 real untimed transactions before the measured edit
  replaces the base.
- Byte compaction uses a real 30K-file edit to fill more than half the 8-MiB
  journal budget, then times a second 30K-file edit and replacement. The driver
  checks actual file growth, one loaded record and actual replacement. No
  synthetic byte accounting is used in the timed case.
- Recovery appends a torn suffix after four real transactions. The timed journal
  sample replays those records and replaces the base without rehashing unchanged
  source bodies. Replacement controls do unchanged work in this case, showing
  recovery overhead rather than claiming equivalent failure recovery.

Every measured round requires identical wire roots, exact hash/reuse counts,
expected cache status and successful publication. Compaction and recovery also
assert loaded history and replacement receipts. Raw samples retain execution
position, written bytes, cache size and the setup history. The original summaries
use medians of per-round ratios to the frozen control. Retained `review-summary.json`
files now reproduce both frozen and same-binary replacement comparisons directly
from those raw samples, using medians of paired ratios, and expose the ordinary
history/order sequence. Missing or duplicate pairs are rejected.

The reviewed harness additionally reads ordinary journal framing before and after
samples, outside timers, and checks depth, replacement, written bytes and growth.
Longer campaigns require replacement at 32 loaded records. Before either byte
budget becomes ambiguous, they reseed untimed and retain that reset in the sample's
setup metadata. The ordinary workload imposes and verifies an append ceiling of
4 KiB plus 2 KiB per edited file; this is a harness constraint, not an assumption
used by the storage implementation. Dedicated byte-compaction cases still time
real byte-limit replacement. Setup logs now use distinct fixture/scenario/round/
step names. These follow-up changes were not used for the retained measurements.

## Results

Both campaigns completed 450 samples on Go 1.27.1. APFS used Darwin arm64 on the
Mac mini; Btrfs used Linux amd64 on Cabal. Both passed regular tests, race tests
and vet for the manifest, snapshot and experimental checkpoint packages, plus
61 Python harness tests. Post-measurement review follow-ups have separate
validation records; they are not retroactively counted as campaign validation. All 900 timed samples passed their behavior
checks, and each of the 300 three-way rounds produced matching wire roots.

For 50K-file edits, APFS writes fall from **8,142,569 to 1,087 bytes** and Btrfs
from **7,335,034 to 1,086 bytes**, comparing current replacement with the journal.
These are local advisory-cache writes, not transferred bytes. The following
phase medians also pool the shallow 0–5-depth sequence:

| Host | Strategy | Load ms | Index ms | Save ms |
| --- | --- | ---: | ---: | ---: |
| apfs | checkpoint | 21.40 | 10.09 | 15.92 |
| apfs | replacement | 21.27 | 10.31 | 16.65 |
| apfs | observation-journal | 33.63 | 0.68 | 5.62 |
| btrfs | checkpoint | 88.68 | 57.96 | 82.75 |
| btrfs | replacement | 90.16 | 56.67 | 83.97 |
| btrfs | observation-journal | 152.93 | 2.34 | 27.57 |

Publication becomes cheaper, while loading still decodes and verifies the whole
base plus history. With prior transactions, tree preparation moves into load;
load plus index remains comparable to or larger than replacement. The byte-limit
case additionally replays a large edit and encodes a candidate delta before
falling back to a complete base. Those paths explain where this implementation
does work, but these phase timers do not isolate codec CPU, allocation, checksum,
syscall and scheduling contributions. Profile them before selecting another
storage change. Medians of phases need not sum to the median total.

The actual untimed byte-compaction setup appended 4,921,133 bytes on APFS and
4,441,404 on Btrfs. The timed second 30K-file update crossed the unchanged 8-MiB
limit on each host, with one record loaded and base replacement verified in every
round. The 64-MiB total-file boundary is regression-tested with synthetic accounting
and real publication/restart; it is not claimed as a timed native scenario.

The following tables retain all fixture/scenario results. F is frozen replacement,
R is replacement in the candidate binary, and J is the observation journal.
Times are median milliseconds; ratios are medians of paired totals. J/R range and
wins retain the observed spread, not confidence intervals. Lower ratios are better.

### APFS

| Fixture | Case | F ms | R ms | J ms | J/F | J/R | J/R range; wins |
| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1000-32-ignore | batch | 50.82 | 43.32 | 45.28 | 1.118 | 1.108 | 0.725–2.209; 2/6 |
| 1000-32-ignore | edit | 36.81 | 36.05 | 43.08 | 1.258 | 1.207 | 0.459–2.204; 1/6 |
| 1000-32-ignore | miss | 58.51 | 50.86 | 63.12 | 1.101 | 1.337 | 0.729–1.685; 2/6 |
| 1000-32-ignore | records | 49.83 | 47.49 | 50.32 | 1.060 | 1.284 | 0.623–1.620; 2/6 |
| 1000-32-ignore | recovery | 45.22 | 36.07 | 37.94 | 1.098 | 1.159 | 0.560–2.041; 2/6 |
| 1000-32-ignore | unchanged | 46.56 | 42.35 | 42.72 | 0.990 | 1.036 | 0.630–1.884; 3/6 |
| 10000-4096-git | batch | 311.42 | 300.97 | 299.25 | 0.995 | 0.952 | 0.791–1.353; 4/6 |
| 10000-4096-git | edit | 287.94 | 247.30 | 265.54 | 0.989 | 1.042 | 0.901–1.371; 3/6 |
| 10000-4096-git | miss | 385.37 | 382.34 | 356.30 | 0.929 | 0.924 | 0.856–0.977; 6/6 |
| 10000-4096-git | records | 309.66 | 291.18 | 268.57 | 0.931 | 0.908 | 0.855–1.017; 5/6 |
| 10000-4096-git | recovery | 289.12 | 277.13 | 278.21 | 0.992 | 1.038 | 0.854–1.238; 3/6 |
| 10000-4096-git | unchanged | 270.91 | 254.15 | 284.72 | 1.027 | 1.092 | 0.771–1.361; 2/6 |
| 10000-4096-ignore | batch | 103.04 | 88.81 | 95.87 | 0.988 | 1.115 | 0.741–1.499; 2/6 |
| 10000-4096-ignore | edit | 97.18 | 83.49 | 81.46 | 0.881 | 0.939 | 0.867–1.342; 4/6 |
| 10000-4096-ignore | miss | 210.77 | 193.68 | 196.95 | 0.911 | 1.004 | 0.865–1.083; 2/6 |
| 10000-4096-ignore | records | 77.48 | 85.83 | 83.01 | 0.990 | 1.008 | 0.768–1.253; 3/6 |
| 10000-4096-ignore | recovery | 78.68 | 82.04 | 89.77 | 1.184 | 1.046 | 0.888–1.376; 3/6 |
| 10000-4096-ignore | unchanged | 73.85 | 79.38 | 84.64 | 1.109 | 1.069 | 0.694–1.173; 2/6 |
| 50000-128-ignore | batch | 300.00 | 294.10 | 288.98 | 0.978 | 0.995 | 0.897–1.120; 3/6 |
| 50000-128-ignore | bytes | 651.95 | 630.83 | 703.52 | 1.077 | 1.120 | 1.074–1.162; 0/6 |
| 50000-128-ignore | edit | 282.24 | 269.28 | 260.85 | 0.937 | 0.986 | 0.835–1.102; 3/6 |
| 50000-128-ignore | miss | 823.21 | 810.57 | 818.44 | 1.002 | 1.015 | 0.949–1.067; 2/6 |
| 50000-128-ignore | records | 271.96 | 287.93 | 276.22 | 1.010 | 0.985 | 0.910–1.101; 3/6 |
| 50000-128-ignore | recovery | 263.29 | 249.80 | 289.13 | 1.064 | 1.125 | 0.972–1.214; 1/6 |
| 50000-128-ignore | unchanged | 255.05 | 248.52 | 260.05 | 1.007 | 1.012 | 0.904–1.154; 3/6 |

### BTRFS

| Fixture | Case | F ms | R ms | J ms | J/F | J/R | J/R range; wins |
| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| 1000-32-ignore | batch | 29.30 | 29.19 | 40.23 | 1.393 | 1.374 | 0.966–1.560; 1/6 |
| 1000-32-ignore | edit | 16.72 | 16.56 | 15.22 | 0.906 | 0.915 | 0.834–0.929; 6/6 |
| 1000-32-ignore | miss | 23.97 | 23.95 | 24.21 | 1.010 | 1.009 | 0.997–1.026; 1/6 |
| 1000-32-ignore | records | 16.34 | 16.34 | 20.66 | 1.270 | 1.269 | 1.190–1.325; 0/6 |
| 1000-32-ignore | recovery | 14.11 | 13.89 | 17.41 | 1.247 | 1.243 | 1.183–1.311; 0/6 |
| 1000-32-ignore | unchanged | 13.85 | 14.09 | 14.36 | 1.030 | 1.022 | 0.986–1.043; 1/6 |
| 10000-4096-git | batch | 377.76 | 406.33 | 419.20 | 1.068 | 1.049 | 0.919–1.112; 2/6 |
| 10000-4096-git | edit | 316.87 | 322.23 | 307.53 | 0.989 | 0.971 | 0.903–1.081; 4/6 |
| 10000-4096-git | miss | 711.55 | 677.17 | 687.10 | 0.996 | 1.010 | 0.836–1.169; 2/6 |
| 10000-4096-git | records | 361.43 | 360.03 | 367.57 | 1.004 | 1.022 | 0.966–1.123; 2/6 |
| 10000-4096-git | recovery | 347.62 | 342.24 | 358.99 | 1.032 | 1.063 | 0.997–1.145; 1/6 |
| 10000-4096-git | unchanged | 302.39 | 298.49 | 306.90 | 1.038 | 1.017 | 0.982–1.140; 2/6 |
| 10000-4096-ignore | batch | 158.28 | 160.31 | 172.99 | 1.059 | 1.039 | 0.929–1.163; 1/6 |
| 10000-4096-ignore | edit | 135.15 | 129.41 | 129.54 | 0.937 | 0.983 | 0.889–1.097; 3/6 |
| 10000-4096-ignore | miss | 451.59 | 446.80 | 473.82 | 1.048 | 1.099 | 0.950–1.304; 2/6 |
| 10000-4096-ignore | records | 143.77 | 133.13 | 134.80 | 0.937 | 1.015 | 0.926–1.148; 3/6 |
| 10000-4096-ignore | recovery | 117.43 | 107.98 | 143.98 | 1.213 | 1.305 | 1.046–1.425; 0/6 |
| 10000-4096-ignore | unchanged | 113.85 | 110.03 | 115.83 | 1.028 | 1.051 | 0.933–1.225; 2/6 |
| 50000-128-ignore | batch | 630.68 | 626.09 | 632.51 | 1.005 | 1.002 | 0.914–1.084; 3/6 |
| 50000-128-ignore | bytes | 1117.72 | 1131.83 | 1313.82 | 1.171 | 1.145 | 1.005–1.281; 0/6 |
| 50000-128-ignore | edit | 617.90 | 622.28 | 575.27 | 0.930 | 0.935 | 0.896–1.006; 5/6 |
| 50000-128-ignore | miss | 1109.20 | 1130.25 | 1110.16 | 1.000 | 0.972 | 0.964–1.104; 5/6 |
| 50000-128-ignore | records | 614.88 | 610.58 | 637.84 | 1.045 | 1.068 | 0.930–1.095; 1/6 |
| 50000-128-ignore | recovery | 552.85 | 518.18 | 655.64 | 1.189 | 1.256 | 1.130–1.311; 0/6 |
| 50000-128-ignore | unchanged | 543.82 | 549.49 | 554.65 | 1.020 | 1.056 | 0.954–1.089; 2/6 |

## Review follow-ups and next slice

The measurement slice described below is now complete in
[Journal depth and preparation profiles](JOURNAL_DEPTH_PROFILE.md). It retains
fixed histories, full cycles, the framed replacement control and diagnostic
profiles. Current journal adoption is rejected on dense-history costs. Review
follow-ups disclose repeated-file history and coupled ordering, separate CPU/heap
instrumentation, and reopen candidate ranking rather than selecting shared
hierarchy validation from the original profiles. This section
preserves the handoff that led to that investigation; its original measurements
and evidence remain unchanged.

- Included here: a shared storage strategy instead of additional preparation
  booleans; an observation-only format with explicit derived-image rejection;
  structural/stamp-only restart coverage, stale-writer rejection, busy-cache
  fallback, shared byte-boundary and damaged-middle-frame coverage; and native
  timing of actual byte compaction and recovery after real journal history.
  Review follow-ups tighten ordinary receipts, retain both paired baselines,
  document the shallow sequence, add equal-sized different-generation rejection,
  and share corruption, busy-cache, structural, coalescing and restart tests across
  journal formats. Redundant observation-only cases and unused helpers are removed.
- Next measurement slice: use fixed depths 0, 8 and 31, with independently
  balanced execution order and history. Include near-full append and a complete
  append/compaction cycle. Investigate selection-phase variation and preceding
  heavy-writer interference; writeback is a hypothesis, not an established cause.
  Add a nonjournal framed observation replacement control if needed to separate
  codec/framing cost from append/replay cost.
- In that slice, profile loading, replay and publication before choosing one
  further optimization. Separate codec/allocation work
  from tree reconstruction, transaction validation and generation checking. Keep
  full preparation, sparse edits, dense edits, no changes and compaction as gates.
  The Btrfs sparse-edit result justifies investigation, not unconditional adoption.
- Then, only where those profiles support it: compare compact encoding, parallel
  semantic validation, one shared observation-change scan, or a cheaper exact
  generation check independently. Preserve every transaction's validation and
  stale-append rejection across equal-sized replacements. Indexed-access versus
  bulk-copy encoding and native-stamp/cache layout remain separate candidates.
- Before production: enforce filesystem eligibility, cache ownership/permissions
  and directory-handle lifetime, carry selection guards through freezing, and
  measure every actual caller. Avoid disk-cache work in already-retained watch
  sessions. Large-file rolling deltas/chunks remain roadmap step 4.

## Evidence and reproduction

Frozen Go/module inputs, candidate Go/module/Python inputs, their hashes, native
reports and validation logs are retained under
[`benchmarks/observation-journal`](benchmarks/observation-journal).
Each host's `raw-logs.tar.gz` contains individual probe and setup logs; `report.json`
contains every timed sample. `SHA256SUMS` covers all retained evidence files.

The archived candidate includes the exact harness used by these measurements.
`post-measurement-harness.patch` and `final-harness-inputs.json` preserve the first,
intermediate validator correction. The subsequent review found that correction
too permissive. `review-followup.patch` is the cumulative patch from the measured
candidate archive to the current Go tests and Python harness/analysis sources;
apply it directly to that archive, not on top of the intermediate patch.
`review-inputs.json` identifies those final sources. No non-test Go source changed
after measurement. The original archives, native reports and timings stay frozen.
Each host's `review-summary.json` identifies its input report and analysis sources.
New review validation lives under `review-validation/`.

Recompute either host's paired analysis into a new file without running tests or
benchmarks:

```sh
python3 scripts/summarize_observation_journal.py \
  docs/benchmarks/observation-journal/apfs/report.json --output /tmp/apfs-summary.json
```

```sh
python3 scripts/benchmark_observation_journal.py --output dist/observation-journal
```

Use an output directory on native APFS or Btrfs. On the measured Mac mini,
`DEVELOPER_DIR=/Library/Developer/CommandLineTools` selects the working installed
toolchain. The initial attempt exited during Python-test preflight when system
Git selected an unusable Xcode toolchain; it contains no timing samples.

`--smoke` runs the ordinary scenarios on 100 files, omitting the 50K byte-compaction
case. It validates harness mechanics only. Neither invocation enables a
production cache.
