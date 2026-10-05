# Checkpoint write after every save, 2026-10-05

After the [expansion](WATCH_EXPANSION.md) change, a one-file save in a
10K-file watch is visible on the runner about 43 ms after the edit, but the
save only finishes, and the watch only takes the next one, about 35 ms later.
The largest step in that tail is `TransferCheckpoint.Advance`, which publishes
the checkpoint the next save's delta is built against.

## What Advance spends

Marks at each step of `Advance`, medians of 30 Git-selected saves at 10K
files per instrumented run, two runs each, in the order main, candidate,
candidate, main:

| Step | Main, ms | Candidate, ms |
|---|---:|---:|
| Read the checkpoint and the receipt, check them | 0.5–0.6 | 0.6–0.7 |
| Merge the accepted changes into the manifest, validate it | 3.2–3.5 | 3.0–3.2 |
| Bind the receipt to the new revision, write it | 1.6–1.7 | 1.6–1.7 |
| Encode the checkpoint as JSON | 11.1–11.7 | 1.5–1.7 |
| Write, flush and rename it, flush the directory | 4.5–5.1 | 4.1–4.2 |
| Retain it: copy the manifest, checks, root hash | 2.2–2.7 | 1.4 |
| `Advance` in all | 26.6–26.7 | 14.8–15.1 |

Explicit-selection runs on main put the encode at 10.1–11.9 ms as well.

On main, `json.Marshal` walks the 10K entries by reflection, and the root
hash the next expansion needs encodes the same manifest a second time.

## Change

- `checkpointState.encode` writes the checkpoint's JSON field by field, and
  the manifest with `proto.AppendManifestJSON`, the encoder behind
  `Manifest.RootHash`. It sizes the buffer once
  (`proto.ManifestJSONSize`), so a checkpoint is one allocation of its own
  length, as with `json.Marshal`.
- A checkpoint's root hash is the SHA-256 of its manifest's JSON, and
  `encode` has just written those bytes. `save` hashes that part of the
  record instead of encoding the manifest again.
- Record writers take an encode function, still called after the storage
  check and before the write, so the publication checks run in the same
  order.

## Why the files and identities are unchanged

`encode` returns exactly what `json.Marshal` returns for the same state, so
the checkpoint file is byte for byte the same. Reads, the byte-exact
comparison with the retained record, and `gc changes` see no difference.

- A table of states, 200 random ones, and a fuzz test over every single byte
  in each string field compare `encode` with `json.Marshal`. Invalid UTF-8,
  HTML characters, control bytes and the extreme numbers are covered.
- A test lists the fields and JSON tags of `checkpointState` and
  `fsidentity.Identity`, and that none of the types involved has its own JSON
  encoding. A new field fails it until `encode` writes the field.
- The manifest part of the output is `AppendManifestJSON`'s, which the same
  tests compare with `json.Marshal` of the manifest; `RootHash` hashes those
  bytes. Every case also checks that hashing the part gives `RootHash`.

## Unchanged

What `Advance` checks and in which order, which files it writes, their
flushes and renames, when it writes them, and every error it returns.

## Not done

- The merge in `Advance` (about 3 ms) builds the same manifest expansion
  built for this delta during the push request. Reusing it would mean keeping
  that manifest in memory from the push request to the apply request, with
  its own place in the checkpoint cache's memory budget. Left for later.
- Writing and flushing the checkpoint (about 4 ms) is the durability barrier
  that makes a save's revision survive a crash.

## Tests

- `internal/changes/checkpoint_json_test.go`: `encode` against `json.Marshal`
  (table, random and fuzz), the hash of the manifest part against
  `RootHash`, and the field list.
- `internal/proto/manifest_json_test.go`: `AppendManifestJSON` and
  `AppendJSONString` against `json.Marshal`, and `ManifestJSONSize` exact
  when no string is escaped and never more than the output.
- `TestCheckpointAdvanceRetainsRecordIdentity` checks that the retained
  record's identity equals the published manifest's `RootHash`. It fails if
  `save` hashes the whole record instead of its manifest.
- `BenchmarkCheckpointEncode` encodes a 10K-entry checkpoint both ways.

## Results, 10K files, Linux

All runs alternate main (`5cd41b0`) and the candidate on the same host, a
4-CPU cloud container on ext4.

`BenchmarkCheckpointEncode`, 10 interleaved runs: 5.23 ms against 1.58 ms
(−70%), one allocation instead of three or four, and 3.6% fewer bytes.

In-process `BenchmarkWatchPhases` (edit to receipt, 30 saves per run, six
alternating pairs), per save:

| Metric | Main | Candidate | Paired ratio | Candidate faster |
|---|---:|---:|---:|---:|
| Apply handler | 46.6 ms | 37.7 ms | 0.817 | 6/6 |
| Process CPU | 79.5 ms | 66.6 ms | 0.857 | 6/6 |
| Wall, edit to receipt | 102.1 ms | 92.1 ms | 0.904 | 6/6 |
| Push handler (stage) | 18.2 ms | 17.7 ms | 0.987 | 3/6 |
| Client | 36.6 ms | 36.1 ms | 0.975 | 4/6 |

The push handler and the client, which run before the file is visible, are
level, as expected.

Watch matrix at 10K, six rounds paired against main with the order
alternating per case ([run order](WATCH_NATIVE.md#run-order)), median ms of
the per-round medians. The rules were written before each run: claim an
end-to-end gain only if both cases have a receipt ratio of 0.95 or less and
the candidate is faster in at least five of six rounds; call it a visible
regression if either case has a visible ratio of 1.05 or more and the
candidate is slower in at least five of six rounds. The first run measured
an earlier version of the encoder that allocated about 25% more than it
needed; the second measured this code.

| Run | Case | Measure | Main | Candidate | Paired ratio (range) | Candidate faster |
|---|---|---|---:|---:|---:|---:|
| 1 | explicit-inplace-edit | receipt | 100 | 88 | 0.877 (0.814–1.011) | 5/6 |
| 1 | explicit-inplace-edit | visible | 59 | 61 | 1.066 (0.851–1.142) | 2/6 |
| 1 | git-tracked-inplace-edit | receipt | 96 | 87 | 0.912 (0.834–0.993) | 6/6 |
| 1 | git-tracked-inplace-edit | visible | 59 | 62 | 1.025 (0.884–1.111) | 3/6 |
| 2 | explicit-inplace-edit | receipt | 91 | 83 | 0.915 (0.824–1.141) | 5/6 |
| 2 | explicit-inplace-edit | visible | 58 | 57 | 1.010 (0.876–1.307) | 3/6 |
| 2 | git-tracked-inplace-edit | receipt | 95 | 90 | 1.018 (0.740–1.168) | 3/6 |
| 2 | git-tracked-inplace-edit | visible | 57 | 63 | 1.132 (0.906–1.318) | 1/6 |

The first run met the receipt rule and showed no visible regression. The
second did not meet the receipt rule, and its Git case met the regression
rule for visible delivery. Everything before the install is the same code
in both builds, so to check that result the instrumented Git runs above
compared each step before the file is visible: none moved consistently
between the two builds, and visible delivery there was 60.4 and 61.9 ms on
main against 63.4 and 62.2 ms on the candidate, inside the spread of main's
own runs. The change is claimed for the time it removes from `Advance` and
the CPU it saves, which show in every in-process pair, not as an end-to-end
gain on this host.

Raw reports: [benchmarks/2026-10-05-checkpoint-write-linux.json](benchmarks/2026-10-05-checkpoint-write-linux.json).
