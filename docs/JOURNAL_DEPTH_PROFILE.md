# Journal depth and preparation profiles

This bounded follow-up to [the observation journal comparison](OBSERVATION_JOURNAL.md)
measures fixed histories, a complete publication cycle and separate diagnostic
profiles. It does not enable a production cache or change the selected snapshot
representation, transfer protocol or durability policy.

## Decision

**Do not adopt the current journal in production.** Retain the experiment and its
controls. A complete repeated-file cycle has paired medians lower by 1.7% on APFS
(unresolved across pairs) and 5.9% on Btrfs, but near-full append is 29.9% and 47.3% slower respectively. Byte
compaction is also consistently slower. Small journal writes are not sufficient
for a general preparation win.

The next optimization is **not selected yet**. The original combined CPU/heap
profiles identify candidates but distort relative allocation-heavy CPU costs.
The review follow-up separates those profiles before ranking hierarchy validation,
bounded reads/checksums and replay-aware compaction. The selected in-memory
representation and production caller paths remain unchanged.

## Questions and controls

The earlier ordinary edit/batch comparison pooled one sequence at journal depths
0–5. This experiment asks whether replay cost grows materially at deeper history,
whether append savings survive compaction, and which work accounts for the total.

Three modes run in the same binary:

- **R: `checkpoint`**, the existing observation replacement strategy.
- **F: `observation-replacement`**, the same framed observation format and reader
  as the journal, replacing the base on every change. It stores no tree image.
- **J: `observation-journal`**, the existing bounded observation-only journal.

R separates the user-facing preparation cost of the existing control. F separates
format/reader overhead from retaining and replaying transactions. There is no
proposed optimization to J in this slice. The baseline is commit
`9156978a9138e7dbb41eeb67138e33b04160d9ba`; exact experimental inputs and toolchain
identities are archived with each campaign.

## Protocol

Use native APFS on Mac mini and Btrfs on Cabal, `GOMAXPROCS=2`, fresh processes and
warm OS caches. Fixtures are 50K files of 128 bytes with `.errandignore`, and a
Git-selected 10K-file tree with 4-KiB bodies. Total preparation includes selection,
load, hashing/stat work, index preparation, verification, publication and wire
hashing. Process startup, fixture changes, cache reset/copy, `sync`, JSON output
and profile serialization are outside the comparison timer.

Fixed-depth trials separately prepare histories of **0, 8 and 31** transactions.
Every sparse history record rewrites the same first file, and timed one-file
edits rewrite that file again. This measures repeated-file history, not dispersed
edits across directories. At each depth, unchanged, one-file edit and 1,000-file
batch cases start from copies of that same prepared cache. Workloads run unchanged, edit,
then batch so changed source paths only expand; every timed edit rewrites all of
its declared paths. Both depth order and variant order cover all six permutations
across six rounds, using different permutation sequences. Each depth/workload
receives every variant order. This is conditional balance, not independence or a
full 36-way cross-product. Both orders derive from the same round number. At depth
0, R position equals depth-block position; at depth 8, J does; at depth 31, F does.
Interactions between block and variant position are therefore not isolated. The
historical driver retains this schedule to reproduce the frozen results. The next
comparison must cross all six depth orders with all six variant orders and test
joint coverage, rather than interpreting marginal coverage as independence.

The 50K fixture additionally measures:

- Six full cycles, each starting with an untimed complete base and timing **32
  appends plus the next edit's compaction**. Every step receives all six variant
  orders across cycles. Cycle totals include all 33 preparations and publication
  bytes; initial population and between-step mutation/drain are excluded. All 33
  edits rewrite the same first file, favoring locality and observation coalescing.
- A near-byte-limit history with two real dense transactions occupying 85–97% of
  the 8-MiB journal budget. Time a sparse append that still fits and a separate
  30K-file edit that forces byte compaction, each from the same prepared history.
  Calibration counts and actual history sizes are retained in setup probe logs.

