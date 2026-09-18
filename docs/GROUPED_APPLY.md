# Grouped apply: implementation and native comparison

The [baseline](APPLY_JOURNAL_SCALING.md) selects synchronization and publication
grouping as the first production candidate. This experiment initially applies
only to two or more existing regular-file replacements sharing one existing
parent. Other changes retain the existing installation path. Both paths share
input verification, staging, rename primitives, conflict checks and receipts.

## Revised comparison

The review fixes add regular-file backup-data synchronization to both installation
paths, explicit member/barrier roles and validation/harness guards. The revised
native campaign measures this final code against the frozen P1 production baseline.
The baseline does not include the inherited backup-data fix. A/B are identical
baseline binaries; candidate ratios therefore measure the whole proposed slice,
including its stronger backup-data ordering.

**Retain the bounded candidate for review.** Four paired apply rounds and three
paired complete-command rounds per host completed on 2026-09-17. All ordinary
128/512-root apply and 128-file fetch candidates beat both same-round controls.
Times below are medians; reductions use median per-round ratios against A/B,
rather than ratios of the displayed medians.

| Native host / operation | Baseline A / B | Candidate | Paired time reduction vs A / B |
|---|---:|---:|---:|
| APFS, 128-root apply | 5.764 / 5.927 s | 1.410 s | 75.3% / 76.2% |
| APFS, 512-root apply | 26.901 / 26.848 s | 5.333 s | 80.0% / 80.1% |
| APFS, 128-file fetch/apply | 5.970 / 6.212 s | 1.587 s | 73.6% / 74.2% |
| Btrfs, 128-root apply | 6.107 / 5.816 s | 2.908 s | 52.1% / 50.3% |
| Btrfs, 512-root apply | 24.153 / 24.489 s | 12.138 s | 49.6% / 50.4% |
| Btrfs, 128-file fetch/apply | 6.164 / 6.064 s | 3.645 s | 40.8% / 44.5% |

Full results, including one/eight/32-root apply and push/watch/creation/submission
controls, are in the [APFS summary](benchmarks/grouped-apply-revised/apfs/summary.md)
and [Btrfs summary](benchmarks/grouped-apply-revised/btrfs/summary.md).

The improvement is specific to eligible single-parent batches. APFS small-operation
controls are near parity in this run. Btrfs one-root apply has a +3.3% / +7.4%
median paired change; its candidate observations span 122–277 ms and controls
117–231 ms. Btrfs push is +3.1% / +2.2%; watch changes sign depending on the
control. One eight-root candidate is slower than both controls despite an overall
26% median paired reduction. These observations do not establish universal
non-regression, and three/four samples per mode do not support tail estimates.

Btrfs paired C/A ratios span 0.463–0.571 at 128 roots, 0.470–0.549 at 512 roots,
and 0.529–0.678 for batch fetch. Against B the respective ranges are
0.481–0.529, 0.492–0.524 and 0.536–0.626. Its revised batch-fetch gains are smaller
than in the initial campaign; separate campaigns cannot isolate the review fixes'
latency cost. Every replaced regular file now requires one extra member sync.

The instrumentation matches the intended mechanism: three journal publications
for eligible groups, about 204–205 KiB serialized at 128 roots and 812–818 KiB at
512, versus about 17 MiB and 269–271 MiB in the frozen baseline. Median journal
validation plus encoding at 512 roots is 2.89 ms on APFS and 8.93 ms on Btrfs.
At 128 roots the candidate performs 274 full synchronizations plus 640 member
fsyncs on Darwin, or 914 fsyncs on Linux, versus 1,550 baseline calls.

The earlier 250-observation campaign remains immutable in the
[pre-review APFS summary](benchmarks/grouped-apply/apfs/summary.md),
[pre-review Btrfs summary](benchmarks/grouped-apply/btrfs/summary.md) and
[initial provenance](benchmarks/grouped-apply/runs.json). It is historical evidence,
not acceptance evidence for the revised code. Neither campaign isolates the
individual effects of journal grouping, synchronization grouping or review fixes.

## Recovery ordering

1. Publish the prepared plan. Materialize and verify every transaction value,
   synchronize its file and item directory, then publish the item directories
   with a transaction-directory barrier. No destination has changed.
2. Bind every item to the common parent's captured identity and publish grouped
   installation intent once. Values and intent are durable before any backup.
