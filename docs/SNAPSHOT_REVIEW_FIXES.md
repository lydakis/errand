# Snapshot review follow-up

## Scope and fixed decision

Keep the shared snapshot API and the retained Merkle candidate fixed while
removing verified redundant work. Memory footprint and line count do not decide
adoption. This follows the review of `9ee7cbd`; the earlier measurements remain
historical evidence, not results for this patch.

The candidate no longer exports the complete source manifest into delta push
recovery records. The changed bodies, delta and complete source identity already
provide retry/recovery inputs. Full uploads retain their complete manifest. A
regression drives the real staged-push path, checks that the record contains no
unchanged inventory, then resumes the same transfer and verifies delivery.

Inside the shared snapshot package, mixed-representation comparisons borrow an
immutable materialized view instead of calling the public cloning export API.
Small-array updates and the bounded tree fallback now share the same array edit
and changed-hierarchy validation path. Public exports still own their slices;
the height safeguard, source selection, merge bases and transactional apply
semantics are unchanged.

## Comparison contract

The frozen comparison contains three variants:

- **baseline:** production code from `9ee7cbd`.
- **candidate:** compact delta records and shared immutable-view optimizations,
  with the existing 4,096-entry Merkle cutoff.
- **flat-control:** the previous flat snapshot engine with the candidate's same
  caller changes, including compact delta records.

All variants receive identical benchmark drivers. Each frozen tree runs its
tests, vet, and race suites on each native runner before benchmarking. The
baseline retains its earlier tests; the new record-size regression intentionally
fails there and belongs to the candidate and flat control. Benchmarks use the
actual filesystem under the results directory, not a default temporary mount.

Seven rounds rotate variant order. Each leaf workload runs on all variants
before the next workload begins, reducing the time separating corresponding
observations. Raw samples, source/binary digests and validation logs are kept.
A subsequent review fixed the default two-variant schedule, where rotation and
reversal canceled each other. Regression coverage now checks alternating pairs
and all six three-variant permutations. The reported three-variant schedules
are unchanged; archived harnesses preserve the exact code used for those runs.
Latency tables use medians of per-round benchmark averages, not individual-edit
p50 or p95 latency. Fixture setup is excluded from the timed edit loops.
The summary uses order-statistic median intervals with at least 95% coverage,
assuming independent paired ratios. With seven pairs that interval is the full
observed range (98.4375% coverage); fewer than six pairs remain unclassified.
This intentionally avoids declaring a small noisy difference significant.
Small metadata workloads use duration-based runs rather than a fixed 100 edits.
Metadata probes use `context.Background()` to match current production callers;
the separate race and cancellation tests continue using cancellable contexts.

The full matrix includes ordinary push, complete watch, both fetch modes,
workspace creation, ephemeral submission, cold metadata, full reconciliation,
and retained metadata with explicit full export. That last probe deliberately
exercises the full wire API; it does not model the compact delta recovery record.
Additional complete watch cases cover small inventories, Git selection with
atomic saves, nested paths with atomic saves, and structural renames. Delivery
contents are verified in the command fixtures.

Before observing results, the target is a median complete-watch improvement of
at least 10% versus the committed baseline on each runner, supported by paired
uncertainty intervals below parity. A repeatable slowdown above 5% in another
operation fails the broader adoption gate. Candidate versus matched flat control
is a separate representation decision: indistinguishable results do not select
a winner. This is a native loopback comparison, not a new rsync/Mutagen or
cross-host network result.

The mini's original job reached its two-hour runtime limit during round six's
push comparison. Its completed leaves and validated native binaries were
retained. The continuation verifies source/binary hashes and the APFS fixture
filesystem, preserves complete leaves, and reruns the entire interrupted push
leaf so its three variants remain adjacent. It then completes the remaining
cases with a 90-minute script budget. The two discarded partial push samples
remain available for audit; they are not mixed with the rerun. The report records
the interruption and resumption, rather than presenting this as one continuous
job. [The exact continuation helper and interrupted report](benchmarks/2026-09-13-snapshot-resume-inputs.tar.gz)
are preserved.

## Native results: Cabal

Cabal completed all seven rounds on Linux amd64, Go 1.27.1 and Btrfs, with
GOMAXPROCS=2. Each variant has all 210 samples and its source digest matches the
frozen overlay. All three passed full tests, vet and the scoped race suites.

The predeclared complete-watch target **does not pass on Cabal**. Baseline and
candidate medians are 520.870 and 541.635 ms, but the median *paired* ratio is
1.000 with an interval of [0.945, 1.185]. This is inconclusive, not evidence of
an end-to-end improvement or a confirmed regression.

The diagnostic client-side component does improve in every pair: its median
falls from 107.600 to 87.970 ms, with a paired ratio of 0.812 and interval
[0.787, 0.899]. This component includes client work, watch delay and transport
overhead; it is not a CPU-only measurement. Candidate receiver staging and
apply medians are 215.700 and 223.800 ms. Their variation and larger share of
total time explain why the reduced client work does not establish a faster
complete watch cycle. Component comparisons are diagnostic, not a replacement
for the predeclared end-to-end gate.