Before each timing group, drain preceding setup writes with `sync`. Every group
requires matching wire roots across controls, exact hash/reuse work, successful
cache reuse/publication, and expected history and compaction. Ordinary publication
also checks independent frame counts and actual file growth. The journal's full-cycle receipt
rejects incomplete, reordered or missing compaction steps.

## Separate interference screen

Repeat identical unchanged R preparation after three treatments: no writer, a
64-MiB buffered write outside the selected source immediately before preparation,
and that same write followed by `fsync` and `sync`. Drain prior activity before
each treatment, and balance all six execution orders. Every sample must hash zero
files and retain the same root. These are diagnostic treatments, not journal
speedup samples. The screen can implicate preceding writes, but cannot by itself
identify kernel writeback as the mechanism or control unrelated machine activity.

The primary screen writes repeating bytes. A separate follow-up uses a fixed-seed
4-MiB pseudorandom block repeated sixteen times, retaining its SHA-256. It matters
on compressed filesystems: 64 MiB of repeating bytes does not imply 64 MiB of
physical I/O. The follow-up retains the same three treatments, fixture sizes and
six balanced orders. It runs after the primary campaign and cannot be treated as
an additional set of matched primary rounds.

## Profiles

In the original campaign, only after all uninstrumented comparisons finished,
CPU and allocation profiles were collected together for 50K repeated-file edits at
depths 0 and 31, near-full sparse append and byte compaction. Three fresh-process
repetitions per mode/case restore the prepared cache before each invocation. These
original profiles enable both instruments in one process. Profiled totals are
retained separately and excluded from timing summaries. CPU profiling stops before the forced GC and
heap-profile serialization. Allocation sampling uses a 64-KiB interval and reports
allocated space, not just live heap. Function CPU samples describe on-CPU work;
they do not explain off-CPU waiting or independently prove a syscall bottleneck.
The 64-KiB allocation sampling also adds CPU work unevenly to allocating paths,
so the combined profiles cannot rank competing optimizations reliably.

The review follow-up uses `--profiles-only` to collect CPU-only and heap-only
profiles in separate fresh processes, retaining the same four scenarios, three
repetitions and three variants. CPU-only runs keep the runtime's normal allocation
sampling rate. Every invocation restores its template, rewrites its declared
paths and checks the same roots, hash counts and history receipts. Results remain
diagnostics, never additional uninstrumented timing pairs. Raw original profiles
remain frozen; new profiles and their measured inputs have their own directory.

The hypotheses are decode/allocation cost, per-transaction replay cost, exact
cache-generation reads/checksums, and selection variation from preceding work.
Use complete preparation and full-cycle totals to judge whether reducing one of
those costs is worth a separate candidate. Phase movement alone is not saved work.

## Decision rule

Do not promote the journal based on append bytes or a single phase. Compare
paired full-cycle totals and fixed-depth complete preparation on both hosts,
including unchanged and dense edits. A mixed or consistently slower lifecycle
keeps production adoption closed. Report six-pair ranges and win counts rather
than turning small median differences into a uniform speedup claim.

Choose at most one subsequent optimization from evidence that agrees across phase
costs and profiles. Preserve exact generation checks, per-transaction validation,
fresh selection/stat checks, cancellation and the selected in-memory engine.
A preceding-write effect is only causal for the explicitly tested treatment;
absence of that effect does not establish the origin of other selection noise.

## Native results

Both campaigns completed on 2026-09-16 with Go 1.27.1, native APFS/Darwin arm64 and
Btrfs/Linux amd64. Compare within each host; their absolute speeds are not a
filesystem comparison. Each has 990 uninstrumented invocations: 324 fixed-depth,
594 cycle steps, 36 byte-limit cases and 36 interference treatments. The 18 cycle
aggregates summarize those steps and are not additional invocations. Each also
has 36 separately profiled invocations. The supplemental writer screen adds 36
uninstrumented invocations per host.

The 50K-file results below show median paired J/R ratios. Parentheses contain all
six pair ratios' minimum/maximum and the number of faster J runs. Smaller is
better. Cycle totals include the record-limit compaction.

