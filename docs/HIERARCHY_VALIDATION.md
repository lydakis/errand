# Shared validation for hierarchy-preserving updates

This slice evaluates one shared snapshot optimization against `dd9fb846bd60`.
It does not adopt journal persistence or change the selected snapshot engine,
selection authority, transfer protocol, transaction boundaries or fsync policy.

## Safety condition

A `manifest.Snapshot` already has a validated hierarchy. Replacing metadata at
existing paths without changing any entry's type cannot introduce a file above a
descendant or a descendant beneath a file. `Snapshot.Update` therefore omits the
second parent/descendant walk only when **every** edit:

- addresses an existing path;
- is a replacement, not a deletion;
- preserves the entry type.

All edit paths and duplicates are checked. Replacement entries still receive
metadata, symlink-containment and byte-total validation.
Cancellation checks and immutable ownership remain intact. Insertions, deletions
and type changes retain the existing hierarchy walk. Membership preservation is
not sufficient: the existing `structural` flag also permits type changes.
Each journal transaction still calls Update independently, so a later valid frame
cannot hide an invalid intermediate hierarchy.

Type changes are recorded during the existing lookup pass and combined with the
existing membership-change flag after that pass. Both flat updates and
tree updates consume the same predicate, including the height-limited fallback.
There is no caller flag that can assert that an unverified batch is safe.

## Scope and measurement

The shared callers include retained watch preparation and source checkpoint
reconciliation. The experiment's observation updates and journal replay also
use this method. Fresh workspace/job initialization can remain unchanged because
it need not update an existing snapshot. Benefits must be measured per operation.

The comparison runs on native APFS/Mac mini and Btrfs/Cabal with GOMAXPROCS=2.
Both builds receive the same new tests and benchmarks; baseline production code
comes from the retained `dd9fb84` archive. The original candidate changes only
Update. The review follow-up also exposes archive.ValidateEntry to share existing
path/metadata checks without repeating replacement-subset hierarchy validation.
Each host validates both builds with native tests, race checks and vet before
measuring. Exact source inputs, toolchains, binary hashes and raw logs are retained.

- Six alternating build pairs measure 100-entry update batches in 1K flat inventories and 10K indexed trees:
  repeated paths, dispersed paths, and mixed insertion/deletion batches. Mixed
  batches deliberately do not qualify for the fast path.
  Type-changing batches have correctness coverage but are not separately timed
  in this matrix; do not infer their performance from the insertion/deletion case.
- Existing command benchmarks measure push, watch, structural watch, watch bursts,
  both fetch modes, persistent workspace creation and ephemeral submission.
  These use loopback HTTP; they do not measure cross-host network delivery.
- Fresh-process preparation with warm filesystem caches compares all three storage modes on a 10K-file tree,
  at history depths 0/8/31, with repeated and dispersed edits. Thirty-six rounds
  fully cross six depth orders with six storage-mode orders. Baseline/candidate
  order alternates within each mode. Source and restored cache bytes match within
  every pair, and both builds must return identical roots and expected work.
- Six pairs of 50K-file near-full append and byte compaction retain the dense
  history that previously rejected journal adoption. These scenarios remain
  separate from the sparse-history comparisons and from command benchmarks.

Filesystem enumeration, stat checks, serialization and other unchanged work are
included in complete preparation and command measurements. A reduction in Update
cost alone does not establish an end-to-end speedup.

## Reproduction

```sh
python3 scripts/benchmark_hierarchy_validation.py --output dist/hierarchy-validation
```

On Mac mini, set `DEVELOPER_DIR=/Library/Developer/CommandLineTools`. `--smoke`
uses a 100-file journal fixture and abbreviated rounds for functional validation only;
command fixture sizes stay unchanged.
The full campaign uses 36 crossed rounds; tests assert joint coverage, rather
than just marginal order balance. Dispersed history tests verify distinct paths
across multiple directories.

