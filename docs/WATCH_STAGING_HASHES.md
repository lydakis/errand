# Redundant manifest hashes in daemon staging, 2026-09-26

After the [staging changes](WATCH_STAGING.md), a profile of one in-place edit
in a 10K-file explicit watch still showed about 21 ms of daemon staging spent
on whole-manifest work whose input had not changed since the previous push.

## What was recomputed

Per one-file delta push to a workspace whose client already has a checkpoint:

| Value | Where | Cost at 10K |
|---|---|---:|
| Creation snapshot: checkpoint validation and root hash | `checkInitialized`, via `TransferSession.Initialize` | 3.1 + 5.9 ms |
| Root of the checkpoint the previous push published | `expandSourceSnapshot`, on a new snapshot of an exported copy | 5.9 ms |
| The same root again | the stage's baseline check, on the checkpoint record | 5.9 ms |

The creation snapshot comes from `workspace.json`, which a watch does not
change. The checkpoint is the one the previous push published, and while its
record stays retained, every read in the next push finds that same record
([checkpoint reuse](CHECKPOINT_REUSE.md)). `workspaceSnapshotDelta` also
needs the base root, but it reused the hash expansion had just computed on
the same snapshot. A measurement prototype that skipped the expansion hash
moved that hash into `workspaceSnapshotDelta` instead of removing it.

## Change

`changes.SourceBase` holds a manifest with its checkpoint validation and wire
root hash (`proto.Manifest.RootHash`), each computed at most once, and only
when first needed.

- **Creation snapshot.** The daemon's `workspaceStore.decode` copies the
  decoded manifest into a `SourceBase` on the record. The record cache keeps
  it with the record under the existing key (workspace ID and
  `workspace.json` stamp), so pushes validate and hash it once per stamp.
  `TransferSession.InitializeBase` takes it; `Initialize` wraps a new one
  around a plain manifest.
- **Checkpoint base.** `TransferCheckpoint.Base` reads the checkpoint exactly
  as `Read` does. It returns a `SourceBase` that borrows the record's
  validated manifest and uses the record's own lazy root. The daemon expands
  deltas against it with `ExpandTransferSourceBase`. Expansion takes the
  baseline root from the base and hands it to the delta comparison instead
  of hashing its snapshot. When the stage reads the same bytes, it gets the
  same record, whose root is already computed.
- Before a client's first push, its base is the creation `SourceBase`.

Each push now skips validating and hashing the creation snapshot, and hashes
the checkpoint once instead of twice.

## Why each reused value belongs to the same bytes

- A `SourceBase` computes both results from its own entries.
  `NewSourceBase` copies its input, `Manifest` returns a copy, and nothing
  accepts a root from outside. A root therefore cannot be paired with other
  content.
- A checkpoint base shares its record's root. A record is reused only when
  the checkpoint file's bytes equal the bytes it was decoded from, and a
  record is never modified. Other bytes are decoded into a new record with
  its own root.
- The creation base comes from the same decode as its record and is retained
  only with that record, so a replaced `workspace.json` gets a new base. It
  relies on the record cache's stamp rule and adds no key of its own.
- Expansion builds its snapshot by copying the base's entries, so the base's
  root is that snapshot's root.
- Every comparison still runs on every push, against bytes read in that
  request: the checkpoint's recorded creation root against the creation base,
  the delta's baseline against the base, and the stage's recheck of the
  current checkpoint against the prepared delta.
- Expansion runs outside the workspace lock. The borrowed record is immutable
  and both results are computed under `sync.Once`, so concurrent requests can
  share a base.

## Unchanged

- Every checkpoint access still reads the whole file and compares bytes,
  including the read in `checkInitialized` (about 2 ms).
- Expansion still validates and copies the base into a snapshot
  (`manifest.New`, about 2 ms), and still hashes the reconstructed source to
  verify the client's declared root.
- `TransferCheckpoint.Initialize` still validates and hashes the creation
  snapshot when it creates a checkpoint, on a client's first push.
- A retained workspace record also holds the base's own entries array, 80
  bytes per entry (0.8 MB at 10K files); the strings are shared. The record
  cache counts it against its 64 MiB budget.

## Results, 10K files, Linux

Watch matrix at 10K, three rounds of seven saves, paired against main
(`c2dbf0e`) with the order alternating per case ([run order](WATCH_NATIVE.md#run-order)).
Visible delivery, median ms:

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 153 | 144 | 0.938 (0.832–1.023) | 2/3 |
| git-tracked-inplace-edit | 182 | 164 | 0.923 (0.865–0.936) | 3/3 |

In one round the explicit case was 2% slower, within this host's ~5% noise
floor; the Git case won every round.

Raw reports: [benchmarks/2026-09-26-staging-hashes-linux.json](benchmarks/2026-09-26-staging-hashes-linux.json).

## Tests

`internal/changes/source_base_test.go` checks that neither the caller's input
nor a returned copy changes the entries a base's root describes; that an
invalid creation snapshot is still refused with its validation error; that a
checkpoint naming another creation snapshot refuses a base whose identity was
already accepted, and that a corrupt one is refused; that a checkpoint
rewritten with the same size and timestamps gets its own identity and is
refused by expansion and by the stage; that expansion refuses a base other
than the delta's baseline; and, under the race detector, that expansion can
share a base with requests holding the lock. In `internal/daemon`, a replaced
`workspace.json` gets a new base and the base is counted in the cache budget.
Pushes are refused while the creation snapshot differs, accepted again once
it is restored, and refused after the checkpoint's creation root changes.
