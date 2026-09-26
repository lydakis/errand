# Watch branch on APFS and Btrfs, 2026-09-25

This compares v0.5.0 (`fed5a22`) directly with the P2 watch branch (`07095fb`,
everything up to [atomic saves, creates and deletes](WATCH_MEMBERSHIP.md)) on
George's hosts. The Linux numbers in the earlier write-ups chain separate
campaigns; these are one paired run per host. The harness is the realistic
watch matrix from the [baseline](WATCH_REALISTIC_BASELINE.md): three rounds,
with baseline and candidate back to back and seven samples per variant per
round. Each case ran its two binaries in the same order every round (see
[Run order](#run-order)).

| Host | Hardware | Filesystem | Conditions |
|---|---|---|---|
| MacBook | M1 Max, 10 cores, 32 GB, macOS 27.0 | APFS, internal SSD | On AC |
| Mini | M4, 10 cores, 16 GB, macOS 27.0 | APFS, internal SSD | Run as an Errand runner job. Git cases used an empty `HOME` (see below) |
| Cabal | 4 CPUs, 15 GB, Linux | Btrfs (zstd:3) on dm-crypt | Another workload shared the host for most of the run |

## Results at 10,000 files

Median visible delivery in ms, v0.5.0 → branch, with the median paired ratio
(branch ÷ v0.5.0).

| Case | MacBook | Mini | Cabal |
|---|---|---|---|
| explicit in-place edit | 345 → 297 (0.85) | 246 → 222 (0.91) | 616 → 263 (0.59) |
| explicit rename-over save | 502 → 293 (0.59) | 318 → 223 (0.70) | 758 → 771 (0.88) |
| explicit create | 494 → 282 (0.58) | 310 → 210 (0.69) | 613 → 347 (0.50) |
| explicit delete | 475 → 275 (0.58) | 289 → 201 (0.70) | 607 → 252 (0.50) |
| Git tracked in-place edit | 956 → 310 (0.33) | 563 → 236 (0.42) | 1461 → 377 (0.37) |
| Git untracked in-place edit | 891 → 285 (0.32) | 530 → 223 (0.42) | 1116 → 647 (0.43) |
| Git mixed in-place edit | 931 → 299 (0.32) | 563 → 232 (0.42) | 1474 → 962 (0.57) |
| Git rename-over save | 945 → 317 (0.32) | 575 → 239 (0.42) | 1465 → 812 (0.52) |
| Git create | 993 → 322 (0.34) | 572 → 239 (0.43) | 1142 → 334 (0.32) |
| Git delete | 915 → 281 (0.31) | 558 → 223 (0.40) | 1346 → 752 (0.53) |
| explicit edit after 31 s idle | 535 → 326 (0.60) | | |
| Git edit after 31 s idle | 924 → 322 (0.35) | | |

- **MacBook and Mini:** every 10K case was faster in 3 of 3 rounds. Git cases
  are about 3x faster on the MacBook and 2.4x on the Mini. Explicit rename-over
  saves, creates and deletes are 30–42% faster, and explicit in-place edits
  9–15% faster.
- **At 1,000 files:** the MacBook's Git cases have ratios of 0.37–0.54 and its
  explicit membership cases about 0.74. The Mini's Git cases are 0.56–0.68. Its
  explicit cases are 0.95–0.98, faster in 2 of 3 rounds, which is within noise.
- **Cabal was contended**, so its ranges are wide, for example 0.53–1.14 for the
  explicit rename-over save and 0.13–0.48 for the Git edit. At 10K, every case
  except the explicit rename-over save (2 of 3) was faster in 3 of 3 rounds. At
  1K, four cases were not consistently faster: explicit create (1.36, 1 of 3),
  explicit edit (0.98, 2 of 3), and Git untracked and mixed edits (2 of 3).
  The candidate traces for those cases show no fallbacks beyond the few
  `invalidated` cycles every host had. An idle rerun would be needed to rule out
  a real Btrfs cost.
- **Traces:** apart from each run's first cycle and a few full `invalidated`
  cycles, candidate preparations were `incremental`, `relisted` or `narrowed`.
  On the MacBook, each 10K rename-over case had one `evidence-changed`
  fallback. The Linux run showed the same thing once, and it has not been
  investigated. On Cabal, a few 10K cycles also used deferred expiry, most
  likely because the contended runs were slower.
- **Noise window:** a short `errand df` run on the MacBook overlapped part of its
  matrix. Samples carry no timestamps, so that window can't be separated out.
  Every MacBook case except the 1K explicit edit control was faster in all
  three rounds.

## Run order

The matrix was meant to alternate which binary runs first each round. With an
even number of cells, reversing the traversal on odd rounds cancelled that
out, so each case ran in one order in all three rounds, the same at both sizes:

- Baseline first: explicit in-place edit, explicit create, Git untracked and
  mixed in-place edits, Git create.
- Candidate first: explicit rename-over save, explicit delete, Git tracked
  in-place edit, Git rename-over save, Git delete.

Every paired campaign on this branch had an even matrix, so the same applies
to the Linux write-ups. Two checks say the effect is small next to the gains:

- **Comparable cases that ran in opposite orders agree.** At 10K on both
  Macs, explicit create against delete, Git untracked against tracked edit, and
  Git create against delete differ by 0.03 or less in paired ratio, in both
  directions. At 1K they differ by up to 0.11, not consistently in one
  direction. Cabal's contention swamps the comparison.
- **A same-binary run measures it directly.** On Linux, with the fixed
  script, four 10K cases over four rounds each, the binary that ran second was
  2% slower at the median (0.97–1.08 across 16 rounds, second faster in 6).
  That is within this host's 5% noise floor
  ([raw](benchmarks/2026-09-26-run-order-linux.json)).

A fixed order can therefore move one case's ratio by a few percent. That matters
only for ratios near 1, such as the Mini's 1K explicit cases. The script now
alternates each cell's order from round to round.

## Mini's Git cases

With the real `HOME`, every Git case on the Mini failed for both binaries:

- The fixture's `git commit` fails in a non-interactive job, because the Mini's
  Git configuration requires signed commits with a passphrase-protected key.
- Both binaries watch the directory holding `~/.gitconfig`, which is `$HOME`.
  fsnotify's kqueue backend opens every entry of a watched directory. From a
  runner job, opening `~/Desktop` blocks on the macOS privacy prompt, so
  `push --watch` hangs before its first receipt. This is a watch bug in v0.5.0
  too, and it is being fixed separately.

The Git cases were therefore rerun with `HOME` set to an empty temporary
directory. The explicit cases used the real `HOME`.

Raw reports: [MacBook](benchmarks/2026-09-25-watch-branch-macbook.json),
[MacBook idle](benchmarks/2026-09-25-watch-branch-macbook-idle.json),
[Mini](benchmarks/2026-09-25-watch-branch-mini.json),
[Cabal](benchmarks/2026-09-25-watch-branch-cabal.json).