`--focused` runs native preflight, the complete command matrix and dense history
cases, omitting the sparse crossed matrix. It is mutually exclusive with smoke.

## Investigation and version boundaries

The original predicate candidate's final native results are retained in
[FINAL.md](benchmarks/hierarchy-validation/FINAL.md). Its initial campaign,
predicate diagnostics, and the subsequent entry-validation comparisons each
describe separate source versions. They are not pooled or substituted for the
selected source's caller controls.

The [initial complete comparison](benchmarks/hierarchy-validation/RESULTS.md)
used a cumulative `preservesHierarchy` predicate inside the lookup loop.
Replacement batches improved on both hosts. The APFS dense journal preparation
cases improved by about 7%, but ordinary command results did not establish a
latency improvement. On Btrfs, mixed 1K updates had a 1.138 median paired ratio
and ephemeral fetch had a 1.162 ratio. Those losses remain in the evidence.

The [focused controls](benchmarks/hierarchy-validation/CONTROLS.md) investigate
those signals rather than removing slower samples:

- Twelve crossed A/A/B rounds execute the same baseline binary twice. Fetch's
  original slowdown does not reproduce; candidate ratios are 0.975 and 0.941
  against the two baseline executions. Mixed 1K updates remain slower, with
  ratios 1.059 and 1.030. These microbenchmarks use a 1s budget, compared with
  300ms in the initial run. Fetch keeps its original 5-iteration budget.
- An isolated deferred type-check pass restores the original loop for structural
  edits. At 300ms, mixed 1K updates have a 1.023 ratio, but qualifying dispersed
  updates have a 0.691 ratio versus 0.599 for the initial candidate. The extra
  lookup pass gives up part of the useful gain, so this alternative is rejected.
- Recording only type mismatches in the existing loop avoids that extra pass.
  The structural flag already handles insertion/deletion. In a separate crossed
  300ms comparison, this version's dispersed 1K ratios are 0.595 on APFS and 0.574
  on Btrfs, winning all 12 pairs on each host. Mixed 1K ratios are 1.003 and 1.019;
  mixed 10K ratios are 1.000 and 0.984. The Btrfs pair ranges remain wide. This is
  the version selected for final caller/dense-history validation.

Compiler diagnostics disprove capture-by-reference as the explanation: the
original predicate is captured by value. The final mismatch flag avoids the
rejected second lookup pass. Direct
final-versus-initial comparisons do not establish that it is cheaper than the
initial predicate: mixed 1K ratios are 1.006 on APFS and 1.021 on Btrfs, with
4/12 and 6/12 wins. It was selected for its direct expression of the safety
condition, not a demonstrated improvement over the initial predicate. These
experiments do not establish a machine-code cause for the Linux timing difference.
The final run measures the original predicate candidate; entry-validation evidence
is recorded separately below.

## Original predicate candidate: final native results

### APFS

The four qualifying 100-entry update cases have paired ratios of 0.546–0.768,
winning every pair. Mixed 1K/10K updates have ratios 1.000/0.999. Most command
ratios are close to parity: push 0.996, fetch 0.994/1.003, workspace creation
1.002 and ephemeral submission 0.997. Watch is 0.980, but its 0.785–1.007 range
does not establish a consistent gain. Watch bursts are 1.038, with a
0.907–1.071 range and only two wins of six. The original run also had a slower
burst median (1.032); retain this historical signal alongside the selected-source
caller controls below.

Dense journal append/compaction ratios are 0.917/0.942, winning all six pairs
each. Journal load falls from about 120 ms to 90 ms in both cases, and compaction
index work from 37 ms to 16 ms. However, the unchanged replacement controls also
have lower complete compaction medians (ratios 0.969 and 0.972), so the entire
6% complete compaction reduction cannot be attributed confidently to validation.
The final journal remains slower than checkpoint replacement: within-build
paired J/R ratios are 1.189 for append and 1.114 for compaction. This still does
not support journal adoption.

