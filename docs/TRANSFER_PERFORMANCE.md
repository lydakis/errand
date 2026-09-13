# Shared transfer performance, 2026-09-12

For the completed implementation, see the [visual checkpoint](TRANSFER_CHECKPOINT.md)
and [final benchmark round](TRANSFER_FINAL_BENCHMARKS.md).

The subsequent [review fixes and final measurements](TRANSFER_REVIEW_FIXES.md)
cover cache invalidation, watcher registration, cross-process pushes, and broader
benchmark fixtures. The measurements below describe the preceding implementation.

This pass improves median watch delivery by 18–23% on the tested fixtures.
Errand remains substantially slower than Mutagen. Ordinary push results are
mixed, so these measurements do not establish a uniform speedup or absence of
performance regressions.

## Same-host comparison

Seven one-file edits per case; values are medians. Both watcher columns measure
verified destination file contents, with final Errand receipts shown separately.

| Host | Files | Previous watch delivery | New watch delivery | New watch receipt | Mutagen delivery | rsync completion | Push completion |
|---|---:|---:|---:|---:|---:|---:|---:|
| mac-mini | 1000 | 287.5 ms | 236.2 ms | 311.3 ms | 19.2 ms | 66.8 ms | 315.0 ms |
| mac-mini | 10000 | 427.7 ms | 331.3 ms | 428.2 ms | 33.0 ms | 568.4 ms | 627.6 ms |
| cabal | 1000 | 254.5 ms | 203.7 ms | 275.0 ms | 26.7 ms | 57.9 ms | 288.3 ms |
| cabal | 10000 | 791.1 ms | 608.0 ms | 792.6 ms | 116.1 ms | 163.7 ms | 1178.5 ms |

The previous-watch column uses the preceding sender-batch prototype on the same
hosts and fixture generator. Previous ordinary-push medians were 367.7/580.8 ms
on Mac mini and 292.0/1378.9 ms on Cabal for 1K/10K files respectively. The new
Mac 10K push median is 8% slower, while Cabal's is 15% faster. These sequential
runs were not randomized or interleaved; this is an exploratory comparison.

Tails remain uneven. Mac 10K watch maximum fell from 526.4 to 349.4 ms; Cabal's
rose from 871.8 to 1519.1 ms. With seven observations, nearest-rank p95 is the
maximum and cannot support a reliable population-tail claim. The Mac rsync
maximum also reached 6499.5 ms in this run. Unrelated host activity was not
controlled, and its contribution has not been isolated.

Mutagen 0.18.1 ran in one-way-safe mode, with its own temporary daemon/data
folder and no global configuration. Initial synchronization and pre-edit flushes
were outside the timer; there was no post-edit flush. Destination polling was
2 ms. Rsync used `-a --checksum --delete`, with matching ignored/VCS exclusions.
Its column is explicit command completion. It was Apple openrsync protocol 29
on Mac mini and rsync 3.5.0-g471e17dc protocol 32 on Cabal. Default size/mtime
quick-check rsync was not measured.

Both endpoints ran on each host: Darwin arm64/APFS on Mac mini and Linux
amd64/Btrfs on Cabal. Each fixture used 1 KiB background files, 80% distinct
bodies, and tiny in-place writes. Tools ran serially per host. These are not
laptop-to-runner network or browser hot-reload measurements, and they do not
prove equal conflict or crash-recovery guarantees between tools.

All four final comparison reports completed. Destination bytes matched,
20-save bursts converged with one receipt, ignored build churn emitted no
receipts, and watches stopped cleanly. Each three-second idle probe reported
0.00 CPU seconds at `ps` precision. This is not a sustained resource test.

## What changed

- Ordinary push freezes and sends delta bodies using a versioned endpoint.
  Rollbacks fail closed; only definite checkpoint rejection permits one new
  request. Lost apply replies recover the original request first.
- Push resolves a compact workspace descriptor instead of downloading its full
  creation manifest. Older servers may still return the full descriptor.
- Watch refreshes hinted files under explicit `.errandignore` policies, with
  live policy and native directory evidence. Git-driven selection, atomic saves,
  structural changes and overflow use full reconciliation. Frozen bodies retain
  normal content verification.
- Save batching is 5 ms, capped at 25 ms. Source resampling retains a 50 ms delay
  that later events cannot bypass. Actual continued edits do not exhaust the
  stable-source retry budget; permanent access/storage failures remain bounded.
- Opaque prepared source plans reuse validated metadata in push and persistent
  fetch. Fetch materializes only changed bodies. Recovery, checkpoint checks,
  body verification, full-source quotas and durable receipts remain in place.
- Workspace creation negotiates the same verified body cache used by submission.
  Creation and submission now use the long admission timeout for upload replies.

