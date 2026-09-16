# Derived index and changed-record checkpoint comparison

This slice compares three fresh-process preparation paths against frozen commit
`aff735ed86409164cb5ef6827a28e198aa058d7e`. Production push, fetch, watch,
workspace creation and submission do not use these disk checkpoints.

## Decision

Keep the comparison as experimental evidence. Neither derived replacement nor the
combined derived-index journal qualifies for production adoption. The final
50K-file edit journal is 13.4% slower on APFS and 19.2% slower on Btrfs by median
paired ratio; its compaction case is 25.1% and 41.4% slower. Full derived replacement
is slower still on edits. The selected contiguous/Merkle engine and production
transfer paths stay unchanged.

The journal does reduce 50K-file edit checkpoint writes from 8,142,462 to 1,080
bytes on APFS and from 7,334,768 to 1,080 bytes on Btrfs. These are local cache
writes, not uploaded bytes. Loading and validating the larger image outweighs
saved index construction and publication in these workloads. This rejects these
storage implementations, not persistent Merkle indexing as a general approach.

## Implementations

- **Observation control:** the committed observation checkpoint, including its
  shared builder and verified-publication contract. Reconstruct the selected
  adaptive index, and replace the observation file on changes.
- **Derived replacement:** store sorted observations and the actual Merkle tree's
  postorder topology, content hashes and subtree hashes. Restore that topology
  without rerunning Cartesian tree construction. Small snapshots retain contiguous
  metadata, and height-limited fallback remains supported.
- **Derived journal:** use the same complete base image, then append only changed
  observations, including stamp-only changes and explicit deletions. Replay through
  the shared manifest update implementation. Coalesce observation edits across
  validated transactions, then merge the observation array once per restart.
  Compact after 32 transactions, before
  exceeding 8 MiB of journal frames, or before the complete file exceeds 64 MiB.

All paths freshly select and stat the source, retain exact native observation
equality, and verify selection and checkout identity before publication. Restored
metadata must satisfy the existing manifest rules. Derived state additionally
checks postorder references, contiguous key ranges, canonical path priorities,
height, entry hashes and subtree hashes. These checks are included in loading;
persisting a derived hash does not by itself make it safe for Merkle diff skips.

The derived format uses bounded batches and a byte-limited encoder. A base plus
checksummed, chained journal frames occupies one file. A nonblocking shared/exclusive
lock protects readers and writers. Append publication checks the exact loaded file
generation under the writer lock. Stale delta appends and busy writers lose only
their advisory cache update. Full replacements intentionally use last-writer-wins:
an older verified snapshot may replace a newer cache, and every subsequent reader
still selects and stats the current source before reuse. A busy reader also skips
the cache and hashes the selected files afresh; its current status is `missing`,
which does not distinguish contention from absence. Complete replacement uses a
fixed temporary file and atomic rename.
No fsync policy for production data or receipts changes.

An incomplete or invalid suffix, including a checksum or semantic failure inside
a frame, recovers the last fully validated prefix and discards all later frames. Fresh source
selection/stat still runs, so recovered observations cannot hide later source
changes. The next successful publication compacts recovered state. Corrupt bases
or identity mismatches rebuild. Cancellation propagates rather than starting a
cold fallback. Private cache ownership, filesystem eligibility and directory-handle
lifetime remain production-adoption gates from the existing roadmap.

## Measurement

The driver builds the frozen control and candidate, then runs a fresh process for
each observation. Its timer includes selection, loading and validation, current
stat/hash work, index updates, verification, publication and the JSON wire root.
Process startup/JSON output are outside the Go timer. Filesystem caches stay warm.
This measures snapshot preparation, not transfer delivery or application readiness.

Six rounds cover all six execution orders. The fixtures are 1K files of 32 bytes,
10K of 4 KiB, 50K of 128 bytes, and a Git-selected 10K/4-KiB tree. Cases include
cache misses, unchanged restarts, one-file edits, 1,000-file batches, actual
32-transaction compaction and interrupted-suffix recovery. Compaction history is
created by real untimed preparations; its final load/replay/replacement is timed.
The observation control is an unchanged restart in the recovery case, identifying
the cost of recovering the candidate's interrupted journal rather than claiming
equivalent recovery work. Every measured sample checks file hash/reuse counts,
publication behavior and matching wire roots across variants.