The scale matters: the APFS dispersed 1K update benchmark falls from roughly
0.071 ms to 0.042 ms per 100-entry batch, while complete watch is about 310 ms.
These are different workloads, not additive phase measurements, but they show
why a large relative metadata gain need not produce a visible command gain.
This change removes redundant hierarchy work; it does not remove filesystem
selection, stat calls, body capture or publication. Repeated journal replay
accumulates enough update work to show a larger load-phase reduction.

### Btrfs and decision

The four qualifying update cases have paired ratios of 0.615–0.828, winning
every pair. Mixed 1K/10K updates are 0.939/0.946, with four wins of six each;
the original mixed-update slowdown does not recur in this final comparison.
Push, watch, watch bursts, structural watch and workspace creation have ratios
0.997, 0.988, 1.009, 0.980 and 0.973. Ephemeral submission is 1.035. These
results do not establish a general complete-command improvement.

Fetch was an unresolved signal in this campaign: ephemeral/persistent ratios are 1.133/1.074,
with ranges 0.699–2.534 and 0.791–1.627, and two wins of six each. The earlier
initial-candidate A/A/B check did not reproduce its fetch slowdown, but that
does not clear this final version. Keep these samples and the APFS burst result
visible. They motivated the selected-source A/A/B comparison below, with separate
phase diagnostics. Do not pool different source versions or timing campaigns
to make them disappear, or infer a particular cause from these aggregate timings.

Dense journal append/compaction ratios are 0.904/0.923, winning five of six
append pairs and all six compaction pairs.
Load falls from roughly 471/456 ms to 403/389 ms, and compaction index work from
104 ms to 79 ms. Unchanged controls also move: append checkpoint is 0.945 and
compaction replacement is 0.941. As on APFS, these are complete measured effects,
not an assertion that validation explains all of the difference. Final J/R
ratios remain 1.357/1.229, so the journal still fails the adoption comparison.

**Retain the shared predicate optimization for review.** The safety condition
is explicit, correctness checks pass, and all 48 final qualifying-update pairs
improve across the two hosts. The demonstrated benefit is less shared metadata
work and faster dense journal preparation. Ordinary command latency remains
unproven, with the caller-level caveats above. This is not a release-wide
performance or regression-free claim, and it does not enable journal storage.

## Safety and remaining work

Both original predicate final runs pass ordinary tests, race checks and vet for manifest,
snapshot, changes, archive, client, daemon, the CLI and the checkpoint experiment,
plus all 73 Python tests. Baseline and candidate receive identical test inputs.

The new regression compares incremental updates with a fully validated manifest
for flat and indexed representations, and the preselected height-limited flat
fallback. It does not force a mid-update height transition. It covers metadata
replacement, a changed parent above an untouched descendant, insertion beneath a
file, valid structural batches, negative sizes and escaping symlink targets.
The new cases also cover a single indexed metadata edit and an accepted type-only
change. Existing suites cover cancellation, overflow, immutable ownership, randomized
hierarchy changes, journal corruption and recovery. Journal replay still validates
each frame through Update; this optimization does not fuse transactions.

Included considered items: rotating/dispersed history, the fully crossed depth/
mode schedule with joint assertions, structural controls, the explicit type
predicate, production callers, separate uninstrumented timing and negative
controls for suspicious regressions.

