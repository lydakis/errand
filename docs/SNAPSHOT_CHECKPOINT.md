# Restart checkpoint: observation reuse experiment

This report preserves the pre-review source and measurements. The
[review follow-up](SNAPSHOT_CHECKPOINT_REVIEW.md) records the fixes, revised native
runs and execution-position breakdown. Its inputs identify the current candidate.

## Decision

Retain the isolated prototype and measurements. Warm fresh-process preparations
improve on APFS and Btrfs, but cache misses add work and small APFS cases remain
variable. Do not enable the prototype in production yet. This completes the
first measurement slice of roadmap step 3, not persisted-index integration.

For 10,000 files of 4 KiB, the final unchanged medians move from 178.48 to 84.06 ms
on APFS and 441.77 to 111.73 ms on Btrfs. One-file-edit medians move from 176.84 to
86.82 ms and 458.09 to 131.82 ms respectively. Every warm 10K pair favors reuse.
All six final cache-miss cases have slower checkpoint medians. Initial population
and warm reuse therefore need separate decisions.

No production `internal/` or `cmd/` implementation changed from `f459bb2` in this
slice. These are source-preparation results, not improvements to current CLI,
watch delivery, transfers, staging or browser readiness. They do not compare
Errand with rsync or Mutagen, nor isolate filesystem performance from host CPU
and operating-system differences.

## What is shared and what is persisted

[`experiments/snapshotcheckpoint`](../experiments/snapshotcheckpoint/README.md)
contains a `Cold` control and checkpoint-aware `Prepare` behind one probe binary.
Both call the existing selection, snapshot and manifest implementation. Both
prepare the existing adaptive index and compute the same wire root. `Cold`
exercises the unchanged `f459bb2` engine; this is not a separately built baseline
CLI comparison, and ordinary one-shot callers may defer index preparation.

The checkpoint stores ordered manifest metadata and native stat observations.
Each new process reselects files and stats every selected path. Matching
observations reuse body hashes; changed paths go through the existing builder.
The probe reconstructs the existing index from checkpoint metadata and applies
edits with `manifest.Update`. It preserves the selected contiguous/Merkle
crossover. It does **not** serialize or reload derived Merkle nodes yet.

The experiment therefore answers whether avoiding repeated file reads and hashing
can pay for loading, revalidation, rebuilding the index and saving observations.
It exposes those remaining costs instead of claiming that all incremental state
now survives a process restart.

Checkpoint identity includes checkout path and native identity, OS, filesystem,
boot session, effective selection policy and options. Changed identity, missing
state or corruption rebuilds. The versioned, checksummed file has a 64 MiB and
200,000-entry limit. A private cache directory outside the source holds one
checkpoint, one fixed temporary file and a nonblocking writer lock. Atomic rename
publishes complete generations. Unchanged calls do not write; edits replace the
whole checkpoint. Interrupted temporary files are removed by the next successful
writer. Advisory cache writes do not fsync; production durability is unchanged.

The [contract](../experiments/snapshotcheckpoint/README.md) records the eligibility
and integration limits. Native change stamps are the reuse evidence here; other
filesystems need an explicit trust policy. The returned snapshot is not proof
that source bytes remain current. Production integration must retain the live
selection guard through freezing, reverify it afterward, and preserve existing
body verification. Cache observations cannot restore authorization, conflict
decisions or accepted destination state.

## Method

The [driver](../scripts/benchmark_snapshot_checkpoint.py) builds one binary, then
launches a fresh process for every measurement. No in-memory index survives.
Filesystem/page caches remain warm. Each case has six pairs with alternating
control/candidate order, `GOMAXPROCS=2`, Go 1.27.1 and `CGO_ENABLED=0`. Tests and
race checks run separately before timing; race checks enable cgo.

Fixtures have directories of 100 files plus an empty `.errandignore`: 1,000 files
of 32 bytes, 10,000 of 4 KiB and 1,000 of 64 KiB. The policy file is counted in
hash/reuse checks, not in the table's fixture file count. Cache-miss cases delete
the checkpoint before each pair. Unchanged cases retain it. Edit cases replace
one file's body with distinct same-sized content before each pair. Mutation and
`sync` are outside timing. These fixtures exercise preparation, not Git's
tracked/untracked inventory or real application workloads.

`Total` is preparation plus wire hashing timed inside Go. JSON output, startup
and process teardown are excluded. The retained Python `elapsed` includes launch
and timeout-polling overhead and is not a precise CLI-startup measurement.
The comparison requires matching wire roots per pair, expected hashing counts
and expected checkpoint writes. Final results contain 108 samples per host.

Measurements ran natively on Mac mini / APFS / Darwin arm64 and Cabal / Btrfs /
Linux amd64. Reports retain OS/toolchain, actual scratch filesystem and mount
metadata, source/harness identities and executable hashes. Six pairs expose
large repeatable effects but do not establish statistical equivalence for small
differences. Hosts were not otherwise isolated from system activity.