3. For each file, recheck its original and ancestor, move it to its own backup,
   synchronize the backup file data, validate the backup, synchronize the item
   directory, and complete a common-parent backup barrier.
   Only then install that file with a no-replace rename and synchronize the
   changed item directory. Keep each backup/replacement pair together rather
   than leaving the entire group absent while collecting backups.
4. Finish the common-parent installation barrier before committing. Grouped
   values have their final mode before staging member synchronization; values
   requiring temporary permission widening retain the existing path.
5. Validate all backups and installed content, publish the committed journal,
   and use the existing durable outcome and authorized cleanup protocol.

On Darwin, member synchronization uses the existing fsync-then-F_FULLFSYNC
publication policy on one filesystem. Linux uses ordinary fsync for members and
the common parent. Each backup is durable before that file's replacement.

Grouped intent can cover an item not yet touched when the process stops. If its
backup is absent and its verified staged value remains, recovery preserves the
live destination, including intervening edits. An installed value cannot still
be at its staged name: installation is a rename and backups were made durable
first. If the original is missing as well, retain recovery evidence and fail
closed. If both backup and staged evidence are absent, use the existing
original-content check and fail closed when evidence is insufficient.

When a backup exists, use the existing quarantine/check/restore logic. A later
edit to an installed destination must never be overwritten. Grouped phase is
valid only for a complete, single-parent plan of nonmissing content replacements.
Mixed grouped/reference phases and grouped intent with created parents or
different parent identities are rejected; committed journals must not contain
grouped intent. Old per-item journal recovery remains available.

Process-crash tests exercise durable intent, partial backups, the backup barrier,
partial installation, the installation barrier, commitment and retry. Injected
errors test rollback and preserved edits. Process termination alone is not a
power-loss test; the ordering argument and explicit synchronization boundaries
are required in addition to the native recovery tests.

Both installation paths use the shared regular-file backup-data sync before
publishing the renamed backup directory entry. This closes an inherited gap:
renaming and syncing directories alone did not synchronize dirty original file
contents. Directory-tree backups retain the reference protocol. This is an
ordering correction, not a reproduced physical power-loss failure.

Rename helpers perform verified renames and return owned handles. Their callers
name each synchronization role and select member sync or publication barrier
explicitly. Per-application tests observe those actual selections and verify
order; they do not infer policy from pointer identity or logical checkpoint
names. A backup-data sync failure prevents replacement and rolls back.

The common parent stays open for the group; item-directory handles are opened
and closed within each operation. Descriptor use does not grow with root count.
The eligibility check adds metadata reads to multi-root fallback transactions;
it does not build another file-content inventory. Deeper or mixed-parent
performance is outside this flat batch comparison.

## Comparison and acceptance

The driver is `scripts/benchmark_grouped_apply.py`. It checks and extracts the
frozen P1 baseline, then builds baseline, candidate and separately instrumented
candidate binaries. A/B run the same baseline binary. Four rounds rotate all
four execution positions for each fixed-1-MiB apply fixture (1, 8, 32, 128 and
512 roots). Three rounds rotate A/B/candidate for the existing push, watch,
128-file fetch, workspace creation and ephemeral submission fixtures. Every
observation measures one operation. Each host produces 125 observations;
attribution timings do not enter ordinary before/after ratios.

Compare candidates with A and B from the same round. Record the controls as
well as the improvement: three or four observations per mode cannot support
tail-latency claims. These native loopback fixtures do not measure WAN delivery
or establish a new comparison with rsync or Mutagen.

The apply fixture times `TransferTarget.Apply`, including verification, merge,
durable staging, installation, outcome and cleanup, with fixed 1 MiB changed
content. It excludes source selection and transport. The batch-fetch fixture
times the complete `FetchChanges` call with apply over loopback for 128 files
of 4 KiB each; remote job execution/capture and fixture construction precede
that timer. See the frozen benchmark inputs for exact boundaries.