The selected-source caller controls below address the fetch and watch-burst
signals without establishing universal non-regression. The
[active queue](SNAPSHOT_ENGINE_PLAN.md#current-work-queue-2026-09-16) now prioritizes
production apply-journal scaling and realistic watch workloads. The deferred storage experiment compares
bounded cache read/checksum work while retaining exact-generation
identity. Keep replay-aware compaction and early encode termination as a separate
candidate, measuring cumulative publication cost as well as load cost. Compact
encoding, shared observation-change scanning and filesystem selection remain
later profiling candidates. Cache eligibility, ownership, directory-handle
binding, invalidation/recovery and actual caller validation remain prerequisites
for persistence adoption. Large-file deltas remain roadmap step 4.

This slice does not enable persistent journal storage or change the selected
in-memory representation. Faster hierarchy validation alone is not an adoption
argument for the journal, nor evidence that fresh workspace creation is faster.

## Review follow-up and isolated alternatives

The comparison driver now pins the baseline archive digest and compares production
inventories, accepting only the declared source differences. It retains the exact
production patch with old/new hashes. Test and fixture equality remains separate.
Both report renderers check candidate archive identity, and integrity checks now
raise errors even under optimized Python. Dense Load/Index phase tables are
generated directly from the same checked samples as the total-cost tables.

[Retained caller diagnostics](benchmarks/hierarchy-validation/CALLER_DIAGNOSTICS.md)
show every timed fetch iteration and the burst phase metrics. No outliers are
removed. The worst Btrfs ephemeral-fetch candidate pair includes a 711 ms
iteration; the other four are 84–172 ms. Calibration is excluded explicitly.

Caller reachability matters: FetchChanges uses applyChangeBundle or
applyWorkspaceJob → PrepareTransferSource/TransferSession.Apply. Neither calls
Snapshot.Update. Ordinary push and workspace/job creation prepare fresh state.
These are negative controls for this specific runtime change, still useful for
observing whole-binary or environmental effects. Retained WatchPush uses
PrepareSnapshot → Builder.update → Snapshot.Update. Its burst workload performs
400 writes separated by 5 ms and includes resampling, publication and convergence.
The final APFS burst ratios span client, staging and apply work; the data does
not isolate validation as their cause. Reachability alone also does not prove
whole-binary non-regression.

The follow-up isolates allocation and entry-only validation in scratch candidates,
then measures the selected source against two executions of the same baseline
binary, fully crossing all six orders. Behavioral checks and measured boundary
results determine which alternatives advance to caller controls.

The first isolated comparison improves replacement/mixed update batches on both
hosts: preallocation ratios are 0.940–0.995 on APFS and 0.859–0.992 on Btrfs;
preallocation plus entry-only validation gives 0.843–0.937 and 0.782–0.948.
These compare against the original predicate candidate, not the published
baseline. [ALTERNATIVES.md](benchmarks/hierarchy-validation/ALTERNATIVES.md)
retains the complete ratios, pair ranges, allocation counts and win counts.
The subsequent deletion-heavy controls evaluate entry-only validation independently
before selecting the source for final command controls. Memory allocation savings
alone are not the adoption criterion.

### Selected follow-up candidate

[BOUNDARIES.md](benchmarks/hierarchy-validation/BOUNDARIES.md) compares the two
ideas independently across the original six cases plus single-edit,
delete-only and delete-heavy boundaries. The entry-only variant is selected for
final caller validation. Its six APFS batch ratios are 0.884–0.951; single-update
ratios are 0.964/0.907 and deletion-only ratios 1.009/1.002. On Btrfs the four
qualifying batches improve (0.876–0.976), but mixed 1K is noisy at 1.043,
5/12 wins, range 0.726–1.414. The final A/A/B includes this case.

Preallocation is not retained. APFS 1K deletion-only is 1.049, and Btrfs single
indexed updates are 1.084 (3/12 wins). Allocated bytes increase on deletion batches.
The full reports retain these losses alongside the replacement-case wins.

The selected implementation validates each replacement through the existing
archive path/type/metadata/symlink checks. Update already sorts and rejects
duplicate edit paths. Structural and type-changing batches still validate the
resulting hierarchy against the complete updated snapshot; unchanged membership
and types reuse the base's validated hierarchy. This removes the earlier,
redundant hierarchy walk over just the replacement subset. The immutable tree,
transaction boundaries, fsync policy and caller protocol remain the same.
The two production files exactly match the measured entry-only variant on both
hosts; no unmeasured allocation tweak is bundled into the final controls.

### Selected source: final caller controls

The final A/A/B comparison uses 12 rounds, all six orders twice, on each host.
Baseline-a and baseline-b execute the same binary from `dd9fb84`; the candidate
includes both the hierarchy predicate and entry-only validation. The timing
budget is 300 ms for update microbenchmarks and five timed iterations for each
command sample. Tests, race checks and vet pass before timing. These results
belong to this source version and are not pooled with earlier campaigns.

On APFS, dispersed 1K updates have paired ratios 0.529/0.528 against the two
baseline executions, winning all 12 pairs in both comparisons. Mixed updates
are 0.940/0.936, also winning every pair. Absolute medians are approximately
71 → 37 microseconds per dispersed batch and 71 → 67 microseconds per mixed batch.

Ordinary watch remains at parity (1.000/0.999). Burst medians are lower
(0.947/0.978, eight wins of 12), so the earlier slower burst median does not
repeat with the selected source. Settle-time ratios remain 1.007/0.983, however,
and burst ranges reach 1.13. The data does not establish a reliable convergence
improvement or identify the cause of the earlier slowdown. APFS fetch ratios
are 1.006/1.003 for ephemeral and 1.013/1.008 for persistent workspaces. Retain
those small positive medians; this is not an assertion of zero overhead.

On Btrfs, dispersed 1K updates have ratios 0.616/0.529, winning every pair
against both baseline executions. Absolute medians are 275/305 → 163 microseconds.
Identical-baseline timings vary substantially (B/A 1.118, range 0.905–1.395),
so preserve both comparisons instead of choosing the larger speedup. Mixed
updates are 0.996/1.017, with six/five wins of 12. The earlier mixed-update
slowdown does not consistently recur, but these data do not prove strict
non-inferiority.

Btrfs watch is 1.040/1.005, with broad ranges also present in A/A. Burst total
time is 1.004/1.007 and settle time 0.999/1.025. Its staging phase remains higher
(1.047/1.048), and apply is 1.030/1.152, while the identical-baseline apply ratio
is 0.924. Keep those phase signals visible; the experiment does not identify
their cause, and total latency does not show a comparable increase. Resample
medians remain eight per burst across all three labels.

Ephemeral fetch is 0.940/1.049 and persistent fetch 0.991/0.934. The original
slower medians do not repeat consistently against both controls. A/A itself
is 0.905 for ephemeral fetch and 1.063 for persistent fetch, with wide ranges.
This supports treating the original attribution as unresolved rather than
claiming that hierarchy validation either caused or fixed a fetch regression.
Observed one-minute host load spans 1.35–9.72 on Btrfs and 2.50–4.22 on APFS;
the hosts were not guaranteed idle. Load observations alone are not a causal
explanation.

[CALLER_CONTROLS.md](benchmarks/hierarchy-validation/CALLER_CONTROLS.md) retains
every paired comparison and phase metric. The
[iteration diagnostics](benchmarks/hierarchy-validation/FOLLOWUP_CALLER_DIAGNOSTICS.md)
retain all 720 timed fetch iterations across the two hosts, excluding calibration.
[followup-verification.json](benchmarks/hierarchy-validation/followup-verification.json)
records the checked source identities and validation scope. All 1,728 follow-up
samples are retained; the archived Go, module and Python inputs match the working
files exactly. The 76 local Python tests pass. Both native caller campaigns pass
ordinary tests and vet for the affected packages, client, daemon, CLI and experiment;
race checks cover manifest, archive, snapshot, changes and the experiment.

**Retain the hierarchy predicate and entry-only validation for review.** The
repeatable benefit is less shared snapshot-update work. Complete command latency
remains approximately unchanged or noisy, with the residual medians and phase
signals above. The controls do not establish a general watch/fetch speedup or a
universal non-regression guarantee. Preallocation remains rejected, and journal
persistence remains unadopted.