## Final paired results

Times are median [minimum, maximum] in milliseconds. Ratio is the median of six
individual checkpoint/control ratios, so it need not equal the ratio of the two
displayed medians. Below 1 favors checkpoint reuse. Wins count faster checkpoint
preparations out of six. Only the final source revision is summarized here;
the two earlier passes remain separate evidence.

| Host | Files x bytes | Scenario | Cold ms | Checkpoint ms | Paired ratio | Wins |
|---|---|---|---:|---:|---:|---:|
| APFS | 1,000 x 32 | miss | 39.99 [35.09, 72.12] | 42.97 [39.49, 73.86] | 1.081 | 2/6 |
| APFS | 1,000 x 32 | unchanged | 38.77 [36.41, 64.37] | 33.98 [29.46, 48.67] | 0.821 | 4/6 |
| APFS | 1,000 x 32 | edit | 42.24 [36.52, 70.11] | 31.50 [28.83, 60.43] | 0.760 | 4/6 |
| APFS | 10,000 x 4,096 | miss | 173.18 [164.83, 199.43] | 204.00 [190.22, 226.80] | 1.191 | 1/6 |
| APFS | 10,000 x 4,096 | unchanged | 178.48 [164.15, 187.54] | 84.06 [71.54, 109.62] | 0.474 | 6/6 |
| APFS | 10,000 x 4,096 | edit | 176.84 [161.81, 200.11] | 86.82 [76.40, 116.71] | 0.501 | 6/6 |
| APFS | 1,000 x 65,536 | miss | 65.28 [64.31, 89.04] | 69.09 [66.01, 96.36] | 1.064 | 2/6 |
| APFS | 1,000 x 65,536 | unchanged | 78.11 [61.74, 107.90] | 31.01 [29.12, 61.16] | 0.392 | 6/6 |
| APFS | 1,000 x 65,536 | edit | 63.10 [61.12, 66.53] | 33.03 [29.96, 56.12] | 0.518 | 6/6 |
| BTRFS | 1,000 x 32 | miss | 20.85 [20.51, 21.10] | 26.76 [26.34, 27.43] | 1.297 | 0/6 |
| BTRFS | 1,000 x 32 | unchanged | 20.63 [20.46, 20.95] | 13.75 [13.40, 14.01] | 0.671 | 6/6 |
| BTRFS | 1,000 x 32 | edit | 20.66 [20.50, 20.92] | 16.43 [16.07, 16.81] | 0.793 | 6/6 |
| BTRFS | 10,000 x 4,096 | miss | 463.14 [384.41, 508.78] | 470.71 [383.62, 523.66] | 1.026 | 2/6 |
| BTRFS | 10,000 x 4,096 | unchanged | 441.77 [411.01, 556.22] | 111.73 [108.19, 122.91] | 0.252 | 6/6 |
| BTRFS | 10,000 x 4,096 | edit | 458.09 [383.96, 601.37] | 131.82 [124.67, 148.15] | 0.276 | 6/6 |
| BTRFS | 1,000 x 65,536 | miss | 229.41 [219.94, 280.78] | 259.42 [238.31, 303.39] | 1.112 | 0/6 |
| BTRFS | 1,000 x 65,536 | unchanged | 239.73 [222.57, 285.60] | 14.07 [13.55, 14.37] | 0.058 | 6/6 |
| BTRFS | 1,000 x 65,536 | edit | 240.32 [224.05, 262.64] | 16.76 [16.45, 20.99] | 0.071 | 6/6 |

The smallest APFS unchanged case wins only four of six final pairs. Its initial
pass had a paired ratio of 1.087, while the final pass has 0.821. That is not
evidence of a reliable small-workspace improvement. Large-body warm cases benefit
substantially from skipping file reads, especially on Cabal. Do not turn those
host-specific ratios into claims about Btrfs versus APFS.

## Remaining work by phase

For the 10K fixture, final checkpoint-path phase medians are below in milliseconds.
Each column is aggregated independently and the medians must not be added to
reconstruct median `Total`. Path canonicalization and cache-placement checks are
inside `Total`, outside these phase timers. Load includes checkout identity,
decoding and checkpoint metadata validation. Hash includes the changed-path
builder and its post-stat checks. Index includes reconstruction, comparison and
edits. Verify includes the selection guard and checkout identity recheck.

| Host / scenario | Selection | Load | Scan | Hash | Index | Verify | Save | Wire |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| APFS / unchanged | 34.59 | 4.84 | 15.46 | 0.00 | 3.03 | 23.21 | 0.00 | 3.13 |
| APFS / edit | 33.54 | 4.64 | 15.30 | 0.04 | 3.00 | 23.54 | 4.01 | 2.64 |
| BTRFS / unchanged | 18.96 | 13.27 | 34.08 | 0.00 | 15.53 | 19.74 | 0.00 | 12.21 |
| BTRFS / edit | 18.83 | 13.23 | 34.30 | 0.08 | 15.21 | 20.03 | 19.77 | 12.15 |

