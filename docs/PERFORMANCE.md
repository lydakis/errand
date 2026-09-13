# Performance baseline

The latest watch, push, fetch and creation comparison is in
[the shared-transfer performance report](TRANSFER_PERFORMANCE.md). The measurements
below are earlier baselines.

## Persistent workspace watch (2026-09-12)

The first watch implementation uses native notifications, a 50 ms save debounce,
session-local content hash reuse, and checkpoint-based source deltas. A one-file
edit transfers about 3 KiB at both 1,000 and 10,000 files. It still walks source
selection and stats the selected files on each pass, validates complete manifest
metadata, and publishes durable staging and apply records. These costs remain
visible; this version does **not** reach Mutagen's latency.

Warm, same-host measurements on the laptop's APFS volume, five samples per row:

| Files | Errand watch: file visible | Errand watch: final receipt | Errand one-shot: receipt | rsync checksum scan | Mutagen: file visible |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 317 ms | 428 ms | 1,043 ms | 130 ms | 37 ms |
| 10,000 | 548 ms | 704 ms | 7,234 ms | 1,190 ms | 42 ms |

Values are medians. Errand delivery includes the debounce; receipt time also
includes completing durable recovery records and client cleanup. File visibility
does not establish crash durability or application readiness. Mutagen is 0.18.1
in one-way-safe mode. The installed rsync is Apple's openrsync (protocol 29),
run with `--checksum` so rapid, same-size edits are detected reliably. It measures
one-shot command completion, not an event watcher. These tools have different
conflict and durability contracts; the table compares observed user-facing latency,
not equivalent transaction guarantees. No build or test ran alongside this laptop
comparison, but unrelated machine activity was not controlled.

Native runner checks with 1,000 files also passed: Cabal (Linux/Btrfs) had median
file visibility of 265 ms and final receipt of 337 ms; Mac mini (Darwin/APFS) had
298 ms and 377 ms respectively. Cabal's sample predates skipping blob negotiation
for small deltas. These runs use a client and disposable daemon on the **same**
runner; they do not measure laptop-to-runner network latency. The initial target
of sub-250 ms delivery remains unmet in the median on these fixtures.

All measured Errand watch sessions used 0.00 CPU seconds at `ps` reporting
precision over three idle seconds, emitted no receipts for ignored build churn,
and coalesced a 20-save burst into one push. This is a short idle probe, not a
long-duration power or resource test.

Reproduce with a CGO-free build and an output directory on the filesystem of interest:

```sh
CGO_ENABLED=0 go build -trimpath -o dist/errand-watch ./cmd/errand
python3 scripts/benchmark_watch.py --binary dist/errand-watch \
  --files 10000 --samples 5 --output dist/watch-benchmark
# Optionally add --mutagen /absolute/path/to/mutagen to compare an isolated session.
```

The harness generates 1 KiB files in subdirectories (80% unique contents), runs
an isolated daemon, verifies destination contents, records both visible delivery
and final receipts, and checks idle behavior, burst coalescing, and Ctrl-C.
Mutagen uses its own temporary data directory and is stopped afterward. Reports
include binary hashes and filesystem facts. Raw local evidence for this pass is
in ignored `dist/watch-final-1000`, `dist/watch-final-10000`,
`dist/watch-linux-final-results`, and `dist/watch-mini-final-results`.

For phase attribution, `go test ./cmd/errand -run '^$' -bench
'Benchmark(Push|Watch)Phases' -benchtime=5x -count=1` reports client, negotiation,
staging and apply time against a real disposable daemon. Its separate 10,000-file
fixture has identical bodies, so do not combine its timings with the table above.
Further latency work should reduce whole-tree work and durable-record overhead
while retaining conflict detection and exact retry semantics. Report file delivery
separately from receipt completion when evaluating those changes.

## Job submission baseline

Measure the cost of sending a no-op job before choosing optimizations. The
end-to-end harness uses generated data and explicitly selected configured
peers. It never snapshots the developer's working tree.

## Reproduce

Build the client being measured, then choose idle peers:

