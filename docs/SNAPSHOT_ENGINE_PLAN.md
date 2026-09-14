# Shared snapshot engine: sequence and experiments

The goal is one shared implementation of snapshot identity, comparison, content
resolution and staging for push, fetch, workspace creation and job submission.
Watch schedules repeated pushes. Persistent and temporary workspaces differ in
lifetime; they should not need independent transfer algorithms.

The [first comparison](SNAPSHOT_INDEX_BENCHMARKS.md) and
[optimization follow-up](SNAPSHOT_INDEX_FOLLOWUP.md) record the completed
representation experiments. The first [production integration](SNAPSHOT_INTEGRATION.md)
added shared validated metadata and retained watch updates. The subsequent
[flat comparison](SNAPSHOT_REVIEW_FOLLOWUP.md) did not carry indexed snapshots
through transfer comparison and cannot settle the representation decision.

The [selected engine](RETAINED_SNAPSHOT_DECISION.md) retains snapshots across
preparation, comparison and accepted-source advancement. Native crossover
measurements select contiguous updates below 4,096 entries and retained Merkle
indexing from that size. Both use the same validation and transfer API.
Memory footprint and line count are not selection criteria. The all-flat variant
remains an experimental control with the same integration. Native APFS and Btrfs
comparisons cover complete watch cycles, phase costs, cold preparation, push,
both fetch modes, creation and submission. The final affected-path run checks
the selected cutoff, batching and materialization implementation.
Full wire/checkpoint serialization remains measured work. No cross-machine or
rsync/Mutagen latency claim follows from native-host loopback results.

The [direct receiver staging slice](DIRECT_STAGING.md) now materializes persistent
push/watch and fetch trees through one verified implementation, removing the
internal archive/extract round trip. Its native measurements support staging
gains on APFS and Btrfs; initial workspace/job population remains the next part
of step 2. Complete watch/push gains and initialization tail latency remain
unresolved rather than inferred from the component results.

The [retained-body staging follow-up](RETAINED_BODY_STAGING.md) removes fetch's
remaining reconstructed source tree and staging's intermediate base publication.
It also fixes publication retry durability and extends concurrent-failure
coverage. Its native comparison uses the direct-staging slice as the baseline.

The [latency diagnosis follow-up](LATENCY_DIAGNOSIS.md) separates fixture setup,
workspace creation, job capture, transfer and cleanup. It removes another archive
round trip from apply's private merge inputs through the shared verified copier.
One APFS setup stall was localized to captured-base verification. Initial
workspace/job population remains the next integration target; persistence and
large-file chunking still follow it rather than changing this slice's scope.

Keep the current content-addressed model. Selection policy determines eligible
paths, a snapshot describes their state, and the content store resolves their
bodies. Changing the snapshot representation and changing the unit of body
transfer are separate decisions. Neither requires abandoning transactional apply.

## Sequence

1. **Choose the snapshot interface and representation.** Compare the flat
   manifest with an immutable partitioned index and a persistent Merkle tree.
   Measure small edits, batches, structural changes, cold builds, full rebuilds
   and export to the existing protocol. Establish shared behavioral contracts.
   The first deliverable is an executable comparison, not a wire-format change.
2. **Integrate shared preparation and staging.** Keep the chosen snapshot through
   preparation and reconciliation instead of repeatedly flattening, validating
   and hashing it. Replace the internal archive/extract round trip with verified
   direct materialization. Route push and fetch through that implementation, then
   creation and submission through its initialization path. Initial population
   and merging with an existing destination remain different policy decisions.
3. **Persist incremental state.** Reuse the index between processes, update only
   changed metadata and bound history/recovery work. Define versioned identity,
   invalidation, cleanup and crash recovery together. Cached observations never
   replace selection checks or proof that the filesystem still matches them.
4. **Optimize large changed files.** Compare whole-file transfer, basis-dependent
   rolling deltas, and content-defined chunks under the same content interface.
   Include cold destinations, shifted insertions, incompressible data, bandwidth
   limits, CPU and storage overhead. Keep whole-file transfer when it is cheaper.

Each production stage gets controlled before/after measurements on Mac mini and
Cabal, covering watch delivery and receipt, ordinary push, both fetch modes,
workspace creation and ephemeral submission. Include small/large trees, cold/warm
state, atomic saves, deletions, bursts and no changes. A win in one operation is
not sufficient evidence for migrating all of them.

## First experiment

`experiments/snapshotindex` has three implementations behind a deliberately small
comparison interface:

```go
next := snapshot.Update(edits)
delta := snapshot.Diff(next)
identity := next.Digest()
manifest := next.Manifest() // explicit full export
```

An edit replaces or deletes one exact path. Deleting a subtree supplies its
descendant deletions explicitly. Input metadata is already validated and sorted;
the experiment does not infer selection, scan the filesystem, read file bodies,
resolve conflicts, or implement authorization. Snapshots own their metadata and
cannot mutate previous versions. Diffs are sorted and contain only changed paths.

