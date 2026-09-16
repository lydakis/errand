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
gains on APFS and Btrfs. The shared initialization slice below extends that
writer to immutable workspace/job capture in step 2. Complete watch/push gains
and initialization tail latency remain
unresolved rather than inferred from the component results.

The [retained-body staging follow-up](RETAINED_BODY_STAGING.md) removes fetch's
remaining reconstructed source tree and staging's intermediate base publication.
It also fixes publication retry durability and extends concurrent-failure
coverage. Its native comparison uses the direct-staging slice as the baseline.

The [latency diagnosis follow-up](LATENCY_DIAGNOSIS.md) separates fixture setup,
workspace creation, job capture, transfer and cleanup. It removes another archive
round trip from apply's private merge inputs through the shared verified copier.
One APFS setup stall was localized to captured-base verification, which the
shared initialization slice below addresses. Persistence and large-file chunking
remain subsequent steps.

The [private-input permission follow-up](PRIVATE_MERGE_INPUTS.md) removes the
second destination accessibility walk and redundant permission work from the
shared copier's disposable merge inputs. Native paired results support many-file
preparation gains while complete-operation effects remain unresolved, including
a possible small APFS regression. A focused follow-up reversed the paired
direction without establishing equivalence. Retaining the component improvement
accepts that uncertainty; both sets of evidence are retained. Verified independent
copies, logical modes and durable publication remain unchanged.

The [shared initialization slice](SHARED_INITIALIZATION.md) now routes immutable
change-base capture for workspace creation, ephemeral staging and persistent job
acquisition through that same writer. Native APFS cloning and Linux reflinks are
verified within each worker; fallback copies hash in flight. The first candidate
exposed a deep-path APFS regression; bounded parent reuse removed it. The final
slice also preserves explicit directory modes when implicit spellings alias them
on APFS. Native validation passes on both filesystems, and capture gains support
retention. Complete-operation and directory-heavy Btrfs results remain mixed;
the higher-iteration push follow-up did not reproduce the final run's >5%
slowdown, without establishing equivalence. Incoming tar decoding
remains a transport boundary. Persisted incremental state and large-file transfer
remain the subsequent roadmap steps.

The [directory materialization comparison](DIRECTORY_MATERIALIZATION.md) controls
capture cleanup I/O and compares the current scheduler with sorted and
order-preserving directory batches. Both batching candidates roughly double deep
APFS capture latency. Separate profiles and temporary counters expose increased
full-path fallbacks, consistent with longer destination leases and greater source
cache pressure from dispatch across directories. They do not isolate the causal
latency contribution of each adapter. The current per-file
scheduler is retained; the benchmark controls and rejected experiments are kept
as evidence.

The [restart checkpoint experiment](SNAPSHOT_CHECKPOINT.md) starts step 3 with
versioned, bounded observation persistence and native APFS/Btrfs measurements.
Warm preparations reuse body hashes after fresh selection and stat checks. The
prototype reconstructs the existing adaptive index from ordered checkpoint metadata;
derived Merkle nodes are not persisted yet. Warm 10K preparations improve on both
hosts, while misses add work and small APFS effects remain uncertain. Production
callers remain unchanged. The next gate is initial-population cost, followed by
measured index restoration/changed-record publication and shared caller integration.

The [checkpoint review follow-up](SNAPSHOT_CHECKPOINT_REVIEW.md) fixes rejection
of ignored directory churn and relative-root cache misses. It adds cancellation
through checkpoint I/O, checks both benchmark modes, and reports execution position.
These are corrections to the isolated prototype, not production adoption.

The [shared-builder comparison](SNAPSHOT_CHECKPOINT_BUILDER.md) now collects
observations during ordinary entry construction and carries validated loaded
metadata into index preparation. It compares prior-state updates with direct
current-state construction on larger and Git-selected fixtures. The
[review follow-up](SNAPSHOT_CHECKPOINT_BUILDER_REVIEW.md) removes unused prior-state
construction from the direct comparator. Corrected native measurements remain
mixed across workloads, so updates remain the default. Eager allocation for
ordinary builds was rejected after native regression
checks; their existing slice-growth policy is retained. Disk checkpoint adoption
remains gated separately from this shared implementation work. Separate-binary
cold controls still have unresolved differences. Actual-caller controls and
identical-code negative controls are now retained alongside those signals;
neither supports a blanket regression-free claim.

The [observation-only journal comparison](OBSERVATION_JOURNAL.md) isolates changed
observation publication from persisted derived-tree loading. It shares framed
storage and rebuilds the selected index. Native comparisons include frozen and
same-binary replacement controls, actual byte/record compaction and recovery after
real history. Writes shrink sharply, but complete preparation remains mixed:
the 50K cumulative six-edit sequence (journal depths 0–5) has median paired
ratios of 0.986 on APFS and 0.935 on Btrfs against the same-binary control,
while byte compaction is 12.0% and 14.5% slower.
The APFS edit effect is unresolved. Retain this as experimental evidence;
production adoption remains open. Next measure independently balanced fixed
history depths, near-full append and a complete compaction cycle, while profiling
loading, replay and publication before selecting one further storage candidate.

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
   Observation replacement, derived-index checkpoints and observation-only journals
   have native measurements. Derived storage candidates are rejected on complete
   preparation cost; observation-only journals have workload-dependent results.
   Production adoption remains open, pending profiling and the adoption gates below.