For eligible groups, journal encoding/publication drops from `2k+2` to three:
prepared, grouped intent and committed. Serialized metadata therefore grows
linearly with root count. The measured complete lifecycle should have `2k+18`
Darwin full synchronization calls plus `5k` member fsync calls, or `7k+18`
Linux fsync calls, versus the baseline's `12k+14`. Counts cover merge scratch,
staging, installation, outcome and cleanup; they are syscall counts, not a
physical device-flush count. The one-root control retains four publications and
26 full synchronizations plus one member sync on Darwin, or 27 fsyncs on
Linux. The extra member sync per existing file is the backup-data fix. The
driver rejects samples with unexpected counts and rejects attribution metrics
in every ordinary mode, including CLI controls. The baseline-only driver now
fails before building against a grouped checkout and directs reproduction to
the frozen baseline archive or the grouped comparison driver.

Run on each native filesystem from the candidate checkout:

```sh
python3 scripts/benchmark_grouped_apply.py --output /new/result/directory --rounds 4
```

Inputs, instrumentation/candidate patches, commands, filesystem evidence,
individual observations and structured summaries are retained by the driver.
Use `--smoke --rounds 1` for only the one/eight-root comparison.

## Validation and provenance

The final candidate passed `go test ./internal/changes ./internal/client
./internal/daemon ./cmd/errand` on both native hosts, plus
`go test -race ./internal/changes -run
'^Test(Grouped|Transfer|ApplySynchronization|BackupDataSync|CopyToRoot)' -count=1`.
The local complete `internal/changes` suite and ten Python evidence-driver tests
also pass.

Recovery tests cover actual process exits, rollback errors, stale/replaced
parents, untouched and already-installed later edits, corrupted/missing evidence,
transaction ownership and historical retries. Review regressions cover mixed
grouped/reference phases, actual synchronization role/order and backup-data
sync failures. Explicit mode boundaries keep `0400` replacements eligible and
route `0000` values requiring temporary permission widening through the
reference path.

For reproduction on the Mac mini, prepend
`/Library/Developer/CommandLineTools/usr/bin` to `PATH`. This selects its working
Command Line Tools Git without changing machine settings. Both comparisons use
Go 1.27.1 and `GOMAXPROCS=2`. The baseline input digest is checked against the
frozen P1 archive. Failed initial setup records are retained in the pre-review
provenance; final validation and benchmark artifacts are separate.

All 250 raw logs were reparsed and checked against their report rows; generated
summaries reproduce exactly. All 210 ordinary observations are free of attribution
metrics. Every mode occupies every execution position for its case. Both archived
Go/Python source sets match the final checkout and share the logical input digest
`a39b194f9d28fdeb0b046f99dd041fd83573e6ccb4d12b1c098447f257834de6`.
The [run provenance](benchmarks/grouped-apply-revised/runs.json),
[native validation logs](benchmarks/grouped-apply-revised/validation) and
[checksum inventory](benchmarks/grouped-apply-revised/SHA256SUMS) retain the evidence.

Before commit, this slice was rebased onto `897173c`, which updates fsnotify from
1.9.0 to 1.10.1. All measured Go/Python files remain unchanged; only `go.mod` and
`go.sum` differ from the archived inputs. The native timings and validation logs
above use 1.9.0. After rebasing, `go test ./internal/changes ./internal/snapshot
./internal/client ./internal/daemon ./cmd/errand` passed locally with 1.10.1.
Benchmarks were not rerun for the upstream dependency update.

## Scope and follow-ups

This slice includes the phase-level journal change, bounded synchronization
grouping, regular-file backup-data synchronization, explicit synchronization
roles, mixed-protocol rejection, permission eligibility boundaries, evidence
driver guards, recovery/error coverage, final-mode-before-sync ordering and native
complete-operation controls. File contents, source selection, transfer
negotiation and historical receipt semantics use the existing implementations.

Keep broader eligibility (multiple parents, creations, deletions, directory
changes and temporarily widened permissions) as a separate recovery and
performance experiment. Before widening eligibility, benchmark mixed-parent
and ineligible transactions to measure fallback overhead. One ineligible root
currently selects the reference path for the whole transaction. Partial grouping would need its own mixed-plan
recovery argument and measurements.

Private merged-output synchronization remains unchanged. Measure a scratch
publication policy separately if its residual cost justifies it. Compact
progress records are also conditional: three full-plan publications remove the
quadratic encoding pattern for eligible groups, so another journal format
must show benefit against this candidate, not just the old baseline.

The next main roadmap slice remains realistic Git/editor-save watch discovery.
It addresses a different bottleneck, especially single-file edits that do not
use this grouping. See the [active tracker](SNAPSHOT_ENGINE_PLAN.md).