| Case | APFS | Btrfs |
|---|---:|---:|
| Repeated-file cycle, 33 preparations | 0.983 (0.967–1.001), 5/6 | 0.941 (0.916–0.953), 6/6 |
| Near-full sparse append | 1.299 (1.242–1.442), 0/6 | 1.473 (1.432–1.600), 0/6 |
| Byte compaction, 30K edits | 1.154 (1.120–1.180), 0/6 | 1.282 (1.230–1.320), 0/6 |
| Depth 0, one edit | 0.950 (0.878–1.079), 5/6 | 0.941 (0.755–0.972), 6/6 |
| Depth 31, one edit | 1.008 (0.894–1.127), 3/6 | 0.962 (0.898–0.976), 6/6 |

The median whole-cycle times are R 9,085 ms / J 8,931 ms on APFS and R 20,645 ms /
J 19,389 ms on Btrfs. These are sums of 33 preparations, not per-push times.
Publication across that cycle falls from 268.7 MB to 8.18 MB on APFS and 242.1 MB
to 7.37 MB on Btrfs (decimal bytes). The near-full append writes just 1,080 bytes,
yet takes longer overall. Bytes written are not physical device I/O.

The repeated-file histories do not establish a monotonic whole-operation slowdown
with record count for this workload; they do not test dispersed history. APFS ratios vary in both directions. The 10K Git fixture is also
mixed: at depth 31, Btrfs unchanged preparation is 5.2% slower in all six pairs,
while edits are 2.9% faster at the paired median, with five wins. Do not reduce
these results to a single restart speedup. All cases, absolute medians, J/R and
J/F controls are in [the complete table](benchmarks/journal-depth/RESULTS.md).

The F control adds roughly 1.6%/3.0% to the sparse cycle on APFS/Btrfs. Its
near-full append ratios to R are 1.065/1.050, compared with J's 1.299/1.473;
its compaction ratios are 1.007/1.013. This locates most of the severe dense-history
penalty beyond the common framed reader, in retained-history work. It does not
isolate one function as the full causal cost. F also computes the full-file
`diskDigest` although its nonjournal writer never uses it; its F/R ratio therefore
includes that unused generation-check preparation, not just framing overhead.
Byte-limit compaction first encodes a delta that does not fit, then discards it
and encodes a complete base. Record-limit compaction bypasses that first encode.
Both costs are properties of this implementation, not unavoidable journal costs.
Reducing them requires a separately measured candidate, not rewriting these results.

### Where the time goes

Selected storage-phase medians for near-full sparse append, milliseconds (R / F / J):

| Phase | APFS | Btrfs |
|---|---:|---:|
| Load | 21.7 / 24.5 / 115.6 | 86.8 / 112.3 / 438.7 |
| Index | 10.2 / 10.2 / 0.7 | 58.7 / 54.8 / 2.0 |
| Save | 15.9 / 17.1 / 9.8 | 81.6 / 85.8 / 54.5 |

Complete Selection/Load/Scan/Hash/Index/Verify/Save/Wire/Other/Total tables,
including all 33-step cycle aggregates, are derived from the original samples:
[APFS](benchmarks/journal-depth/review-followup/apfs-phases.md) and
[Btrfs](benchmarks/journal-depth/review-followup/btrfs-phases.md).
They also show paired `(Total - Wire)` ratios. Adoption still uses complete Total;
wire work is part of the requested preparation, not an invalid common addend.
Phase medians are individually computed and need not sum to the median Total.

Load already includes base-tree construction and each transaction's validated
updates. J's small later Index phase moves some work earlier; it does not remove
all index work. At depth 31 with sparse history, J Load is only 35.3/163.1 ms.
Two dense transactions cost much more to load than 31 repeated-file transactions.
This contrast is between deliberately different edit volumes and overlap, not a
controlled estimate of record-count sensitivity for arbitrary sparse histories.

