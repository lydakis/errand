# Performance baseline

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

The two shapes each contain 8 MiB, split across one file or 512 files. Timing
includes cloning or copying, content verification, syncing, and publication;
fixture construction and cleanup are excluded. Set `TMPDIR` to a writable
directory on the runner's job-storage filesystem. A RAM-backed `/tmp` hides
disk flush costs and is not representative of jobs stored on disk.
Record the filesystem type, mount options, and storage hardware with the
results. Clone, copy, and sync costs can differ between filesystems on the
same operating system; a result on Btrfs is not a Linux-wide guarantee.
Each capture uses at most 16 file workers. Check concurrent submissions and
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