4. **Optimize large changed files.** Compare whole-file transfer, basis-dependent
   rolling deltas, and content-defined chunks under the same content interface.
   Include cold destinations, shifted insertions, incompressible data, bandwidth
   limits, CPU and storage overhead. Keep whole-file transfer when it is cheaper.

Each production stage gets controlled before/after measurements on Mac mini and
Cabal, covering watch delivery and receipt, ordinary push, both fetch modes,
workspace creation and ephemeral submission. Include small/large trees, cold/warm
state, atomic saves, deletions, bursts and no changes. A win in one operation is
not sufficient evidence for migrating all of them.

## Review follow-ups and timing

- **Included in the first step-3 prototype:** versioned checkout/selection/boot/
  filesystem identity, bounded current/temp files, atomic replacement, corruption
  fallback, concurrent writers and cancellation. Cache placement rejects source
  symlink/case aliases. Advisory cache writes do not change production fsync rules.
  Native paired measurements include cache misses, unchanged restarts and edits,
  with frozen inputs and all earlier candidates retained.
- **Current step-3 gate:** the shared observation pass and loaded-state ownership
  handoff have been measured. The derived-index comparison below measures restoring
  index state and publishing changed records against the frozen observation prototype.
  Load validation, full rebuild/compaction and recovery costs are part of that
  comparison. The [shared-contract slice](SNAPSHOT_CONTRACTS.md) consolidates the
  native reuse predicate with the in-memory builder and makes checkpoint writers
  accept only verified observations. It preserves session ownership,
  unsupported-platform behavior and pack verification.
  These are prerequisites for the next slice, not reasons to replace the
  adaptive representation. Full stat inventory and selection verification remain separate costs;
  neither storage change can be assumed to remove them. Integrate only after those
  gates, preserving the selection guard through freezing and testing every caller.
- **Next comparison controls from shared-contract review:** isolate inline stamp
  storage and hash-cache locality without weakening exact native-evidence equality;
  compare concrete indexed encoding with bounded bulk copying; and evaluate
  finalized `Changed` metadata on the verified result when extending publication.
  Keep pre-review frozen results distinct from the accessor/fallback cleanup.
- **Completed in the shared-builder slice:** native observations and ancestor
  expansion use the ordinary builder pass; unsupported identity and over-limit
  observation counts avoid duplicate preparation; validated loaded snapshots are
  reused without repeat validation/copying. Review fixes bind reused symlink targets
  to their stamps, compute each fingerprint once, use one explicit admission
  decision before loading, and retain metadata-only validation for the direct
  comparator. Regression coverage includes Git ancestor ceilings, unsupported
  empty/nonempty selections, and corrupt direct loads. Corrected comparisons use
  the same adaptive engine. Larger trees,
  multiple edit densities and Git-selected fixtures are included. Frozen controls,
  regression checks and rejected allocation behavior remain in the evidence.
  The [derived-index comparison](DERIVED_INDEX.md) implements opt-in restoration
  and bounded changed-observation journal variants against this frozen control.
  Loading includes metadata, topology and derived-hash validation; the comparison
  also times actual compaction and interrupted-suffix recovery. Production
  adoption remains a separate decision based on the complete preparation cost.
  The final native comparison rejects both current derived storage candidates
  for the larger indexed workloads: their larger load cost exceeds saved index
  work, including after bounded buffer reuse and coalesced observation replay.
  Small-fixture results remain mixed. Review follow-ups clarify experimental
  receipts and reader/writer behavior, and cover byte-boundary publication,
  damaged middle frames and exact restored-tree diffs. Archived timings stay frozen.
  The [observation-only journal comparison](OBSERVATION_JOURNAL.md) now measures
  those paths against frozen and current observation replacement, including actual
  byte-triggered compaction and recovery with prior journal history. Sparse-edit
  writes shrink substantially, but complete results remain mixed and byte compaction
  regresses on both hosts. Ordinary edits/batches cover cumulative depths 0–5,
  coupled to execution order, not steady state. Review fixes retain both paired
  baselines, enforce ordinary receipt/history checks, and cover equal-sized stale
  generations with shared framed-store regressions.
  Next measure fixed depths 0/8/31 with independently balanced order, near-full
  append and a full compaction cycle. Probe selection variation and interference
  from preceding writers before attributing it to writeback. If needed, add a
  framed observation replacement control to isolate codec/framing costs.
  Profile load/replay/publication costs, then select one independently measured
  candidate, such as compact encoding or parallel
  semantic validation, where the profiles support it.
  The current restore rehashes serially; its lower index phase partly moves work
  into loading. Follow with exact-generation-check and shared-change-scan candidates
  while preserving stale-writer, corruption and intermediate-state validation.
  Storage-mode cleanup is included with the observation-journal variant. Retain the selected
  in-memory representation, production caller path and full adoption gates.
  The observation control checks its 64 MiB payload limit after encoding; the derived
  candidate limits its output buffer while encoding. Pre-encoding admission remains
  a separate follow-up. Native hash-buffer allocation also deserves profiling
  if cold preparation stays material; observed GC counts alone do not explain its
  latency.
