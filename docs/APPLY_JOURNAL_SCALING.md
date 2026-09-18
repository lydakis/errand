# Production apply-journal scaling

This is P1 in the [shared engine tracker](SNAPSHOT_ENGINE_PLAN.md). It measures
the existing production application protocol before choosing an optimization.
The production implementation was unchanged for this baseline. The subsequent
[grouped-apply candidate](GROUPED_APPLY.md) is evaluated against these frozen inputs.

## Result and selected next experiment

**Test phase-level journal publication with grouped item synchronization first.**
Both native campaigns show synchronization dominating this workload. Repeated
encoding is real and grows quadratically, but a representation-only change
would leave most of the measured latency. Reducing the number of full-plan
publications per transaction may reduce both costs; keep measuring them
separately in the candidate comparison.

Six rounds on each host produced 180 individual operation observations: 120
ordinary timings and 60 attribution runs. Both used Go 1.27.1 and identical
frozen inputs, SHA-256
`ea33e79dedfc7428bf53a011e225b2c568694b80af0b77a4536a6b8f2164367e`.
The [APFS evidence](benchmarks/apply-journal-scaling/apfs/report.json) came from
Mac mini job `01M2PTFG7R1AYKHBZR295AE7DK`; the
[Btrfs evidence](benchmarks/apply-journal-scaling/btrfs/report.json) came from
Cabal job `01M2PTFG7R12TYS5QE1MVN57AQ` on 2026-09-17.

Ordinary complete-apply medians pool the twelve A/B observations per root count:

| Independent roots | APFS median | Btrfs median | Journal publications | Synchronization calls |
|---|---:|---:|---:|---:|
| 1 | 128 ms | 122 ms | 4 | 26 |
| 8 | 449 ms | 523 ms | 18 | 110 |
| 32 | 1,576 ms | 1,401 ms | 66 | 398 |
| 128 | 5,751 ms | 5,366 ms | 258 | 1,550 |
| 512 | 25,937 ms | 24,706 ms | 1,026 | 6,158 |

Separate instrumented measurements, reporting median component durations and
median per-operation shares of instrumented complete-apply time:

| Host / roots | Synchronization | Share | Journal validation + encoding | Share |
|---|---:|---:|---:|---:|
| APFS / 128 | 4,734 ms | 81.3% | 222 ms | 3.8% |
| APFS / 512 | 19,780 ms | 74.0% | 3,465 ms | 13.0% |
| Btrfs / 128 | 4,847 ms | 89.0% | 134 ms | 2.5% |
| Btrfs / 512 | 20,804 ms | 84.9% | 2,047 ms | 8.4% |

At 512 roots the smallest measured synchronization share was still 72.0% on
APFS and 83.9% on Btrfs. The dominant term does not depend on a single favorable
sample. Journal bytes increase from about 17 MiB at 128 roots to 269–271 MiB at
512 roots despite fixed changed content. Byte counts include machine-specific
filesystem identities, so they need not be identical across hosts.

The complete path has `12k+14` synchronization calls in every attribution sample.
For this existing-file fixture, their count breaks down as follows:

| Call sites | Count |
|---|---:|
| Private merged-output file copies | k |
| Transaction value file copies | k |
| Backup and install source/destination directories | 4k |
| Journal files | 2k+2 |
| Shared directory barrier helper, including journal directories | 4k+8 |
| Transfer receipt/state files | 4 |

Inclusive journal publication accounts for only part of this: approximately
2.06/1.86 seconds at 128 roots and 11.84/10.57 seconds at 512 roots on APFS/Btrfs.
Do not attribute all synchronization time to the journal. Private merged-output
file synchronization alone takes about 0.42/0.36 seconds at 128 roots and
1.49/1.34 seconds at 512 roots. Keep that publication-policy candidate separate
from changing the transaction protocol. Compact progress records remain gated
on the cost left after grouping, rather than an automatic second rewrite.

There is meaningful environmental variation. Ordinary 512-root observations
span 22.9–32.0 seconds on APFS and 23.2–29.0 seconds on Btrfs. Btrfs eight-root
ordinary timings span 369–1,259 ms; its instrumented median is lower than either
ordinary A/B median. The instrumentation does not make production faster; the
execution controls show why tiny differences and tail estimates would be
misleading here. These are diagnostic baselines, not before/after speedups,
cross-host hardware comparisons, or current watch/push/fetch CLI latencies.

## Question and workload

Does a many-root apply spend most of its time validating/encoding the complete
journal, waiting for synchronization, or doing other work?

The fixture changes exactly 1 MiB of file content across 1, 8, 32, 128 or 512
independent roots. All roots are existing regular files in the workspace root;
all bytes differ from their common base. Thus both old and new content sizes stay
fixed, parents already exist, and no conflicts require an external merge process.
This isolates root-count scaling, not directory depth or large-file transfer.

