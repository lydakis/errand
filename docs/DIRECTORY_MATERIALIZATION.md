# Directory materialization comparison

Baseline: `36d2f59` (shared verified initialization, including the permission fix).

## Decision

Keep the current production materializer. Directory batching regressed APFS deep
capture, both when sorting by directory and when preserving the incoming manifest
order. The measurement and diagnostic tooling is retained. Neither experimental
patch is applied to production, and no durability or verification contract is
relaxed. All four native campaigns and both hosts' diagnostic runs completed.

This closes a measurement question, not a claim of a new production speedup.
The next main roadmap step remains persisted incremental snapshot state.

## What changed in measurement

Capture now uses `testing.B.Loop`, synchronizes its fixture before timing, and
retains each captured tree until the sample ends. Every iteration gets a fresh
job directory whose containing directory is synchronized outside timing. Capture
still includes byte/metadata verification, native cloning or copying, member
synchronization and durable publication.

The previous benchmark deleted the captured tree between timed iterations.
Stopping the timer excludes deletion's synchronous cost, but cannot exclude its
subsequent filesystem writes from a later flush. Retaining outputs removes that
source of interference. No old/new cleanup experiment establishes how much of
the previous variance it caused; past timing tables should not be reinterpreted
as measurements under this new fixture policy.

Retention also increases the number of directory entries and clone references
within a sample. Per-sample means do not reveal a first-to-last iteration trend;
no stationarity or cleanup-policy speedup is claimed. A future precision campaign
can compare 1x/3x/7x retention and use even, position-balanced pairs.

The native driver runs `sync` between processes. This is not a cold-cache test or
a guarantee that the host has no unrelated I/O. Complete-operation benchmarks
retain their existing setup/cleanup policies and Go calibration behavior. Only
the capture benchmark's between-iteration cleanup has changed.

## Compared implementations

- **Current:** one file task per worker, with bounded retained source/destination
  parent handles and one destination-parent lease per file.
- **Sorted batches:** sort files by parent and schedule at most eight adjacent
  files under one destination-parent lease. Source checks still run per member.
- **Order-preserving batches:** the same batching without sorting. Only adjacent
  members of the same directory share a batch.

Here, incoming order means the manifest's canonical path order, not fixture
insertion order. The deep-chain manifest already groups files by parent, so both
patches form the same batches. The follow-up removes sorting and changes batch
dispatch order; it does not test different batch membership. Singleton-directory
fixtures do not exercise multi-file batching, but still change destination/source
open ordering and task bookkeeping. They are not identical-code noise controls.

Both candidates retain at most 16 parent handles per adapter, protect borrowed
handles from eviction, verify source bindings and bodies, and preserve permission
restoration and durability barriers. The existing Darwin batched full flush and
Linux fsync policy are unchanged. The experiment affects the shared writer used
by initialization and push/fetch staging; it does not replace source selection,
wire metadata, conflict handling, or transactional publication.

The fixed batch size also trades away some parallelism for small same-directory
sets. These two candidates do not exhaust directory-oriented designs, and their
rejection does not establish that every possible grouping strategy is slower.

## Native comparisons

Both variants receive byte-identical tests, fixtures and module definitions,
including the revised capture benchmark. The driver rejects mismatched inputs
and toolchains and records source, harness and binary hashes. Source identities
are checked again at the end. `GOMAXPROCS=2`, `CGO_ENABLED=0` for timing, and
`GOFLAGS=-p=1` for build/test execution. APFS ran on the local M1 Max on AC power;
Btrfs ran on Cabal. Neither is a cross-machine transfer or browser readiness test.

The first campaign uses seven alternating pairs with seven captures per sample.
Complete operations use three timed iterations per sample. The follow-up uses
five alternating capture-only pairs, again with seven iterations. All samples
are retained. Values below are medians of per-sample means in milliseconds;
paired ratio is the median of candidate/current ratios and need not equal the
ratio of medians. Brackets show the full minimum-to-maximum range of sample means,
not individual-operation tail latency. The iterations within one sample are not
independent pairs. Small differences on these noisy hosts are observations, not
established causal gains. No samples were discarded.

### Sorted batches: APFS

| Case | Current median [range] ms | Candidate median [range] ms | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 16.019 [15.812–16.925] | 17.189 [16.353–18.029] | 1.073 | 1/7 |
| capture-512-files | 107.988 [88.825–114.409] | 100.290 [96.335–108.076] | 0.980 | 4/7 |
| capture-512-directories | 155.380 [143.064–193.261] | 155.274 [146.369–431.264] | 1.023 | 3/7 |
| capture-32-deep | 215.301 [204.878–240.196] | 422.997 [394.017–473.170] | 2.024 | 0/7 |
| capture-8-deep-wide | 258.271 [250.172–287.324] | 248.402 [245.130–281.807] | 0.971 | 6/7 |
| workspace-create | 1072.630 [1012.217–1252.426] | 1050.120 [1007.598–1128.856] | 0.976 | 5/7 |
| ephemeral-job | 1327.834 [1300.548–1360.122] | 1326.635 [1294.370–1497.460] | 0.995 | 4/7 |
| fetch-false | 136.568 [130.512–156.193] | 141.063 [133.533–146.151] | 0.996 | 5/7 |
| fetch-true | 280.357 [267.772–346.050] | 273.862 [263.679–351.498] | 1.005 | 3/7 |
| watch | 441.650 [431.147–505.166] | 446.569 [432.183–448.279] | 1.007 | 3/7 |
| push | 795.467 [772.636–1042.065] | 810.503 [786.732–930.845] | 1.018 | 1/7 |

