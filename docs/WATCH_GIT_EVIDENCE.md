# Git selection evidence for watch (P2d), 2026-09-25

The [realistic watch baseline](WATCH_REALISTIC_BASELINE.md) found that every
Git-selected watch cycle did a full reconciliation (`no-evidence`), running full
Git selection three times per cycle: in preparation, in the preparation guard,
and in the post-freeze guard. Each selection took ~110 ms at 10K files on the
Linux host, so Git-selected edits took about twice the explicit fast path.

## Change

`internal/snapshot/git_evidence.go` gives Git-driven selection the same kind of
proof explicit `.errandignore` selection already had. After a full selection,
the watch records:

- **Tracked set:** the index's stamp. If Git rewrote the index (for example an
  editor's background `git status` refreshing stat data), a digest of
  `git ls-files --stage` without object ids decides instead.
- **Ignore and config sources, by contents:** `info/exclude`, the global
  excludes file, every config file `git config --show-origin` reports
  (including includes), every `.gitignore` in unexcluded directories (in any
  case, for case-insensitive filesystems), and `.gitignore` files above a
  subdirectory root.
- **Include targets:** all declared, even absent; `onbranch:` keeps full selection.
- **Repository location:** a linked worktree's `.git` file and the Git
  directory's `commondir` file, which locate the index, config and excludes.
- **Standard config files, even absent:** `~/.gitconfig` and the XDG config
  (or the `GIT_CONFIG_GLOBAL` file), `config.worktree`, and the system config
  that `git var GIT_CONFIG_SYSTEM` reports, unless `GIT_CONFIG_NOSYSTEM` is
  set. Git before 2.42 cannot report that path, so with the system config
  enabled it keeps full selection.
- **Directory stamps:** every directory not excluded by a directory pattern,
  plus ancestors of every selected file. Directories that Git collapses only
  because their files are individually ignored (`logs/` under `*.log`) stay
  stamped, because a new nested `.gitignore` can reopen a file there.
  `git check-ignore` separates the two cases. Each subdirectory listed in a
  directory the walk entered must be one the walk stamped or excluded: a
  directory created between the walk and its parent's stamp leaves the
  checkout without evidence until the next full cycle, since Git does not list
  it while empty.

Content-only edits then refresh the hinted files, as on the explicit fast path.
Any change to the proof falls back to full selection. Structural events,
overflow and 30 s expiry behave as before. The capture queries run
concurrently. Capture records every source before Git decides anything from
it: `git check-ignore` confirms the listed ignored directories after the walk
has recorded each `.gitignore`, and repeated location and config queries that
differ drop the evidence.

Tests (`git_evidence_test.go`) cover detecting each of these without event hints:
in-place `.gitignore` edits, a nested `.gitignore` reopening an ignored file,
`info/exclude`, `git add -f` of an ignored file, created and removed files,
configuring `core.excludesFile`, ancestor `.gitignore` above a subdirectory
root, and creating an absent include target or standard config file. They
also check that the system config path comes from Git and that
`GIT_CONFIG_NOSYSTEM` leaves it out, and that an index stat refresh and churn
inside an excluded `build/` do not invalidate the proof, and that ignore rules,
config or a linked worktree's `.git` file changed during capture are not
trusted. `watch_relist_test.go` creates an empty directory during capture and
files in it without events.

## Results

Same host and harness as the baseline. Baseline binary `4ed6aa4`, candidate
`0f3f167` (run 1) and `09e3675` (run 2, with concurrent capture). Three rounds
per case, with baseline and candidate back to back and alternating order.
Seven samples per variant per round. The table shows median visible delivery
in milliseconds, then the median paired ratio across rounds (candidate ÷
baseline), with the number of rounds the candidate won.

| Files | Case | Baseline | Candidate | Ratio | Candidate faster | Run |
|---:|---|---:|---:|---:|---:|---|
| 1,000 | git-untracked-inplace-edit | 135 | 62 | 0.460 | 3/3 | 1 |
| 1,000 | git-tracked-inplace-edit | 142 | 80 | 0.484 | 3/3 | 2 |
| 1,000 | git-mixed-inplace-edit | 134 | 61 | 0.451 | 3/3 | 1 |
| 1,000 | git-tracked-atomic-edit | 150 | 141 | 0.940 | 2/3 | 2 |
| 1,000 | git-tracked-inplace-create | 140 | 136 | 1.002 | 1/3 | 2 |
| 1,000 | explicit-inplace-edit (control) | 81 | 65 | 0.953 | 2/3 | 1 |
| 10,000 | git-untracked-inplace-edit | 587 | 292 | 0.497 | 3/3 | 1 |
| 10,000 | git-tracked-inplace-edit | 699 | 316 | 0.470 | 3/3 | 2 |
| 10,000 | git-mixed-inplace-edit | 664 | 315 | 0.462 | 3/3 | 1 |
| 10,000 | git-tracked-atomic-edit | 681 | 631 | 0.929 | 3/3 | 2 |
| 10,000 | git-tracked-inplace-create | 691 | 641 | 0.928 | 3/3 | 2 |
| 10,000 | explicit-inplace-edit (control) | 305 | 292 | 0.945 | 3/3 | 1 |

- Git-selected edits take about half the time (−50 to −55%) and now match the
  explicit fast path (10K: 316 vs 292 ms).
- Full Git cycles (atomic saves, creates) are ~7% faster at 10K. They still
  run full selection, but the post-freeze guard now checks the evidence instead
  of selecting a third time. Capture raises the full preparation itself from
  ~273 to ~335 ms at 10K.
- Run 1's serial capture made 1K creates slower in two of three rounds
  (ratio 1.192). With concurrent capture (run 2) they are at parity (1.002,
  range 0.80–1.12). Treat this as unresolved, not as a proven non-regression.
- The explicit path's code is unchanged, so its 5% ratio is a rough noise floor
  for this host. Effects under ~6% in this table are not established.

Raw reports: [benchmarks/2026-09-25-p2d-git-evidence-linux.json](benchmarks/2026-09-25-p2d-git-evidence-linux.json).

## Not covered

- APFS/Btrfs native runs. The Mac and Cabal effects are expected to be larger
  because their Git selection is slower, but that has not been measured.
- Repositories with submodules (unsupported for Git selection anyway), sparse
  checkouts, and `GIT_DIR`-based layouts.
- Very large ignored-but-not-excluded trees, where the directory walk at
  capture could grow. The walk runs on full cycles only.
