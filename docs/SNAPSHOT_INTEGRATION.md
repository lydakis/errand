# First shared snapshot integration: historical hybrid

This report records the initial hybrid implementation and its measurements.
The [review follow-up](SNAPSHOT_REVIEW_FOLLOWUP.md) records fixes and a flat
experiment. The [active plan](SNAPSHOT_ENGINE_PLAN.md) tests retained Merkle
state across transfer boundaries before deciding adoption.

This is the first production integration after the representation experiments.
`internal/manifest.Snapshot` owns validated immutable metadata, exact comparison,
lookup, subtree traversal, updates, byte totals and cached wire identity.

```mermaid
flowchart LR
    A[Selection and filesystem observation] --> B[Validated snapshot]
    B --> C[Exact metadata changes]
    C --> D[Existing transfer and body verification]
    D --> E[Existing transactional staging and apply]
    B --> F[Retained watch state]
    F -->|Refresh observed paths| B
```

## Representation and boundaries

Cold snapshots own a sorted slice. Construction validates paths, entry fields,
symlink targets, hierarchy, strict ordering and byte-total overflow. The sorted
validator looks up each distinct ancestor chain once and decodes digests into
stack storage; it does not build a map of every file. Cold
comparison merges two sorted sequences without making complete path maps or
sorting their union. Root hashes retain the existing JSON protocol encoding,
including the distinction between nil and empty entry slices.

The first update can lazily build the persistent Cartesian tree selected by the
experiments. Watch explicitly prepares it during full selection, so the first
edit does not pay that construction cost. Full reconciliation includes that cost.
Subsequent updates copy affected search paths. Indexed comparison
skips equal subtrees, including independently constructed trees. The internal
binary Merkle identity is not a replacement for the protocol root hash.
Tree construction hashes disjoint subtrees using at most four workers, capped
by `GOMAXPROCS`; small trees stay serial.

Deterministic path priorities do not guarantee balance for chosen inputs. Height
is checked before recursive cold construction and after each edit, with a bound
of 128. Valid snapshots exceeding that bound use ordered flat updates. A batch
validates final hierarchy and byte totals, so replacing a directory together
with its descendants or renaming a maximum-size file remains valid. Derived
caches are synchronized; cancellation never publishes an incomplete index.

Source selection remains outside this metadata module. Watch still checks
selection evidence, policy changes, structural changes, expiry and native event
overflow. Source packers and receivers still verify file bodies. Cached hashes
and an immutable manifest do not establish that live files are unchanged.

## Integrated paths

- `changes.workspaceDelta` uses shared ordered comparison, lookup and subtree
  selection. It preserves complete change roots, directory metadata changes,
  merge bases, quotas and the existing bundle identity.
- `PrepareTransferSource` and `ExpandTransferSource` retain the validated
  snapshot through preparation. Expansion reuses its checked wire identity;
  staging reads the already-checked full byte total in constant time. Push's
  receiver and persistent fetch use these factories.
- Watch retains the snapshot between refreshes and updates only refreshed hash
  cache entries. It no longer clones the complete hash-cache map on each edit.
  The existing watch API still exports a complete manifest for its caller.

The archive/materialization cycle in staging is unchanged. Conflict decisions,
body verification, checkpoint revisions, retry identity and durable receipts
remain on the existing path. Workspace creation and ephemeral submission have
not yet been migrated to a shared initialization API. Their command benchmarks
are regression controls for this slice, not evidence that this migration is
complete.

## Verification and measurement

The production contracts cover input and export ownership, wire identity,
randomized updates against an independent map oracle, structural replacement,
independent indexes, subtree-prefix boundaries, malformed input, size overflow,
adversarial tree height, concurrent cache creation, and cancellation before and
during work. Existing transfer tests cover stale checkpoints, corrupt bodies,
quotas, sparse bodies, replay and conflict behavior.

The new benchmarks compile against the committed pre-integration public APIs:

- `BenchmarkPreparedMetadata`: prepare and expand a one-file change in 1K/10K
  entry manifests in flat and nested package layouts, without filesystem or
  transport work.
