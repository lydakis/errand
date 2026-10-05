# Client origin record on every push, 2026-09-26

Before each push, the client recovers interrupted transfers for its checkout.
To find the relationships bound to that checkout, it reads `origin.json` in
every directory under `workspace-transfers`, including other checkouts' and
other runners'. `origin.json` held the full creation manifest, so each read
decoded it: 12.8–15 ms per relationship for a 10K-file watch save, repeated
for every relationship in the state directory.

## Who needs the creation manifest

| Reader | Needs |
|---|---|
| `push`, `push --watch`, transfer recovery, `df` | Checkout path and identity, workspace ID, peer |
| `fetch --apply` of a workspace job | Creation root to match the job; the full manifest only when it stages a new application |
| `gc changes` | The full manifest, as a pin for creation bodies, when the checkout is still present |

## Change

`origin.json` now holds only the small fields plus `initial_root`, the
creation manifest's root hash. The manifest moves to `initial.json` in the
same directory. Job application and GC load it on demand and reject it
unless its root hash equals `initial_root`. Pushes and recovery never open it.

A cache keyed on the file's identity was the alternative. It would still pay
the decode on the first push and after any rewrite, need an identity that
cannot miss an in-place rewrite, and either deep-copy the manifest (itself
proportional to the tree) or rely on every caller never mutating it. The
per-push readers never use the manifest, so moving it out removes the cost
without any of those conditions.

## Why it is safe

- **Ordering.** Creation writes and fsyncs `initial.json`, fsyncs the
  directory, then writes and fsyncs `origin.json` and fsyncs the directory and
  its parent as before. An origin therefore always names a durable manifest.
  A crash before the origin is written leaves a directory without one, which
  recovery skips and GC collects after the cutoff, as before.
- **Binding.** `initial_root` ties the two files together. A missing,
  truncated or foreign `initial.json` fails job application and GC for that
  relationship instead of being used. GC keeps the damaged state, reports it
  and continues with other relationships.
- **No rewrites.** Both files are created exclusively and never replaced.
  Rejected creations still remove `origin.json` before the rest of the
  directory.
- **Earlier format.** An origin from an earlier errand is recognized and
  reported, never read as a current one (below).

Job application used to hash the embedded manifest twice; it now hashes the
loaded manifest once to verify it. GC hashes it once per relationship whose
checkout is present.

## Workspaces created by an earlier errand

There is no migration. An `origin.json` with an embedded `initial` manifest
and no `initial_root` gets its own error, which names the checkout and a
preview of the recovery:

```
errand: workspace api was created by an earlier errand, and this version cannot use its local transfer state.
Recreate it from /Users/me/src/api. This shows what recreating deletes on the runner and how to keep it, and removes nothing:
  errand workspaces recreate --on mini api
```

`push` and `push --watch` fill in the workspace name and the peer as given.
`fetch --apply` fills in the peer and prints the workspace ID, and `gc changes`
prints the recorded peer URL and workspace ID; `recreate` accepts both. An
origin that lacks `initial_root` without an embedded manifest is still
reported as an invalid workspace origin.

Push cannot recover by itself. Only `workspaces create` records a
relationship, because it holds the creation manifest and bodies that job
results are relative to. Push fails without one, so treating the old record
as missing would not help. Rebuilding it for the existing runner workspace
would mean converting the old record or fetching the manifest from the
runner, which is a migration.

### Interrupted applies

Recovery reads only the checkout path and identity, workspace ID and peer,
which both formats share. It therefore still runs for earlier-format
relationships. A `fetch --apply` the earlier errand left interrupted finishes
when the recreate preview runs, or earlier when another transfer into that
checkout recovers its applies (applying a job's results there, or pushing
another workspace from it). The upgrade never waits for the
previous version, and later transfers on the checkout no longer report an
apply transaction with no matching local state.

### Recreating

Recreation deletes state the user may want, so the recovery is a preview
first. `errand workspaces recreate --on mini api`:

1. Refuses while jobs hold the workspace.
2. Finishes any interrupted apply into the checkout that created it, and
   refuses if that checkout has moved.
3. Shows the size of the working tree it would delete, including files jobs
   left there; the workspace's retained jobs, and that this version cannot
   apply them (`fetch --output` still exports them, before and after); and
   the commands that capture the tree's changes and artifacts first:
   `errand --on mini --workspace api --no-apply -- true`, then
   `fetch --output` of that job. Ignored files that are not artifacts are not
   captured and are rebuilt by the next job. Named caches are kept.
4. Removes nothing and prints the same command with `--yes`.

With `--yes` it selects the new snapshot from the checkout first, so a local
refusal leaves the workspace in place. It then removes the runner workspace,
recovers again under the checkout lock and discards the earlier relationship,
and creates the workspace with the same name, project, artifacts and caches.
Creation flags no longer need repeating, and no state directory is removed by
hand. If creation fails after removal, the error prints the `workspaces
create` command with those settings.

For a workspace whose relationship is current, recreation keeps the old
relationship: its jobs can still be applied with `fetch --apply`.

## Results, 10K files, Linux

Watch matrix at 10K, three rounds of seven saves, paired against main
(`c2dbf0e`) with the order alternating per case ([run order](WATCH_NATIVE.md#run-order)).
Visible delivery, median ms:

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 166 | 146 | 0.948 (0.857–0.953) | 3/3 |
| git-tracked-inplace-edit | 180 | 160 | 0.889 (0.859–0.982) | 3/3 |

The saving grows with the number of relationships in the state directory;
these runs have one.

Raw reports: [benchmarks/2026-09-26-origin-state-linux.json](benchmarks/2026-09-26-origin-state-linux.json).

## Tests

- `TestWorkspaceOriginOmitsCreationManifest` checks the origin's exact fields
  and the manifest round trip. With both relationships' `initial.json`
  damaged, origin reads, recovery and inventory still succeed.
- `TestWorkspaceOriginRejectsUnboundCreationManifest` covers foreign,
  truncated and missing manifests, and that GC continues past the damaged
  relationship.
- `TestWorkspaceOriginRecognizesEmbeddedManifestFormat` writes an origin in
  the earlier format and checks the recovery text, from the reader and from
  push, and that an origin missing only `initial_root` stays generic damage.
- `TestEarlierTransferStateFinishesInterruptedApply` leaves an apply marked
  applying in an earlier-format relationship and checks that recovery
  finishes it and leaves no apply transaction in the checkout.
- `TestRecreateEarlierWorkspaceShowsLossesBeforeRemoving` checks the command
  printed by push, watch, a profile push, a URL push, `fetch --apply` and
  `gc changes`; that the preview names the losses and changes nothing; that
  the suggested capture exports the artifact; and that `--yes` keeps the
  artifacts, deletes the runner-only file, discards the earlier state, keeps
  old results exportable, and then pushes and collects cleanly.
- `TestRecreateKeepsCurrentJobsApplicable` applies an old job after
  recreating a current workspace.
- `TestRecreateRefusesBeforeRemoving` checks the busy refusal and that a
  snapshot the checkout refuses leaves the workspace in place.
- `TestWorkspaceOriginFollowsDurableCreationManifest` checks that no origin is
  written when the manifest write fails, and that a directory interrupted
  before its origin is collected.
- `TestTransferGCKeepsCreationBodies` advances the checkpoint past creation
  and checks that GC keeps the creation bodies, which only the manifest pins.