### Sorted batches: Btrfs

| Case | Current median [range] ms | Candidate median [range] ms | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 36.691 [34.810–68.864] | 37.837 [36.101–75.449] | 1.032 | 2/7 |
| capture-512-files | 259.232 [201.007–328.713] | 245.370 [198.708–324.342] | 0.876 | 5/7 |
| capture-512-directories | 446.377 [391.721–533.522] | 435.462 [386.075–610.659] | 0.928 | 4/7 |
| capture-32-deep | 298.317 [294.062–428.743] | 322.771 [301.670–530.030] | 1.011 | 2/7 |
| capture-8-deep-wide | 556.371 [492.001–684.616] | 579.771 [508.909–655.844] | 1.034 | 2/7 |
| workspace-create | 1696.461 [1519.946–2007.917] | 1734.777 [1535.401–1899.278] | 1.056 | 2/7 |
| ephemeral-job | 721.100 [699.564–806.447] | 708.189 [690.456–806.221] | 0.987 | 5/7 |
| fetch-false | 90.991 [84.262–115.308] | 89.889 [84.105–130.658] | 1.036 | 3/7 |
| fetch-true | 204.218 [193.440–279.287] | 201.777 [192.846–280.773] | 0.987 | 4/7 |
| watch | 373.316 [363.092–509.802] | 380.023 [357.282–515.372] | 1.030 | 2/7 |
| push | 778.388 [756.133–894.681] | 778.566 [745.712–812.972] | 0.971 | 5/7 |

The APFS deep-chain slowdown appears in all seven pairs. Other improvements are
small or workload-dependent; Btrfs workspace creation also has a 1.056 paired
ratio, slower in five of seven pairs. None of this supports a general command
speedup or latency equivalence. APFS's repeatable deep-chain regression is enough
to reject the sorted candidate without tuning away individual adverse samples.

### Incoming manifest order retained: APFS

| Case | Current median [range] ms | Candidate median [range] ms | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 16.149 [15.494–17.372] | 16.460 [15.702–16.693] | 1.010 | 2/5 |
| capture-512-files | 99.058 [91.294–121.201] | 93.723 [90.149–229.044] | 0.962 | 4/5 |
| capture-512-directories | 149.794 [136.659–161.972] | 151.449 [139.648–194.458] | 0.965 | 3/5 |
| capture-32-deep | 201.695 [186.356–205.678] | 442.080 [416.508–452.190] | 2.217 | 0/5 |
| capture-8-deep-wide | 255.357 [246.667–263.652] | 249.367 [241.746–293.719] | 0.981 | 4/5 |

The deep-chain slowdown remains in all five pairs. Sorting alone therefore does
not explain the first candidate's failure. This follow-up does not rerun complete
commands because the candidate already fails the capture adoption gate.

### Incoming manifest order retained: Btrfs

| Case | Current median [range] ms | Candidate median [range] ms | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 35.823 [35.515–47.841] | 35.879 [35.633–37.719] | 1.008 | 2/5 |
| capture-512-files | 250.898 [204.327–277.139] | 214.304 [198.745–412.452] | 0.854 | 4/5 |
| capture-512-directories | 430.278 [393.532–479.582] | 511.811 [462.846–553.206] | 1.204 | 1/5 |
| capture-32-deep | 315.232 [303.158–365.319] | 306.105 [298.234–442.293] | 0.946 | 3/5 |
| capture-8-deep-wide | 514.736 [482.516–599.344] | 615.284 [496.450–648.680] | 1.060 | 0/5 |

Btrfs's observed paired ratios are 14.6% lower for flat 512-file capture, 20.4%
higher for separate directories and 6.0% higher for wide/deep trees. The latter
is slower in all five pairs, but the ranges show substantial variation. These
mixed observations do not establish a Linux improvement or justify adopting
batching as an unconditional Linux-only strategy.

## Evidence about the APFS regression

The separate three-capture syscall profiles show aggregate delay in Go's
`os.rootOpenDir` increasing from 0.444 seconds to 5.11 seconds for sorted batches.
Native `Fclonefileat` delay rises much less, from 0.237 to 0.411 seconds. These
observations make repeated directory traversal a leading explanation for the
regression, without isolating its contribution to wall time. Aggregate mutex
delay stays substantial: 3.33 seconds for current scheduling and 3.57 seconds
for sorted batches. Most is associated
with the source adapter's parent lookup. That lookup holds its cache mutex across
directory open/stat/close operations; separating those calls from the lock is a
future hypothesis requiring its own correctness design and measurements.

