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
and no `initial_root` gets its own error, which prints the recovery for that
workspace:

```
errand: workspace api was created by an earlier errand, and this version cannot read its local transfer state; recreate it from its checkout with: errand workspaces rm --on mini 01M… && errand workspaces create --on mini api && rm -r '…/workspace-transfers/…-01M…'
```

`push` and `push --watch` fill in the workspace name and the `--on`, `--url`
or `--profile` they were given. `fetch --apply` and `gc changes` print `NAME`
and `PEER` placeholders. An origin that lacks `initial_root` without an
embedded manifest is still reported as an invalid workspace origin. Recovery
skips earlier-format relationships, as it skips damaged ones.

Push cannot recover by itself. Only `workspaces create` records a
relationship, because it holds the creation manifest and bodies that job
results are relative to. Push fails without one, so treating the old record
as missing would not help. Rebuilding it for the existing runner workspace
would mean converting the old record or fetching the manifest from the
runner, which is a migration.

What the printed recovery does:

- `workspaces rm` removes the runner workspace at once: its working tree,
  creation snapshot and remote transfer state. GC has nothing left to collect.
  Job receipts and retained results keep their normal retention. It refuses
  while jobs are active. Files that jobs generated in the persistent tree,
  such as build outputs, are lost; named caches are not.
- `workspaces create` uploads the checkout as the new creation snapshot,
  negotiating with the runner's shared body cache. Creation flags such as
  `--artifact` or `--cache` must be repeated if they were used originally.
- `rm -r` drops any pending `push.json`. That records a push to the removed
  workspace, and the checkout still holds its source, so nothing is lost.
  Without this step, `gc changes` reports the directory as failed on every run
  and never collects it.
- Jobs of the old workspace can no longer be applied with `fetch --apply`.
  `fetch --output` still exports their retained results.

Not covered: a `fetch --apply` into the checkout that was interrupted and not
yet recovered by the earlier errand. Its apply transaction stays in the
checkout, this version cannot recover it through the unreadable
relationship, and later transfers on that checkout report an apply
transaction with no matching local state. Let the earlier errand finish any
interrupted application (its next push or `fetch --apply` there does) before
upgrading.

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
- `TestPushPrintsRunnableRecoveryForEarlierTransferState` checks the exact
  command printed by push, watch and a profile push, runs it, and then pushes
  and collects cleanly.
- `TestWorkspaceOriginFollowsDurableCreationManifest` checks that no origin is
  written when the manifest write fails, and that a directory interrupted
  before its origin is collected.
- `TestTransferGCKeepsCreationBodies` advances the checkpoint past creation
  and checks that GC keeps the creation bodies, which only the manifest pins.
