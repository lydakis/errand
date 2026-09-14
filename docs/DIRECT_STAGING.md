# Direct receiver staging

Baseline: `f85e950` (`Reuse immutable checkpoints across receiver requests`).

This records the original direct-staging measurement freeze. Subsequent feedback
fixes and the comparison against this slice are tracked in
[retained-body staging](RETAINED_BODY_STAGING.md).

## Scope

Persistent push/watch and persistent fetch now build their final private
`base` and `remote` trees directly. Previously, staging reconstructed the base,
packed both trees into temporary tar archives, extracted them again, synchronized
the extracted files, then discarded the archives and first base tree.

The receiver now reconstructs the base once and copies incoming source bodies
directly into the remote tree. One materializer handles both retained blob
readers and source-file readers. It verifies hashes and exact lengths while
copying, bounds concurrent file work with the existing staging worker pool,
sets final modes, and synchronizes directories from children to parents.
Retained-blob reconstruction uses that same implementation wherever it is called.

This removes the internal archive/extract round trip. It does not replace
network archive transport, the source-freezing path, or initial job/workspace
creation with a new transfer protocol. Those remain separate integration work
in stage 2 of [the shared-engine sequence](SNAPSHOT_ENGINE_PLAN.md).

## Preserved contracts

- Bundle validation and metadata/body limits precede staging.
- Source types, symlink targets, sizes and physical modes are checked; file
  descriptors are checked again after copying. Temporary source access is
  restored after all workers have joined, including on error.
- Content is copied into independent files, never linked to mutable sources.
- Every file and directory receives its member synchronization before the
  full tree barrier. The existing Darwin batched flush and Linux fsync helpers
  remain in use. Bundle metadata and tree names are durable before attempt
  publication; the attempt rename retains its parent-directory barrier.
- Checkpoint revisions, accepted-source semantics, apply journals, conflicts,
  retry identities and public staging layout are unchanged. The selected
  contiguous/Merkle snapshot representation remains fixed.

The new tests cover restricted tree modes, changed symlinks, independent staged
contents, corrupt source/base bodies, file-to-symlink substitution, cancellation,
and synchronization failures. Existing transfer, recovery and blob-store tests
exercise the shared implementation. The stronger symlink/independence test was
added after timing freeze; production and benchmark source are unchanged.

## Measurement contract

Frozen baseline and candidate run on native APFS and Btrfs with GOMAXPROCS=2.
The baseline receives only the identical new staging benchmark. Binaries are
compiled once per host; reports retain source, binary and harness hashes,
filesystem identity, raw output, iteration counts, execution order and load.

Seven alternating pairs measure complete watch, ordinary push, persistent fetch,
and isolated staging of one small file, 128 files and one 8 MiB changed file.
Three alternating pairs screen ephemeral fetch, workspace creation, submission,
atomic saves and structural edits. An untimed watch run warms each variant.
The stage microbenchmark excludes fixture creation and post-stage verification;
each stage is verified and removed outside the timer to bound retained history.
The staging fixtures use repeated-byte bodies (base `A`, incoming `B`), with
identical bodies across the 128-file batch. They exercise verified copying and
filesystem publication, not incompressible payloads or network compression.

Report medians and paired ratios. Seven-pair ranges are per-comparison median
bounds under independent pairs; they are not simultaneous confidence across all
workloads. Three pairs cannot establish equivalence. Keep outliers, investigate
repeatable regressions and distinguish staging gains from complete-operation
gains. Native loopback timings are not cross-machine or rsync/Mutagen comparisons.
The operation fixtures exercise the client and daemon APIs over loopback; CLI
process startup and cross-host transport are outside these measurements. Fetch
includes download and local apply, with remote execution/result capture excluded.

[Frozen overlays and executed driver](benchmarks/2026-09-14-direct-staging-inputs.tar.gz)
reproduce the measurement trees from the baseline commit. The archive separately
retains the final test-only overlay.

## Results

Both Cabal/Btrfs and Mac mini/APFS completed all 114 observations using Go 1.27.1.
All times below are medians of per-sample operation means, in milliseconds; paired ratios
divide candidate by baseline within each adjacent pair. A ratio below 1 is faster.
Ratios need not equal the ratio of the two separately calculated medians.

| Cabal workload | Baseline | Candidate | Median paired ratio | Pair range | Faster pairs |
|---|---:|---:|---:|---:|---:|
| Stage one small file | 53.4 | 28.8 | 0.583 | 0.520–0.627 | 7/7 |
| Stage 128 files | 683.5 | 154.3 | 0.226 | 0.173–0.330 | 7/7 |
| Stage one 8 MiB file | 208.1 | 90.5 | 0.435 | 0.433–0.447 | 7/7 |
| Watch edit to receipt | 406.4 | 380.5 | 0.944 | 0.863–1.575 | 6/7 |
| Push | 814.2 | 805.1 | 0.991 | 0.857–1.055 | 4/7 |
| Persistent fetch | 236.7 | 216.8 | 0.916 | 0.625–1.126 | 6/7 |