Temporary Go overlays count actual parent-cache decisions without changing the
frozen sources. Across three APFS deep-chain captures:

| Adapter/action | Current | Sorted batches | Original-order batches |
|---|---:|---:|---:|
| Source full-path fallbacks per capture | 26–34 | 110–133 | 130–140 |
| Source retained-parent opens per capture | 46–102 | 124–131 | 110–127 |
| Destination fallback leases per capture | 38–55 | 26–28 | 28–29 |

A destination fallback lease covers one file in current scheduling, but eight
files in this deep-chain batching fixture. Fewer lease acquisitions therefore
conceal more files resolved from the original root. Destination leases last for
the entire batch. Its cache inherits leaf-first eviction even though destination
identity verification is disabled, so a borrowed leaf prevents ancestor eviction.

The source adapter has a separate cache and releases each lease when the file
is opened. Batches do not extend source leases across files. They spread workers
over more directories concurrently, which can increase pressure on the source
cache's leaf-first eviction. The observed fallback counts are consistent with
this explanation. Neither these counters nor whole-process profiles isolate
the causal latency share of either adapter.

These are instrumented diagnostic counts, not timing samples. Mutex and syscall
profiles cover whole benchmark processes, including untimed setup/cleanup;
concurrent delays can exceed elapsed time and must not be added as wall-time
components. Each profile is one three-capture process per variant/shape/campaign,
with mutex profiling and tracing enabled together. Baseline APFS `rootOpenDir`
delay also varies from 0.444 to 0.616 seconds across the two campaigns. The profile
point values are diagnostic observations, not replicated latency estimates.
On Btrfs, fsync accounts for about 84% of profiled syscall delay in
both sorted variants, including fixture synchronization. Lock delay is much
smaller there (0.290 versus 0.249 seconds), and the timing results remain mixed.

The Btrfs counters also show more source full-path fallbacks: 5–24 per capture
for current scheduling, 43–86 for sorted batches, and 46–52 for original-order
batches. Filesystem costs and worker interleaving differ between hosts; these
counts do not predict equal wall-time effects on APFS and Btrfs.

## Validation and retained evidence

Changes-package tests, race checks and vet passed for both variants on APFS and
Btrfs in the first campaign, and for the order-preserving comparisons on both
hosts. The local snapshot Python suite passed 11 tests. The counter overlay
compiled and exercised both deep shapes for all three variants on both hosts.
Full repository validation was already completed for `36d2f59`; no production
Go source changes are proposed by this slice.

Inputs, complete reports, validation logs, raw compact profiles and diagnostic
outputs are in `benchmarks/directory-traversal`. `inputs.tar.gz` reconstructs the
first comparison; `ordered-inputs.tar.gz` reconstructs the order-preserving one.
The experiment patches and measurement contract are in
[the experiment README](../experiments/materialization/README.md). Run the frozen candidate's
`scripts/benchmark_materialization.py` with both source paths and a new output
path outside both source trees. On APFS set
`DEVELOPER_DIR=/Library/Developer/CommandLineTools`.
The counter driver is `scripts/profile_materialization_paths.py`; its output
includes the exact instrumented source and original source identity.

The frozen drivers remain unchanged for exact historical reproduction. The
current drivers add early output/source overlap rejection, mount metadata and
accurate capture-only scope. The current diagnostic driver also records toolchain
and imported harness identities and requires the expected samples and complete
source/destination counters. Those post-review checks are not retroactively
attributed to the archived campaigns. Mount options were not captured in the old
reports; current machine state cannot recover the historical values.

Post-review validation covered 20 focused Python tests (materialization diagnostics,
snapshot helpers and campaign cleanup). A native APFS diagnostic run validated both
three-capture shapes and their six counter records each. A renamed-benchmark
reproduction now fails with its log and failed report retained and scratch removed.
This validates tooling behavior, not a new timing campaign; Btrfs performance
results remain the original archived runs.

Cabal first campaign: `cabal/01M2HNJW8E8D0F1R9FGYTXM0JN`.
Cabal follow-up: `cabal/01M2HQ5M12QJ5DY68V3NFZHBH6`.

## Roadmap disposition

The requested cleanup control, longer wide/deep comparisons and parent-cache
contention investigation are covered here. Keep the current materializer while
moving to persisted incremental state, then large-file body transfer.

The small review follow-ups belong in this measurement slice: diagnostic failure
checks and regression coverage, fuller provenance for future runs, patch-to-source
identity mapping, timing spread and a qualified account of the two adapters.

If directory-heavy capture remains a measured priority after step 3, compare two
separate hypotheses against current per-file scheduling: relax leaf-first eviction
only for the private destination cache, and reduce source-cache mutex time around
filesystem calls. Source identity checks remain enabled. Neither is an adopted
optimization or a reason to delay persisted incremental state. Preserve descriptor
bounds, borrowed-handle lifetimes and eviction-time verification; cover realistic
shallow/broad trees, small sets, deep/wide shapes, restricted permissions and
complete operations. Use replicated, separately collected profiles and retention
sensitivity measurements when attributing smaller effects. Do not revive either
rejected batching patch as an assumed improvement.