- `BenchmarkWatchPreparation`: first edit, retained edit, and full reconciliation
  on native filesystems, including selection guard verification. It manually
  delivers invalidations; native event latency is measured by the command test.
- Existing command benchmarks: loopback HTTP push/watch on 10K files, creation
  and submission on 1K files, and fetch for ephemeral/persistent results.

`scripts/benchmark_snapshot_integration.py` runs candidate tests, vet and focused
race checks, builds both versions once, then alternates their order with
`GOMAXPROCS=2`. The default is five rounds; the final integration check uses
three rounds after the diagnostic runs. Metadata benchmarks use a 150 ms target;
filesystem preparation uses ten
iterations per result, avoiding excessive untimed setup during Go calibration.
Each command result averages three operations. Fixture setup and compilation
are outside timed results. Raw logs, source digests, binary digests, filesystem,
toolchain and all samples are retained. HTTP stays on the benchmark host;
these measurements do not include cross-host network latency or compare against
rsync/Mutagen.

Reproduce with a preserved baseline module at the committed revision, copying
the two new API-compatible benchmark files into that module first:

```sh
python3 scripts/benchmark_snapshot_integration.py \
  --baseline /path/to/baseline \
  --output ./snapshot-integration-results \
  --rounds 3 \
  --revision 9f3708a8a2b5ff7b9e7a84df3bd48809101e9d58
```

`BenchmarkSnapshotRetention` is a separate GC diagnostic, not a timing benchmark.
On the local Apple M1 Max, three isolated measurements retained about 3.04 MB
for a 10K-entry indexed snapshot, and 560–1,264 bytes after deleting all but one
leaf. The 1K case retained about 303 KB and 1.6–1.7 KB respectively. These are
heap-allocation differences after GC, not RSS bounds or a long-session soak.
The [raw retention results](benchmarks/2026-09-13-snapshot-retention.txt) include
all three samples.

## Diagnostics and acceptance

The [initial comparison](benchmarks/2026-09-13-snapshot-integration-initial.json)
retains all five completed Cabal rounds and the partial Mac mini diagnostic.
The initial Cabal candidate made 10K cold preparation 13% slower and first-edit
preparation 165% slower, despite a 29% retained-edit improvement. It was not
accepted unchanged. Sorted validation, ancestor-chain reuse and explicit watch
index preparation address those costs. The intermediate nested-path local probe
identified repeated ancestor lookup: caching the checked chains reduced its
median from 16.3 ms to 12.4 ms, against 13.6 ms at the baseline revision.

An initial harness parsing error was corrected with verbose Go benchmark output.
A subsequent duration-calibrated filesystem run spent excessive time repeating
untimed first-edit setup; the final harness uses ten fixed iterations there.
Superseded jobs were stopped before replacement jobs started on the same host.
A separate filesystem check established that Cabal's default `/tmp` is tmpfs.
Its [completed temporary-filesystem run](benchmarks/2026-09-13-snapshot-integration-tmpfs.json)
is retained as a diagnostic. The final Cabal comparison places fixtures on the
Btrfs workspace volume; the harness now does this explicitly with `TMPDIR`.
The partial Mac mini diagnostic is not a reliable end-to-end comparison.

## Final measured results

Medians of three paired rounds. Values are milliseconds, baseline → candidate;
percentages compare those medians. These are short controlled comparisons,
not confidence intervals or guarantees about every workload. All samples and
verification logs are in [the final data](benchmarks/2026-09-13-snapshot-integration.json).