The matched flat control's watch median is 538.157 ms. Candidate/flat paired
ratio is 1.024 with interval [0.922, 1.099], also inconclusive. These complete
watch results do not establish either representation as the faster engine.
The candidate is consistently faster in three metadata comparisons against
flat control: 10K flat expansion and the 10K retained one-edit and 100-edit
workloads with explicit export. That does not establish a universal tree win.

No comparison on Cabal has all paired ratios above 1.05. Several creation and
fetch medians are worse, with wide intervals that cross parity. This is an
absence of a confirmed regression under the stated gate, not proof that every
operation is unchanged. The complete native tables and raw observations below include those cases
rather than selecting only favorable measurements.

## Native results: Mac mini

The mini completed seven rounds on Darwin arm64, Go 1.27.1 and APFS with
GOMAXPROCS=2, using the recorded continuation for the final 35 paired leaves.
All 525 observations belonging to previously complete leaves are unchanged;
the two partial interrupted-push observations were replaced by the adjacent
three-variant rerun. Each variant has exactly 210 unique case/round samples,
and all source and retained-binary hashes match the validated inputs.

The complete-watch target **does not pass on the mini** either. Baseline and
candidate medians are 380.682 and 377.135 ms; the median paired ratio is 0.988
with interval [0.871, 1.067]. The client-side component's paired ratio is 0.930,
but its interval [0.793, 1.017] crosses parity. Its lower point estimate is not
a confirmed improvement under the stated method.

The matched flat control's watch median is 397.546 ms. Candidate/flat paired
ratio is 0.987 with interval [0.918, 1.049], again inconclusive. This comparison
does not establish a complete-watch representation winner.

Two metadata results need explicit disclosure:

- Against the committed baseline, 10K nested preparation is slower in all seven
  pairs: 7.581 to 7.825 ms, paired ratio 1.029, interval [1.011, 1.081]. This is
  a consistent small slowdown; its magnitude is not established above 5%.
- Against matched flat control, 10K flat expansion is slower in all seven pairs:
  6.247 to 6.517 ms, paired ratio 1.096, interval [1.022, 1.217]. This is a
  representation-specific disadvantage on the mini, despite the corresponding
  Cabal result favoring the candidate. It is not a demonstrated regression
  against the committed baseline.

No comparison's interval is wholly above 1.05 on either host. That does not mean
zero regressions, and several other point estimates remain uncertain.

## Complete-command tables

[Full results, samples, source/binary hashes and validation logs](benchmarks/2026-09-13-snapshot-review-fixes.json)
are preserved. Times are milliseconds. Ratios are medians of paired ratios,
which can differ from the ratio of the displayed medians. Below 1 favors the
patch. Every complete-command comparison in these tables is inconclusive under
the stated interval rule, including those with favorable point estimates.

### Cabal / Btrfs

| Workload | Committed | Patch | Matched flat | Patch / committed | Patch / flat |
|---|---:|---:|---:|---:|---:|
| Watch, 10K | 520.9 | 541.6 | 538.2 | 1.000 | 1.024 |
| Watch, 1K | 255.8 | 243.4 | 241.2 | 0.912 | 1.015 |
| Watch, Git atomic save | 1106.9 | 1074.6 | 1082.4 | 0.836 | 0.982 |
| Watch, nested atomic save | 719.8 | 725.3 | 709.0 | 0.972 | 0.983 |
| Watch, rename | 653.3 | 589.4 | 589.6 | 0.944 | 1.023 |
| Push | 981.6 | 920.8 | 970.0 | 0.927 | 0.974 |
| Ephemeral fetch | 101.0 | 114.3 | 90.2 | 1.065 | 1.220 |
| Persistent fetch | 244.5 | 293.2 | 244.7 | 1.199 | 1.029 |
| Workspace creation | 1745.7 | 2072.0 | 1721.4 | 1.019 | 1.199 |
| Ephemeral submission | 747.0 | 825.2 | 884.5 | 1.025 | 0.958 |

### Mac mini / APFS

| Workload | Committed | Patch | Matched flat | Patch / committed | Patch / flat |
|---|---:|---:|---:|---:|---:|
| Watch, 10K | 380.7 | 377.1 | 397.5 | 0.988 | 0.987 |
| Watch, 1K | 314.0 | 321.4 | 309.1 | 0.999 | 1.015 |
| Watch, Git atomic save | 989.5 | 975.3 | 1078.4 | 0.994 | 0.883 |
| Watch, nested atomic save | 517.5 | 477.1 | 484.2 | 0.922 | 1.008 |
| Watch, rename | 489.6 | 479.5 | 465.4 | 0.947 | 0.989 |
| Push | 541.8 | 538.8 | 541.5 | 0.948 | 0.968 |
| Ephemeral fetch | 107.5 | 111.8 | 110.3 | 1.023 | 1.006 |
| Persistent fetch | 232.5 | 233.2 | 237.4 | 0.996 | 0.994 |
| Workspace creation | 2348.2 | 1982.8 | 2039.0 | 0.866 | 1.148 |
| Ephemeral submission | 1525.6 | 1407.1 | 1920.5 | 0.999 | 0.850 |