Tests, vet and race checks run before timing. Source and harness identities are
checked before and after the campaign; reports retain every measured sample and
phase. The archived control and candidate sources accompany the results.

The initial candidate allocated each encoding batch and repeatedly grew decoded
arrays. It also copied the entire observation list for each replayed transaction.
The corrected candidate reuses its bounded buffer, preallocates admitted decoded
arrays, and coalesces observation replay. Both revisions keep the same index
validation and publication checks. Their evidence stays separate.

The [initial reports](benchmarks/derived-index/initial-summary.json) contain 432
measured samples per host. On 50K-file edits, the initial journal's paired total
ratio was 1.155 on APFS and 1.252 on Btrfs. Initial load-phase medians were
79.12 ms versus 21.79 ms on APFS and 328.99 ms versus 76.43 ms on Btrfs. Index
phase medians fell from 10.05 to 0.67 ms and from 59.56 to 2.63 ms respectively.
These separate phase medians are diagnostic, not additive decompositions of the
paired total. Persisting the topology saved index work, but reading and validating
the larger representation outweighed that saving in this initial implementation.

Archived [inputs and job provenance](benchmarks/derived-index/provenance.json)
identify both revisions. Tests, vet and race checks passed on both native hosts
before timing. Hosts were not otherwise isolated from unrelated activity; six
pairs describe these workloads and do not establish population tail bounds.

## Corrected native results

Both final campaigns completed 432 samples, with equal roots and expected
hash/reuse/publication behavior for every measured comparison. All times below
are median milliseconds. Ratios are medians of the six per-round candidate/control
ratios, not divisions of the displayed medians. Below 1 favors the candidate.
The [complete summaries](benchmarks/derived-index/summary.json) retain wins, ranges,
phase medians, cache size and bytes written; full reports retain individual samples.

Review follow-ups renamed the experimental receipt fields without changing storage
behavior. `JournalRecordsLoaded` is the number of valid transactions replayed before
publication, so an append reports the previous count and a compaction reports the
history it consumed. `ReplacedBase` means an existing derived base was replaced,
including ordinary derived-mode rewrites, compaction and recovery. An initial base
write sets `Written` but not `ReplacedBase`. `CheckpointBytes` counts published bytes
when `Written` is true; on an unchanged hit it is the loaded file size. Failed writes
may report partial bytes and are rejected by the campaign's `CacheError` check.

The archived sources and measurements remain unchanged. Their `JournalRecords`
and `Compacted` fields have the same meanings as today's `JournalRecordsLoaded`
and `ReplacedBase`, respectively. The tables describe those archived candidates,
before the reporting rename and additional review coverage; they are not a new
measurement of the current source or harness hashes.

### Mac mini / APFS