- **Before checkpoint production adoption:** make filesystem eligibility executable,
  verify cache ownership/permissions, and bind cache operations to validated
  directory handles or controlled locations. Account for mount aliases as well as
  symlink/case aliases. Clarify native timestamp and mmap/racy-write assumptions.
  These are adoption requirements, not guarantees established by private sibling
  benchmark caches. Compatible policy-change reuse stays a later optimization.
- **Implemented for the initialization comparison:** strengthen benchmark provenance.
  Include benchmark support/helper sources and imported harness modules, record
  toolchain identity, and define which inputs must match between variants with
  a default mismatch guard. Filename patterns alone do not establish equivalent
  benchmark workloads. Preserve the current frozen evidence; its measured source
  identities were checked separately.
- **Included in the shared initialization candidate:** evaluate a cohesive internal
  materialization policy that groups scratch/durable permissions, restoration,
  member flushes and publication barriers around the same verified writer.
  Preserve the private path's skipped permission bookkeeping and durable callers'
  synchronization guarantees. Measure the proposed policy before adopting it;
  do not add discarded map construction merely to reduce branching.
- **Included in the initialization benchmark matrix:** add a genuinely deep directory
  fixture. The existing `inputs-nested` case measures 1,024 files spread across
  32 one-level directories, not depth. Keep its original labels and raw results.
  New cases cover a 32-level chain and 128 independent eight-level branches; the
  chain exposed the first initialization candidate's regression.
- **Included in the initialization review fix:** preserve capture through search-only
  directories without changing source permissions. Cover ordinary/deep paths,
  symlink metadata and APFS aliases, plus deterministic cloned-output verification,
  exhausted parent-cache capacity and eviction-time identity rejection.
- **Completed before step 3:** remove between-iteration deletion from capture
  timing, synchronize fixtures before measurement, and extend wide/deep native
  paired comparisons. Separate mutex/syscall profiles and temporary counters
  support a traversal hypothesis for the directory-batching regressions. Keep
  current per-file scheduling;
  neither sorted nor original-order batches passed the APFS capture gate. The
  complete comparison and its measurement limits are in the linked report.
- **Included in this measurement slice after review:** reject benchmark output
  inside frozen sources before work, require actual diagnostic samples and complete
  adapter counters, and add regression coverage. Record mount metadata and imported
  diagnostic harness/toolchain identities for future runs. Preserve the historical
  artifacts, map patches to measured source identities, show timing spread and
  distinguish manifest dispatch order, destination lease duration and source-cache
  pressure in the report. These fixes do not adopt a runtime optimization.
- **After step 3, only if directory-heavy capture remains a measured priority:**
  compare relaxing leaf-first eviction for the private destination cache against
  current per-file scheduling. Destination verification is already disabled; this
  changes only its eviction rule. Separately evaluate reducing source-cache mutex
  time around directory open/stat/close calls. Neither hypothesis is validated by
  aggregate mutex percentages. Preserve descriptor bounds, borrowed-handle lifetime,
  source binding checks and eviction-time verification before considering adoption.
- **Before adopting another directory-materialization candidate:** include realistic
  shallow/broad layouts, small sets, deep/wide shapes, restricted directories and
  complete operations. For smaller effects, use even, position-balanced pairs,
  check 1x/3x/7x output-retention sensitivity and replicate mutex and syscall profiles
  in separate processes. These precision experiments belong with that candidate,
  not as a prerequisite for moving to persisted incremental state.
- **When benchmark orchestration next needs another shared behavior:** factor the
  comparison drivers around that concrete need. Reusing provenance helpers is useful
  now; a broad campaign-driver rewrite is not needed to retain rejected experiments.
- **When new fixture/build inputs appear:** include fixture permissions and symlink
  metadata, embedded assets and other build inputs in provenance. These are not
  grounds for invalidating the existing byte-identical fixture comparisons.

The earlier private-input review added restricted-input three-way merge coverage
and corrected that decision's uncertainty. The initialization follow-up includes
the earlier three considered items, with frozen first and revised candidates and
all native samples retained. Its final adoption decision and remaining uncertainty are in the linked
report; persisted incremental state and large-file transfer remain steps 3 and 4.

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