The internal digest is deterministic within a representation but is not a new
wire contract. The flat model hashes the existing JSON manifest. The other models
hash entries and their tree/partition structure. The follow-up uses an explicit
version byte, length-prefixed strings and fixed-width integers for entry hashing,
avoiding per-entry JSON encoding. All file metadata participates,
including mode, type and symlink target. Content hashes remain whole-file SHA256.

| Representation | Implementation hidden behind the interface | Main risk |
|---|---|---|
| Flat | Sorted slice, ordered comparison, full JSON root hash | Whole-tree copying and hashing on edits |
| Partitioned | 256 stable path-hash partitions, copy changed partitions, reuse entry digests | Fixed partition count, full ordered export requires sorting |
| Tree | Deterministic Cartesian tree, copy search paths, compare structural edits by splitting around absent roots | Expected rather than worst-case height bounds; full export remains linear |

Cold tree construction allocates small blocks of at most 256 nodes instead of
one allocation per entry. Blocks limit the memory retained by an old allocation
when newer snapshots share a few nodes. They do not guarantee minimum retained
memory; block retention and process-lifetime behavior need integration checks.
Independent subtree hashes are prepared by a bounded pool, up to four workers
and no more than `GOMAXPROCS`. Trees below 512 entries stay serial. Node counts
are prepared first, workers own disjoint unpublished subtrees, and ancestor hashes
are completed only after workers finish. This trades available CPU parallelism
for lower cold-build latency; single-entry updates remain serial.

All dependencies in this experiment are in-process values from `proto`. There
are no mock filesystem or transport adapters. In production, filesystem discovery
supplies validated observations; transport exchanges required metadata/content;
transactional publication checks the destination and records the result. Those
cross-process and filesystem responsibilities must remain explicit.

The comparison interface is not a proposal to expose interchangeable strategies
to users. The production module should hide the winning representation behind a
small concrete snapshot type. It should own identity, comparison and incremental
updates, with full export at an intentional boundary. Selection and conflict
policy remain caller-visible requirements.

## Benchmark contract

The harness uses 1K, 10K and 100K root-level files, with realistic SHA256 strings,
modes and sizes. It measures metadata only; file sizes do not represent bytes
read or transferred. Fixture generation is outside timing.

- `build`: construct the index and its identity from a complete manifest.
- `edit1`, `edit100`, `add1`, `delete_root`: update, compare and obtain identity.
  Every representation receives the same edits. `delete_root` deliberately
  deletes the tree's highest-priority entry to exercise structural comparison.
- `edit1_export`: update and compare, then export and JSON-encode the complete
  manifest and obtain its existing wire identity. The flat model reuses its
  already-computed identity. Other models hash the encoded bytes once.
- `rebuild_diff`: rebuild from complete current metadata and compare. This makes
  the penalty of failing to retain an index visible; filesystem scanning is extra.
- `equal_diff`: compare independently constructed equal snapshots whose digests
  are already available. This is not the cost of a no-change push or scan.
- `diff_delete100`, `diff_disjoint`: compare independently built snapshots after
  100 distributed deletions or replacement of every path. Construction is outside
  timing. These follow-up cases check larger structural changes.

The ordered map oracle checks randomized updates and reverse diffs, independent
construction, metadata identity, slice ownership, structural replacement,
deletion to empty and long metadata. Race and static checks cover the experimental
package. Structural comparison is also tested without shared node pointers.
Construction must produce identical snapshots under `GOMAXPROCS` 1, 2 and 4.

Reproduce from the repository root, using a fresh output directory:

```sh
python3 scripts/benchmark_snapshot_index.py \
  --output /tmp/snapshot-index-results \
  --revision "$(git rev-parse HEAD)"
```

The script builds once, runs the contracts, then records five samples per case
with a 150 ms Go benchmark target, `GOMAXPROCS=2`, and `CGO_ENABLED=0`. Compilation
and fixture setup are excluded from benchmark timing. All samples are retained;
short microbenchmarks characterize these implementations, not production latency.
With `--baseline-source /path/to/preserved/module`, it builds and tests both
versions with the same workload, then alternates baseline/candidate and
candidate/baseline ordering over five rounds. There are 90 cases per version per
round. Each version's source digest and binary hash are recorded separately.

## Adoption gates

Do not add an eagerly constructed tree to every existing caller solely because
its edit benchmark is fast. First establish that warm paths can retain it and
avoid a complete export, and that cold creation/rebuild costs remain acceptable.
Compare metadata encoding and allocation costs before selecting a representation.
The follow-up removes the tree's flattening fallback, but its hash-priority height
and resource bounds still need a production decision alongside cancellation,
input validation and retained-memory measurements.

Direct staging must preserve verified bodies, frozen selection, protected cache
paths, conflict decisions, source stability checks, stale-checkpoint detection,
idempotent retries and durable receipts. Fault tests must cover interruption,
corruption, missing cached content, and recovery before bypassing any existing
archive path. Chunking later must reconstruct and verify the accepted file body
before the same publication path runs.

The representation experiment itself changes no production callers, protocol
fields, on-disk checkpoints or transfer semantics. The separate production
integration and its measurements are tracked in [its report](SNAPSHOT_INTEGRATION.md).
The earlier end-to-end checkpoint is in [the previous benchmark report](TRANSFER_FINAL_BENCHMARKS.md).