The measured operation is `TransferTarget.Apply`: input verification, merge,
staging, journal publication, installation, durable receipt and transaction
cleanup. Source selection, transport and initial transfer staging are excluded.
The benchmark also checks exact destination bytes, receipt paths/states, absence
of pending transactions, and historical retry preserving a subsequent edit.
Those checks and fixture construction are outside timing. Setup files receive
member synchronization and containing-directory barriers before measurement.

## Attribution and controls

`scripts/benchmark_apply_journal.py` freezes the Go source, benchmark and Python
inputs. It builds one ordinary binary and a separately instrumented temporary
copy. The production files in the checkout are never instrumented. Both invoke
the same application functions with the same synchronization ordering.

The temporary copy records:

- Every direct `.Sync()` call in `internal/changes`, with its original source
  location, count and elapsed time. Darwin member `fsync` is recorded separately
  if used.
- Validation and encoding inside `writeApplyJournalAtRoot`, including the encoded
  bytes actually submitted, with the trailing newline.
- Inclusive journal-publication and core `ApplyToWorkspace` durations.

These probes cover the synchronization calls reached by this fixture. The
method-expression callback in the legacy `writeRawJSONFile` helper is not
instrumented; that helper has no production callers in the measured path.

The outer Go benchmark reports the complete transfer-apply duration. Inclusive
spans overlap: core apply includes publication; publication includes validation,
encoding and its file/directory synchronization. Do not sum those spans.
Synchronization sums are call durations, not a count of physical device flushes.
Go's Darwin `File.Sync` requests `F_FULLFSYNC`, falling back on unsupported filesystems;
ordinary Linux `File.Sync` uses `fsync`. These are native filesystem measurements,
not a platform-independent cost model.

Each process performs one measured operation (`-benchtime=1x`). Six rounds cross
all six execution orders of ordinary A, ordinary B and attribution. A and B run
the **same binary**, exposing execution-position/environment variation rather
than build differences. Root-count order also changes between rounds. Each host
runs its own campaign sequentially with `GOMAXPROCS=2`; instrumentation timings
must not be substituted for ordinary latency observations. Six observations per
mode/root provide a bounded diagnosis, not reliable tail-latency estimates.

Each output includes the frozen input archive and digest, instrumentation patch,
Go version, filesystem probe, build logs, individual operation logs, structured
samples and a generated summary. An interrupted or failed campaign records its
failure, terminates its child process group and removes scratch fixtures through
the existing campaign helper.

## Decision gate

The selected candidate should reduce repeated barriers and journal publications
by phase, with an explicit recovery argument and crash-injection tests before
adoption. A compact journal that preserves all
current barriers would address only part of that cost.

If validation and encoding remain material after that change, compare an
immutable plan with atomically replaced progress records against an immutable
plan with checksummed append records, keeping installation ordering fixed
initially.

Any candidate must preserve transaction ownership, parent/workspace identities,
conflict checks, recoverable backups, historical outcomes, subsequent edits and
cleanup authorization. A warm successful-apply benchmark does not prove those
properties. Created parents, deletions, directory metadata, conflicts, interrupted
installation and interrupted cleanup require separate recovery coverage.

Also keep private merged-output synchronization separate from journal changes.
`copyMergeSubtree` calls `copyPath`, which synchronizes each copied file even
though it resides in disposable merge scratch. A second copy is then verified
and synchronized inside the transaction. This differs from the already-private
verified merge **input** materializer. Its measured share can justify a bounded
publication-policy experiment, but never disabling synchronization on generic
copies or on the transaction's recoverable values.

## Reproduce

To reproduce this baseline after production changes, extract
`docs/benchmarks/apply-journal-scaling/apfs/inputs.tar.gz` into an empty directory
and run from that directory on each native runner:

```sh
python3 scripts/benchmark_apply_journal.py \
  --output /path/on/native/filesystem/apply-journal-results \
  --revision 16d747d --rounds 6
```

The output path must not exist. `--smoke --rounds 1` runs the one/eight-root
fixture only. The revision is descriptive; the archived input hash identifies
the exact benchmark source, including uncommitted instrumentation machinery.

## Validation

- All 180 native operations passed the installed-content, receipt, cleanup and
  subsequent-edit retry checks.
- Five Python harness tests passed, including preservation of direct Sync-call
  order, probe-anchor failure, execution-order balance, rejection of averaged or
  incomplete observations, and non-overlapping summary arithmetic.
- Focused existing transfer retry, recovery and conflict-receipt tests passed
  locally. Both native benchmark binaries compiled the complete changes test
  package. This slice does not change the recovery implementation.
- All 180 raw logs reparse to their recorded samples; both generated summaries
  reproduce exactly. The frozen Go sources and benchmark driver matched the
  baseline checkout before the grouped candidate, and both hosts have the same
  logical input digest.
- [SHA256SUMS](benchmarks/apply-journal-scaling/SHA256SUMS) covers the 197 retained
  evidence files. Formatting and whitespace checks pass for the changed source.
