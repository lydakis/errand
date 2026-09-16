# Shared reuse and verified observation publication

This slice follows the [shared-builder review](SNAPSHOT_CHECKPOINT_BUILDER_REVIEW.md).
It establishes the contracts needed before persisting derived index state. The
adaptive contiguous/Merkle representation, wire format, selection policy and
production durability rules are unchanged. Disk checkpoints remain experimental.

## Implementation

The in-memory `Builder` stores the same native stamp used by restart checkpoints.
Both use one equality rule for advisory reuse: device, inode, size, full mode,
mtime, ctime and birth time where available. The builder checks the same evidence
after hashing. Unsupported evidence never enters its reuse cache; the existing
portable post-read checks remain. Watch invalidations still discard affected
hashes, including when the filesystem reports matching stamps.

Directory membership evidence remains a separate rule. A selection guard must
detect membership changes, while observation verification may accept ignored
sibling churn that leaves the selected directory's identity and mode unchanged.

Pending observations are private. A successful `ObservedBuild.Verify` transfers
them to `VerifiedObservations`, which exposes length and value-returning indexed
access without exposing its backing slice. Failed builds return no pending batch.
Verification consumes the batch on success or failure; copies of a build share
the consumption state. Zero values and collection-disabled builds cannot produce
publishable observations. A verified empty batch remains valid.

The checkpoint writer accepts only verified observations bound to the identity's
source-root path. Encoding copies records into a bounded 256-record buffer to
preserve immutable access. This adds per-record copying relative to the previous
encoder, which sliced mutable storage directly; it is not an encoding speedup.
Tests that intentionally construct corrupt checkpoints use a
test-only raw writer; cancellation and concurrent-writer tests still exercise the
production writer.

This type proves observation verification occurred. It does not freeze files,
authorize selection, establish cache-directory safety, or replace packed-body
verification. The checkpoint caller still verifies selection and checkout identity
after preparing its index and before publishing. Builds remain serially owned.

## Validation and measurement

Regression coverage includes native stamp fields, dirty-event invalidation,
failed/partial/cancelled publication, value-copy consumption, immutable record
access, empty and disabled collection, wrong-root publication, and preservation of
the prior generation after rejected writes. Existing source-change, corruption,
structural-edit and restart comparisons remain in place.

The complete Go suite and `go vet ./...` passed. The subsequent value-copy
correction passed focused snapshot/checkpoint tests, vet and native race tests on
both hosts. FreeBSD/amd64 snapshot tests compile, exercising the unsupported-native-
evidence build path without claiming a FreeBSD runtime test. The checkpoint and
snapshot Python harness tests passed (19 tests, including the new inventory case).

The native comparison freezes `001c997` and the candidate inputs separately. It
uses eight alternating pairs per case on Mac mini/APFS and Cabal/Btrfs. It covers
10K-entry watch preparation (first edit, retained edit and reconciliation), all six
command paths, and 50K-entry checkpoint miss, unchanged, one-edit and direct
construction. Command paths use loopback HTTP and three iterations per invocation;
checkpoint timing includes preparation and wire hashing. These are warm-filesystem
measurements, not cross-host delivery or application-readiness timings.

The comparison includes the adopted CI permission-fixture correction in
`internal/changes/source_search_test.go`.
The benchmark code and other helper inputs must match the frozen baseline.

## APFS results

Mac mini job `01M2KSNTW3K3VVFEEKYNZZWDB7` completed with 144 caller/preparation
samples and 64 checkpoint samples. Source and harness hashes matched at the start
and end. The [raw report](benchmarks/snapshot-contracts/apfs/report.json) and
compressed logs retain all samples and race-test output.

Times below are median milliseconds. Ratios are the median of within-round
candidate/baseline ratios, not the ratio of the displayed medians. Below 1 is
faster. Eight pairs are descriptive evidence, not a universal regression bound.

