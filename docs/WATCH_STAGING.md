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
write. Memory: records are evicted least recently used first once their
estimated retained size passes 64 MiB, the checkpoint cache's budget, and a
removed workspace's record is dropped at removal.

### Results

Phase benchmark, three alternating base/candidate pairs, 15 iterations each:
10K `WatchPhases` 220–223 → 168–173 ms per operation, and 1K `small` 82–88 → 75–78 ms.

Watch matrix, three rounds, back-to-back pairs in an order fixed per case
([run order](WATCH_NATIVE.md#run-order)), against
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

## Change 2: stream manifest root hashes (`1e3c1af`)

After change 1, a 10K one-file push still hashed about four whole manifests
(`expandSourceSnapshot`, the client's push, `checkInitialized`, and checkpoint
records), ~53 ms of CPU. `Manifest.RootHash` JSON-encoded the manifest by
reflection and then hashed it. It now streams the same bytes into SHA-256
through a hand-written encoder, which hands any string that needs escaping
to `json.Marshal`. A test checks byte equality with `json.Marshal` on edge
cases (quotes, HTML characters, control bytes, invalid UTF-8, U+2028) and
random manifests. The wire identity is unchanged.

A 10K-entry hash drops from 10.7 to 5.9 ms and from 2.7 MB to 33 KB allocated
(`BenchmarkManifestRootHash10K`). Phase benchmark, three pairs: 161–165 →
142–160 ms per operation.

Watch matrix at 10K files, four rounds, against change 1 (`487d36c`):

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 201 | 171 | 0.856 (0.772–0.873) | 4/4 |
| git-tracked-inplace-edit | 233 | 186 | 0.800 (0.776–0.884) | 4/4 |

The watch gain is larger than the CPU saved per push. The lower allocation
rate (less GC work on a 4-vCPU host) may explain part of it; that has not
been isolated.

Raw reports: [benchmarks/2026-09-25-manifest-hash-stream-linux.json](benchmarks/2026-09-25-manifest-hash-stream-linux.json).

## Change 3: retain published checkpoint records (`193b592`)

Each push decoded the checkpoint written by the previous push, which carries
the full source manifest (~10 ms at 10K). `TransferCheckpoint.save` used to
drop its caches ("never keep a cache across a write"). It now retains the
record it just published, built from the exact published bytes and passed
through the same validation `read` applies. The read-side contract is
unchanged: every read loads the file in full and reuses a record only on an
exact byte match, so an interrupted or later replacement is decoded as before.
A failed publication retains nothing. States containing invalid UTF-8, which
JSON would rewrite, are not retained. Tests check that the retained state
equals decoding the published file, that it does not alias the caller's
manifest, and that invalid UTF-8 states are skipped. Retention adds ~4 ms of
validation per push; the saved decode is larger.

The phase benchmark was too noisy to separate this change (staging −5 to −13 ms,
apply mixed). Watch matrix at 10K, four rounds, against change 2 (`1e3c1af`):

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 175 | 154 | 0.885 (0.804–0.943) | 4/4 |
| git-tracked-inplace-edit | 184 | 173 | 0.949 (0.880–0.949) | 4/4 |

The Git-case effect is close to this host's ~5% noise floor, but the candidate
won every round in both cases.

Raw reports: [benchmarks/2026-09-25-checkpoint-retention-linux.json](benchmarks/2026-09-25-checkpoint-retention-linux.json).

## Cumulative, 10K files, Linux

Git-tracked in-place edit, median visible delivery: 700 ms (v0.5.0) → 316 ms
(Git evidence) → 217 ms (record cache) → 186 ms (streamed hashes) → 173 ms
(checkpoint retention). Explicit in-place edit: 307 → 154 ms. These
medians come from separate paired campaigns on the same host. Treat the
chain as approximate.

## Remaining candidates

- Redundant hashes of the same manifest (for example `checkInitialized`
  re-hashing the unchanged creation manifest on every push).
- Checkpoint encoding on every save (~12 ms at 10K; reflection-based JSON of
  the full manifest).
