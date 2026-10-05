# Checkpoint reads on every push, 2026-10-04

After the [blob](WATCH_BLOB_RETAIN.md), [Git status](WATCH_GIT_STATUS.md),
[staging hash](WATCH_STAGING_HASHES.md) and [origin](WATCH_ORIGIN_STATE.md)
changes, a one-file save in a 10K-file explicit watch still read the daemon's
copy of the client's checkpoint (`push/<client>/checkpoint.json`, 1.4 MB at
10K files) four times before the file became visible, and once more after it.
Each read took about 2 ms, about 9 ms per save in all.

## Where a save reads the checkpoint

Per one-file delta push, from a watch whose client already has a checkpoint:

| Read | Where | Request and lock hold | Before visible | Main, ms |
|---|---|---|:---:|---:|
| Base for expansion | `pushBase` → `TransferCheckpoint.Base` | upload, first hold | yes | 2.2 |
| Creation check | `InitializeBase` → `checkInitialized` | upload, second hold | yes | 2.5–2.8 |
| Stage baseline | `stage` → `readVersion` | upload, second hold | yes | 2.0–2.2 |
| Apply precheck | `Apply` → `readVersion` | apply | yes | 1.9–2.1 |
| Advance | `Advance` | apply, after the install | no | 2.1–2.7 |

Medians of 30 saves in each of two instrumented runs. The watch client asks
for the base once per session (`push/base`), not per save; `fetch` and
`gc changes` read the checkpoint outside the save path.

None of these reads decoded anything. `Advance` retains the record it
publishes, and every later read found those bytes: the first read in a
request matched the receiver's retained record, later ones the request's own.
The time went into the read itself:

| Part of one read | Main, ms |
|---|---:|
| Open the destination and storage, path guards | 0.10–0.15 |
| `io.ReadAll` of the record, 3.3 MB allocated | 1.4–2.4 |
| `bytes.Equal` with the retained record | 0.22–0.26 |
| Recheck the paths | 0.02 |

`io.ReadAll` reads into chunks of growing size and then copies them into one
slice of the final length, so each read allocated more than twice the record
and copied it once more before the comparison read it again. Five reads per
save left about 16 MB of garbage.

## Change

`TransferCheckpoint.read` compares the record while reading it, in
`readTransferRecordMatching`:

- It takes the record it would have compared first: the request's own, or
  else the receiver's retained one.
- It reads the file through a pooled 64 KiB buffer and compares each chunk with
  the same span of that record's bytes. It reports a match only when every
  chunk is equal and the file ends exactly where the record does. A match
  returns no bytes and allocates nothing in proportion to the record.
- At the first chunk that differs, or a file that is longer or shorter, it
  returns the file's bytes instead: the prefix that matched, copied from the
  record, then the differing chunk and the rest of the file, read under the
  same limit. Those bytes take the existing path: the receiver's retained
  record if its bytes are equal, otherwise a full decode and validation.
- Without a retained record, it reads the file exactly as before.

Opening the guarded storage, the regular-file check (now shared through
`openTransferRecord`), the size limit, the relationship check on every hit,
the retention rules and the number of reads are unchanged.

## Why the byte-exact comparison still holds

The rule, from the comment on `checkpointReadCache`: every access opens the
guarded storage and reads the bounded regular record in full, and reuse
depends on exact bytes, so an in-place edit is detected even when size and
timestamps are restored. There is no global cache and no timestamp shortcut.

- **Every access still reads the whole file.** A match requires every byte up
  to end of file. A longer file differs in the chunk that runs past the
  record's end; a shorter one reaches end of file early. Either way the read
  is a mismatch.
- **Only the order changed.** Before, the file went into a new buffer and was
  then compared; now each chunk is compared as it arrives. The same bytes are
  compared with the same record, and no size, timestamp or hash stands in for
  them.
- **A mismatch yields exactly the file's bytes.** The prefix taken from the
  record is the part the comparison has just found equal to the file. The
  rest is what the file holds after it. A decoded record is therefore built
  from the file's bytes, as before.
- **The limit holds.** A match is exactly as long as the retained record,
  which was itself read or written within the limit. A mismatch is checked
  against the limit before it is returned, as before.
- **The same record is chosen.** The record compared first is the one the old
  code compared first, and the fallback to the retained record is unchanged,
  so every read returns the record it returned before.

The retained record's bytes are never modified, so concurrent requests can
compare against them.

## Alternatives not taken

- **One read per lock hold.** The stage could reuse the record the creation
  check read a moment earlier under the same hold, and `Advance` the one the
  apply precheck read. That removes reads, but it changes the rule that every
  access reads the file. Each read now costs about 0.5 ms, so the gain would be
  small.
- **A file stamp as the key.** [Checkpoint reuse](CHECKPOINT_REUSE.md) already
  rejected timestamp shortcuts: size and times can be restored after an
  in-place edit, and file timestamps are coarse enough for two writes to share
  one.

## What a read costs now

| Read | Main, ms | Candidate, ms |
|---|---:|---:|
| Base for expansion | 2.2 | 0.60–0.63 |
| Creation check | 2.5–2.8 | 0.57–0.59 |
| Stage baseline | 2.0–2.2 | 0.49–0.50 |
| Apply precheck | 1.9–2.1 | 0.57–0.60 |
| Advance, after the install | 2.1–2.7 | 0.54–0.57 |

