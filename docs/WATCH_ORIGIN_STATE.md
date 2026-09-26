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
- **Earlier format.** There is no migration. An `origin.json` written by an
  earlier client has no `initial_root` and is rejected as an invalid workspace
  origin, like any damaged origin: `push` reports it, recovery skips it, and
  `gc changes` reports it and leaves it in place. Recreate such a workspace;
  its old directory under `workspace-transfers` can be deleted.

Job application used to hash the embedded manifest twice; it now hashes the
loaded manifest once to verify it. GC hashes it once per relationship whose
checkout is present.

## Tests

- `TestWorkspaceOriginOmitsCreationManifest` checks the origin's exact fields
  and the manifest round trip. With both relationships' `initial.json`
  damaged, origin reads, recovery and inventory still succeed.
- `TestWorkspaceOriginRejectsUnboundCreationManifest` covers foreign,
  truncated and missing manifests, and that GC continues past the damaged
  relationship.
- `TestWorkspaceOriginRejectsEmbeddedManifestFormat` writes an origin in the
  earlier format.
- `TestWorkspaceOriginFollowsDurableCreationManifest` checks that no origin is
  written when the manifest write fails, and that a directory interrupted
  before its origin is collected.
- `TestTransferGCKeepsCreationBodies` advances the checkpoint past creation
  and checks that GC keeps the creation bodies, which only the manifest pins.