```sh
mkdir -p dist
go build -trimpath -o dist/errand-benchmark ./cmd/errand
python3 scripts/benchmark.py --binary ./dist/errand-benchmark \
  --on cabal --on mac-mini --output dist/benchmarks/my-baseline
```

The output directory must be new. Record the client revision alongside the
report; the harness records its version string and binary SHA-256, caller
platform, runner versions and facts, and raw per-job evidence. Generated
reports stay under ignored `dist/`: they can contain private peer addresses,
tool paths, and job handles. Review them before sharing.

Defaults are five samples per scenario and 128 files of 64 KiB each (8 MiB).
Use `--samples`, `--files`, and `--file-bytes` to vary the workload. The runner
must provide `/usr/bin/true`; peers are selected only through repeated `--on`.

Run this separately from builds, tests, and other benchmarks. The harness
refuses a runner with active or queued jobs at the initial probe, but does not
reserve it or exclude unrelated processes. Keep caller power mode, network
route, runner load, and software versions comparable between runs.

## What is timed

- **Local reference:** launch and wait for `/usr/bin/true` locally.
- **No snapshot:** run the same command in a fresh empty remote workspace.
- **Cold snapshot:** generate unique content before each timed invocation;
  require the CLI to report that all file content was shipped.
- **Cached snapshot:** seed a separate fixed tree with one warmup job, excluded
  from summaries; require subsequent invocations to ship zero file-content bytes.

Each remote timer surrounds the attached CLI process through exit. It includes
client startup, snapshot preparation when enabled, transport, admission,
execution, settlement, and log observation. Fixture generation, runner probes,
and the subsequent status query are outside that timer. Scenario order is
shuffled within each trial with a recorded seed; peers run sequentially.

A sample is valid only after a terminal receipt confirms successful execution,
change capture, cleanup, and complete logs. Cache negotiation failure or
fallback invalidates a snapshot sample. A failed sample stops the run and
leaves an incomplete report; warmups and invalid samples are excluded from
summary statistics. Inspect `complete` before comparing reports.

`shipped_bytes` is the CLI's logical file-content accounting, not measured
wire traffic. Cached jobs still send metadata and perform snapshot validation.
`process_ms` comes from the runner receipt and excludes transaction overhead.
Do not interpret wall time minus process duration as network latency, or
subtract timestamps from different machines to infer phases.

The default run creates 16 jobs per peer and up to 48 MiB of unique cached
file content per peer, plus metadata and retained job state. It does not clear
shared caches, collect jobs, or change runner configuration. Local fixture and
client state directories are temporary. A client timeout stops measurement;
inspect any saved handle before retrying because the remote job may continue.

## Local snapshot costs

```sh
go test ./internal/snapshot -run '^$' \
  -bench '^BenchmarkSnapshotPreparation$' -benchmem -benchtime=100ms -count=3
```

The Go benchmarks measure selection, manifest hashing, full packing, and
packing with all file content already cached. Two shapes have the same 8 MiB
payload: 128 files of 64 KiB and 4,096 files of 2 KiB. Fixtures are prepared
outside timing on a warm local filesystem, without Git metadata or ignore
rules. Archive output goes to `io.Discard`; these are preparation costs, not
network or runner measurements. Cached packing still reads and hashes each
file to enforce snapshot consistency.

For Git selection with an ignored dependency tree:

```sh
go test ./internal/snapshot -run '^$' -bench '^BenchmarkGitSelection$' -benchtime=5x -count=3
```

This fixture selects 128 source files and excludes 128 dependency directories
containing 2,176 files, including nested ignore files. It measures repository
metadata, file selection, and the two observations of the frozen ignore policy.
Fixture construction and validation of the selected paths are outside the
timer. Global and system Git configuration files and default ignore files are
isolated.

## Runner baseline capture

Before a command starts, the runner preserves an independent, durable copy of
the submitted files for later change merging. To measure this step separately:

```sh
go test ./internal/changes -run '^$' -bench '^BenchmarkCaptureWorkspaceBase$' -benchtime=1x -count=5
```