| Mac mini workload | Baseline | Candidate | Median paired ratio | Pair range | Faster pairs |
|---|---:|---:|---:|---:|---:|
| Stage one small file | 44.2 | 30.7 | 0.654 | 0.497–0.791 | 7/7 |
| Stage 128 files | 80.1 | 42.4 | 0.490 | 0.425–0.859 | 7/7 |
| Stage one 8 MiB file | 118.7 | 66.2 | 0.487 | 0.246–0.961 | 7/7 |
| Watch edit to receipt | 348.9 | 344.9 | 0.995 | 0.775–1.247 | 4/7 |
| Push | 497.9 | 485.9 | 0.962 | 0.040–4.468 | 6/7 |
| Persistent fetch | 238.3 | 222.1 | 0.932 | 0.867–0.974 | 7/7 |

The isolated staging improvements are repeatable on both filesystems. APFS
persistent fetch also improved in every pair. Other full-operation improvements
remain uncertain. In Cabal ordinary push, the measured staging handler fell from
169.7 to 143.2 ms, while client-side work remained approximately 489 ms and apply
varied. Eliminating one component's work does not eliminate the other costs.

On APFS, the watch staging handler improved in every pair (74.85 to 65.57 ms
median), while complete watch latency was essentially flat. Apply and client-side
work remain larger costs. APFS push includes a 12.40-second baseline observation
and a 2.16-second candidate observation; their client-side components were 12.14
and 1.93 seconds. Both are retained, and their cause is not established here.

The original three-pair control screens were:

| Workload | Cabal median paired ratio (range) | APFS median paired ratio (range) |
|---|---:|---:|
| Ephemeral fetch | 1.054 (0.924–1.172) | 0.987 (0.985–0.996) |
| Workspace creation | 0.974 (0.751–1.145) | 1.240 (1.103–1.408) |
| Ephemeral submission | 1.046 (1.021–1.141) | 0.830 (0.556–1.317) |
| Atomic saves | 0.943 (0.810–1.191) | 1.016 (0.939–1.027) |
| Structural watch | 1.170 (0.531–1.808) | 0.961 (0.949–0.971) |

### Cabal control investigation

The original three-pair controls showed adverse signals for ephemeral submission
(median paired ratio 1.046, slower in 3/3 pairs) and structural watch (1.170,
slower in 2/3). Ephemeral submission does not call the changed production path.
Both received a separate seven-pair comparison and seven same-baseline-binary
control pairs, with unchanged production and benchmark sources.

| Follow-up workload | Candidate/baseline median ratio | Pair range | Faster pairs | Same-binary median ratio | Same-binary range |
|---|---:|---:|---:|---:|---:|
| Ephemeral submission | 0.888 | 0.633–1.028 | 6/7 | 0.985 | 0.848–1.137 |
| Structural watch | 0.929 | 0.810–1.272 | 5/7 | 0.997 | 0.762–1.206 |

Neither adverse signal reproduced consistently in matched pairs. This does not
establish equivalence or a new speedup: structural watch's separate baseline and
candidate medians were 524.7 and 607.1 ms, despite its favorable median paired
ratio. The same-binary spread demonstrates substantial measurement variability.
Keep the original and follow-up results separate and retain every observation.

### APFS creation investigation

Workspace creation does not call the changed materialization path. Its original
three-pair slowdown received a separate seven-pair comparison and seven
same-baseline-binary control pairs, using unchanged source inputs.

The follow-up baseline/candidate medians were 1,803.9/1,541.9 ms. The median paired
ratio was 0.941 (range 0.729–1.176), with the candidate faster in 4/7 pairs.
The same-binary median ratio was 0.940, with a much wider 0.734–32.578 range.
Two baseline-only observations averaged 17.11 and 34.78 seconds per operation.
These stalls are retained and their cause is unresolved. The original adverse
signal did not reproduce consistently; creation performance remains inconclusive.

## Validation and decision

Both baseline and candidate passed the full Go test suite on each host. Candidate
`go vet ./...` and race checks covering changes, daemon, client and CLI packages
passed on both hosts. After the final test-only additions, the staging regression
tests passed under the race detector locally, on Cabal and on Mac mini. The
production source hash still matches the measured candidate exactly.

Adopt this bounded receiver-staging change for its repeatable staging reductions
on both filesystems and the shared verified materialization implementation.
APFS persistent fetch has a measured full-operation gain; no reliable full watch
or ordinary push gain is claimed. No consistent new full-operation regression
was established by the follow-ups, but equivalence and general tail-latency
improvements are not established either.

This completes the direct receiver portion of sequence step 2. Initial workspace
and job population still use their existing path. Their integration and the
unexplained initialization/client stalls remain subsequent work; this change
does not claim to resolve them.

[Complete reports, comparisons, source/binary hashes and raw logs](benchmarks/2026-09-14-direct-staging.json)
contain 228 primary/control observations and 84 follow-up observations. The
[input archive](benchmarks/2026-09-14-direct-staging-inputs.tar.gz) includes the
executed drivers, frozen overlays, final test-only overlay and comparison helper.
