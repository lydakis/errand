# Realistic watch baseline (P2a/P2b), 2026-09-25

This is the P2 baseline before any watch change. It measures watch delivery
for the source layouts and saves that users actually produce: Git-selected
trees (untracked, fully tracked, half tracked), atomic editor saves, and file
creation/deletion. Every run records why watch preparation fell back to full
reconciliation (`ERRAND_TRACE_WATCH=1`, added for this measurement).

Revision `4ed6aa4` (v0.5.0 plus tracing only). Host: Linux cloud container,
4 vCPU Xeon 2.8 GHz, ext4 on virtio, git 2.43. Client and daemon on the same
host. Mutagen 0.18.1 (one-way-safe) and rsync 3.2.7 (`-a --checksum --delete`)
ran alongside as references. **This is not Cabal (Btrfs) or the Mac (APFS)**:
absolute numbers are much lower than the earlier native reports, so compare
rows with each other, not with those reports.

## Results

Two rounds (forward, then reverse order) of seven one-file changes per case;
14 watch samples per row. Values are milliseconds. Mutagen/rsync ran in round
one only (7 samples). "Prep" is the client's watch preparation, from the trace.

| Files | Case | Errand visible, median (range) | Errand receipt | Mutagen visible | rsync -c | Prep full / incremental | Prep reasons |
|---:|---|---:|---:|---:|---:|---:|---|
| 1,000 | explicit-inplace-edit (fast path) | 76 (70–82) | 89 | 24 | 57 | 37 / 5 | incremental 16, invalidated 4 |
| 1,000 | explicit-atomic-edit | 91 (84–110) | 104 | 23 | 57 | 23 / – | structural 16, invalidated 4 |
| 1,000 | explicit-inplace-create | 94 (68–108) | 110 | 24 | 57 | 22 / 5 | structural 14, invalidated 4, incremental 2 |
| 1,000 | explicit-inplace-delete | 88 (82–108) | 101 | 19 | 58 | 25 / 4 | structural 14, invalidated 4, incremental 2 |
| 1,000 | git-untracked-inplace-edit | 140 (133–160) | 155 | 22 | 58 | 51 / – | no-evidence 16, invalidated 2 |
| 1,000 | git-tracked-inplace-edit | 146 (135–183) | 157 | 18 | 58 | 60 / – | no-evidence 16, invalidated 2 |
| 1,000 | git-mixed-inplace-edit | 145 (138–169) | 156 | 22 | 57 | 59 / – | no-evidence 16, invalidated 2 |
| 1,000 | git-tracked-atomic-edit | 145 (135–157) | 155 | 20 | 58 | 59 / – | structural 17, invalidated 2 |
| 1,000 | git-tracked-inplace-create | 140 (128–161) | 151 | 25 | 58 | 57 / – | structural 14, no-evidence 2, invalidated 2 |
| 1,000 | git-tracked-inplace-delete | 147 (127–156) | 157 | 23 | 57 | 67 / – | structural 14, no-evidence 3, invalidated 2 |
| 10,000 | explicit-inplace-edit (fast path) | 307 (275–370) | 341 | 78 | 146 | 228 / 5 | incremental 17, invalidated 2 |
| 10,000 | explicit-atomic-edit | 401 (352–447) | 428 | 103 | 147 | 115 / – | structural 16, invalidated 2 |
| 10,000 | explicit-inplace-create | 393 (369–429) | 424 | 107 | 150 | 110 / 6 | structural 14, incremental 4, invalidated 2 |
| 10,000 | explicit-inplace-delete | 408 (351–502) | 439 | 70 | 143 | 111 / 5 | structural 14, incremental 4, invalidated 2 |
| 10,000 | git-untracked-inplace-edit | 605 (560–696) | 636 | 76 | 147 | 223 / – | no-evidence 18 |
| 10,000 | git-tracked-inplace-edit | 675 (648–728) | 709 | 103 | 147 | 268 / – | no-evidence 18 |
| 10,000 | git-mixed-inplace-edit | 663 (623–715) | 694 | 106 | 148 | 260 / – | no-evidence 18 |
| 10,000 | git-tracked-atomic-edit | 712 (659–794) | 742 | 105 | 146 | 277 / – | structural 18 |
| 10,000 | git-tracked-inplace-create | 652 (618–690) | 680 | 98 | 144 | 266 / – | structural 14, no-evidence 4 |
| 10,000 | git-tracked-inplace-delete | 680 (641–716) | 713 | 93 | 144 | 268 / – | structural 14, no-evidence 4 |

