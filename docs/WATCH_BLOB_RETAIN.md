# Blob retention without a store scan, 2026-09-26

Before a push mutates the workspace, the daemon retains the pushed file bodies
in the transfer blob store (`TransferBlobStore.Retain`, called from
`TransferSession.Apply`); the client does the same when fetch imports a job's
bodies. Retain listed the whole store and lstat'ed every
entry. It needed that listing for three things: which of the pushed bodies
were already stored, the byte total for the `MaxBytes` budget, and which
`.blob-*` insertion files an interrupted retention had left behind.

Every save adds a body, and bodies stay until `gc changes`. The scan therefore
grew with the length of a watch session, not with the size of the push. In a
profile of one-file saves to a 10K-file workspace, it was the largest step
before the file became visible, at about 40 ms (roughly 5 µs per stored body
on that host).

## Change

The blob directory now holds a usage record, `.usage`, containing
`{"bytes":N}`: the total size of the published bodies.

- **With a record,** Retain lstats only the bodies the push needs and budgets
  from `N`. Its cost follows the size of the push, not the size of the store.
- **Without a record,** Retain scans the store as before, reclaims insertion
  files, and writes a record. This happens for a new store, after an
  interrupted or failed retention, after `Prune`, or when the record is
  malformed.
- Stored bodies that the push reuses are verified exactly as before: size,
  type and full content hash.
- `Stats` and dry-run `Prune` still scan and leave the record alone.

## Why the record is exact

Only `Retain` and `Prune` write the store. `MaterializeBase` and the other
reconstruction paths only read it. Every writer holds the relationship's
operation lock, including across processes:

- In the daemon, pushes and remote `gc changes` hold the workspace's control
  gate. The daemon's state-directory lock keeps other processes out.
- On the client, `fetch` and `gc changes` hold `lockWorkspaceTransfer`, a file
  lock shared by every CLI process. `df` and dry-run collection take it without
  waiting and only read.

The record lives on disk next to the bodies. No process keeps it in memory, so
a change made by another process (for example `gc changes` in the CLI) cannot
leave a stale copy behind.

Retain orders its steps so the record exists only while it matches the store,
even after a crash:

1. When bodies are missing, Retain removes the record before creating any
   insertion file. The data barrier that already precedes the renames makes
   that removal durable. No crash can keep the old record next to a newly
   published body.
2. The new record is written under an insertion name and synced with the
   bodies, so the same barrier makes its contents durable.
3. Retain renames it to `.usage` only after the final directory barrier.
   A record that survives a crash therefore never counts a body whose rename
   was lost.
4. The last rename is not synced. If a crash loses it, the next Retain scans.
5. Any failure after step 1 leaves no record, and the next Retain scans.

`Prune` removes the record and syncs the directory before it deletes anything.
The next Retain rebuilds the record with one scan.

The budget is the same as before: stored bytes plus the missing bodies, at
most `MaxBytes`. The old scan counted insertion files and then subtracted
those it reclaimed. The record never counts them.

## Insertion files

Because the record is removed before any insertion file exists, a process
crash, error or cancellation during retention always leaves the store without
a record. The next Retain scans and reclaims those files before budgeting.

The new record uses an insertion name until the final rename. An abandoned
record is therefore reclaimed like any other insertion file.

One case is left to `gc changes`. A system crash before the data barrier, on a
filesystem that persists the new insertion files but not the earlier removal in
the same directory, can leave both a record and insertion files. The record
still counts the published bodies exactly, and the insertion files are never
budgeted. `Prune` reclaims them. Journaling filesystems normally commit these
directory updates in order, so this should be rare; it was not tested on real
hardware.

## What changed besides speed

- A save that adds bodies syncs one more small file (the new record). On
  Darwin this is a member sync, which the existing barrier drains.
- The scan also rejected unexpected names and non-regular files anywhere in the
  store. With a record, Retain checks only the names it needs: a needed name
  that is not a regular file is still an error. Stats, `Prune` and any
  record-less Retain still check the whole store.
- A malformed record is rebuilt from a scan. A `.usage` that is not a regular
  file is an error, as the scan already treated it.

## Results, 10K files, Linux

Watch matrix at 10K, three rounds of seven saves, paired against main
(`c2dbf0e`) with the order alternating per case ([run order](WATCH_NATIVE.md#run-order)).
Visible delivery, median ms:

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit | 152 | 114 | 0.751 (0.743–0.780) | 3/3 |
| git-tracked-inplace-edit | 174 | 138 | 0.795 (0.734–0.881) | 3/3 |

Each run starts with about 8K stored bodies, so the saving grows with longer
sessions: every edit adds a body until `gc changes`.

Raw reports: [benchmarks/2026-09-26-blob-retain-linux.json](benchmarks/2026-09-26-blob-retain-linux.json).

## Tests

`internal/changes/transfer_blobs_usage_test.go`:

- Budgeting from the record is exact at capacity, a refused retention keeps
  the record, and an empty body fits at capacity. An entry the scan would
  reject goes unnoticed by Retain but not by `Stats`.
- The record is absent during every member sync and both barriers of an
  adding retention. Only the missing body and the record are written.
- Four failures after the removal (a changed source, a failed data barrier, a
  failed final barrier, a corrupt stored body) leave no record. The next
  retention rebuilds it exactly.
- A scan reclaims abandoned insertion files, including an abandoned record,
  without budgeting them. A scan that finds every body stored still writes a
  record, and the next retention writes nothing.
- A record left with insertion files (the system-crash case) budgets only
  published bodies, and `Prune` reclaims the insertion files.
- A second store value on the same directory stands in for the `gc changes`
  process. A dry run and a failed pin check keep the record. A real prune
  removes it and syncs before deleting any body. The next retention budgets
  from the smaller store; a kept record would have refused it.
- Garbage, negative and `null` records are rebuilt. A directory or symlink in
  place of the record is an error. A directory at a needed hash is an error
  and leaves the record in place.

`assertTransferBlobsComplete` now also checks that any record equals the
published bytes, which extends the existing blob tests.
`TestTransferBlobBatchPublicationAndRetry` now expects the record among the
synced members and checks that it is unnamed at both barriers.

Mutation checks were run by hand. Each of these fails at least one test:

- keeping the record during insertion;
- naming it before the final barrier;
- preparing it after the data barrier;
- recording the old total;
- budgeting reclaimed insertion files;
- not writing a record after a scan;
- trusting negative or field-less records;
- treating a non-regular needed body as missing;
- counting the record in scans;
- not removing the record in `Prune`;
- not syncing that removal before deleting.

The explicit regular-file check on the record is backed up by the open-time
identity check and the scan. Removing it alone fails no test.

## Benchmark

`BenchmarkTransferBlobRetain` (`internal/changes`) retains one new body per
operation into a store that already holds 1K, 8K or 16K bodies. This is the
cost `BenchmarkWatchPhases` misses, because its fixture has one distinct body.
The first retention, which builds the record, is excluded.

## Not covered

- Out-of-band edits to the blob directory. It is private storage that only
  `Retain` and `Prune` may change. Removing `.usage` or running `gc changes`
  rebuilds the record.
- Physical power-failure testing. The tests check ordering, not device flushes.
- Older binaries reject `.usage` as an unexpected store entry. Runners and
  clients are updated together.
