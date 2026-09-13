# Snapshot index comparison, 2026-09-13

This is the historical first round. See the [optimization follow-up](SNAPSHOT_INDEX_FOLLOWUP.md)
for subsequent changes and comparisons; the numbers below describe the original
prototypes, not the current implementations.

This completes the first representation experiment in the [shared engine plan](SNAPSHOT_ENGINE_PLAN.md). No production transfer path changed. The persistent tree is the preferred candidate for retained snapshots, subject to fixing cold-build cost and structural comparison before adoption.

## 10,000-entry metadata operations

Five-sample medians in milliseconds. Updates include comparison and obtaining the representation identity. Export includes full JSON encoding and the existing protocol root hash.

| Host | Operation | Flat | Partitioned | Tree |
|---|---|---:|---:|---:|
| mac-mini | Cold build | 2.5379 | 4.2897 | 4.8981 |
| mac-mini | One-file edit | 2.3942 | 0.0054 | 0.0026 |
| mac-mini | 100-file edit | 2.5083 | 0.2285 | 0.2181 |
| mac-mini | Add one file | 2.4195 | 0.0062 | 0.0026 |
| mac-mini | Delete tree root | 2.3674 | 0.0053 | 0.3459 |
| mac-mini | One-file edit + full export | 4.1771 | 3.2857 | 2.5396 |
| mac-mini | Rebuild + compare | 2.3127 | 3.9738 | 5.5658 |
| cabal | Cold build | 10.3842 | 21.5796 | 23.6601 |
| cabal | One-file edit | 10.5510 | 0.0483 | 0.0153 |
| cabal | 100-file edit | 10.5893 | 1.2352 | 1.3255 |
| cabal | Add one file | 10.2556 | 0.0465 | 0.0139 |
| cabal | Delete tree root | 10.2124 | 0.0548 | 1.4938 |
| cabal | One-file edit + full export | 17.4893 | 13.3282 | 11.2321 |
| cabal | Rebuild + compare | 10.3747 | 19.1341 | 26.1496 |

The one-edit allocation medians at 10K entries are approximately 4.7 KiB for the tree and 7.5 KiB for the partitioned index on both runners, versus 3.5 MiB for flat on Mac mini and 2.8 MiB on Cabal. These are allocated bytes per operation, not retained index size. JSON buffer pooling and garbage collection affect the flat allocation measurements.

## Scaling and limitations

At 100K entries, a retained tree still updates and compares a single edit in about 2.7 microseconds on Mac mini and 14 microseconds on Cabal. Full export costs about 24 ms and 102 ms respectively. Its difficult structural deletion costs about 4.8 ms and 14.5 ms because the current prototype flattens the changed subtree. The partitioned design handles that deletion in about 19 and 105 microseconds.

Cold creation currently favors the flat representation. At 100K entries, build medians are 23 ms flat versus 52 ms tree on Mac mini, and 99 ms flat versus 242 ms tree on Cabal. Rebuilding a tree after every filesystem scan would therefore be the wrong integration.

These measurements exclude filesystem inventory, content hashing, body transfer, staging, fsync and application reload. They do not change the previously measured watch/push/fetch latency or establish parity with Mutagen or rsync. APFS and Btrfs identify the runner environments; this experiment performs no timed filesystem operations and makes no claim about filesystem-specific gains.

## Decision

Keep a small shared snapshot interface and pursue a persistent tree for retained state. Avoid committing its current shape or digest to the protocol yet. The partitioned design is a useful comparator with cheaper structural changes, but its update cost grows with partition size and ordered export requires a full sort. The flat representation remains the cold-path reference.

Before production migration:

1. Remove avoidable cold construction overhead, including per-entry JSON encoding/allocation, and improve structural comparison. Repeat the same benchmark matrix.
2. Prove that a shared caller can retain the index through preparation and reconciliation without immediately exporting every entry. Keep full discovery and policy invalidation when evidence requires it.
3. Integrate verified direct staging, then measure the actual transfer operations. Metadata-only gains are not sufficient to claim that the user experience improved.

These are the remaining gates within stages 1 and 2 of the plan, not additional independent feature proposals. Durable incremental storage and large-file transfer deltas follow afterward.

## Reproduction and evidence

[Raw samples and contract test output](benchmarks/2026-09-13-snapshot-index.json) retain all 720 observations. Each host used the same source digest, Go 1.27.1, GOMAXPROCS=2 and CGO_ENABLED=0. The Mac mini reported arm64/APFS; Cabal reported amd64/Btrfs. Local race and static checks also passed.

The script and workload definitions are in [the plan](SNAPSHOT_ENGINE_PLAN.md#benchmark-contract). Each benchmark uses a 150 ms target and five repetitions, in Go benchmark order rather than randomized/interleaved host trials. Larger cold cases can execute only one or a few iterations per sample. These are exploratory microbenchmarks, not confidence intervals or reliable latency-tail estimates.

Parent commit: `9f3708a8a2b5ff7b9e7a84df3bd48809101e9d58`. The source digest includes the uncommitted experimental Go files, proto package and module files. The raw report records the exact input list and each host binary hash.

```text
bcb849dc7c8d50420b7595e57be1af45d3bd7693eee811930a9905e135aba481
```