The shapes each contain 8 MiB, split across one file, 512 files, or 512 files
each in its own directory. Timing
includes cloning or copying, content verification, syncing, and publication;
fixture construction and cleanup are excluded. Set `TMPDIR` to a writable
directory on the runner's job-storage filesystem. A RAM-backed `/tmp` hides
disk flush costs and is not representative of jobs stored on disk.
Record the filesystem type, mount options, and storage hardware with the
results. Clone, copy, and sync costs can differ between filesystems on the
same operating system; a result on Btrfs is not a Linux-wide guarantee.
Each capture uses at most 16 workers, for files and then each directory depth. Check concurrent submissions and
filesystems without cloning before generalizing the measured gains or tuning
that limit.

Measure CLI invocation to first command output as well as CLI exit when
evaluating startup improvements. Successful completion includes result capture
and cleanup after the command has already produced output. Report first-run
and cached runs separately; cached content still requires validation and a
fresh baseline before execution.

## First measurement

Results and interpretation below use the September 4, 2026 local run.

Caller: Apple M1 Max, Darwin arm64 27.0.0, AC power, Go 1.27.0. Client:
`41b2eec` (`0.1.0-dev+41b2eec`). Both runners reported `0.1.0-dev+800b7a7`:
Cabal was Linux amd64 with 4 CPUs; Mac mini was Darwin arm64 with 10 CPUs.
The configured tailnet connections were used; direct versus relayed routing
was not recorded. No runner upgrades were performed for this measurement.

Five samples per scenario, 8 MiB / 128 files, milliseconds:

| Runner | Scenario | Median | Min–max | File-content bytes shipped |
| --- | --- | ---: | ---: | ---: |
| Local | No-op reference | 2.4 | 2.3–2.7 | N/A |
| Cabal | No snapshot | 114 | 104–132 | 0 |
| Cabal | Cached | 880 | 831–1,370 | 0 |
| Cabal | Cold | 1,795 | 1,726–1,854 | 8,388,608 |
| Mac mini | No snapshot | 1,125 | 820–1,185 | 0 |
| Mac mini | Cached | 1,789 | 1,472–1,948 | 0 |
| Mac mini | Cold | 2,213 | 1,818–2,284 | 8,388,608 |

All 32 remote jobs, including two warmups, had successful receipts. Measured
process durations were below the receipt's millisecond resolution on Cabal
and 2–5 ms on Mac mini. These are observed ranges from a small sample, not
latency percentiles or performance guarantees.

Raw local evidence is in
`dist/benchmarks/baseline-isolated-41b2eec/report.json`, started at
`2026-09-05T00:39:32Z` (September 4 in New York). Client binary SHA-256:
`15c3ad90bfacb4b6fbf0eb5a6b62fb01a2a2b76c45545114ca0ceafb44741637`.
This run followed a small harness smoke test and an exploratory full run;
all cold samples still used unique content. Local tests and microbenchmarks
had finished before this run began.

Local preparation costs, median of three benchmark repetitions in milliseconds
per operation, measured separately after the remote run:

| Shape | Select | Hash manifest | Pack full | Pack cached |
| --- | ---: | ---: | ---: | ---: |
| 128 × 64 KiB | 11.43 | 7.55 | 10.48 | 10.28 |
| 4,096 × 2 KiB | 20.18 | 106.79 | 198.04 | 191.27 |

Raw local output: `dist/benchmarks/snapshot-isolated-41b2eec.txt`. Each
repetition used `-benchtime=100ms`; the slower cases had very few iterations.
Use longer runs for optimization comparisons. The packing results include
content revalidation and explain why cached preparation still depends on
file count. They do not account for the whole end-to-end latency.

### What to investigate next

Snapshot reuse eliminates file-content transfer, but substantial transaction
cost remains. The large difference between the runners even without snapshots
calls for timing admission-to-start, settlement, and transport/attachment
separately. Record tailnet routing and repeat with matching client/runner
versions before attributing the difference to an operating system or choosing
an optimization.

