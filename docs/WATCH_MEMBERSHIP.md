# Watch membership changes: atomic saves, creates and deletes (P2c/P2e), 2026-09-25

The [realistic baseline](WATCH_REALISTIC_BASELINE.md) found that watch uses full
selection for any created, removed or renamed file. That includes rename-over
saves, where an editor writes a temporary file and renames it over the
original. After the [Git evidence](WATCH_GIT_EVIDENCE.md),
[staging](WATCH_STAGING.md) and [expiry](WATCH_EXPIRY.md) changes, these were
the slowest common cases. At 10K files they took about 265 ms with
`.errandignore` and about 490 ms with Git, against about 150 and 175 ms for an
in-place edit.

## Change (`ba46823`, `07095fb`)

Selection evidence now also keeps each stamped directory's listing (entry
names and types) from the moment the selection was proven. A created,
removed or renamed non-directory is recorded as an entry change rather than a
reason for full selection. Directory and control events still force full
selection, as before.

Each preparation relists every stamped directory whose stamp changed, plus the
parent of every hinted entry change, and compares it with the proven listing:

- **Same names and types** (a rename-over save; the temporary file came and
  went): the new directory stamp is accepted.
- **Created files** are decided as full selection would decide them. Explicit
  selection uses the `.errandignore` matcher and the walk's cache,
  transaction and `.git` exclusions. Git selection runs one
  `git ls-files -co --exclude-standard` query with literal pathspecs for the
  created names, then applies the same metadata, transaction and cache
  filters as `gitListFiles`.
- **Removed files** are deleted from the manifest.
- **Other selected files** in a relisted directory whose stat evidence no
  longer matches the hash cache are observed again. This catches a
  replacement whose native event was lost.

Policy contents, the Git index and all other directory stamps are verified as
before. Relisting uses the same ordering as capture: stamp first, then list.
The evidence check after the source work rejects any change in between.

Full selection is still used when:

- a directory appears, disappears or changes type, or any entry changes type;
- a `.gitignore`, `.errandignore` or `.git` name appears or disappears;
- a Git-selected directory loses its last selected file (Git derives
  directories from files, so the directory entry must go);
- policy or tracked-set evidence changed;
- more than 256 names were created at once under Git selection.

New trace reasons: `relisted` (names unchanged), `narrowed` (files created or
removed) and `membership` (fell back to full selection).

### Tests

`watch_relist_test.go` runs each case against both an explicit fixture and a
Git fixture, and compares every result with a fresh full selection:

- Rename-over saves stay incremental. A later unhinted creation in a relisted
  directory fails the post-freeze guard and is picked up by the next
  relisting.
- An unhinted replacement in another directory is observed again.
- Created and removed files, with and without events, are handled without full
  selection. This includes ignored files, a temporary file caught before its
  rename, and Git files created in a directory that previously held only
  ignored files.
- These use full selection: a file replaced by a directory or a symlink, a
  created or removed directory, a created ignore file, and a Git removal that
  empties a directory.
- A seeded random sequence of 120 preparations per fixture mixes in-place
  saves, rename-over saves, creations and removals, a quarter of them without
  events. Each preparation matches a fresh selection, and most stay
  incremental.
- A native fsnotify test checks that real rename-over saves stay incremental.

Mutation checks were run by hand: dropping any of the safeguards above (ignore
decision, Git directory pruning, re-observation, control names, type changes,
relisting stale directories, Git exclusions) fails at least one test.

## Results

This used the same host and harness as the baseline. The baseline binary was
`df66ec5` (every earlier change on this branch) and the candidate was
`07095fb`. There were three rounds, with baseline and candidate back to back
in an order fixed per case ([run order](WATCH_NATIVE.md#run-order)), and seven samples per variant per round. The table
shows median visible delivery in ms and the median paired ratio across rounds
(candidate ÷ baseline).

| Files | Case | Baseline | Candidate | Ratio (range) | Candidate faster |
|---:|---|---:|---:|---:|---:|
| 1,000 | explicit-atomic-edit | 78 | 58 | 0.756 (0.705–0.780) | 3/3 |
| 1,000 | explicit-inplace-create | 75 | 61 | 0.803 (0.783–0.816) | 3/3 |
| 1,000 | explicit-inplace-delete | 70 | 53 | 0.747 (0.693–0.900) | 3/3 |
| 1,000 | git-tracked-atomic-edit | 153 | 68 | 0.464 (0.437–0.503) | 3/3 |
| 1,000 | git-tracked-inplace-create | 138 | 63 | 0.456 (0.419–0.546) | 3/3 |
| 1,000 | git-tracked-inplace-delete | 132 | 51 | 0.400 (0.339–0.434) | 3/3 |
| 1,000 | explicit-inplace-edit (control) | 61 | 58 | 0.997 (0.897–1.035) | 2/3 |
| 1,000 | git-tracked-inplace-edit (control) | 60 | 60 | 0.974 (0.956–1.141) | 2/3 |
| 10,000 | explicit-atomic-edit | 261 | 155 | 0.585 (0.556–0.647) | 3/3 |
| 10,000 | explicit-inplace-create | 267 | 156 | 0.584 (0.580–0.604) | 3/3 |
| 10,000 | explicit-inplace-delete | 268 | 148 | 0.587 (0.541–0.611) | 3/3 |
| 10,000 | git-tracked-atomic-edit | 493 | 182 | 0.368 (0.351–0.389) | 3/3 |
| 10,000 | git-tracked-inplace-create | 481 | 176 | 0.367 (0.366–0.368) | 3/3 |
| 10,000 | git-tracked-inplace-delete | 493 | 167 | 0.337 (0.334–0.341) | 3/3 |
| 10,000 | explicit-inplace-edit (control) | 153 | 149 | 0.975 (0.952–1.268) | 2/3 |
| 10,000 | git-tracked-inplace-edit (control) | 176 | 178 | 0.997 (0.966–1.016) | 2/3 |

- Rename-over saves, creations and deletions now take about as long as an
  in-place edit: 148–182 ms at 10K, against 149–178 ms for the edit controls.
  Measured cycles in those cases were `relisted` or `narrowed`, except one
  10K Git rename-over cycle that fell back with `evidence-changed`. Its cause
  was not investigated.
- Removing full Git selection accounts for most of the gain: those cases are
  about 63–66% faster at 10K. The explicit cases are about 41% faster.
- The in-place edit controls are unchanged (ratios 0.97–1.00). This is within
  the host's ~5% noise floor.
- Incremental preparation grows by a few milliseconds for relisting: at 10K,
  about 10 ms explicit and 21 ms Git, against 5 and 15 ms for an in-place
  edit. Full preparations for initial and invalidated cycles, which now also
  record directory listings, were at parity with the controls (explicit 233 →
  230 ms, Git 441 → 449 ms).

Raw reports: [benchmarks/2026-09-25-watch-membership-linux.json](benchmarks/2026-09-25-watch-membership-linux.json).

## Not covered

- Native APFS and Btrfs runs of this change alone. The whole branch was
  compared with v0.5.0 on APFS and Btrfs in [WATCH_NATIVE.md](WATCH_NATIVE.md).
- Creating or removing directories (`mkdir`, `git checkout` across branches,
  unpacking archives) still uses full selection.
- Relisting reads the whole directory, and after a lost event it also lstats
  that directory's selected files. The cost therefore grows with directory
  size. Very large flat directories were not measured.
- Starting from a clean Git worktree, the first change flips `GitInfo.Dirty`.
  That fails the preparation's final metadata check once, and the retry uses
  full selection. This behavior predates the change and was not measured.
