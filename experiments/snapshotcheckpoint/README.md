# Restart checkpoint experiment

This is the first bounded prototype for snapshot roadmap step 3. Production
callers are unchanged from `f459bb2`. The experiment measures whether preserving
source observations pays for checkpoint loading, fresh selection/stat work,
index reconstruction and publication in a new process.

## Contract

- Each call runs `snapshot.SelectFilesGuarded` again and stats every selected path.
  The checkpoint cannot authorize paths or restore destination/conflict state.
- A key binds the canonical checkout path, root device/inode, filesystem identity,
  OS, boot session, effective selection policy and selection options. A mismatched
  identity causes a full rebuild. Missing boot/stat evidence falls back to cold
  preparation instead of trusting the checkpoint.
- Entries retain native identity, size, full mode, mtime and ctime; APFS also retains
  birth time. Only matching observations reuse metadata and body hashes. Changed
  paths use the existing snapshot builder, with pre/post stat checks before their
  observations can be published. Directory checks retain identity/type/mode but
  permit timestamp/size churn from ignored siblings; fresh selection checks still
  reject changed membership. The trust assumption is native local filesystem
  change stamps; network/coarse-timestamp filesystems need a separate eligibility
  decision before any production adoption.
- The in-memory representation stays unchanged: `manifest.New`, `PrepareUpdates`
  and `Update` use the existing small-vector/retained-Merkle crossover. The disk
  format stores ordered metadata and observations, not serialized derived tree
  nodes. Index reconstruction is timed explicitly. This does not yet eliminate
  index construction across restarts.
- One versioned, checksummed gob payload is limited to 64 MiB and 200,000 entries.
  Format 2 encodes observations in batches of 256 with cancellation checks between
  batches. Reading, checksumming, validation and writing also check cancellation;
  individual I/O reads are capped at 64 KiB. A single record/syscall remains an
  indivisible operation. Format 1 caches rebuild as expendable older versions.
  Oversized, unreadable, malformed, nonregular and mismatched caches are expendable.
  The directory must be caller-owned, private and outside the source, including
  symlink and APFS case aliases. The checksum detects corruption, not malicious
  modification by the same local user.
- A nonblocking writer lock protects one fixed temporary file and atomic rename.
  Readers see a complete generation. Concurrent writers may skip an update;
  last-complete-writer wins is safe because every reader revalidates selection and
  stamps. A writer removes the previous interrupted temporary file while holding
  the lock, keeping storage to one checkpoint, one temporary file and one lock.
- Unchanged restarts do not rewrite the checkpoint. Any changed observation causes
  a full checkpoint replacement in this prototype. There is no journal or history.
  Removing the private cache directory while unused resets it completely.
- Cache writes deliberately do not fsync. These files are recomputable hints,
  never durable source state or transfer receipts. Crash damage causes a cache miss;
  reboot invalidates prior observations. Existing materialization fsync and
  publication guarantees are untouched. A failed cache write does not fail a valid
  preparation; cancellation or source/selection failure does.

The returned snapshot does not prove that its bytes are still current. This probe
ends after source preparation and wire hashing. Production integration must carry
the live selection guard through freezing and verify it afterward, preserving the
ordinary pack/materialization checks. No production caller consumes this API yet.

## Measurement

`scripts/benchmark_snapshot_checkpoint.py --output OUT --rounds 6` builds one probe
binary and alternates fresh processes using `Cold` and `Prepare`. `Cold` calls the
unchanged selection/build/index implementation from `f459bb2`; both modes prepare
the same adaptive index and compute the same wire root. Thus this compares
preparation strategies in the same binary, not released CLI startup performance.

Fixtures contain 1,000 x 32-byte, 10,000 x 4-KiB and 1,000 x 64-KiB regular files,
spread over directories of 100 files, plus an explicit `.errandignore`. Cache-miss,
unchanged restart and one-file edit cases use six alternating pairs each. Each
process starts with no in-memory snapshot; filesystem/page caches remain warm.
Fixture mutation and `sync` occur before each pair, outside the measured interval.

The driver verifies matching roots for every pair, expected hashed-file counts and
whether a checkpoint was written for both modes, including cold reuse/status checks.
Each sample records its position in the pair. Results retain every observation, exact source,
harness and binary identities, native filesystem/mount metadata, test/race/vet logs
and separate selection/load/scan/hash/index/verification/save/wire timings. The
initial pass predates the verification timer and some recovery checks; later
passes have separate frozen inputs and are not pooled with it.

The [review follow-up](../../docs/SNAPSHOT_CHECKPOINT_REVIEW.md) records the directory
and relative-root fixes, cancellable format 2, and first/second position breakdowns.

`Total` measures preparation plus wire identity inside the fresh Go process.
Python's `elapsed` also includes process launch and timeout-polling overhead, so
it is retained as driver-observed elapsed time, not a precise CLI-startup metric.
No transfer, remote staging, workspace creation or browser-ready latency is measured.

## Adoption gates

Warm improvements alone do not justify unconditional use. First address or bound
cache-miss cost and small-workspace regressions. Preserve the measured engine;
compare disk-index reuse and changed-record publication only as explicit follow-up
experiments. Profile codec/load, stat inventory, index reconstruction, verification
and writes separately before choosing a different storage format.

Then integrate at the shared preparation boundary, retain the post-freeze selection
guard, and measure ordinary push, both fetch modes, watch restart, workspace creation
and ephemeral submission. Initial populations and already-running watch sessions
need separate policies; existing in-memory reuse should not pay disk costs each edit.
