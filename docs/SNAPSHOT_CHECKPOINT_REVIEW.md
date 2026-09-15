# Checkpoint review fixes and validation

## Decision and changes

Keep the checkpoint prototype isolated and ready for another code review. The
review fixes and the small "include now" items are implemented. Production
callers, the adaptive contiguous/Merkle engine, wire protocol and production
fsync behavior are unchanged. Cache misses and small APFS workloads still need
work before adoption.

- Directory post-build checks preserve native identity, type, full mode and the
  builder's observed mode. They tolerate size/timestamp changes caused by ignored
  siblings. Fresh selection verification still rejects membership changes, and
  regular files/symlinks retain full fingerprint checks. Refreshed directory
  observations are recorded before checkpoint publication.
- Checkout roots become absolute before resolving symlinks. Relative and absolute
  invocations through a symlink-spelled working directory reuse the same cache.
- An additional empty-selection regression found during verification is fixed:
  reconstructed snapshots retain the cold builder's nil entry list. JSON `null`
  and `[]` differ, so allocating an empty slice had changed the wire root.
- Cache reads, checksums, validation and writes now use the caller's cancellation
  context. Format 2 stores gob batches of 256 observations with checks between
  batches; individual reads are capped at 64 KiB. Encoding checks cancellation
  before starting. A single record or filesystem syscall remains indivisible.
  Cancelled loads propagate cancellation rather than triggering a cache miss;
  cancelled writes do not publish partial generations. This remains one full
  checkpoint replacement, not a journal or persisted Merkle-node format.
- The benchmark checks hash, reuse, write and cache-status behavior for both
  modes. Every sample records whether it ran first or second in the pair.

Regression tests use the real builder and a per-call seam to schedule mutations
between construction and verification. Ignored siblings succeed on both miss and
hit paths; selected siblings, directory mode changes/replacement and file edits
are rejected. Relative-root, empty-selection, codec cancellation, malformed batch
and cancellation/publication cases have durable coverage.

## Final native results

The final candidate was tested with the same three fixtures and six alternating
pairs per case as the [pre-review experiment](SNAPSHOT_CHECKPOINT.md). Each mode
starts in a fresh Go process with warm filesystem caches. Timings include source
preparation plus wire hashing; they exclude process startup, transfer, staging
and application readiness. Both modes still prepare the same adaptive index.

Times below are median [min, max] milliseconds. Paired ratio is the median of
checkpoint/control ratios within each pair, not a ratio of the displayed medians.
Wins count faster checkpoint runs out of six. Host names include their filesystem
to avoid interpreting a comparison of different machines as a filesystem study.

| Host / filesystem | Files x bytes | Scenario | Cold ms | Checkpoint ms | Paired ratio | Wins |
|---|---|---|---:|---:|---:|---:|
| Mac mini / APFS | 1,000 x 32 | miss | 43.59 [34.93, 63.69] | 54.75 [38.71, 71.83] | 1.118 | 1/6 |
| Mac mini / APFS | 1,000 x 32 | unchanged | 35.95 [35.33, 45.71] | 33.74 [26.87, 43.34] | 0.873 | 4/6 |
| Mac mini / APFS | 1,000 x 32 | edit | 37.23 [35.37, 52.84] | 31.52 [28.42, 50.41] | 0.835 | 4/6 |
| Mac mini / APFS | 10,000 x 4,096 | miss | 167.67 [163.74, 192.14] | 202.91 [189.33, 224.13] | 1.217 | 1/6 |
| Mac mini / APFS | 10,000 x 4,096 | unchanged | 173.59 [162.31, 191.65] | 76.65 [71.60, 106.48] | 0.452 | 6/6 |
| Mac mini / APFS | 10,000 x 4,096 | edit | 166.83 [163.57, 198.07] | 90.25 [76.44, 104.98] | 0.541 | 6/6 |
| Mac mini / APFS | 1,000 x 65,536 | miss | 64.48 [61.57, 78.31] | 68.12 [65.36, 93.51] | 1.069 | 1/6 |
| Mac mini / APFS | 1,000 x 65,536 | unchanged | 76.43 [63.78, 92.19] | 32.98 [29.64, 51.82] | 0.434 | 6/6 |
| Mac mini / APFS | 1,000 x 65,536 | edit | 79.82 [63.08, 86.33] | 33.25 [30.09, 56.83] | 0.410 | 6/6 |
| Cabal / Btrfs | 1,000 x 32 | miss | 20.83 [20.56, 21.10] | 26.43 [26.06, 27.00] | 1.270 | 0/6 |
| Cabal / Btrfs | 1,000 x 32 | unchanged | 20.76 [20.51, 21.14] | 14.11 [13.35, 16.90] | 0.679 | 6/6 |
| Cabal / Btrfs | 1,000 x 32 | edit | 20.77 [20.47, 20.97] | 16.48 [15.71, 16.83] | 0.790 | 6/6 |
| Cabal / Btrfs | 10,000 x 4,096 | miss | 443.36 [411.75, 554.48] | 461.87 [390.51, 527.84] | 1.113 | 2/6 |
| Cabal / Btrfs | 10,000 x 4,096 | unchanged | 454.79 [422.10, 546.34] | 112.74 [102.82, 114.60] | 0.240 | 6/6 |
| Cabal / Btrfs | 10,000 x 4,096 | edit | 419.15 [362.04, 501.72] | 133.78 [123.77, 144.78] | 0.319 | 6/6 |
| Cabal / Btrfs | 1,000 x 65,536 | miss | 262.14 [212.30, 314.09] | 276.53 [240.71, 307.56] | 1.088 | 3/6 |
| Cabal / Btrfs | 1,000 x 65,536 | unchanged | 241.65 [228.29, 255.25] | 14.30 [13.65, 14.81] | 0.059 | 6/6 |
| Cabal / Btrfs | 1,000 x 65,536 | edit | 260.67 [237.61, 291.88] | 16.57 [16.23, 20.07] | 0.066 | 6/6 |