| Fixture | Case | Control ms | Derived ms | Paired ratio | Journal ms | Paired ratio |
|---|---|---:|---:|---:|---:|---:|
| 1000-32-ignore | batch | 50.27 | 42.25 | 0.955 | 43.77 | 0.887 |
| 1000-32-ignore | compaction | 32.81 | 35.37 | 1.012 | 40.72 | 1.210 |
| 1000-32-ignore | edit | 50.94 | 41.16 | 0.995 | 40.22 | 0.970 |
| 1000-32-ignore | miss | 50.65 | 43.44 | 0.950 | 57.99 | 1.140 |
| 1000-32-ignore | recovery | 50.08 | 37.36 | 0.763 | 59.63 | 0.989 |
| 1000-32-ignore | unchanged | 45.64 | 51.30 | 1.228 | 33.82 | 0.881 |
| 10000-4096-git | batch | 298.28 | 318.96 | 1.052 | 288.86 | 0.973 |
| 10000-4096-git | compaction | 275.42 | 302.92 | 1.122 | 290.32 | 1.045 |
| 10000-4096-git | edit | 279.37 | 294.31 | 0.993 | 262.25 | 0.997 |
| 10000-4096-git | miss | 415.11 | 380.29 | 0.905 | 406.23 | 0.996 |
| 10000-4096-git | recovery | 282.33 | 293.46 | 1.092 | 315.15 | 1.068 |
| 10000-4096-git | unchanged | 246.62 | 304.46 | 1.181 | 255.18 | 1.037 |
| 10000-4096-ignore | batch | 114.64 | 104.77 | 0.994 | 106.96 | 1.134 |
| 10000-4096-ignore | compaction | 82.93 | 102.61 | 1.196 | 112.83 | 1.336 |
| 10000-4096-ignore | edit | 79.50 | 96.01 | 1.221 | 108.23 | 1.247 |
| 10000-4096-ignore | miss | 197.07 | 206.69 | 1.019 | 192.57 | 1.005 |
| 10000-4096-ignore | recovery | 78.29 | 104.83 | 1.288 | 103.83 | 1.181 |
| 10000-4096-ignore | unchanged | 102.07 | 112.64 | 1.119 | 106.36 | 1.050 |
| 50000-128-ignore | batch | 292.09 | 352.38 | 1.233 | 336.92 | 1.128 |
| 50000-128-ignore | compaction | 282.42 | 353.66 | 1.270 | 346.02 | 1.251 |
| 50000-128-ignore | edit | 268.24 | 353.90 | 1.299 | 303.69 | 1.134 |
| 50000-128-ignore | miss | 828.67 | 833.87 | 1.009 | 844.88 | 1.016 |
| 50000-128-ignore | recovery | 265.27 | 356.89 | 1.350 | 363.95 | 1.302 |
| 50000-128-ignore | unchanged | 253.17 | 303.74 | 1.199 | 315.31 | 1.175 |

### Cabal / Btrfs

| Fixture | Case | Control ms | Derived ms | Paired ratio | Journal ms | Paired ratio |
|---|---|---:|---:|---:|---:|---:|
| 1000-32-ignore | batch | 28.82 | 29.23 | 1.017 | 41.43 | 1.458 |
| 1000-32-ignore | compaction | 16.32 | 16.67 | 1.020 | 20.62 | 1.258 |
| 1000-32-ignore | edit | 16.70 | 16.94 | 1.020 | 15.35 | 0.917 |
| 1000-32-ignore | miss | 24.24 | 24.33 | 1.008 | 24.50 | 1.011 |
| 1000-32-ignore | recovery | 14.08 | 16.70 | 1.187 | 16.67 | 1.197 |
| 1000-32-ignore | unchanged | 14.02 | 14.44 | 1.028 | 14.23 | 1.014 |
| 10000-4096-git | batch | 412.87 | 440.83 | 1.075 | 452.40 | 1.122 |
| 10000-4096-git | compaction | 355.70 | 425.88 | 1.208 | 430.57 | 1.219 |
| 10000-4096-git | edit | 319.16 | 389.52 | 1.202 | 349.74 | 1.092 |
| 10000-4096-git | miss | 696.89 | 677.18 | 1.002 | 708.13 | 1.023 |
| 10000-4096-git | recovery | 331.57 | 421.36 | 1.265 | 426.53 | 1.284 |
| 10000-4096-git | unchanged | 305.28 | 342.67 | 1.105 | 334.85 | 1.075 |
| 10000-4096-ignore | batch | 162.48 | 219.35 | 1.342 | 201.70 | 1.225 |
| 10000-4096-ignore | compaction | 127.36 | 187.12 | 1.469 | 185.67 | 1.472 |
| 10000-4096-ignore | edit | 127.37 | 189.09 | 1.495 | 157.00 | 1.202 |
| 10000-4096-ignore | miss | 418.04 | 432.12 | 1.049 | 432.83 | 1.025 |
| 10000-4096-ignore | recovery | 113.53 | 185.14 | 1.680 | 192.22 | 1.757 |
| 10000-4096-ignore | unchanged | 114.61 | 154.75 | 1.301 | 146.88 | 1.283 |
| 50000-128-ignore | batch | 617.66 | 879.68 | 1.418 | 762.57 | 1.231 |
| 50000-128-ignore | compaction | 630.01 | 880.73 | 1.406 | 890.19 | 1.414 |
| 50000-128-ignore | edit | 604.66 | 901.87 | 1.468 | 733.31 | 1.192 |
| 50000-128-ignore | miss | 1112.08 | 1208.40 | 1.073 | 1191.91 | 1.073 |
| 50000-128-ignore | recovery | 545.58 | 898.11 | 1.633 | 913.34 | 1.764 |
| 50000-128-ignore | unchanged | 546.66 | 705.01 | 1.314 | 686.19 | 1.242 |