The local microbenchmarks also expose file-count sensitivity. Profile many-file
workloads before changing snapshot preparation, and preserve validation of
cached content when evaluating shortcuts. Measure representative builds and
tests next to establish the actual break-even point for useful work.

### Persistent-workspace transfer preparation

To measure local source freezing and receiver staging separately:

```sh
go test ./internal/changes -run '^$' -bench '^BenchmarkTransferPreparation$' -benchtime=10x -count=3
```

The fixture has an unchanged 8 MiB file and a small edited file. The benchmark
reports preparation time and retained bytes per attempt, without an artificial
throughput figure for unchanged files that staging skips. Setup and cleanup are
outside the timed section. Staging materializes only the changed base paths.
It does not measure network time: a new push still describes its complete
selected source snapshot, but negotiated cache hits omit file bodies from the
upload. Applying an acknowledged unchanged stage does not upload it again.

Durable source blobs remain private to each transfer relationship. Push also
uses the existing evictable upload cache; it does not change durable transfer
retention or introduce block-level deltas.

### Push cache maintenance measurement

September 12, 2026, Apple M1 Max, Darwin arm64, AC power, Go 1.27.1.
A temporary native daemon and a Unix-socket capture proxy used synthetic data
and isolated client/configuration state. The fixture contained 1,000 1-KiB files
across ten directories, every fifth body identical, an empty `.errandignore`,
and a 7-byte edited file. The targeted push applied only the edited path; the
second push was a no-op. These are single local samples, not Blue or network latency
measurements. "Before" is the initial uncommitted push-cache implementation;
"after" includes the review fixes, not a released client upgrade.

| Targeted push measurement | Before review fixes | After review fixes |
| --- | ---: | ---: |
| Changed file-body bytes | 7 | 7 |
| Multipart request bytes (CLI receipt) | 159,369 | 159,369 |
| Negotiation request bytes | 90,185 | 72,275 |
| All HTTP request-body bytes | 249,573 | 231,663 |
| All HTTP response-body bytes | 152,168 | 152,168 |
| End-to-end seconds | 15.07 | 16.04 |
| No-op seconds | 5.87 | 5.92 |
| Cold workspace creation seconds | 10.15 | 10.17 |
| Warm workspace creation seconds | 10.19 | 10.32 |

The captured multipart bodies matched the CLI receipts, including retry bodies
in separate corruption tests. Deduplication reduced negotiation bytes by about
20%; metadata still dominated this small-file upload. There is no demonstrated
wall-time improvement. Total body counts include workspace lookup, negotiation,
staging, and apply, but exclude HTTP headers and transport framing.

A 10,000-file version exposed the existing 15-second response-header timeout
on push staging. After switching staging to the bulk-operation HTTP client,
the previously interrupted push recovered its frozen source and applied in
107.26 seconds. A subsequent no-op completed in 23.59 seconds, with a
1,567,655-byte multipart upload and 720,275-byte negotiation request, despite
sending no file bodies. The applied file was checked on disk. This reused
fixture validates timeout recovery; it is not a fresh-run latency comparison.

Isolate the added cache population work from source freezing, extraction, and
durable staging with:

```sh
go test ./internal/daemon -run '^$' -bench '^BenchmarkSnapshotCacheIngestion$' -benchtime=1x -count=3
```

For the same 1,000-file content shape, three local samples measured 189–198 ms
with an empty cache and 207–242 ms with a warm cache. Setup and initial warming
are excluded. One ingestion pass inserts 801 unique bodies instead of 1,000;
warm ingestion still copies and verifies those bodies. This measures cache
population alone, not filesystem-wide physical write amplification. The small
measured cost does not justify adding a second verified-cache lookup path in
this maintenance patch. Source freezing and durable staging retain their
existing integrity checks and can dominate total time.

Local capture logs, the synthetic CLI probe, and binary SHA-256 values are in
the ignored `dist/benchmarks/push-cache-review/` directory. The probe uses a
private Unix socket and temporary daemon state; its HTTP-body counts are not
transport-level wire measurements.

### Native filesystem staging optimization