In the original combined-instrumentation profiles, across three near-full appends, J's load accounts for 25.0% of sampled CPU
on APFS and 40.6% on Btrfs. `Snapshot.Update` accounts for 14.6%/18.5%, including
replayed transactions and the current one-file edit. Hierarchy replacement
validation alone accounts for 7.3%/5.5%; these are nested costs, not additive
percentages. Btrfs gob decoding is 6.8%, while
checkpoint checksums are 13.0% across their call sites. The broad `decode` symbol
must not be described as pure serialization overhead.

Allocated space for those three J runs totals about 995/1,001 MiB. `io.ReadAll`
accounts for 202/197 MiB, including load and append's second full read for exact
generation checking. Decoding allocates about 329/347 MiB cumulatively, including
validated index updates. These are sampled allocations, not retained memory, and
memory size is not the decision criterion. Both CPU and allocation instrumentation
are enabled in these diagnostic runs; profiling overhead prevents translating
sample percentages directly into uninstrumented milliseconds or promised savings.
It can also change relative rankings. These percentages are retained as historical
diagnostic observations, not the basis for selecting hierarchy validation first.

Sparse APFS preparations still spend much of their sampled CPU in filesystem
selection and stat calls: depth-31 J selection is 40.8%, and the observed builder
is 21.1%. Those costs remain even with an ideal codec. The syscall samples do not
establish the amount of off-CPU waiting. Filesystem enumeration is a separate
shared-path candidate, not evidence for changing the index or weakening selection.

### Preceding-write screen

Neither writer treatment produces a consistent slowdown across fixtures and
hosts. For the 50K fixture, the repeating-byte writer's paired total ratio is
1.001 on APFS and 0.956 on Btrfs; the incompressible writer's is 1.000 and 0.962.
Btrfs mounts with `compress=zstd:3`, so the incompressible follow-up is the stronger
physical-write treatment, although actual device bytes were not measured.

The supplemental Git/APFS run instead has a 0.914 writer/quiet ratio (six wins),
with nearly unchanged median selection time, 99.6 versus 100.2 ms. The synced
Git/Btrfs treatment has a wide 0.862–1.623 range. These results do not support the
specific claim that preceding writes explain the selection variation. They also
do not prove absence of contention or explain variation in other runs. Keep both
screens separate and retain the unexpected directions. Neither screen measures or
forces concurrent kernel writeback, and neither reproduces an R/F cache publication
between adjacent variants. This question remains open. The shared treatment helper
now accepts the chunk and records logical bytes per sample for both screens.

## Separated-profile review follow-up

Both hosts completed 72 new diagnostic invocations: four cases × three variants ×
three repetitions × two separate instruments. Every CPU-only invocation omits the
heap-profile flag and leaves the runtime's ordinary allocation sampling rate in
place; heap-only invocations do not start CPU profiling. Default allocation
profiling overhead can still appear in CPU samples. The independent original
uninstrumented timing comparisons are not repeated or pooled with these runs.

Near-full J append, cumulative share of CPU samples across three CPU-only runs:

| Function | APFS | Btrfs |
|---|---:|---:|
| Framed load | 27.4% | 38.6% |
| Snapshot.Update | 13.7% | 17.0% |
| Hierarchy validation | 7.4% | 5.2% |
| Checkpoint checksum, all call sites | 2.1% | 12.7% |
| File selection | 32.6% | 19.0% |

These are overlapping cumulative samples, not additive costs or predicted speedups.
The corrected diagnostics still show hierarchy work, but do not establish it as
best to optimize first. Checksum cost differs sharply between hosts, and selection
remains substantial. Bounded read/checksum work, replay/compaction policy and shared
validation therefore remain competing candidates for independent measurement.
Clean profiles do not change the decision against adopting this implementation.

The original cycle's median wire totals are R/J 484.8/521.4 ms on APFS and
2,009.5/2,120.6 ms on Btrfs. J does not have a lower median wire cost in these runs.
Paired `(Total - Wire)` cycle ratios are 0.977 and 0.932; these are supplementary
views of the same data, not new evidence of statistical confidence. Complete Total
remains the decision metric, and the small APFS effect remains unresolved.

