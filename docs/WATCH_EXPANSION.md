# Delta expansion on every push, 2026-10-05

After the [checkpoint read](WATCH_CHECKPOINT_READS.md) change, the largest
cost left before a one-file save in a 10K-file watch becomes visible was the
daemon expanding the client's delta into the full source manifest. Phase
marks in the daemon put it at about 20 ms per save: 13 ms from the base
checkpoint to the merged manifest, and 6 ms more to hash the result.

## What expansion does

`ExpandTransferSourceBase` takes the client's checkpoint (the base) and a
delta, and rebuilds the client's full source manifest:

1. Validate the delta bundle.
2. Build a snapshot of the base, which validates all 10K entries.
3. Compare the base's identity (the SHA-256 of its manifest JSON) with the
   delta's baseline.
4. Merge the base and the delta, then validate the merged manifest: every
   entry again, plus each path's source rules.
5. Hash the merged manifest and compare it with the root the client sent.
6. Recompute the delta from the two snapshots and compare it with the one
   sent.

Profiled on 10K entries, in a benchmark of this function with the base's
identity already known:

| Step | Share | Where the time went |
|---|---:|---|
| Validation, steps 2 and 4 | about 45% | `path.Clean` builds a cleaned copy of each path to compare with it; `path.Dir` cleans again for every ancestor |
| Hashing, step 5 | about 25% | JSON for the hash scans each string with seven comparisons a byte, then SHA-256 |
| Merge, step 4 | about 15% | Each entry walks its ancestors with `path.Dir` |

Step 3 hashes the base only on the first push after each save: the
checkpoint `Advance` publishes is retained without its identity, so the next
expansion computes it, before any of that save's files can appear.

## Change

- `internal/relpath` answers the two questions without building strings.
  `IsClean(p)` reports `path.Clean(p) == p` in one pass over the bytes.
  `Dir(p)` returns `path.Dir(p)` for a clean path by slicing at the last
  slash.
- The archive path check, the change path check and the archive parent walk
  use them. The parent walk runs only after its path has passed the clean
  check. The checkpoint merge uses `relpath.Dir` for a path that is clean,
  since its ancestors are too, and `path.Dir` otherwise.
- The Git metadata check walks path components without allocating a slice.
- Manifest JSON for the root hash scans strings with a 256-entry table of the
  bytes `json.Marshal` copies unescaped, instead of seven comparisons a byte.
- `Advance` computes the identity of the checkpoint it publishes while
  retaining it, after this save's install, so the next push finds it ready.
- Retaining that checkpoint validated its manifest a second time. Both callers
  of `save` (`Initialize` and `Advance`) validate the manifest before
  publishing it, so `save` skips that one check and keeps every other check a
  decoded record gets (relationship, creation digest, initial checkpoint and
  application identity).

## Why the rules are unchanged

`relpath.IsClean` and `relpath.Dir` agree with `path.Clean` and `path.Dir` for
every path of up to six bytes over `/`, `.` and `a`, and under fuzzing.
Every path that reaches `relpath.Dir` has passed `IsClean`, and the merge
falls back to `path.Dir` for one that has not. So every check accepts and
refuses exactly the paths it did.

The JSON table holds the bytes the old comparisons let through, and anything
else still goes to `json.Marshal`. A fuzz test checks the encoding against
`json.Marshal` for every single byte and for fuzzed strings in each field,
so root hashes, which are part of the wire protocol, are unchanged.

A checkpoint is still validated before it is published, and a checkpoint read
from disk is validated in full as before. Only the retained copy of a
checkpoint the daemon has just validated and written skips the repeat.

## Unchanged

What expansion checks and in what order, the root hash format, the checkpoint
format, when a checkpoint is read and how its bytes are compared, and every
error a malformed delta or checkpoint produces.

## Tests

- `internal/relpath/relpath_test.go`: every path of up to six bytes over `/`,
  `.` and `a`, and a fuzz test, against `path.Clean` and `path.Dir`.
- `FuzzManifestJSONMatchesEncodingJSON` in `internal/proto`: every single
  byte, and fuzzed strings, in each string field, against `json.Marshal`.
- `TestCheckpointAdvanceRetainsRecordIdentity` in `internal/changes`: after
  `Advance`, the retained record already holds the published manifest's
  identity, and a new request decoding the file gets the same state and
  identity. It fails without the change.
- `BenchmarkExpandTransferSource` in `internal/changes` expands a one-file
  delta against a 10K-entry checkpoint, with the base's identity known
  (`hashed`) and computed (`fresh`).

## Results, 10K files, Linux

All runs alternate main (`175601c`) and the candidate on the same host, a
4-CPU cloud container on ext4.

`BenchmarkExpandTransferSource`, 10 alternating runs each:

| Base identity | Main | Candidate | Change |
|---|---:|---:|---:|
| Known (`hashed`) | 12.0 ms | 9.1 ms | −25% |
| Computed (`fresh`) | 15.2 ms | 11.2 ms | −27% |

Allocations per expansion fall from about 10,280 to 65, and bytes by 20%.
In a watch, main expands against a freshly published checkpoint (the
`fresh` row) and the candidate against one `Advance` already hashed (the
`hashed` row): 15.2 ms against 9.1 ms before the file is visible, with about
2.3 ms of that hashing moved after the install. A 10K-entry root hash
(`BenchmarkManifestRootHash10K/stream`) takes 2.30 ms against 2.97 ms.

In-process `BenchmarkWatchPhases` (edit to receipt, 30 saves per run, six
alternating pairs), per save:

| Metric | Main | Candidate | Paired ratio | Candidate faster |
|---|---:|---:|---:|---:|
| Push handler (stage) | 18.3 ms | 14.4 ms | 0.787 | 6/6 |
| Apply handler | 32.8 ms | 33.8 ms | 1.003 | 3/6 |
| Client | 33.2 ms | 33.0 ms | 0.965 | 5/6 |
| Process CPU | 77.7 ms | 72.4 ms | 0.956 | 5/6 |
| Wall, edit to receipt | 84.4 ms | 80.8 ms | 0.952 | 4/6 |

The apply handler is level: it now hashes the checkpoint it publishes and no
longer validates it twice.

Watch matrix at 10K, six rounds paired against main with the order
alternating per case ([run order](WATCH_NATIVE.md#run-order)), median ms of
the per-round medians. The decision rule was written before the run: claim
an end-to-end gain only if both cases have a visible-delivery ratio of 0.97
or less and the candidate is faster in at least five of six rounds.

| Case | Measure | Main | Candidate | Paired ratio (range) | Candidate faster |
|---|---|---:|---:|---:|---:|
| explicit-inplace-edit | visible | 52 | 48 | 0.901 (0.815–1.027) | 4/6 |
| explicit-inplace-edit | receipt | 86 | 79 | 0.936 (0.791–0.984) | 6/6 |
| git-tracked-inplace-edit | visible | 54 | 47 | 0.841 (0.815–1.140) | 5/6 |
| git-tracked-inplace-edit | receipt | 87 | 78 | 0.885 (0.785–1.139) | 5/6 |

Both cases came out faster, but explicit visible delivery was faster in
only four of six rounds, so by the rule above this is not claimed as an
end-to-end gain. The change is kept for the push-handler time and CPU it
removes, which show in every in-process pair. Main's visible delivery on
this host was 52–54 ms in this run, against 83–95 ms in the
[checkpoint read](WATCH_CHECKPOINT_READS.md) runs a day earlier, so only
ratios within one run compare.

Raw reports: [benchmarks/2026-10-05-expansion-linux.json](benchmarks/2026-10-05-expansion-linux.json).