The September 12 follow-up changed two parts of staging. Cached materialization
now sets permissions through the open descriptor, matching streamed extraction.
The retained measurements combine that change with sync batching, so they do
not establish an isolated speedup from descriptor-based chmod. In addition,
transfer staging was draining the drive cache separately for every file on
Darwin. It now shares baseline capture's member `fsync` implementation, followed
by a full `File.Sync` at each publication boundary. Linux keeps its normal
`File.Sync` semantics.

Baseline capture, staged-tree synchronization, and retained-blob verification
and insertion share a worker implementation capped at 16 tasks per operation.
Siblings can synchronize concurrently; every depth finishes before its parents
receive final permissions and synchronization. This matters on disk-backed Btrfs as well as APFS. Blob
retention prepares verified temporary bodies, completes a full data barrier,
then assigns their permanent hash names and completes a second directory
barrier. A crash before the first barrier cannot leave a permanent name pointing
at unflushed data. Retention reclaims abandoned temporary bodies before quota
accounting, so retrying an interrupted batch does not budget its bytes twice.
Published blobs remain intact; stats and dry-run pruning remain read-only.
A retry finding every body already stored verifies them and
completes only the directory barrier. Baseline reconstruction also uses member
syncs before its full tree flush and publication. These helpers cover both push
and fetch. Content hashing, quotas, source consistency checks, and checkpoint
publication remain required.

Tests cover restricted files, implicit parents, restricted roots with punctuation
in child names, bounded parallelism, completion of all permission restoration
after a member error, failed publication, and recovery using retained blobs.
The first worker failure remains the reported cause when its siblings cancel.
Successful tests establish ordering and error propagation; they do not simulate
a physical power failure or certify a storage device's flush implementation.

The following historical comparisons use installed **0.4.3** versus the initial
working patch based on `ca39562`, before the two-phase blob publication review
fix. Each native host ran its own isolated client and daemon
through a Unix-socket capture proxy. Source, client state, and daemon state lived
on the runner's job-storage filesystem, with short socket paths kept separately.
The mini used APFS on SSD, macOS 26.6.2/arm64. Cabal used Btrfs with
`compress=zstd:3`, Linux 7.1.9/amd64. A previous `/tmp` test on Cabal measured
RAM-backed storage and concealed its per-file synchronization cost; it must not
be used as a disk baseline.

The fixture contains 1,000 1-KiB files in ten directories, every fifth body
identical, an empty `.errandignore`, and one 7-byte edited file. Workspace creation
and the first push are single observations. Later edits report the median of two
pushes; no-ops report the median of three. Benchmarks ran separately from tests
on each host. Baseline, intermediate variants, and final candidate ran in that
order with fresh isolated state, without dropping filesystem caches or reserving
the host. These small samples show mechanism and magnitude, not percentiles.

| Host / filesystem | Operation | 0.4.3 seconds | Candidate seconds |
| --- | --- | ---: | ---: |
| Mac mini / APFS | Create workspace | 7.50 | 0.62 |
| Mac mini / APFS | First edited push | 10.91 | 0.69 |
| Mac mini / APFS | Later edited push | 4.71 | 0.61 |
| Mac mini / APFS | No-op push | 4.42 | 0.49 |
| Cabal / Btrfs | Create workspace | 9.52 | 1.54 |
| Cabal / Btrfs | First edited push | 13.71 | 1.64 |
| Cabal / Btrfs | Later edited push | 7.15 | 1.55 |
| Cabal / Btrfs | No-op push | 7.01 | 1.27 |

The final Cabal later-edit samples ranged from 1.34 to 1.76 seconds; its no-ops
ranged from 1.18 to 1.61 seconds. The mini's later edits were 0.59–0.64 seconds
and no-ops 0.49–0.50 seconds. The probe verified applied file contents and
changed-path counts after every push. Multipart receipts remained 159,369 bytes
for edits and 158,345 bytes for no-ops, carrying 7 and 0 file-body bytes
respectively. These are complete CLI timings on one host, **not** measurements
of laptop-to-runner transport or Blue's browser readiness. Installed clients and
services were not upgraded.

