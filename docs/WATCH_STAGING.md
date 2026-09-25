# Daemon staging cost for one-file pushes, 2026-09-25

The [realistic watch baseline](WATCH_REALISTIC_BASELINE.md) showed daemon
staging at ~105 ms for a one-file push to a 10K-file workspace (1K: ~21 ms),
even when the client's preparation took 5 ms.

## Profile

CPU profile of `BenchmarkWatchPhases` (10K files, 30 iterations, in-process
daemon), restricted to `handleWorkspacePush`. Times are per push:

| Cost | Per push | Where |
|---|---:|---|
| Decode and validate `workspace.json`, which holds the full creation manifest | ~62 ms | `workspaceStore.read` via `pushWorkspace` |
| Hash whole manifests (JSON-encode, then SHA-256) | ~40 ms total, part client side | `proto.Manifest.RootHash` from `Snapshot.RootHash`, `checkInitialized`, checkpoint records |
| Read and validate the transfer checkpoint | ~20 ms | `TransferCheckpoint.read` |

## Change 1: cache decoded workspace records (`487d36c`)

`workspaceStore.read` keeps the decoded, validated record per workspace ID. It
returns a deep copy while the opened file's device, inode, size, mode and
timestamps are unchanged. Records are only replaced by `replaceJSONDurable`
(write and rename), so any replacement or in-place write misses the cache and
is decoded and validated as before. Tests cover isolation of returned copies,
immediate visibility of replacements, and rejection of an in-place corrupt
write. Memory: one decoded manifest per workspace the daemon has read.

### Results

Phase benchmark, three alternating base/candidate pairs, 15 iterations each:
10K `WatchPhases` 220–223 → 168–173 ms per operation, and 1K `small` 82–88 → 75–78 ms.

Watch matrix, three rounds, back-to-back pairs with alternating order, against
the Git-evidence build (`09e3675`). Median visible delivery in ms:

| Files | Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---:|---|---:|---:|---:|---:|
| 1,000 | explicit-inplace-edit | 69 | 65 | 0.881 (0.864–1.005) | 2/3 |
| 1,000 | git-tracked-inplace-edit | 76 | 74 | 0.997 (0.885–1.067) | 2/3 |
| 1,000 | explicit-inplace-create | 88 | 77 | 0.943 (0.823–0.949) | 3/3 |
| 10,000 | explicit-inplace-edit | 288 | 204 | 0.709 (0.635–0.777) | 3/3 |
| 10,000 | git-tracked-inplace-edit | 326 | 217 | 0.672 (0.659–0.694) | 3/3 |
| 10,000 | explicit-inplace-create | 387 | 317 | 0.822 (0.786–0.834) | 3/3 |

At 10K, one-file edits are about 30% faster and creates about 18% faster. The
1K effects are within this host's ~5% noise floor, except creates.

Raw reports: [benchmarks/2026-09-25-workspace-record-cache-linux.json](benchmarks/2026-09-25-workspace-record-cache-linux.json).

## Remaining candidates

- Reuse manifest root hashes instead of re-encoding whole manifests (~40 ms at 10K).
- Checkpoint read/validation (~20 ms at 10K).
