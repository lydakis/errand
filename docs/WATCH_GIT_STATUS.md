# No `git status` on incremental watch cycles, 2026-09-26

Every incremental watch cycle ran `git status` twice, although push never
uses what it reports. Selection evidence kept the repository metadata
(`GitInfo`: HEAD commit and dirty flag) from the last full selection. Its
`verify` ran a fresh `git status` and compared the two. `verify` runs after the
hinted files are refreshed and again in the post-freeze guard. Outside a
repository (explicit `.errandignore` selection), each run was a failing
`git status` followed by `git rev-parse`. Inside one, `git status` checks the
whole worktree, not only the watched root. A profile of a one-file save at 10K
files put this at 8.4 ms per cycle with explicit selection and 28–30 ms with
Git selection.

## Why it is not needed

- Push does not send repository metadata. `PushRequest` has no Git fields,
  and the push path discarded the `GitInfo` that watch preparation returned.
  Only job submission records HEAD and dirty state (`spec.git_commit`,
  `spec.git_dirty`, shown by `errand ps` and given to the job as
  `ERRAND_GIT_COMMIT` and `ERRAND_GIT_DIRTY`).
- HEAD and dirty state never decide which files are selected. The selection
  evidence proves selection on its own. For explicit selection, that is the
  policy bytes and the directory stamps. For Git selection, it is also the
  index stamp or tracked-set digest and every ignore and config source
  ([Git evidence](WATCH_GIT_EVIDENCE.md)). A commit or checkout that changes
  the tracked set fails that evidence. One that only moves HEAD leaves the
  selection as it was. Under Git selection, HEAD and the index are also
  watched control files, so either change runs full selection on the next
  cycle anyway.
- A removed or broken repository still fails Git evidence: the index is gone
  or `git ls-files` fails. Explicit selection does not depend on Git.
- Content is still verified. Every shipped body is checked against its
  manifest entry when it is frozen.

## Change

- Selection evidence no longer records or compares `GitInfo`. Its preliminary
  check and its final check became identical, so they are now one `verify`.
- `Watch.Prepare` and `Watch.PrepareSnapshot` no longer return `GitInfo`.
- A watch's `SelectionGuard` binds no repository metadata. A watch without
  evidence (for example on a platform without ctime stamps, or with an
  `onbranch:` include) still reselects fully in its guard and compares the
  selected paths and policy, but no longer HEAD and dirty state. The same
  holds for the reselection that checks each full capture.
- Job snapshots are unchanged. Selection reports `GitInfo` from one
  `git status`. The job's guard, checked before and after packing, still
  rejects a changed HEAD or dirty state, so the recorded commit and dirty flag
  describe what was shipped. Workspace creation and `push` without `--watch`
  use the same guard as before.

A watch still runs `git status` as part of full selection: once when it
starts, and twice in each full cycle, once to select and once when the guard
reselects to check the capture. A watch without evidence also reselects in its
post-freeze guard, so it runs three per cycle. Git selection needs the status
to know whether the root is a Git worktree and to tell a broken repository
from a plain directory.

This also removes a spurious retry noted in
[WATCH_MEMBERSHIP.md](WATCH_MEMBERSHIP.md). Starting from a clean Git
worktree, the first edit flipped the dirty flag. The final metadata check
failed, the watch resampled after 50 ms, and the retry used full selection.
That edit is now an ordinary incremental cycle. An edit that lands while a
clean worktree is being captured no longer fails the capture either.

## Results, 10K files, Linux

Watch matrix at 10K, three rounds of seven saves, paired against main
(`c2dbf0e`) with the order alternating per case ([run order](WATCH_NATIVE.md#run-order)).
Visible delivery, median ms:

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 157 | 143 | 0.918 (0.885–0.988) | 3/3 |
| git-tracked-inplace-edit | 179 | 146 | 0.862 (0.813–0.882) | 3/3 |

Incremental preparation fell from 5 to 1 ms (explicit) and from 15 to 1 ms
(Git) in the same runs.

Raw reports: [benchmarks/2026-09-26-git-status-linux.json](benchmarks/2026-09-26-git-status-linux.json).

## Tests

`watch_git_status_test.go`:

- `TestWatchIncrementalCycleRunsNoGitStatus` puts a `git` wrapper first on
  `PATH` that logs its arguments. On the explicit and the Git fixture it runs
  two incremental cycles, each with its post-freeze guard, and requires no
  `status` invocation and no full selection.
- `TestGitWatchFirstEditInCleanWorktreeStaysIncremental` commits the
  fixture's untracked file so Git reports no changes, then edits a tracked
  file. The previous guard still verifies and the next cycle is incremental.
- `TestSelectionGuardBindsRepositoryMetadataOnlyForJobs` uses an `onbranch:`
  include so the watch has no evidence. Job selection reports the exact HEAD
  and dirty state before and after an edit and a commit, and the job's guard
  rejects both changes. The watch's guard accepts both.

Several Git evidence tests kept an extra untracked or modified file so that
the dirty flag could not change and hide what they tested. Those files are
removed.

Mutation checks were run by hand:

- Running `git status` again in `verify` fails the first test.
- Recording and comparing the metadata again also fails the clean-worktree
  test.
- Binding metadata in the watch's guard fails the third test at the watch
  guard.
- Dropping the comparison from the guard, or building job guards without the
  metadata, fails the third test at the job guard.

## Not covered

- Full cycles still run `git status` twice. For explicit selection the result
  is metadata the watch does not use. For Git selection a cheaper worktree
  probe would do. Either change would split watch selection from the job
  selection it shares, so neither was tried.
- `push` without `--watch` and workspace creation still bind HEAD and dirty
  state in their guard, although neither sends it. They run once, not per
  cycle.
