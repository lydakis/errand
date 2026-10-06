# Rereading only the file that changed mid-push

## Problem

A save that lands while the previous push is still reading that file makes
the push stop with "source changed" and resample. Before this change the watch
then discarded every cached hash and reselected the whole tree, so the retry
rehashed every file. Bursts of saves (format on save, an editor writing twice,
a build rewriting a file) hit this often. With
`ERRAND_TRACE_WATCH=1` it shows as `prepare=full reason=invalidated`.

## Change

Source-change errors now name the regular file whose bytes, size or mode
changed while it was hashed or packed. When every cause names such a file,
the watch marks those files dirty, as if their native events had arrived, and
keeps its cached hashes and selection evidence; a failed preparation also
keeps the hints it had consumed. Cached hashes are keyed by each file's
identity, size, mode and timestamps, and a dirty file's hash is dropped before
rereading, so a change with identical stat evidence is still rehashed. A
changed file may have been replaced, so its directory is relisted.

Anything else still forces full reconciliation: a path that became a
directory or link, a vanished entry, a directory, selection policy or Git
index change, a path outside the watched root, or a mutation joined with a
storage failure.

## Measurements

Linux cloud host (4 CPUs, ext4), `scripts/benchmark_watch.py --selection git
--git-tracking all --samples 5 --pause-seconds 0.5 --skip-once --skip-rsync
--trace`, main `154c370` against this change. The burst is 20 saves 5 ms apart.

| Files | Burst, last save visible | Burst, last receipt | Retry preparation |
|---|---:|---:|---|
| 10K, before | 0.60 s | 0.64 s | full, 457 ms |
| 10K, after (3 runs) | 0.19–0.24 s | 0.21–0.26 s | relisted, 4–6 ms |
| 100K, before | 5.0 s | 5.1 s | full, 4.6 s |
| 100K, after | 0.36 s | 0.49 s | relisted, 50 ms |

Each after-run hit the source-change retry one or two times during its burst;
a single save without a concurrent write takes the same path as before.