The regression benchmark includes cache materialization and its subsequent
durability pass, since materialization alone missed the expensive interaction:

```sh
go test ./internal/daemon -run '^$' \
  -bench '^BenchmarkCachedSourceStaging$' -benchtime=1x -count=3
```

Use `TMPDIR` on the filesystem being evaluated. The fixture is 1,000 cached
1-KiB regular files with final mode 0644. Fixture setup and deletion are excluded;
content verification, permission finalization, member synchronization, and the
full publication flush are included.
On the M1 Max laptop, two baseline samples took 4.87–4.88 seconds. Three samples
with the optimization took 0.18–0.25 seconds (median 0.18 seconds).

Local raw evidence and harness copies are retained under the ignored
`dist/benchmarks/native-staging/`, including binary hashes, filesystem facts,
HTTP-body timings, and the source patch. The final Darwin candidate SHA-256 was
`96c8ee5e1ed826284cb503d6764cb48aa5b6a16635718666d720383439d1650a`;
the Linux candidate was
`98d4475ae97d9bdacfc119489c5330f27d97f687cc132e274f857cbbed6fb065`.

Full source freezing, metadata exchange, and validation still scale with the
selected tree. A 10,000-file laptop run with batched synchronization and parallel
blob retention still took 7.78 seconds for a later edit and 7.64–7.66 seconds for
no-ops. These measurements preceded the final error-cause-only correction.
Reducing this further requires measuring source copying, inventories, cleanup, and metadata
exchange separately; skipping them based only on local changes would weaken
remote consistency and recovery guarantees.

### Reproducible persistent-push harness

The checked-in harness runs the selected binary as both client and an isolated
native daemon. It generates the same 1,000-file content shape described above,
creates a persistent workspace, then alternates targeted edits and no-op pushes:

```sh
go build -trimpath -o dist/errand-benchmark ./cmd/errand
python3 scripts/benchmark_push.py --binary ./dist/errand-benchmark \
  --output dist/benchmarks/push-standard
python3 scripts/benchmark_push.py --binary ./dist/errand-benchmark \
  --directories 1000 --output dist/benchmarks/push-directories
python3 scripts/benchmark_push.py --binary ./dist/errand-benchmark \
  --workspaces 4 --output dist/benchmarks/push-concurrent
```

Run on each native host. Put the new output directory on its actual job-storage
filesystem; source, client state, and daemon data all live there during the run.
Only the short Unix-socket path uses `/tmp`. Installed services, user config,
shared caches, and real development workspaces are untouched. Temporary data is
removed after the run, while reports and daemon logs remain under the output
directory. These local reports include filesystem/device facts and should be
reviewed before sharing.

The report records binary hash and version, filesystem facts, fixture parameters,
per-command wall time and transfer receipts. Every push must report the expected
changed-path count and match the applied file on disk. Check `complete` before
comparing runs. Creation is sequential; with multiple workspaces, each edit/no-op
round submits pushes concurrently to the same daemon and waits for all of them.
Round duration includes dispatch and preparing the tiny edits; per-command time
begins after the edit is written. The first edit seeds durable blobs and should
be reported separately from later edits.

Failed commands retain their arguments, workspace/sample identity, elapsed time,
exit code or timeout status, and the last 8,192 characters of stdout and stderr
with truncation flags. Successful siblings from a failed round are saved too;
the report stays incomplete. The inherited environment is never recorded.

Unlike the historical probe, this harness connects directly to the daemon
without a capture proxy. It measures CLI latency and receipt bytes, not individual
HTTP phases or wire traffic. Repeat both baseline and candidate with this same
harness, outside other tests and benchmarks. Alternate their run order to reduce
warming and host-load bias; keep first-edit results separate from warm results.
The fixture has sibling directories, so these results do not establish latency
for deeply nested trees. Increasing `--directories` exposes
directory work; increasing `--workspaces` measures contention across operations,
each of which can independently use up to 16 staging workers. Neither setting
changes the production concurrency limit.