| APFS workload | Baseline ms | Candidate ms | Paired ratio | Candidate wins |
|---|---:|---:|---:|---:|
| First edit, 10K entries | 23.10 | 22.99 | 0.993 | 5/8 |
| Retained edit, 10K entries | 22.60 | 22.56 | 0.997 | 4/8 |
| Reconciliation, 10K entries | 211.84 | 205.60 | 0.998 | 4/8 |
| Workspace creation | 548.09 | 542.12 | 0.998 | 5/8 |
| Ephemeral job | 527.11 | 529.13 | 1.000 | 4/8 |
| Push | 483.43 | 486.29 | 1.013 | 2/8 |
| Watch | 308.81 | 307.58 | 0.985 | 5/8 |
| Fetch ephemeral | 99.79 | 99.43 | 0.998 | 4/8 |
| Fetch persistent | 198.88 | 198.61 | 1.010 | 3/8 |
| Checkpoint miss, 50K entries | 876.93 | 862.24 | 0.989 | 4/8 |
| Checkpoint unchanged, 50K entries | 269.18 | 278.35 | 1.022 | 3/8 |
| Checkpoint edit, 50K entries | 293.50 | 292.90 | 1.011 | 4/8 |
| Direct construction, 50K entries | 284.27 | 281.43 | 0.989 | 4/8 |

These measurements show broadly similar performance with small mixed changes.
They do not establish a speedup from the stronger publication contract. The
unchanged checkpoint's 2.2% paired slowdown and push's 1.3% slowdown remain visible.

## Btrfs results and decision

Cabal job `01M2KSNTW3V174EETKE16TNQ4F` completed the same 208 samples with matching
source/harness identities. Its [raw report](benchmarks/snapshot-contracts/btrfs/report.json)
and compressed logs retain all evidence. The paired summary for both hosts is
[summary.json](benchmarks/snapshot-contracts/summary.json).

| Btrfs workload | Baseline ms | Candidate ms | Paired ratio | Candidate wins |
|---|---:|---:|---:|---:|
| First edit, 10K entries | 5.22 | 5.44 | 1.043 | 3/8 |
| Retained edit, 10K entries | 5.60 | 5.78 | 1.002 | 4/8 |
| Reconciliation, 10K entries | 223.56 | 219.47 | 0.977 | 5/8 |
| Workspace creation | 1745.14 | 1619.08 | 0.904 | 5/8 |
| Ephemeral job | 731.39 | 722.82 | 0.995 | 4/8 |
| Push | 792.60 | 793.77 | 0.972 | 4/8 |
| Watch | 375.49 | 373.78 | 0.998 | 4/8 |
| Fetch ephemeral | 92.90 | 92.08 | 0.980 | 5/8 |
| Fetch persistent | 207.87 | 201.56 | 0.990 | 5/8 |
| Checkpoint miss, 50K entries | 1121.07 | 1142.52 | 1.007 | 3/8 |
| Checkpoint unchanged, 50K entries | 536.29 | 540.07 | 0.994 | 5/8 |
| Checkpoint edit, 50K entries | 624.79 | 625.30 | 0.994 | 5/8 |
| Direct construction, 50K entries | 535.85 | 556.53 | 1.041 | 2/8 |

Another task's Cabal comparison overlapped the initial job period and was stopped
at approximately 20:26:55 ET. This job started at 20:25:45. We did not record
per-sample wall timestamps, so we cannot assert that every early timed sample was
uncontended. The summary retains all eight pairs and separately reports the final
six caller pairs, preserving three pairs in each execution order. This sensitivity
check is not a numerical correction or a substitute for the full results.
Checkpoint scenarios run after all caller rounds and fixture construction, outside
the initial overlap window; the sensitivity filter applies only to caller samples.

In those later pairs, first-edit preparation is 1.061 (1/6 wins), retained edits
1.002, reconciliation 0.949 and watch 0.978. Ephemeral submission and ephemeral
fetch move to 1.018 and 1.039. First-edit preparation therefore remains a small
slowdown signal, about 0.3 ms in the later medians. Direct checkpoint construction
also retains a slowdown signal, with a wide per-pair ratio range of 0.708–1.876.
Neither host establishes a universal speedup or regression-free result.

Keep this as a shared-contract slice: it removes divergent file-reuse rules and
prevents unverified or mutable observation batches from reaching the production
checkpoint writer. Retained-edit timings remain close to baseline on both hosts.
The recorded slowdown signals should remain comparison cases for the next index
persistence experiment, rather than being hidden by aggregate averages. No
adaptive-engine or default-strategy change follows from these measurements.

The earlier preflight attempts collected no timing samples because the input
filter accidentally excluded the extracted baseline. A regression test now covers
that output-directory case. A subsequent pair of runs was stopped after the
value-copy ownership defect was reproduced; their partial samples are retained
separately and excluded from the final summary.

