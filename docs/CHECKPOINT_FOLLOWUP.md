# Receiver checkpoint follow-up

Baseline: `de25693` (`Reuse validated transfer checkpoint reads`).

## Candidates

1. Remove the second guarded read and unused public manifest export during
   existing-session initialization. Keep creation-manifest validation, relationship
   checks, and first-initialization blob durability. Replace apply's refused-merge
   dry run with the same selected merge-base check, without revalidating the
   unchanged, previously validated checkpoint.
2. Keep decoded metadata and lazy identity in a private immutable record. Session
   callers request revision, delta, and merge-base operations instead of borrowing
   manifest slices. Public exports still copy. Memoized identity uses `sync.Once`
   so independent read-only request handles can share a record safely.
3. Compare that request-local candidate with a receiver-owned LRU shared across
   daemon requests. Every request still performs guarded, bounded record I/O,
   exact-byte comparison, and relationship checks. The workspace store retains
   at most 32 records and 64 MiB of accounted backing buffers, entry arrays and
   strings. Active operations may retain evicted records; this is not a total
   process-memory limit. Workspace removal releases matching entries. Transfer
   GC leaves the pinned checkpoint intact. Publication invalidates cached records
   before writing; successful writes are not speculatively seeded into the cache.

The implementation belongs to the shared `changes` receiver code. Push/watch
use the longer-lived daemon owner. Persistent fetch uses the same record and
validation improvements; separate CLI processes cannot share an in-memory cache.
Fresh workspace/job creation and ephemeral fetch remain regression controls.
Merkle selection, wire identity, fsync batching and publication barriers stay fixed.

## Measurement plan (declared before native timings)

Freeze three trees: committed baseline plus identical lifecycle benchmark,
request-local candidate, and shared-cache candidate. The local and shared
candidates differ only in enabling retention in `openWorkspaces`.

Use native APFS and Btrfs fixtures with GOMAXPROCS=2. Validate the shared candidate
with the full Go suite, vet, and scoped race tests (including daemon); validate
the local candidate's affected packages. Compile all three variants on each host.
Record source, harness and binary hashes, Go version, fixture filesystem and load.

Seven rotating/reversing rounds compare 1K/10K cold reads and existing-session
initialization across all three variants. Also record enabled/disabled cache
reads through independent request handles. Complete watch uses seven rounds of
five edits per variant. One untimed watch run per variant warms executable and
filesystem paths before those observations; retain its raw log separately.

Three alternating pairs of ordinary push, persistent and ephemeral fetch, fresh
workspace creation, and job submission screen for regressions against baseline.
These controls do not establish equivalence. Keep every measured observation;
record host load without post-hoc outlier removal. No new rsync/Mutagen or
cross-host network claim follows from these native loopback measurements.

Report medians and paired candidate/baseline ratios. Seven-pair ranges provide
per-comparison sign-test bounds under independent pairs, not simultaneous
confidence across workloads. Treat component improvements separately from
complete-command latency. A shared-cache speed claim needs a complete-watch
improvement over the request-local candidate; a warm-read microbenchmark alone
is insufficient. An inconclusive experiment stays labeled inconclusive.

## Cabal watch comparison

All seven complete-watch rounds finished before the control-output parser failed.
The retained raw output records 121 observations at that point: 98 lifecycle
observations, 21 watch observations, and the first ordinary-push pair. The collector
then encountered `EVALUATION` lines interleaved between a fetch benchmark's name
and numeric summary. The benchmark process succeeded. Its unpaired output is
retained separately, and that unfinished fetch pair is repeated in full during
continuation. Completed observations remain unchanged. Continuation checks native
binary/source hashes, Go version, platform and fixture filesystem before resuming.

| Watch comparison | Median times (ms) | Median paired ratio | Seven-pair range |
|---|---:|---:|---:|
| Local / baseline | 448.9 / 515.2 | 0.967 | 0.756–1.127 |
| Shared / baseline | 402.5 / 515.2 | 0.873 | 0.669–1.035 |
| Shared / local | 402.5 / 448.9 | 0.901 | 0.781–1.070 |
| Shared / local staging | 160.8 / 184.5 | 0.911 | 0.787–0.981 |

These support a staging improvement from cross-request reuse on Cabal. Complete
watch improved in six of seven paired comparisons, but its bounds still cross
parity. A complete-watch speedup is therefore not established by this run alone.
The medians and paired ratios summarize different quantities; ratios are not
calculated by dividing the two displayed medians.

## Control follow-up plan

Cabal's three-pair screen produced adverse paired medians for ephemeral fetch
(1.202), workspace creation (1.084), and ephemeral submission (1.276). The
observed ranges were broad and crossed parity. This is insufficient to establish
regression equivalence, and these estimates are not discarded.

Before interpreting them, run a separate seven-round confirmation for those
three cases. Rotate a baseline, a second label executing the *identical baseline
binary*, and the shared candidate through all six orders. Use the same retained
binaries, three iterations per observation, native filesystem, and GOMAXPROCS=2.
Record load and process names/CPU/state without command arguments. Keep the
confirmation separate from the original screen. This tests whether comparable
variation occurs without changing code; it does not automatically attribute all
variation to noise or certify the absence of regressions.

