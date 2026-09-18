# Parent-publication barrier addendum

Compare committed P1b (`fd3ce97`) with one bounded change: member-sync each
installation parent except the last, then finish with a full barrier on the last.
Every parent is still opened and identity-verified. The planner requires every
parent's device to match the transaction device; other devices use the reference
path, whose existing cross-device rename errors still apply. This does not add
cross-device support. Backup-data synchronization and each backup-parent barrier
are unchanged.

On Darwin, this replaces N final `F_FULLFSYNC` calls with N-1 ordinary member
fsyncs and one full barrier. Apple's [fcntl documentation](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/fcntl.2)
states that the full barrier persists preceding fsyncs on the same device. Linux
continues to fsync every directory because member and barrier use the same syscall.
Each parent's own fsync publishes its entries; syncing a sibling cannot replace it.
Single-parent groups and single-root reference applications keep their existing
publication sequence.

## Recovery and validation

All original-file backups are durable before the final installation publication
phase starts. A member failure or failure of the final barrier aborts before
commit and uses the existing rollback protocol. A completed member is not by
itself a durability claim on Darwin: the existing `parent-published` fault-injection
checkpoint now also covers that intermediate state. Recovery still has the durable
intent, staged-value evidence and original backups. No journal phase or historical
receipt semantics change, and no in-progress group is committed until the final
barrier succeeds.

Tests require member-member-barrier ordering across three distinct parents,
no commit after a first-member, second-member or final-barrier failure,
restoration after those failures,
and rejection of cross-device grouping. The process-exit test between parent
publications is retained. These tests do not simulate physical power loss.

## Comparison method

Both native runners execute the package tests and focused race checks before
timings. Frozen baseline and candidate sources, matching benchmark fixtures and
dependencies, raw logs, and runner provenance are retained under
`benchmarks/parent-publication`. Only `internal/changes/apply_group.go` differs in
production source. The common driver accepts explicit multi-parent eligibility
metadata because both variants now support that path.

The two baseline labels use the same binary. Variant and workload order rotate.
Each sample is one complete apply, including receipt and cleanup. Fixture setup
and content/retry checks stay outside timing. There are 24 rounds for a single
4 KiB file, and 12 rounds each for 128 files across one, eight, and 128 parents:
180 individual operations per host. No background-write-load experiment is part
of this comparison.

## Results and decision

Retain the change. [The complete comparison](benchmarks/parent-publication/comparison.md)
contains both controls, same-round paired reductions and faster-pair counts.

On APFS, 128-parent apply fell from 1499.83 / 1498.32 ms to 1001.09 ms:
33.2% / 33.1% paired reduction, faster in all 12 pairs against each control.
Candidate observations ranged from 993.9 to 1018.3 ms; every observation was below
every baseline observation. This saves about 0.50 seconds, less than the proposed
0.63-second hypothesis, and does not remove all of the multi-parent overhead.
Eight-parent apply improved 3.3% / 3.2%, faster in 11/12 pairs against each
control. Flat batches improved only 0.6% / 0.7%; single-file results were 0.5% /
0.3% slower. Treat those small movements as near parity, not useful gains.

Btrfs has no consistent improvement. In the flat case the candidate was 7.2%
slower than control A and 0.6% slower than control B by paired medians, while
identical control B was itself 7.2% slower than A. Eight-parent results ranged
from parity to 3.9% slower depending on the control; 128-parent results ranged
from 3.6% faster to 0.9% slower. The single-file comparison also changed sign
between controls. These observations do not establish a small Linux regression
or prove tight non-regression. They are consistent with substantial host/run
variation; the Linux synchronization syscalls and their ordering are unchanged.
No Btrfs speedup is claimed.

Both hosts passed `go test ./internal/changes` and focused grouped-apply and
synchronization race tests before measurement. Local focused tests also passed;
the new ordering and same-device tests failed against the original production
code before the fix. The existing multi-parent process-exit recovery matrix is
included in the package tests. The cross-device eligibility test injects a
different identity device; it is not a mounted cross-device integration test.
The source/log verifier validated all 360 individual observations and the exact
candidate production source. The common Python harness tests passed (13 tests).

## Review follow-up

The frozen archives and individual measurements are unchanged. The strengthened
verifier additionally checks every baseline Go source/test and module against
`fd3ce97`, and compares all archived inputs with only the intended production and
publication-test files allowed to differ. This covers ordinary test helpers and
the common Python harness, not just files named `*_benchmark_test.go`.

Post-measurement production edits clarify synchronization comments only. Their
exact before/after hashes are recorded in `benchmarks/parent-publication/review-source.json`
using the existing source-transition verifier. A line-comment-stripped comparison
confirmed unchanged implementation. Review also expanded the publication fixture
to three parents and added a failure on the second member after the first succeeds.
These changes do not claim new benchmark observations. The focused publication
and single-parent synchronization-order tests passed locally with the race detector;
the Python evidence tests passed, including mismatched-baseline and changed-input
rejection cases. The derived table now names the median-of-paired-ratios statistic
separately from the displayed duration medians.

## Follow-ups

P2, the realistic Git/editor-save watch matrix, remains next. The tracker keeps
B1 as a separate experiment: compare phase-level Btrfs `syncfs` with per-file
fsync under both idle and unrelated background-write load, with explicit phase
ordering and tail-latency acceptance criteria. No `syncfs` candidate is enabled
or measured here. Exchange adoption and the inherited R1 recovery questions
remain separate. This addendum does not change their guarantees or priority.
Separating historical archive verification from current-checkout verification
remains a later benchmark-tooling cleanup; the strict source check and explicit
`--checkout` support stay in place for this slice.