## Assessment

Keep the compact recovery records and internal immutable-view changes as the
review follow-up: the regression proves resumability without the unchanged
inventory, the shared snapshot invariants pass, and Cabal shows a consistent
client-side reduction. Do not describe this patch as meeting the overall watch
speed target. It does not, and neither host establishes a complete-command win.

Keep the selected retained Merkle design and 4,096-entry cutoff for this patch.
The matched control exposes mixed metadata tradeoffs and no complete-watch
winner; it does not justify another architecture switch. This is not a claim
that the tree is universally fastest. The receiver profile gives the next
performance investigation a concrete target without changing the current
crash-recovery or conflict contract.

## Considered work

Internal-copy reduction and broader workloads are included because they remove
known work and improve the relevance of the measurement. A separate isolated
priority-hash experiment replaces only path-priority SHA-256 with a process-local
keyed hash. It keeps SHA-256 content and wire identities, plus the height bound.
Its local CPU results cannot establish a native-runner or end-to-end improvement.

Checkpoint consolidation remains separate: source acceptance and receiver
conflict outcomes have different inputs. The follow-up profile below now gives
checkpoint reuse a concrete performance reason to investigate. That strengthens
its priority for a subsequent bounded change; it does not justify removing
conflict/refusal semantics or durability requirements.

## Receiver diagnostic

After Cabal completed the comparison, an isolated copy of the frozen candidate
captured CPU and execution-trace profiles over 20 warm watch cycles. Profiling
starts after initial watch warm-up and stops before daemon/fixture cleanup. The
mini's comparison continued on its separate machine. The diagnostic changes only
the benchmark driver; production code remains frozen.

The [profile report](benchmarks/2026-09-13-watch-receiver-profile.json) and
[raw profiles plus exact overlays](benchmarks/2026-09-13-watch-receiver-profile.tar.gz)
show:

- `fsync` accounts for 5.453 seconds, or 87.54% of the 6.230 seconds of recorded
  syscall delay. Stacks include transfer records, source/staging synchronization,
  the apply journal and apply-root directory synchronization.
- Of 8.630 seconds of CPU samples, `TransferCheckpoint.read` accounts for 1.780
  seconds cumulatively and `Manifest.RootHash` for 1.620 seconds cumulatively.
  These cumulative stacks overlap and must not be added together.
- The instrumented watch loop takes 603 ms/cycle. Profiling adds overhead, so
  that number is excluded from the paired performance comparison.

These are aggregate profiler observations, not additive wall-clock percentages.
They identify receiver checkpoint reuse and durable publication as better next
investigation targets than another tree-priority change. A future optimization
must distinguish redundant work from required durability barriers and retain
the existing refusal, conflict and crash-recovery behavior. This patch removes
no barriers and changes no receiver checkpoint contract.

## Priority-hash experiment

The [isolated local probe](benchmarks/2026-09-13-priority-hash-probe.json) uses
seven adjacent pairs on an M1 Max with GOMAXPROCS=2. Both variants pass the
manifest/changes race suites, including the height-fallback fixture adapted to
the experimental priority function. [Exact overlays](benchmarks/2026-09-13-priority-hash-inputs.tar.gz)
are retained for reproduction.

| Workload | SHA priorities (ms) | Keyed priorities (ms) | Median paired ratio | Keyed wins |
|---|---:|---:|---:|---:|
| Cold preparation + identity, 10K | 7.013 | 6.407 | 0.916 | 7/7 |
| Cold preparation + identity, 100K | 66.984 | 62.691 | 0.943 | 7/7 |
| Retained 10K, 1 edit + export | 6.363 | 6.344 | 0.997 | 4/7 |
| Retained 10K, 100 edits + export | 6.849 | 6.842 | 0.996 | 4/7 |
| Retained 100K, 1 edit + export | 62.595 | 61.530 | 0.988 | 5/7 |
| Retained 100K, 100 edits + export | 61.419 | 63.054 | 1.028 | 1/7 |

Do not adopt keyed priorities in this patch. The cold CPU gain is consistent in
this local probe, but it does not establish a complete-operation win and retained
batch behavior is mixed. The production tree and 4,096-entry cutoff remain fixed
for the native runner comparison. Further node-hash changes are not inferred to
be faster from this experiment.

## Reproduction

[Source overlays](benchmarks/2026-09-13-snapshot-review-fix-inputs.tar.gz) include
all three frozen variants, their source hashes and the exact benchmark harness.
Reconstruct each over the recorded parent, remove its listed deleted files, then
apply its overlay. Each reconstructed source digest was checked before reporting.
Run from the reconstructed candidate:

```sh
python3 scripts/benchmark_snapshot_integration.py \
  --baseline ../baseline --flat-control ../flat-control \
  --output ../results --revision snapshot-review-fixes-9ee7cbd \
  --rounds 7 --scope full
```

Use `scripts/summarize_snapshot_benchmarks.py --host NAME=RESULTS --output FILE`
to preserve raw logs and compute paired comparisons. The summary's tests cover
round matching, insufficient sample counts and uncertain regression decisions.