These runs verify that substantial warm gains remain. They are not an interleaved
comparison between the pre-review and fixed implementations, so small changes
across campaigns cannot establish equivalence or a regression caused by these
fixes. The position effect and six-pair sample size still limit small-effect
claims. The prior measurements are retained without pooling or rewriting them.

## Execution position

The pre-review APFS 10K unchanged checkpoint medians were 73.00 ms first and
101.26 ms second; the corresponding first-selection medians were 25.49 and
48.71 ms. The within-pair ratio medians were 0.389 when checkpoint ran first and
0.617 when it ran second. The warm win held in both orders, but their aggregate
median concealed the distribution shape. The cause was not isolated, so these
numbers do not establish a scheduling, vnode-reclaim or cache-pressure diagnosis.

The final Mac mini/APFS run is split by position below. Each cell is a median
of three samples, in milliseconds. This reports observed position sensitivity;
it does not adjust away or explain the effect.

| Files x bytes | Scenario | Cold first | Cold second | Checkpoint first | Checkpoint second |
|---|---|---:|---:|---:|---:|
| 1,000 x 32 | miss | 43.56 | 43.97 | 47.96 | 61.55 |
| 1,000 x 32 | unchanged | 35.51 | 36.36 | 34.45 | 28.37 |
| 1,000 x 32 | edit | 36.81 | 37.66 | 30.78 | 47.81 |
| 10,000 x 4,096 | miss | 166.37 | 188.09 | 189.76 | 222.98 |
| 10,000 x 4,096 | unchanged | 165.34 | 175.17 | 72.91 | 103.27 |
| 10,000 x 4,096 | edit | 164.05 | 184.48 | 76.90 | 103.40 |
| 1,000 x 65,536 | miss | 65.19 | 63.59 | 67.80 | 81.71 |
| 1,000 x 65,536 | unchanged | 64.27 | 85.28 | 29.95 | 51.44 |
| 1,000 x 65,536 | edit | 66.61 | 85.90 | 34.71 | 30.86 |

[`review-position-summary.json`](benchmarks/snapshot-checkpoint/review-position-summary.json)
contains Total and Selection medians by position for both hosts and all cases,
separately for the pre-review, first review-fix and final candidates. Historical
positions are derived from the documented alternating order; new samples record
them explicitly. The historical Python `elapsed` values remain driver-observed
polling times; they are not used in these tables or conclusions.

## What remains in the next slice

The [roadmap](SNAPSHOT_ENGINE_PLAN.md) keeps the larger changes separate:

1. Collect reusable observations through the existing builder and share ancestor/
   stamp rules. Remove duplicate miss work and bypass unsupported/oversized caches
   without repeating preparation.
2. Compare constructing current state directly with reconstructing prior state
   and applying edits; both use the same adaptive engine. Evaluate redundant
   validation/copy removal through a validated ownership boundary. Include larger
   entry counts, multiple edit densities and Git-selected fixtures.
3. Use those measurements to compare persisted index restoration and changed-record
   publication. Integrate at the shared preparation boundary only after the
   corresponding gates pass, then benchmark push, both fetch modes, watch restart,
   creation and ephemeral submission.

Before production, filesystem eligibility must be executable and cache ownership,
permissions and directory lifetime must be enforced. Mount aliases and native
timestamp/mmap assumptions need explicit treatment. The private sibling caches
in these campaigns do not prove those guarantees. Compatible policy-change reuse
remains a later optimization. Directory-cache eviction/source-lock experiments
stay after step 3 if capture is still a priority; large-file deltas remain step 4.

## Validation and provenance

The final driver ran package tests, race tests, vet and the Python benchmark-contract
tests on both hosts. All 108 final pairs across the two hosts produced matching
wire roots and expected hash/reuse/write/status behavior. Local focused tests and
race/vet checks also passed. The final native package runs include the added
empty-selection regression.

The retained [input inventory](benchmarks/snapshot-checkpoint/inputs.json) identifies
two review revisions in addition to the earlier candidates. `review-fixes` is the
first revised candidate; `review-final` adds the empty-selection fix and test.
Their complete raw logs and reports are retained in the corresponding `-apfs`
and `-btrfs` directories. They are not pooled as repeats of the same source.
Archives omit older benchmark artifacts while preserving all measured Go/module,
Python harness and fixture inputs. Archive identities, Go-source digests and full
Python inventories were verified against the reports; final Go/harness inputs
also match the current checkout. Documentation was completed after measurement.

| Revision | Mac mini job | Cabal job |
|---|---|---|
| First review fixes | `mac-mini/01M2JJR2MDFA0KS4XRY8VH903Y` | `cabal/01M2JJR2MDE8DG1W2V0PAKWZ43` |
| Final, including empty selection | `mac-mini/01M2JK09RYBFBPTDDXPZ40YA49` | `cabal/01M2JK09RYWV505XHMYMQQJH3C` |