## Review follow-up

The review follow-up removes repeated interface access from checkpoint comparison,
skips discarded portable hash checks on supported filesystems, moves the raw codec
adapter into tests, and avoids resolving unused roots when collection is disabled.
The tables above measure the frozen pre-review candidate, not these follow-up edits.

The unused-root regression was checked against both implementations on Linux
(`cabal/01M2KX9A08Q0F7A5JSP7S20MKH`): overlaying the frozen observation implementation
fails with `getwd: no such file or directory` after the working directory is removed;
the review implementation passes. The ordinary empty-selection build remains the
behavioral reference. Darwin can still resolve that removed directory, so the
before/after failure claim is specific to Linux.

Native follow-up jobs `mac-mini/01M2KWT839BNCC84WX7JPW5E6R` and
`cabal/01M2KWT8XS7WP9GYTA173ZB6G9` each passed `go test -p=1 ./...`, `go vet ./...`
and race tests for snapshot, checkpoint and changes packages. Each then completed
64 timing samples: eight alternating pairs for four cases. Current source and
harness hashes match the frozen `review-inputs.*` and both reports at start/end.
The local snapshot Python harness suite passed 14 tests.
These measurements precede integration with upstream watch-retry commit `a1ec59a`.
The rebase preserves its typed source-mutation errors alongside the shared stamp
checks; frozen archives continue to identify the exact measured revisions.

The control here is the frozen **pre-review candidate**, not `001c997`. All rows
use 10K entries. Comparison timings exclude fixture creation, verification,
checkpoint I/O and transfer. Times are median milliseconds; paired ratios use
within-round candidate/control ratios.

| Filesystem / workload | Control ms | Review ms | Paired ratio | Review wins |
|---|---:|---:|---:|---:|
| APFS comparison, unchanged | 1.6790 | 0.5479 | 0.326 | 8/8 |
| APFS comparison, one edit | 1.6791 | 0.5479 | 0.326 | 8/8 |
| APFS first edit | 147.2771 | 143.6142 | 0.991 | 5/8 |
| APFS retained edit | 147.7539 | 146.1455 | 0.999 | 4/8 |
| Btrfs comparison, unchanged | 0.6263 | 0.2016 | 0.321 | 8/8 |
| Btrfs comparison, one edit | 0.6031 | 0.1969 | 0.325 | 8/8 |
| Btrfs first edit | 5.3248 | 5.2592 | 1.009 | 4/8 |
| Btrfs retained edit | 5.5811 | 5.5491 | 1.024 | 3/8 |

The comparison loop is about 3.1x faster on both hosts. These results do not
establish a watch-preparation or end-to-end transfer speedup. Btrfs retained edits
show a 2.4% paired slowdown with a wide 0.657–1.110 ratio range; keep this visible
as mixed evidence rather than claiming regression-free performance. APFS absolute
watch times are substantially higher for **both** variants than in the earlier
campaign, despite matching architecture and Go version. This run does not identify
the cause, so only within-campaign comparisons support the review-cleanup claim.

The [paired summary](benchmarks/snapshot-contracts/review-summary.json),
[APFS report](benchmarks/snapshot-contracts/review-apfs/report.json), and
[Btrfs report](benchmarks/snapshot-contracts/review-btrfs/report.json) retain all
samples; compressed logs and frozen sources sit alongside them. The initial
failed attempts collected no timings. Selecting the mini's installed Command Line
Tools resolved Git's Xcode-license failures; serializing test packages produced a
passing Cabal full-suite rerun after the initial active-writer deadline failure.

## Next

Compare restoring derived index state and writing changed records against this
frozen observation implementation. Include loading, validation, compaction and
recovery costs. Production adoption still requires filesystem eligibility, cache
ownership and directory-lifetime checks, early byte admission and selection guards
through freezing. Whole-file versus delta/chunk transfer remains a later step.

Carry these considered items into that comparison:

- Isolate the larger inline stamp's effect on watch hash-cache locality. Preserve
  exact native-evidence equality; a compact digest alone is not an equivalent rule.
- Compare concrete indexed encoding with bulk copying into the bounded buffer,
  including allocation, total encode time and cancellation behavior.
- When extending the publication API, evaluate moving finalized `Changed` metadata
  onto the verified result. Current callers already verify before reading it.