On the same 10K unchanged runs, the control's builder phase takes 112.21 ms on
APFS and 364.32 ms on Btrfs; checkpoint reuse reduces that phase to approximately
zero. The warm path still pays for selection, stat inventory, guard verification,
decoding, index construction and wire hashing. Those are the next measured costs.

The APFS 10K checkpoint is about 1.55 MiB, Btrfs about 1.40 MiB; native stamp
encoding differs. One-file edits hash one regular file but rewrite that entire
checkpoint. Btrfs save time is 19.77 ms in the final edit case. Index construction
is 3.00 ms on APFS and 15.21 ms on Btrfs there. Persisting the tree alone cannot
remove selection, stat or verification work, and a write journal alone cannot
remove unchanged-restart load costs.

Misses perform an additional observation scan, changed-path post-stat work and
checkpoint encoding/publication on top of ordinary preparation. The final 10K
Btrfs miss has broad timing overlap; it does not negate that extra work or prove
negligible overhead. Every host/fixture's final median is slower on a miss.

## Next bounded slice and roadmap

1. Address or explicitly bound initial-population cost. Investigate collecting
   reusable observations during the existing builder's reads and validation,
   rather than scanning and statting the full tree again on a miss. Preserve
   source-change detection and measure miss, unchanged and edit paths together.
2. Compare storage choices against this frozen prototype: restoring derived index
   state and publishing changed records address different measured phases.
   Include load/validation cost, full rebuild/compaction and interrupted updates.
   Do not select either solely on asymptotic cost or code size. Keep the current
   engine and fresh selection checks as controls.
3. Only then integrate the winning checkpoint path at shared preparation boundaries
   and run the complete-operation matrix: push, both fetch modes, watch restart,
   creation and ephemeral submission. Explicitly distinguish initial population
   from restart reuse and keep existing watch-session memory reuse inexpensive.

The earlier considered directory-cache eviction and source-lock experiments remain
after step 3 if capture is still a measured priority. Large-file deltas/chunking
remain step 4. This slice changes neither transaction boundaries nor the minimum
unit of body transfer. [The roadmap](SNAPSHOT_ENGINE_PLAN.md) tracks those gates.

## Validation and retained evidence

Native package tests, race tests and vet passed on both hosts for the final
candidate. Contracts cover reuse, backdated same-size edits, atomic replacement,
chmod, deletion, nested symlinks, policy/checkout/boot invalidation, corruption,
interrupted temporary-file cleanup, nonregular caches, concurrent writers,
cancellation, cache-write failure and APFS case-alias placement. The case-alias
test skips on case-sensitive filesystems. All 108 final pairs across the two hosts
produce equal wire roots with the expected hash/write behavior.

All three source versions and all six campaigns are retained under
[`benchmarks/snapshot-checkpoint`](benchmarks/snapshot-checkpoint/inputs.json).
Each campaign contains the original `report.json` and `logs.tar.gz`, which includes
every raw sample, seed run, test/race/vet output and environment record. Inputs
archives omit older `docs/benchmarks/` artifacts but retain all measured Go/module
sources, scripts and fixtures. Their Go-source digests and full Python harness
inventories were checked against their reports. The original final inputs match
the pre-review candidate; the follow-up retains revised inputs separately.
Archive SHA256 values are in `inputs.json`.

| Pass | Inputs | APFS job | Btrfs job |
|---|---|---|---|
| Initial | `initial-inputs.tar.gz` | `mac-mini/01M2HTF34MQPZ3WQ2AJ7PKRH7W` | `cabal/01M2HTF46C273BZ3ZHWR5CQBRH` |
| Verification | `verification-inputs.tar.gz` | `mac-mini/01M2HTRFRCKDWGSQACRM9S76G0` | `cabal/01M2HTRFR5TFAWYNMH9DNA2MVD` |
| Final | `inputs.tar.gz` | `mac-mini/01M2HTXJQ04D278VHY2AEQTCCN` | `cabal/01M2HTXJQ03RGV0A5ER2E248N0` |

The initial pass predates the explicit verification timer, filesystem identity
and additional cache-recovery checks. The verification pass includes those;
the final pass also rejects APFS case aliases that would put the cache inside
the source. They are retained to show the sequence, not pooled as repetitions of
one implementation. Reports live in `initial-apfs`, `initial-btrfs`,
`verification-apfs`, `verification-btrfs`, `apfs` and `btrfs` respectively.

To rerun, extract an inputs archive into an empty directory, use that directory
as the working directory, and choose a new output directory on the filesystem
under test:

```sh
python3 scripts/benchmark_snapshot_checkpoint.py --output /path/to/new-results --rounds 6
```