| Operation | Mac mini / APFS | Cabal / Btrfs |
|---|---:|---:|
| Prepare metadata, 1K flat | 0.69 → 0.73 (+4.6%) | 3.14 → 2.45 (-21.9%) |
| Expand metadata, 1K flat | 0.95 → 0.65 (-31.6%) | 4.91 → 3.71 (-24.3%) |
| Prepare metadata, 1K nested | 0.82 → 0.80 (-2.5%) | 4.07 → 4.35 (+6.8%) |
| Expand metadata, 1K nested | 1.16 → 0.98 (-15.4%) | 5.45 → 4.29 (-21.1%) |
| Prepare metadata, 10K flat | 6.61 → 6.08 (-8.0%) | 35.01 → 26.73 (-23.7%) |
| Expand metadata, 10K flat | 9.41 → 7.21 (-23.4%) | 43.28 → 27.25 (-37.1%) |
| Prepare metadata, 10K nested | 8.10 → 7.79 (-3.9%) | 39.29 → 31.91 (-18.8%) |
| Expand metadata, 10K nested | 12.15 → 11.46 (-5.7%) | 54.34 → 39.20 (-27.9%) |
| Watch preparation, 1K first edit | 31.36 → 28.34 (-9.6%) | 4.62 → 4.38 (-5.2%) |
| Watch preparation, 1K retained edit | 27.87 → 26.64 (-4.4%) | 4.60 → 4.33 (-5.8%) |
| Watch preparation, 1K full reconciliation | 68.39 → 67.53 (-1.3%) | 25.12 → 26.83 (+6.8%) |
| Watch preparation, 10K first edit | 35.10 → 34.12 (-2.8%) | 9.16 → 5.40 (-41.0%) |
| Watch preparation, 10K retained edit | 29.19 → 27.24 (-6.7%) | 7.84 → 5.26 (-32.9%) |
| Watch preparation, 10K full reconciliation | 204.28 → 190.27 (-6.9%) | 191.49 → 201.06 (+5.0%) |
| Complete push, 10K | 553.64 → 537.73 (-2.9%) | 982.19 → 998.74 (+1.7%) |
| Complete watch cycle, 10K | 390.41 → 376.85 (-3.5%) | 572.52 → 578.01 (+1.0%) |
| Fetch + apply, ephemeral | 104.05 → 102.13 (-1.9%) | 88.32 → 122.47 (+38.7%) |
| Fetch + apply, persistent | 226.95 → 233.64 (+2.9%) | 246.58 → 261.96 (+6.2%) |
| Workspace creation, 1K | 1073.88 → 1426.86 (+32.9%) | 1796.07 → 1907.74 (+6.2%) |
| Ephemeral submission, 1K | 1671.64 → 1446.72 (-13.5%) | 766.12 → 783.08 (+2.2%) |

The clearest gains are metadata expansion and retained edits. Full Btrfs
reconciliation is 5–7% slower because it now prepares the index for subsequent
edits. Whole watch was 390 → 377 ms on Mac mini and 573 → 578 ms on Cabal.
That is not near-instant synchronization or a demonstrated broad end-to-end win.

Two apparent end-to-end regressions received focused controls instead of being
discarded as noise. Mac creation ranged from 0.6 to 2.1 seconds even at the
unchanged baseline. Its [five-pair follow-up](benchmarks/2026-09-13-snapshot-integration-creation-probe.json)
measured medians of 1,303 → 1,219 ms; the median paired ratio was 0.983.
The [Btrfs fetch follow-up](benchmarks/2026-09-13-snapshot-integration-fetch-probe.json)
measured 95.0 → 87.7 ms, with paired ratios from 0.754 to 1.569. Neither
reproduced a sustained slowdown. Both controls remain noisy, so no reliable
creation or fetch speedup is claimed. Their data includes the exact probe scripts.

The production source matches the measured snapshots. The full production Go
suite, vet and race checks for manifest/snapshot/changes/archive passed on both
hosts. A final test-only addition isolates individual metadata-field changes;
the manifest race suite passed locally afterward.

The filesystem benchmarks cover in-place watch edits and full reconciliation.
They do not replace a fresh cross-machine rsync/Mutagen comparison, or measure
end-to-end atomic-save and burst workloads. Those broader acceptance checks
remain appropriate before releasing the complete shared transfer engine.

## Remaining sequence

Keep the snapshot through more internal boundaries to avoid full export and
revalidation. Next, separately replace staging's internal archive/extract cycle
with verified direct materialization, preserving its recovery and publication
contracts. Route initialization through the same content-resolution layer after
that path is established. Persisted inventory and large-file deltas remain later
steps, as described in [the sequence](SNAPSHOT_ENGINE_PLAN.md).
