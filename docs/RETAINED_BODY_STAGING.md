# Retained-body receiver staging

This follows [direct receiver staging](DIRECT_STAGING.md). Its comparison baseline
is the reviewed direct-staging implementation, including its uncommitted files,
not bare `f85e950`. The frozen baseline production hash is
`9bfb4e20ce0dbed669dda7291d160ad8afedc8b16d795913f488e2bee8453d09`.

## Changes

Persistent fetch previously reconstructed its remote delta from retained blobs
into `.source-*/change-base`. Staging then copied those files into the attempt's
`remote/` tree, verifying and synchronizing the same contents a second time.
`StagePreparedFromBlobs` now supplies retained bodies directly to staging.

Staging also constructs `base/` directly under the private attempt. It no longer
publishes an intermediate `change-base`, synchronizes that publication, and
renames it to `base`.

Both trees use the shared verified materializer. Push/watch still read their
frozen upload trees; fetch reads retained blobs. Exact lengths, content hashes,
independent copies, logical permissions, quotas, source identity checks, worker
joining, member synchronization and tree barriers remain in place. This slice
does not consolidate the remaining full-tree barriers.

Symlinks are created after files and implicit directories. This makes the writer
reject case-insensitive symlink-parent collisions instead of writing through a
link it just created. Rejected copies are closed and discarded without flushing
their unusable data. The preliminary file-stat pass remains unchanged; no
performance claim depends on removing it.

## Publication and regression coverage

If the attempt rename succeeds but the parent-directory synchronization fails,
staging now removes the published name and synchronizes that removal. Retrying
an existing attempt also repeats the parent barrier before returning success.
This fixes a publication-retry weakness that predated direct staging.

Added coverage includes:

- A 32-file, three-level restricted source, with successful and corrupt-copy
  paths, source permission restoration and unchanged checkpoint revision.
- Failed publication followed by retry, including failure of the retry barrier.
- Retained-body staging after deleting the original source, missing/corrupt
  blobs, quota enforcement, retry identity and independent staged copies.
- A symlink-parent alias on case-insensitive filesystems and rejected content
  that must not reach synchronization or publication.

After timing freeze, the symlink fixture was strengthened to create its target
before its alias. This prevents the old writer from failing merely because the
link is dangling. The corrected regression fails against the frozen baseline
and passes under the race detector on local APFS. The archive keeps this final
test-only overlay separately; production and benchmark sources are unchanged.

## Measurement contract

Frozen baseline and candidate receive identical benchmark sources. Seven
alternating pairs cover small/batch/large staging, small/batch/large persistent
fetch, ordinary push and watch. Each observation averages three operations.

The new fetch workloads change 128 files of 4 KiB each or one 8 MiB file. Their
deterministic pseudorandom contents are identical between variants and resist
filesystem compression. Edits happen in an idle persistent workspace, then a
completed job captures the result. Fetch timing includes download and local
application; edits, remote execution, capture and final content verification
are outside the timer. The original staging microbenchmarks retain their
compressible repeated-byte fixtures.

Runs use GOMAXPROCS=2 and native APFS/Btrfs fixtures, including for validation.
They exercise local client/daemon APIs over loopback, excluding CLI startup and
cross-host transport. They are not rsync/Mutagen comparisons or measurements of
application readiness. Workspace creation and ephemeral submission do not call
the changed staging path and are outside this campaign.

The harness records source, production, benchmark and binary hashes, filesystem
mount options, per-observation host load, execution order and raw logs. Each
benchmark process group has a 300-second outer timeout; partial logs survive a
timeout. Baseline and candidate receive full Go tests, vet and race checks before
measurement. The harness verifies source stability after the last observation.

The [frozen overlays and executed driver](benchmarks/2026-09-14-retained-body-staging-inputs.tar.gz)
reproduce both source hashes from `f85e950`.

## Cabal / Btrfs results

All 112 observations completed. Baseline and candidate passed full tests, vet
and race checks on native Btrfs. The fixture mount uses `compress=zstd:3`; the
new random-body fetch workloads avoid compressible-payload distortion.

Times are medians of observation means in milliseconds. Paired ratios divide
candidate by baseline in each adjacent pair. Their median can differ from the
ratio of the two separate medians. Ranges span the seven paired ratios and are
not simultaneous confidence bounds across workloads.

