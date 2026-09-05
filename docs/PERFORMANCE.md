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
