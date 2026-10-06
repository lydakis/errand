# Job change bases from the snapshot cache

When a job changes files, its change bundle carries the old bytes of the changed
paths (the base archive) so `--apply` and `fetch --apply` can merge three-way
into a checkout that has moved on. Collection reads only the changed roots, once.
A job that changes nothing never reads its base, and a daemon restart never reads
one either: recovery keeps a bundle that was already committed and otherwise
removes the base.

## Before

Every job still wrote a complete private copy of its baseline tree before the
command started, with a clone attempt, a per-file fsync and a directory barrier,
then deleted it at settlement:

- an ephemeral job copied its extracted workspace;
- a job on a persistent workspace copied the workspace's creation `change-base`,
  although that tree never changes for the workspace's life.

On a Linux cloud host (ext4, 10,000 files, `-- true`) the copy took 3.9–5.9 s and
its removal another 1.1–1.4 s, more than half of an 8–10 s warm job. Without the
fsyncs the copy still took 4.6 s.

## After

A job's change base is its baseline manifest plus the body of each file,
addressed by hash. Collection packs the base archive from the manifest and those
bodies (`snapshot.PackContent`), verifying each body's size and SHA-256 while
copying; the archive is byte-identical to packing the tree.

A job on a persistent workspace reads its bodies in place from the workspace's
creation `change-base`. That tree was written and verified when the workspace
was created, never changes, and cannot be removed while a job holds the
workspace, so the job copies and pins nothing.

An ephemeral job reads its bodies from the runner's snapshot cache, pinned:

- The cache keeps in-memory pins (hash to holder count). Size eviction, TTL
  expiry and `gc cache` skip pinned blobs. A blob that proves corrupt is still
  removed; collection then fails on that body rather than packing wrong bytes.
- A warm job pins each cache hit while extracting, under the cache lock and only
  if the blob it copied and verified is still the one at the path.
- A cold job pins what it verifies and inserts into the cache before the command
  starts. An insert that fails leaves a blob other jobs pin in place.
- Bodies the cache cannot hold (cache disabled, a file over the cache budget, a
  failed insert) are copied into the job's private store, one file per hash,
  without fsync. That is the only per-file copy left.
- Pins are released when the job settles or its admission is rolled back. They
  live in memory because nothing reads a job base after a restart.

Pinned blobs can hold the cache above its byte budget while their jobs run; the
private copies they replace used the same disk outside any budget. Releasing a
job's pins evicts back down to the budget.

For an ephemeral job the `change-base-captured` event now reports `copied=N/M`:
the job copied N of its M distinct bodies into its private store. A job on a
persistent workspace captures nothing and has no such event.

## Measured

Linux cloud host, ext4, 10,000 files, `-- true`, phases from job events. Main
and this change ran interleaved on the same host:

| Job | Before | After |
|---|---:|---:|
| ephemeral, cold | 13.8–14.0 s | 5.9–8.1 s |
| ephemeral, warm | 8.6–10.0 s | 1.6–3.7 s |
| on a persistent workspace | 2.9–7.4 s | 0.49–0.62 s |

What remains of an ephemeral job is extraction (0.7–2.8 s warm, 3.5–4.6 s cold)
and, for a cold job, inserting the upload into the cache (1.6–2.6 s); both still
copy every file. Run and settlement dropped from about 1.5 s to 0.4 s because
there is no base tree left to remove. An earlier campaign on another host of the
same kind measured warm jobs at 8.8–10.6 s before and 1.3–3.3 s after.

At 1,000 files the whole-loop scoreboard harness (earlier host) moved a warm job
from 1.6 s to 0.58 s. Native APFS and Btrfs runners are still to be measured;
file creation on this host costs about ten times a laptop SSD, so their gain is
expected to be smaller in absolute terms.

Three-way `fetch --apply` after a local edit made while the job ran merged
correctly for ephemeral and persistent-workspace jobs, and with the cache
disabled.

## Not changed

- Workspace creation still copies its `change-base` once per workspace, which
  its jobs now read in place. Making it a manifest plus durable pins means
  teaching cache GC about workspace records.
- Warm extraction still copies and hashes every file out of the cache. Cloning
  from the cache on APFS and Btrfs is a separate piece.
- No CLI, API, configuration or bundle format change.
