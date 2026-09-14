# Receiver checkpoint reuse

This report records the request-local implementation committed as `de25693`.
See [the follow-up comparison](CHECKPOINT_FOLLOWUP.md) for subsequent changes.

## Decision and scope

Compare a bounded receiver optimization against `19faf4c`. Keep the retained
Merkle representation, 4,096-entry cutoff, wire identities, receipt format,
source acceptance rules and durability barriers fixed.

`TransferCheckpoint` retains one decoded, validated record and a lazy manifest
identity. Each read still opens the destination and private storage, verifies
their identities and paths, checks for a regular record, reads it with the
existing size limit, and compares the exact bytes with its retained record. Matching bytes reuse
validation and decoding; different bytes are decoded and validated again. Relationship
checks run on cache hits as well. This deliberately avoids filesystem timestamp
assumptions on APFS or Btrfs, including same-size edits with restored mtime.

Publication clears the cache before attempting a write, including writes that
fail after rename. A new handle or restarted process starts cold. Separate
handles see another writer's published bytes on their next read. The caller's
existing destination transfer lock still serializes checkpoint and application
operations. The cache does not grant authority to skip a revision check or to
reuse observations of files modified by a running application.

`TransferSession` now has pointer receivers so staging, apply, recovery and
initialization share that one cache. Production sessions are created within a
request or local transfer operation; no daemon-wide map retains workspaces.
Internal operations borrow immutable checkpoint metadata and its lazy root.
Public checkpoint versions export independent manifest slices.

This is shared by the receiver side of push/watch and persistent-workspace
fetch. Workspace-origin initialization uses the same checkpoint implementation.
Ephemeral fetch, fresh workspace creation and submission are also measured as
regression controls; they do not all perform repeated checkpoint reads and are
not promised the same gain. No receiver-specific copy of the snapshot engine is
introduced.

## Validation contract

New tests exercise public manifest ownership, in-place record edits with restored
mtime, atomic record replacement, missing records, symlink records, changed
relationships and advancement through a separate handle. Existing checkpoint
and session suites continue to cover refusal, partial conflicts, selection,
receipt replay, storage replacement, source versus destination merge bases,
recovery and garbage collection. Those suites run with reuse enabled.

The native comparison uses identical benchmark drivers in frozen baseline and
candidate trees. Both run full tests, vet and the harness's scoped race suite on
each host. Seven adjacent pairs alternate variant order for each workload.
Fixtures live on the measured native filesystem, not an assumed `/tmp` mount.
The `receiver` scope includes all ten complete-command workloads plus 1K and
10K checkpoint reads. The latter include record I/O, path guards and the public
manifest copy, with the first cold read included in each timed run.

Use the existing paired median intervals for interpretation. Seven pairs yield
the complete observed ratio range with 98.4375% coverage under the independent
pair assumption. A complete-operation speed claim requires that interval below
parity; the watch goal remains at least 10% median improvement on both hosts.
A confirmed slowdown above 5% in another operation fails the regression gate.
Component wins alone do not establish an end-to-end speedup. Times are medians
of per-round benchmark averages, not individual-edit percentiles. This measures
native loopback operations, not cross-host network latency.

## Rejected SHA-keyed cache

The first candidate hashed each record to identify a cache hit and eagerly
computed its manifest identity on every cold read. Cabal's seven paired rounds
showed a strong warm-read improvement (10K: 36.214 to 6.923 ms), but ordinary
watch remained inconclusive (497.660 to 511.893 ms; paired ratio 1.008,
interval [0.941, 1.082]). Only nested atomic saves established a complete-command
improvement, approximately 3%. No comparison established a slowdown above 5%.

A bounded diagnostic over 20 warm watch cycles counted 120 reads and 60 hits.
The cache was being used; repeated hashing and eager cold identities still
consumed CPU. `TransferCheckpoint.read` occupied 2.120 seconds cumulatively in
7.680 seconds of CPU samples; SHA-256 under checkpoint reads occupied 0.740
seconds. Those figures overlap and are not additive wall-clock measurements.
The diagnostic's instrumented latency is excluded from the paired comparison.

This evidence prompted an exact-byte cache and lazy identity instead. Retaining
one raw record avoids the hashing cost and detects any byte change directly.
The SHA-keyed Mini run was deliberately stopped after three complete rounds;
its 36 matched pairs and raw logs remain preserved. It is not presented as a
seven-round run, and no observations are mixed into the corrected comparison.