## Mac mini watch comparison

The Mini also completed all seven watch rounds and the first push pair before
encountering the same interleaved-output parser failure. Its control continuation
uses the same corrected helper and verifies the same invariants as Cabal.

| Watch comparison | Median times (ms) | Median paired ratio | Seven-pair range |
|---|---:|---:|---:|
| Local / baseline | 355.9 / 354.1 | 1.005 | 0.944–1.072 |
| Shared / baseline | 344.3 / 354.1 | 0.972 | 0.935–1.032 |
| Shared / local | 344.3 / 355.9 | 0.968 | 0.914–1.028 |
| Shared / local apply | 154.5 / 160.3 | 0.956 | 0.905–0.986 |
| Shared / local staging | 74.13 / 78.92 | 0.933 | 0.866–1.001 |

Apply improved in all seven paired comparisons, including against the committed
baseline. Complete-watch bounds cross parity. The request-local initializer's
10K cold-handle case improved from 10.798 to 9.890 ms, paired ratio 0.927 and
range [0.856, 0.951]. Cold reads themselves show no established change.

Cold-handle lifecycle cases do not attach a shared cache in any variant.
The separate independent-handle cache probe is mostly warm after its first read: Cabal
34.077 to 2.515 ms; Mini 7.189 to 0.376 ms. These are checkpoint-access costs,
not complete-command timings, and the probe does not include checkpoint writes.

## Cabal control confirmation

The separate confirmation contains 63 observations. Baseline and its second
label have identical binary hashes. Candidate/baseline paired medians were
0.986 for ephemeral fetch, 1.042 for creation, and 1.033 for submission. Against
the second baseline label, they were 1.021, 1.008, and 1.001 respectively.
The original large adverse point estimates did not repeat consistently.

However, the identical-binary ratios themselves ranged from 0.758–1.231 for
fetch, 0.585–1.307 for creation, and 0.672–1.534 for submission. One candidate
fetch observation was 4.054 times its paired baseline. It remains included.
These results demonstrate substantial benchmark variation; they do not prove
zero regression or equivalent latency tails. No confirmed control regression
was identified, and no control speedup is claimed.

## Decision

Keep all three changes as a bounded receiver optimization. Removing repeated
reads/validation has a direct work reduction and a consistent Mini initializer
improvement. The private record removes borrowed manifest slices from session
callers without changing the selected snapshot representation. Cross-request
reuse avoids repeated decoding and identity calculation, with a consistent
Cabal staging improvement and Mini apply improvement against the local candidate.

Do not claim a confirmed complete-watch speedup on both hosts, the earlier 10%
whole-cycle goal, near-instant watch, or zero regressions. The complete-operation
bounds remain broad, and control observations include outliers. The longer-lived
cache's retention/identity/invalidation behavior is covered independently of its
latency. Further optimization should use new measurements of the remaining work;
this slice does not weaken durability or change source acceptance to chase timings.

## Verification and provenance

The shared candidate passed the full Go suite, `go vet`, and race checks for
changes, daemon, client and CLI packages on both hosts, using Go 1.27.1. The local
candidate passed its affected changes/daemon suites. The final merge-base test
also makes the staged attempt internally consistent before presenting its forged
base, so it reaches the merge-base guard rather than failing the digest guard.
This is the sole Go-source difference after the timing freeze; production and
benchmark source are unchanged. Final native checks cover that strengthened test.

[Exact input overlays and executed drivers](benchmarks/2026-09-13-checkpoint-followup-inputs.tar.gz)
reproduce all three frozen source hashes from `de25693`. The archive includes
the original collector, its control continuation with the corrected parser, the
same-binary confirmation driver, and the final test overlay. The original
measurement plan is retained separately from this results narrative.


## Completed controls

Both primary reports contain 149 unique observations. All 121 observations from
each initial job are unchanged in its final report. There are another 63
observations in the separate Cabal confirmation. Raw logs include both unpaired
fetch outputs from the parser failures; those outputs are not used as matched
observations. The final strengthened regression tests passed with the race
detector on Cabal and Mini.

| Control | Cabal paired median ratio | Mini paired median ratio |
|---|---:|---:|
| Ordinary push | 0.907 | 0.975 |
| Persistent fetch | 0.976 | 1.022 |
| Ephemeral fetch | 1.202 | 0.969 |
| Workspace creation | 1.084 | 1.012 |
| Ephemeral submission | 1.276 | 0.641 |

These are three-pair screens, not speedup or equivalence claims. All ranges
cross parity. Mini push includes an extreme baseline observation (paired ratio
0.029); persistent fetch includes a candidate slowdown of 1.674 in one pair.
Neither is discarded. The Cabal confirmation above separately addresses its
adverse control estimates without replacing these values.

[Reports, comparisons, raw logs, and final regression results](benchmarks/2026-09-13-checkpoint-followup.json)
include job identities, source/binary hashes, execution order, load observations,
and continuation provenance. Production source matches the frozen shared
candidate; only the strengthened test and documentation changed afterward.