### Remaining cost and next comparison

For 50K-file edits, final APFS load medians are 21.75 ms for the observation control
and 73.36 ms for the journal; Btrfs is 76.16 versus 293.93 ms. The corresponding
index phase falls from 10.29 to 0.69 ms on APFS and 54.43 to 2.03 ms on Btrfs.
Journal save medians fall from 16.59 to 10.12 ms and 81.32 to 50.31 ms. The
larger load cost outweighs those savings in the measured complete preparation.
These phase labels include decoding, allocations, semantic validation and I/O;
this campaign does not attribute the slowdown to one of them in isolation.

Restoration still recomputes every path priority, entry hash and subtree hash.
It saves Cartesian topology construction, but it moves hash work into the load
phase. Its validation loop is serial while ordinary construction parallelizes
part of the hashing. The smaller index phase alone is therefore not a measure of
CPU work eliminated, and this comparison does not establish that another derived
representation cannot win.

The next bounded comparison prioritizes an **observation-only journal** that
rebuilds the selected index. This isolates changed-record writes from the cost of
carrying the derived image. Keep a frozen observation-replacement control and
measure complete preparation, byte-triggered and record-triggered compaction,
and recovery after actual journal history. The current timed recovery case starts
from a base without journal records; correctness coverage also exercises recovery
after complete transactions and a damaged middle frame.

Profile loading and validation before selecting subsequent candidates. Compare
compact encoding and parallel semantic validation independently, with all hash
checks retained. Separately evaluate a cheaper exact-generation check and one
shared observation-change pass that projects metadata edits and stamp-inclusive
journal edits. Any generation-check change must reject stale appends even across
equal-sized replacements and retain corruption/recovery guarantees; file size
alone is insufficient. Keep per-transaction semantic validation when considering
replay changes. None of these follow-ups changes the selected in-memory engine.

Bulk-copy encoding and stamp-layout experiments remain separate considered items.
The production filesystem/ownership/handle-lifetime gates remain required before
any disk cache becomes a caller default. Large-file transfer stays step 4.

## Considered items

- Included: finalize `Changed` on the immutable verified observation result.
  Publication now reads the result after post-build directory checks, and tests
  cover ignored-sibling churn and mutable-build isolation.
- Included: enforce the derived encoder's byte budget while writing and keep
  cancellation checks between bounded concrete batches.
- Included after review: explicit loaded-record/base-replacement receipt names;
  documented last-writer-wins replacements and busy-reader rebuilds; exact restored
  tree diffs; writer admission at and one byte beyond each byte budget; and valid
  prefix recovery when a middle transaction is corrupt or truncated. The byte
  boundary tests use synthetic accounting with real publication and restart checks,
  rather than allocating 64 MiB cache fixtures.
- Next slice: observation-only journal first, then separately measured compact
  encoding and parallel validation after load profiling. Cheaper generation checks,
  shared change scanning and storage-mode cleanup belong with their respective
  measured candidates, not a rewrite of this completed comparison.
- Deferred: a separate indexed-access versus bulk-copy encoding comparison, and
  native-stamp/cache layout experiments. The storage comparison preserves exact
  stamp equality and avoids mixing either independent variable into its results.
- Still required before production: filesystem eligibility, cache ownership and
  handle lifetime, and selection guards through body freezing. Chunk/delta body
  transfer remains roadmap step 4.

## Reproduction

```sh
python3 scripts/benchmark_derived_index.py --output dist/derived-index
```

Use a native filesystem output directory; do not put Btrfs measurements on tmpfs.
`--smoke` runs the complete scenario/validation logic on a 100-file fixture and
is only a harness check. Neither benchmark mode activates a production cache.