[Follow-up evidence](benchmarks/journal-depth/review-followup/README.md) retains
all separate profiles, receipts, current source archives and the harness patch
relative to the original campaign. Native Go tests, race checks, vet and all 71
Python tests pass on both hosts. A separate 100-file ignore/Git smoke run validates
the shared interference helper; its timings are not performance evidence.

## Next candidate and deferred items

1. **Complete this review slice:** separate CPU/heap diagnostics, preserve the
   original results with repeated-file and coupled-order limitations, publish full
   phase accounting, and share the interference loop. Do not change storage or
   production validation while measuring these controls.
2. **Next experiment, select one candidate after the clean profiles:** compare the
   case for bounded cache reads/streaming exact-generation checks, a journal budget
   based on replayed edits or delta/base size, and shared type-preserving hierarchy
   validation. Early termination of a delta encode at the remaining journal budget
   is another concrete compaction candidate. Measure one independently first; do
   not bundle candidates and infer which helped. Include cumulative publication
   cost when a budget causes earlier compaction, rather than celebrating a shorter
   load phase. Preserve full-file generation identity, equal-sized stale-writer
   rejection, corrupted-middle-frame rejection and cancellation.
3. **Broaden that comparison:** keep repeated-file edits as a named case and add
   deterministic rotating and dispersed paths, including mixed replacement/add/
   delete/type-change batches. Use a crossed 36-round depth/variant-order schedule
   with joint-coverage assertions. A shared validation candidate requires an explicit
   predicate: every edit hits an existing path, is not a deletion, and preserves
   type. The existing `structural` flag is insufficient. The base snapshot must
   already satisfy hierarchy invariants. Preserve ordinary metadata validation and
   valid intermediate states at every journal transaction, even when a later frame
   could repair an invalid one. Check actual production batch shapes before claiming
   gains for production callers.
4. **Later:** compact encoding, shared observation-change scanning, stamp/cache
   layout, and parallel semantic validation stay separate candidates. Shared
   filesystem selection remains a broader opportunity because fresh selection/stat
   work persists regardless of storage. Filesystem eligibility, ownership and
   directory handles, recovery, post-freeze guards, misses and all actual callers
   remain production adoption gates. Already-retained watch sessions should not add
   disk-cache work per edit. Large-file deltas/chunks remain roadmap step 4.

## Evidence and validation

[Retained evidence](benchmarks/journal-depth/README.md) includes every sample,
setup receipt, exact source archive, source/harness/binary hashes, native mounts,
CPU/allocation profiles and test logs. The original campaigns passed the
experimental Go suite, race checks, vet and all 70 then-existing Python tests. Independent post-fetch checks recompute
paired summaries and cycle aggregates, match all variant roots, confirm expected
sample counts and matched archived measured inputs to the Go/Python sources at
the original handoff.
The supplemental reports pass equivalent source/root/summary checks.
The review follow-up changes Python harness inputs; the original archives identify
the original measured driver and remain the reproduction source for those timings.
Prior observation-journal evidence is unchanged.

The only Go additions are the framed replacement experiment/control, opt-in probe
profiling, and its inclusion in shared recovery coverage. The benchmark drivers
and report are reviewable changes; no production speedup is shipped here.

## Reproduction

```sh
python3 scripts/benchmark_journal_depth.py --output dist/journal-depth
```

On the measured Mac mini, set
`DEVELOPER_DIR=/Library/Developer/CommandLineTools`. `--smoke` uses 100 files,
omits byte-limit cases, and validates the harness rather than performance.
`--profile-repetitions 0` omits diagnostic profiles.
`--profiles-only` collects the separated profiles without re-running the timing
campaign; it requires a positive repetition count. Each native report separates
ordinary samples, full-cycle steps, interference trials and profiled samples.

Run the supplementary incompressible-writer screen with:

```sh
python3 experiments/snapshotcheckpoint/interference_probe.py --output dist/journal-interference
```