Of the remaining 0.5 ms, about 0.1 ms opens the storage and checks its paths,
and about 0.4 ms reads and compares the 1.4 MB record. In a warm
microbenchmark the read and comparison take 0.14 ms against 1.3–1.7 ms for
`io.ReadAll` and `bytes.Equal`, and allocate 0.5 KB against 3.3 MB.

## Unchanged

- Five reads per save, four of them before the file is visible.
- The first read after a daemon restart has no retained record. It reads,
  decodes and validates the record as before.
- The checkpoint is still encoded, written and re-validated by `Advance`
  after the file is visible.

## Tests

`internal/changes/checkpoint_read_test.go`:

- `TestCheckpointReadMatchesRetainedBytesWithoutCopying` reads an unchanged
  1.4 MB checkpoint through new request handles, matched first against the
  receiver's retained record and then against the request's own. It requires
  each read to allocate less than a sixteenth of the record. Main allocates
  3.15 MB per read and fails it.
- `TestCheckpointReadRefusesEditsPastFirstChunk` edits the last entry's
  digest, appends a newline (still valid JSON) and drops the last byte, each
  in place with times restored. Both a handle that holds the record and a new
  request refuse the edited records, and the appended newline is decoded
  afresh from the bytes on disk.
- `TestReadTransferRecordMatchingComparesEveryByte` flips the first byte, the
  bytes on both sides of a chunk boundary and the last byte, appends a byte
  and a chunk, and truncates by a byte, to one chunk and to nothing, for a
  record that ends inside a chunk and one that ends on a chunk boundary. Only
  the unchanged file matches, and every mismatch returns exactly the file's
  bytes, never the record's.
- `TestReadTransferRecordMatchingKeepsRecordChecks` refuses a symlink, reports
  a missing record as missing, and refuses a record past the size limit that
  starts with the retained bytes.

Mutation checks, each reverted afterwards:

- Restoring main's read path fails the allocation test (3.15 MB per read).
- Accepting end of file at any length fails the truncated checkpoint and the
  short reads.
- Comparing only the first chunk fails the last-entry and trailing-newline
  checkpoints and the every-byte test.
- Matching as soon as the record's length is reached, without reading to end
  of file, fails the appended byte on a record that ends on a chunk boundary.

## Results, 10K files, Linux

The change removes work: about 6.7 ms of reading before the file is visible
and about 2 ms after it, and about 16 MB of garbage per save. On this host,
that shows in process but not in end-to-end delivery, which varies by more
than the change.

In-process `BenchmarkWatchPhases` (edit to receipt, 30 saves per run,
alternating main and candidate), per save:

| Metric | Main | Candidate | Paired ratio | Candidate faster |
|---|---:|---:|---:|---:|
| Stage handler | 32.8 ms | 28.4 ms | 0.860 | 6/6 |
| Apply handler | 47.3 ms | 43.6 ms | 0.925 | 5/6 |
| Process CPU | 125.5 ms | 108.6 ms | 0.889 | 6/6 |
| Wall, edit to receipt | 124.6 ms | 114.8 ms | 0.927 | 5/6 |

Watch matrix at 10K, paired against main (`d74be06`) with the order
alternating per case ([run order](WATCH_NATIVE.md#run-order)). Visible
delivery, median ms of the per-round medians. Campaigns 1 and 2 ran three
rounds each, back to back on the same binaries. Campaign 3 ran six rounds on
fresh builds, with the decision rule written down before it started: claim
an end-to-end gain only if both cases had a paired ratio of 0.97 or less and
the candidate was faster in at least four of six rounds.

| Campaign | Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---|---:|---:|---:|---:|
| 1 | explicit-inplace-edit | 95 | 74 | 0.781 (0.766–1.085) | 2/3 |
| 1 | git-tracked-inplace-edit | 96 | 77 | 0.798 (0.759–1.035) | 2/3 |
| 2 | explicit-inplace-edit | 84 | 86 | 1.041 (1.020–1.072) | 0/3 |
| 2 | git-tracked-inplace-edit | 94 | 94 | 0.997 (0.695–1.386) | 2/3 |
| 3 | explicit-inplace-edit | 85 | 81 | 0.965 (0.860–1.202) | 4/6 |
| 3 | git-tracked-inplace-edit | 83 | 83 | 1.034 (0.888–1.129) | 2/6 |

Campaign 3's receipt times agree: 1.018 (0.827–1.099, 2 of 6 faster) for
explicit and 0.982 (0.899–1.084, 4 of 6) for Git. Single cells on this host
moved by 20–30 ms between rounds, several times the 6.7 ms the change removes
before the file is visible, so end-to-end delivery shows no difference
either way. The daemon does the same work for both cases, so the Git ratio
above 1 is not a slowdown specific to Git selection. This change is kept for
the CPU and allocation it removes, not for a measured end-to-end speedup.

Raw reports: [benchmarks/2026-10-04-checkpoint-reads-linux.json](benchmarks/2026-10-04-checkpoint-reads-linux.json).