- [SHA-keyed source overlays](benchmarks/2026-09-13-checkpoint-reuse-sha-inputs.tar.gz)
- [Superseded measurements and logs](benchmarks/2026-09-13-checkpoint-reuse-sha.json)
- [Read-hit and CPU diagnostic](benchmarks/2026-09-13-checkpoint-reuse-sha-profile.json)
- [Raw profiles and diagnostic overlays](benchmarks/2026-09-13-checkpoint-reuse-sha-profile.tar.gz)

## Corrected comparison

[Exact frozen source overlays](benchmarks/2026-09-13-checkpoint-reuse-inputs.tar.gz)
contain the baseline and corrected candidate. Both use the identical 12-case
receiver harness. Seven paired rounds are complete on both native filesystems,
with 84 unique case/round observations per variant per host. [Raw reports, paired comparisons and validation logs](benchmarks/2026-09-13-checkpoint-reuse.json)
are retained.

### Cabal / Btrfs

Each variant has 84 unique case/round observations. Source hashes match the
frozen inputs and all seven execution orders match the alternating schedule.
Both variants passed full tests, vet and the scoped race suite on Go 1.27.1.

Repeated 10K checkpoint reads improve from 33.486 to 2.411 ms, paired ratio
0.071 and interval [0.058, 0.079]. Receiver staging within the ordinary watch
case also improves in every pair: 213.1 to 181.7 ms, paired ratio 0.878,
interval [0.795, 0.943]. These support the targeted component improvement.

Ordinary watch improves in six of seven pairs: 520.000 to 455.958 ms, paired
ratio 0.877 and interval [0.698, 1.025]. Its median clears the 10% goal, but the
interval still crosses parity, so the strict complete-watch gate does not pass.
Nested atomic saves improve in all seven pairs, with ratio 0.926 and interval
[0.820, 0.972]. Other complete-command comparisons are inconclusive.

No comparison establishes a slowdown above 5%. This does not mean zero
regressions: ephemeral fetch, creation and submission have worse point estimates,
and some fetch samples are especially noisy. All observations remain included.

Times below are milliseconds; ratios are medians of paired ratios and may differ
from the ratio of the displayed medians.

| Workload | Baseline | Corrected | Paired ratio | Result |
|---|---:|---:|---:|---|
| CheckpointRead/1000 | 2.853 | 0.586 | 0.204 | faster |
| CheckpointRead/10000 | 33.486 | 2.411 | 0.071 | faster |
| FetchCompletion/persistent=false | 94.027 | 106.512 | 1.133 | inconclusive |
| FetchCompletion/persistent=true | 240.446 | 250.661 | 1.004 | inconclusive |
| PushPhases | 926.623 | 865.542 | 0.932 | inconclusive |
| WatchPhases | 520.000 | 455.958 | 0.877 | inconclusive |
| WatchWorkloads/git-atomic | 978.863 | 992.050 | 0.938 | inconclusive |
| WatchWorkloads/nested-atomic | 687.904 | 630.477 | 0.926 | faster |
| WatchWorkloads/small | 242.589 | 237.300 | 0.965 | inconclusive |
| WatchWorkloads/structural | 617.290 | 571.618 | 0.887 | inconclusive |
| WorkspaceCreationAndSubmission/ephemeral-job | 756.601 | 764.781 | 1.035 | inconclusive |
| WorkspaceCreationAndSubmission/workspace-create | 1722.270 | 1759.267 | 1.034 | inconclusive |

### Mac mini / APFS

The initial job completed five rounds, with 60 unique observations per variant.
It was deliberately stopped at that round boundary before its two-hour runtime
cap could interrupt another long paired workload. The final two rounds completed
on the same Mini using the retained native binaries. The continuation verified
source, binary and harness hashes, Go version, architecture and fixture
filesystem before running. Final checks verified the original samples unchanged
and all seven execution orders against the alternating schedule;
there was no one-sided sample to replace at this boundary. The boundary was
chosen from progress and the runtime allowance, before inspecting timing results.

[Continuation helper and original report](benchmarks/2026-09-13-checkpoint-reuse-resume-inputs.tar.gz)
preserve the exact procedure and job identities. The final report records the
continuation provenance separately from the observations.