The first comparison exposed retry exhaustion during burst saves on both hosts.
Those failed logs were retained. The rerun passed after fixing resampling and
invalidating prepared observations after a failed source freeze. A new regression
fixture initially spent its deadline in setup; reducing background files fixed
that test setup without reducing its 60-write burst.

## Shared operations and remaining cost

The same 1K-file fetch fixture measured download plus local apply of a completed
job result, excluding job execution. Five measured iterations:

| Host | Previous persistent fetch | New persistent fetch | Previous fixed-result fetch | New fixed-result fetch |
|---|---:|---:|---:|---:|
| Mac mini | 455.1 ms | 222.4 ms | 118.7 ms | 141.6 ms |
| Cabal | 82.2 ms | 42.5 ms | 6.4 ms | 5.3 ms |

Persistent fetch improved by about half. The unchanged fixed-result path is a
control and moved in opposite directions across hosts. On Cabal, warm workspace
creation was 261.6 ms versus 286.7 ms in the preceding sender-batch run; warm
submission was 184.0 versus 191.5 ms. The first Mac creation/submission repeat
was substantially slower for both operations. A follow-up baseline/candidate/
candidate/baseline sequence on Mac mini did not reproduce it:

| Warm operation | Pre-pass baseline | Final candidate |
|---|---:|---:|
| Workspace creation | 612.0 ms | 550.1 ms |
| Job submission control | 617.3 ms | 523.3 ms |

Each column contains four warm observations from two three-iteration runs;
cold sample 0 and Go calibration were excluded. The submission control also
improved, so the full creation difference cannot be assigned to body-cache reuse.
This check does not establish a statistically reliable tail improvement.

The separate 10K-identical-body phase fixture reports these average milliseconds
per watch operation: Mac client/scheduling 144, staging 100, apply 162; Cabal
117, 196, 89. Those phases still include substantial complete-manifest and durable
state work. The fixture differs from the comparator table; its times cannot be
subtracted from that table to assign costs or isolate durability barriers.

A local, identical metadata microbenchmark reduced validation of 10K changed
roots from 21.71 to 10.87 ms and allocations from 9.96 to 4.68 MB per operation
(five iterations, M1 Max). This measures metadata processing, not end-to-end
file transfer or filesystem throughput.

The existing recovery-history scan and 4096-attempt bound remain unchanged.
Long-lived sessions need a separate retention/index design; this short benchmark
does not validate performance near that bound. Complete manifests and durable
records still impose work beyond changed-file delivery. Atomic editor saves and
Git-driven selection have not received the in-place-write speedup.

## Validation

The scoped changes/client/daemon/snapshot/CLI suites passed across Darwin and
Linux; focused race checks covered snapshot preparation, upload deadlines and
compact descriptors. Final Mac client/CLI tests passed after correcting the burst
fixture deadline. Static checks and benchmark compilation passed. Independent
reviews identified the permanent-error retry concern, which was fixed and tested.
These tests preserve recovery, conflict, quota and source-selection contracts;
they do not establish long-duration watch reliability or benchmark tail bounds.

## Reproduction and evidence

```sh
CGO_ENABLED=0 go build -trimpath -o dist/errand-watch ./cmd/errand
python3 scripts/benchmark_watch.py --binary dist/errand-watch \
  --files 10000 --samples 7 --idle-seconds 3 --output dist/watch-comparison \
  --mutagen /absolute/path/to/mutagen
go test ./cmd/errand -run '^$' \
  -bench '^Benchmark(WorkspaceCreationAndSubmission|FetchCompletion)$' -benchtime=5x -count=1
go test ./internal/changes -run '^$' \
  -bench '^BenchmarkBundleValidationManyRoots$' -benchtime=5x -count=1
```

The shared-operation benchmarks log individual samples; Go's one-iteration
calibration precedes the five measured iterations and must be excluded. Creation
sample 0 is cold, while samples 1–4 reuse the same daemon cache. Fetch excludes
remote job execution and includes download plus local application.

Raw comparison reports, source hashes and binary provenance are retained under
`/private/tmp/errand-optimized-v2-20260912`, with the prior comparison under
`/private/tmp/errand-competitors-20260912`. Failed burst runs are under
`/private/tmp/errand-optimized-20260912`; final validation and shared Mac metrics
are under `/private/tmp/errand-final-validation-20260912`. The controlled creation
follow-up is under `/private/tmp/errand-initial-abba-20260912`. The measured binaries
precede only the permanent-OS-error retry classification and test-fixture cleanup;
those changes do not affect the measured successful-save path.

Measured binary SHA-256:

- Mac mini: `e0918359fcc34d155674f33b8dc91a4d9cf000974d4047f2d878f380fa4289e4`
- Cabal: `2a33b4bbf81c7a7d732a281ca1d2a3595851505c29cec5c5a0af3b734e94b77f`