| Workload | Baseline ms | Candidate ms | Median paired ratio | Pair range | Faster pairs |
|---|---:|---:|---:|---:|---:|
| Stage one small file | 29.6 | 26.5 | 0.877 | 0.710–1.037 | 6/7 |
| Stage 128 files | 142.3 | 127.1 | 0.975 | 0.505–1.391 | 4/7 |
| Stage one 8 MiB file | 92.8 | 89.1 | 0.960 | 0.852–1.087 | 6/7 |
| Fetch one small file | 216.8 | 206.3 | 0.973 | 0.456–1.321 | 4/7 |
| Fetch 128 files | 6524.8 | 6303.5 | 0.959 | 0.921–1.027 | 6/7 |
| Fetch one 8 MiB file | 961.6 | 855.0 | 0.888 | 0.690–0.994 | 7/7 |
| Watch edit to receipt | 402.6 | 374.3 | 0.943 | 0.715–1.168 | 6/7 |
| Push | 792.2 | 788.7 | 1.023 | 0.918–1.243 | 3/7 |

The 8 MiB fetch improved in every pair, with a median paired reduction of
11.2%. Other comparisons remain inconclusive. The batch-fetch reduction is
encouraging but one pair was slower. No comparison establishes a consistent
regression. In particular, the lower separate push medians do not establish an
improvement: its paired median ratio is 1.023 and its range crosses 1.

## Mac mini / APFS results

Baseline and candidate passed full tests, vet and race checks on native APFS.
Seven workloads completed all seven pairs. Push completed six pairs; the final
baseline process exceeded its 300-second limit before emitting a timing result,
and its candidate counterpart did not run. The campaign therefore contains 110
completed observations, one failed observation and one unrun observation.
No failed sample was replaced, and the six-pair push comparison is descriptive.

| Workload | Baseline ms | Candidate ms | Median paired ratio | Pair range | Faster pairs |
|---|---:|---:|---:|---:|---:|
| Stage one small file | 30.0 | 25.8 | 0.844 | 0.769–0.964 | 7/7 |
| Stage 128 files | 54.6 | 45.2 | 0.875 | 0.461–1.060 | 5/7 |
| Stage one 8 MiB file | 141.6 | 122.9 | 0.844 | 0.224–4.753 | 4/7 |
| Fetch one small file | 228.0 | 210.4 | 0.941 | 0.631–1.001 | 6/7 |
| Fetch 128 files | 6613.5 | 6148.1 | 1.002 | 0.648–1.063 | 3/7 |
| Fetch one 8 MiB file | 1627.0 | 1906.3 | 1.089 | 0.798–5.986 | 3/7 |
| Watch edit to receipt | 348.4 | 344.8 | 0.959 | 0.739–1.143 | 4/7 |
| Push (six completed pairs) | 495.5 | 499.2 | 0.993 | 0.189–8.897 | 4/6 |

Small-file staging improved in every pair, with a median paired reduction of
15.6%. Other APFS comparisons remain inconclusive. Large-file fetch has an
adverse median signal: the paired median is 8.9% slower, with candidate faster
in only three pairs. Its very wide range prevents a firm regression conclusion;
it also prevents claiming an APFS large-fetch improvement. The lower separate
batch-fetch medians are likewise not proof of a gain: its paired median is 1.002.

Completed benchmark processes took up to 291.2 seconds, although the timer
excludes setup, job capture and verification. For example, a baseline watch
observation measured 400.2 ms/op inside a 289.0-second process. These records
show substantial untimed overhead, but do not identify its cause. The timeout
censors the final push sample, so this campaign cannot clear APFS push of risk.

The failed driver stopped before its normal benchmark-binary cleanup. The
original Errand job subsequently reported incomplete output retention with
`change_deadline`. A separate completed collector job recovered `report.json`
and all 127 text logs without the benchmark executables. Both source hashes
were independently rechecked on the runner after the timeout and still matched.
The combined record preserves the timeout, incomplete retention and recovery
job identifiers. The original Errand benchmark job is terminal.

## Decision and remaining work

Keep this bounded implementation ready for review: it fixes publication retry
correctness, removes duplicate persistent-fetch reconstruction, and shares the
verified materializer across directory and retained-body sources. Its measured
benefits are the consistent Btrfs large-fetch reduction and APFS small-stage
reduction. It does not establish a general end-to-end push/watch improvement or
prove that every workload is regression-free.

Before making a broader performance claim, profile APFS large-file fetch and
separate benchmark setup/cleanup from the timed operation. The benchmark driver
also needs failure-safe scratch cleanup before another long remote campaign;
its partial-log preservation worked, but retaining the entire failed output
was unsuccessful. These limitations are recorded rather than changing the
measurement contract after the failure.

The preliminary source-stat checks and successful durability barriers remain.
Initial workspace creation and ephemeral job population remain the next shared
staging integration, outside this receiver slice. No snapshot representation
or Merkle crossover decision changed.

[Complete reports, paired comparisons, hashes and raw logs](benchmarks/2026-09-14-retained-body-staging.json)
include all completed samples and the failed push log. The input archive
preserves the actual executed driver and the separate final test-only overlay.