Both variants passed full tests, vet and the scoped race suite on Go 1.27.1.
Repeated 10K checkpoint reads improve from 7.254 to 0.368 ms, paired ratio 0.050
and interval [0.045, 0.051]. Receiver staging improves in every pair: 95.06 to
86.88 ms, paired ratio 0.910 and interval [0.820, 0.939].

Ordinary watch is 406.464 to 397.875 ms, paired ratio 0.968 and interval
[0.892, 1.025]. It improves in six of seven pairs, but neither its point estimate
nor its interval meets the complete-watch goal. Structural saves improve in all
seven pairs, with ratio 0.954 and interval [0.823, 0.988]. Other complete-command
comparisons are inconclusive.

No comparison establishes a slowdown above 5%. Ephemeral submission has a worse
paired point estimate, 1.290, with a broad [0.751, 1.467] interval; nested saves
and small-workspace watch also have worse point estimates. Push includes a large
baseline outlier, reflected in its [0.078, 1.030] interval. Those observations
remain included and do not justify a claim of zero regressions.

| Workload | Baseline | Corrected | Paired ratio | Result |
|---|---:|---:|---:|---|
| CheckpointRead/1000 | 0.796 | 0.120 | 0.147 | faster |
| CheckpointRead/10000 | 7.254 | 0.368 | 0.050 | faster |
| FetchCompletion/persistent=false | 126.792 | 119.684 | 0.958 | inconclusive |
| FetchCompletion/persistent=true | 272.221 | 261.547 | 0.945 | inconclusive |
| PushPhases | 557.874 | 538.415 | 0.952 | inconclusive |
| WatchPhases | 406.464 | 397.875 | 0.968 | inconclusive |
| WatchWorkloads/git-atomic | 1026.546 | 1007.595 | 0.974 | inconclusive |
| WatchWorkloads/nested-atomic | 511.011 | 526.196 | 1.030 | inconclusive |
| WatchWorkloads/small | 343.732 | 345.768 | 1.004 | inconclusive |
| WatchWorkloads/structural | 509.287 | 485.165 | 0.954 | faster |
| WorkspaceCreationAndSubmission/ephemeral-job | 1857.583 | 2298.888 | 1.290 | inconclusive |
| WorkspaceCreationAndSubmission/workspace-create | 1882.333 | 1861.546 | 1.015 | inconclusive |

## Assessment

Keep this as a bounded, measured receiver optimization. Exact-byte reuse and
lazy identities make repeated reads approximately 14 times faster on Btrfs and
20 times faster on APFS, using paired ratios. Receiver staging improves in all
seven pairs on each host, approximately 12% and 9% respectively. This confirms
that the saving reaches a real command phase rather than only a warm microbench.

The complete-watch goal does not pass on either host. Complete-command evidence
supports Btrfs nested atomic saves and APFS structural saves; other operations
remain inconclusive. No comparison establishes a regression above 5%, but the
noisy and adverse point estimates remain uncertainty, not proof of equivalence.
This slice does not establish near-instant watch, a cross-host improvement or a
new rsync/Mutagen comparison. The selected snapshot representation and durability
barriers remain unchanged; further work should target the remaining measured
application and persistence costs separately.

## Reproduction

From this repository, reconstruct both frozen variants over the same parent:

```sh
bench_root=$(mktemp -d)
mkdir "$bench_root/baseline" "$bench_root/candidate"
git archive 19faf4c | tar -x -C "$bench_root/baseline"
git archive 19faf4c | tar -x -C "$bench_root/candidate"
tar -xzf docs/benchmarks/2026-09-13-checkpoint-reuse-inputs.tar.gz -C "$bench_root"
python3 "$bench_root/run.py"
```

Run on each native host, allowing enough time for full tests, vet, race checks
and seven paired rounds. `results/report.json` records the source, harness and
binary hashes, execution order, filesystem and observations. The harness places
fixtures under `results/fixtures`, verifies that filesystem, and uses
`GOMAXPROCS=2`. `scripts/summarize_snapshot_benchmarks.py` produces paired
comparisons from the resulting directory. The checkpoint microbenchmark mostly
measures warm reuse; complete-command benchmarks include the actual mixture of
cold reads and hits within each operation.
