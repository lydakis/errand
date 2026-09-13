# Final transfer benchmark round, 2026-09-12

The completed code was built and measured on both runners. All benchmark steps
passed. This is a performance checkpoint, not a claim of Mutagen parity or a
controlled proof that every operation improved.

See the [visual implementation explanation](TRANSFER_CHECKPOINT.md).

## File delivery and completion

Seven one-file edits per workload; values are medians in milliseconds. Watch
delivery checks destination contents; receipt includes durable completion.
Mutagen is destination delivery; rsync and ordinary push are command completion.

| Host | Selection / files / save | Watch delivery | Watch receipt | Push | Mutagen | rsync checksum |
|---|---|---:|---:|---:|---:|---:|
| mac-mini | explicit-1000 | 243 | 334 | 320 | 46 | 59 |
| mac-mini | explicit-10000 | 310 | 402 | 582 | 40 | 572 |
| mac-mini | git-atomic-10000 | 668 | 758 | 748 | 40 | 561 |
| cabal | explicit-1000 | 187 | 255 | 282 | 26 | 57 |
| cabal | explicit-10000 | 577 | 743 | 1110 | 114 | 165 |
| cabal | git-atomic-10000 | 913 | 1072 | 1224 | 112 | 166 |

Each host ran tools serially. Both endpoints were on that host: Mac mini uses
Darwin arm64/APFS and Cabal uses Linux amd64/Btrfs. Go processes used
`GOMAXPROCS=2` and the Errand build used `CGO_ENABLED=0`. Fixtures contain 1 KiB
background files with 80% distinct bodies. The two selection/save modes vary
together; their difference does not isolate the cost of either factor alone.

Mutagen 0.18.1 ran in one-way-safe mode, with isolated temporary state and no
global configuration. Initial and pre-edit flushes were outside the timer;
there was no post-edit flush. Destination polling was 2 ms. Rsync used
`-a --checksum --delete`, with matching ignored/VCS exclusions. Default size/mtime
quick-check rsync was not measured. Versions and binary hashes are in the raw data.

These are same-host measurements, not laptop-to-runner networking or browser
hot-reload timings. The tools do not promise identical conflict or crash-recovery
semantics. Seven observations do not support reliable population-tail estimates.
Ordinary push had large outliers, including 6.78 seconds in the Cabal Git/atomic
case. No slower samples were dropped.

Compared with the earlier exploratory 10K explicit-selection baseline in
[the preceding report](TRANSFER_PERFORMANCE.md), final watch delivery is lower
from 428 to 310 ms on Mac mini and 791 to 577 ms on Cabal. These sequential
observations suggest improvement but are not an interleaved causal comparison.
Mutagen remains substantially faster, particularly for Git/atomic saves.

## Other operations

The 1K-file shared-operation fixture reports means. Creation and submission
exclude cold sample 0 and average warm samples 1–4. Fetch averages all five
measured samples, excludes remote execution, and includes download plus local
apply. Go calibration is excluded everywhere.

| Host | Warm workspace creation | Warm ephemeral job completion | Fixed-result fetch | Persistent fetch |
|---|---:|---:|---:|---:|
| mac-mini | 534.7 ms | 1010.0 ms | 110.8 ms | 221.5 ms |
| cabal | 264.8 ms | 191.0 ms | 5.6 ms | 40.3 ms |

The ephemeral-job measurement includes a trivial command, polling and result
completion, not just admission. Mac mini had two warm samples near 1.5 seconds
and two near 0.5 seconds; the high mean is retained. This run does not establish
whether that variation is a regression or identify its cause.

## Where the watch time goes

A separate 10K-identical-body fixture measured five operations per case. These
phase means use a different fixture from the comparison table and cannot be
subtracted from its timings. Client includes preparation, scheduling and transport.

| Host / operation | Client ms | Stage ms | Apply ms |
|---|---:|---:|---:|
| mac-mini / push | 272.2 | 90.5 | 172.7 |
| mac-mini / watch | 124.3 | 91.2 | 171.0 |
| cabal / push | 448.0 | 183.9 | 95.8 |
| cabal / watch | 108.6 | 162.2 | 85.6 |

Small deltas made no cache-negotiation request in this fixture. Complete metadata
and durable state work still dominate much of the path.

## Expiry, convergence and verification

Two measured saves after a 31-second idle pause each exercised the preparation
expiry path on a 1K-file explicit-selection fixture:

| Host | Median delivery | Median receipt |
|---|---:|---:|
| mac-mini | 272 ms | 347 ms |
| cabal | 204 ms | 276 ms |

All eight reports verified final selected file contents, recorded one receipt
for a 20-save burst including shutdown drain, emitted no unexpected idle receipts,
and exited cleanly. Each three-second idle probe recorded zero process CPU at
the counter resolution reported in its raw data. This is not a sustained idle
resource measurement. It does not validate long histories or the 4096-attempt bound.

The preceding focused regression run passed for changes, snapshots, clients and
CLI watch flows. The implementation had also passed scoped macOS/Linux suites
and focused race/static checks. This final round ran benchmarks without rerunning
those tests because no production code changed afterward.

## Reproduction and evidence

The [raw reports and samples](benchmarks/2026-09-12-final.json) retain binary
provenance, source-manifest hash, every comparison sample, convergence checks,
shared-operation samples and phase logs. Source capture and orchestration are in
`/private/tmp/errand-bank-final-20260912`; fetched complete logs are under
`/private/tmp/errand-bank-final-mini/results` and
`/private/tmp/errand-bank-final-cabal/results`.

Runner jobs: `mac-mini/01M2CDBN255SBQS4WCKWMJPH5E` and
`cabal/01M2CDBN15ZN786EYXWAFZJ1SA`. Production files remained byte-identical to
the measured source through commit; only documentation and benchmark evidence
were added or updated.

```sh
GOMAXPROCS=2 CGO_ENABLED=0 go build -trimpath -o /tmp/errand-final ./cmd/errand
GOMAXPROCS=2 python3 scripts/benchmark_watch.py --binary /tmp/errand-final \
  --mutagen /absolute/path/to/mutagen --files 10000 --samples 7 \
  --idle-seconds 3 --output /absolute/path/to/new-result-directory
# Repeat with --selection git --save-mode atomic.
# Use --files 1000 --samples 2 --pause-seconds 31 for expiry.
GOMAXPROCS=2 go test ./cmd/errand -run '^$' \
  -bench '^Benchmark(WorkspaceCreationAndSubmission|FetchCompletion|PushPhases|WatchPhases)$' \
  -benchtime=5x -count=1
```