A separate single-round run edited in place after 31 s idle (10K, explicit,
four samples): 493, 401, 402 and 422 ms visible, every preparation `expired`
at 107–116 ms, against the 307 ms fast-path median.

"Prep full" for the fast-path rows is only the non-incremental cycles (initial
and invalidated), which is why it exceeds the structural rows' full cost.

## Fast-path phase split (P2b)

`go test ./cmd/errand -run '^$' -bench 'BenchmarkWatchPhases|BenchmarkWatchWorkloads/(small|git-atomic)' -benchtime=10x -count=3`
(in-process daemon, identical 1 KiB bodies; three runs, ms per operation):

| Workload | Total | Client | Daemon staging | Daemon apply |
|---|---:|---:|---:|---:|
| 1K explicit in-place (`small`) | 79–82 | 27 | 21–22 | 30–31 |
| 10K explicit in-place (`WatchPhases`) | 211–234 | 60–63 | 103–111 | 48–58 |
| 10K Git atomic (`git-atomic`) | 548–603 | 384–428 | 112–119 | 50–55 |

## What this says

1. **Only one of ten realistic cases takes the fast path.** Git selection is
   always `no-evidence`; atomic saves, creates and deletes are `structural`;
   any edit more than 30 s after the last full scan is `expired`. At 10K those
   fall-backs cost +90 to +400 ms over the fast path.
2. **Git selection is the largest gap**: about 2× the explicit fast path at
   both sizes (+300 to +400 ms at 10K). The trace's full preparation is
   ~270 ms, and the phase split shows ~400 ms of client time, so the selection
   guard's re-verification after freezing (a second full Git selection) is
   also material. Untracked vs tracked vs mixed makes little difference.
3. **Even the fast path scales with tree size.** At 10K the client's
   incremental preparation is 5 ms, yet daemon staging is ~105 ms and apply
   ~50 ms for a one-file change (1K: ~21 and ~30 ms). That whole-tree daemon
   work, not the watcher, is the fast-path floor.
4. Mutagen's delivery also grows with size here (≈20 → ≈100 ms), but stays
   3–7× below Errand. rsync `--checksum` is below every Errand 10K case.

## Next pieces, ranked by measured gap

- **P2d, Git fast path** (largest: ~350 ms at 10K). Avoid full reselection for
  content-only edits when Git's controls (index, HEAD, config, ignore files)
  are unchanged, and avoid repeating full selection in the post-freeze guard.
- **Daemon staging/apply scaling** (~155 ms of the 10K fast path). Profile
  staging for a one-file push to find the whole-manifest work.
- **P2c/P2e, atomic saves and create/delete** (~90–100 ms at 10K explicit).
- **30 s expiry** (~100 ms at 10K for anyone editing less often than twice a
  minute). Decide what the periodic full scan protects against before changing it.

## Reproduce

```sh
CGO_ENABLED=0 go build -trimpath -o dist/errand-p2 ./cmd/errand
python3 scripts/benchmark_watch_matrix.py --binary dist/errand-p2 \
  --output dist/p2-watch-matrix --mutagen "$(command -v mutagen)"
```

Raw reports (every sample and preparation trace):
[benchmarks/2026-09-25-p2-watch-baseline-linux.json](benchmarks/2026-09-25-p2-watch-baseline-linux.json).

## Limits

Single host, same-host daemon, one git version. The benchmark still passes
`edit.txt` as the push path for edit cases (as earlier reports did); create and
delete cases push without a path. Fixture files are 1 KiB with 80% distinct
bodies. The phase benchmark uses a different, identical-body fixture, so its
times are not subtracted from the table.