### Review-fix validation

September 12, 2026: the harness above reran installed 0.4.3 and the working
patch on the same native hosts and filesystems. The measured patch includes
two-phase blob publication, the shared full durability barrier, and bounded
directory finalization, before the subsequent abandoned-temporary cleanup and
benchmark-diagnostics fixes. “Final patch” below identifies that measured
revision, not later working-tree changes. Each run used fresh isolated state. Baseline ran first,
then the publication fix, then the directory optimization; caches were not
dropped and the hosts were not reserved. Tests finished before each benchmark
run. These small samples demonstrate the improvement but are not percentiles.

Standard 1,000-file, ten-directory fixture, seconds:

| Host / filesystem | Operation | 0.4.3 | Final patch |
| --- | --- | ---: | ---: |
| Mac mini / APFS | Create workspace | 7.66 | 0.63 |
| Mac mini / APFS | First edited push | 11.42 | 0.70 |
| Mac mini / APFS | Later edited push | 4.65 | 0.65 |
| Mac mini / APFS | No-op push | 4.58 | 0.55 |
| Cabal / Btrfs | Create workspace | 8.96 | 1.54 |
| Cabal / Btrfs | First edited push | 14.76 | 2.44 |
| Cabal / Btrfs | Later edited push | 7.42 | 1.47 |
| Cabal / Btrfs | No-op push | 7.62 | 1.52 |

Creation and first edits are single observations; later edits are medians of two
samples, no-ops of three. Final Cabal later edits ranged from 1.25–1.68 seconds
and no-ops from 1.20–2.00 seconds. These complete local CLI timings exclude
laptop-to-runner transport and application readiness.

The new directory-heavy capture benchmark justified reusing the bounded worker
loop for directory finalization. On Cabal, the median of three captures with
512 directories fell from 1.99 to 0.57 seconds; on APFS it was 0.123 versus 0.113
seconds. In the end-to-end 1,000-directory fixture, Cabal workspace creation
fell from 5.51 to 2.42 seconds. Its later pushes remained variable, with a final
median of 2.83 seconds for edits and 2.26 seconds for no-ops. Directory batching
improves capture; it does not eliminate source inventory and staging work.

Four workspaces also pushed concurrently through one isolated daemon. All 24
pushes on each host passed the receipt and applied-content checks. Final
later-edit medians were 1.59 seconds per push on APFS and 2.81 seconds on Btrfs;
no-op medians were 1.30 and 2.43 seconds. This establishes correct behavior for
the tested concurrency, not unlimited capacity or a worst-case descriptor
bound. No global semaphore or change to the per-operation limit was warranted
by this run.

Both platforms passed the full Go race suite, `go vet ./...`, and 28 Python
tests after the publication fix. After directory batching, the affected changes
and daemon race suites and full vet passed again. Regression coverage includes
no permanent blob names before the data barrier, failures at both barriers,
cancellation before renaming, cached retry without source, ordinary restricted
root restoration, and joining sibling directory work before restricting parents.

Raw reports and logs are under ignored `dist/benchmarks/review-fix-{mini,cabal}/`
and `dist/benchmarks/review-final-{mini,cabal}/`. Final binaries:

- Darwin arm64 SHA-256: `69ed4b4dcd9efe527c535f40e4cfb1b38c81467417cbfeea13203b5ac4034593`
- Linux amd64 SHA-256: `9c741babaea40b0bc6f079a73def9f58d5545bcc88a0b1d436521fb15f8aa88b`

The subsequent recovery/diagnostics pass also passed changes and daemon race
tests, full `go vet ./...`, and all 32 Python tests on both hosts. Its isolated
smoke fixture used 64 files, 16 sibling directories, and two workspaces, with
eight verified pushes per host. Reports are under ignored
`dist/benchmarks/review-recovery-{mini,cabal}/`; these smoke runs validate
behavior, not a new performance comparison. The abandoned-batch regression
failed before cleanup was added and passed afterward. The restrictive-umask
test also failed when chmod was removed through a temporary Go overlay.
