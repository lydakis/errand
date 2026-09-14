# Transfer latency diagnosis and direct merge inputs

The baseline is `f64148c`, the committed direct receiver-staging slice. This
follow-up separates benchmark setup from operation latency and tests one bounded
change in the shared apply path. The selected snapshot representation is unchanged.

## Diagnostic evidence

The first instrumented Mac mini/APFS probe completed both operations. It used
one operation per process, with Go CPU and syscall tracing enabled; these are
diagnostic timings, not an uninstrumented performance comparison.

| Phase | Push fixture (10,000 files) | Large fetch fixture (8 MiB changed file) |
|---|---:|---:|
| Fixture creation | 2.02 s | 0.08 s |
| Workspace creation | 23.55 s | 1.79 s |
| Warm push | 0.47 s | — |
| Job execution and capture | — | 1.85 s |
| Measured operation | 0.50 s | 1.63 s |
| Temporary-directory cleanup | 0.82 s | 0.19 s |
| Entire process | 27.73 s | 6.34 s |

The push trace attributes about 18 seconds of aggregate syscall delay to
`CaptureWorkspaceBaseContext` verifying captured files through
`snapshot.PackContextWithPhysicalModes`. Reads dominate that path. This localizes
one observed setup stall to base verification; it does not prove that every
previous APFS outlier has the same cause or that cloning itself is responsible.

The large-fetch trace attributes about 0.67 seconds of syscall delay to
`materializeVerifiedMergeInputs`. Its existing implementation writes temporary
tar files, seeks back and extracts them to prepare apply's private merge inputs.
Syscall-profile durations are aggregate traced delays, not additive shares of
wall time. The source checks and final publication barriers remain necessary.

## Changes

- Benchmark scratch directories now own generated fixtures and test executables.
  Cleanup runs on failure and interruption, restores traversal permissions on
  private directories, and preserves diagnostic logs and failure metadata.
- Push/fetch benchmarks emit named phase boundaries and optional Go trace regions,
  including fixture setup, creation, warm-up, job capture and cleanup.
- `scripts/profile_transfer_latency.py` runs short bounded diagnostics and retains
  text CPU/syscall profiles. Raw traces and executables are scratch artifacts.
- Apply's verified base/remote merge inputs use the same verified copier and
  source-access checks as transfer staging, without intermediate tar files.

Merge-input copies remain private, independently owned scratch. Their previous
archive/extract path did not synchronize these inputs. The new adapter preserves
that policy; the existing journal synchronizes chosen outputs before installation.
Durable transfer staging still supplies member synchronization and final tree
barriers to the shared copier. Source-stat checks remain in place. Export and
strict live-source capture retain their existing paths in this experiment.

Coverage checks restricted source permissions, rejection of corrupt content,
independent merge-input bodies and retained logical modes. The existing tampered
staging test still requires failure before any destination write; its obsolete
archive-specific error substring was replaced with the caller's verification
context.

## Paired comparison

The native comparison uses identical phase-instrumented benchmark sources in
baseline and candidate, five alternating pairs per workload and `GOMAXPROCS=2`.
Large fetch uses three measured operations per process (plus Go's calibration
invocation); push uses one. All setup remains outside the operation timer and is
reported separately. Candidate full tests, vet and race checks precede timing.
Source/binary hashes, raw logs, load, filesystem information and failures are
retained. No profiler runs during these comparisons.

Both campaigns completed every pair, with unchanged source hashes and no
timeouts. Medians below describe these five-pair screens, not a statistical
guarantee or a cross-host network result.

| Native host | Operation | Baseline | Direct merge inputs | Median reduction | Faster pairs |
|---|---|---:|---:|---:|---:|
| Mac mini, APFS | 8 MiB fetch/apply | 1,038 ms | 925 ms | 10.9% | 4/5 |
| Mac mini, APFS | Push/apply | 500 ms | 480 ms | 4.1% | 4/5 |
| Cabal, Btrfs | 8 MiB fetch/apply | 872 ms | 825 ms | 5.4% | 4/5 |
| Cabal, Btrfs | Push/apply | 784 ms | 786 ms | -0.2% | 2/5 |

Retain the direct-copy change: it removes measured redundant I/O, improves the
large-fetch median on both filesystems, and shows no consistent push regression.
The Btrfs push result is effectively unchanged. The same merge-input helper is
used by push/watch and both fetch apply modes; this screen times ordinary push
and persistent large fetch, so it does not establish gains for every caller.

Variability remains material. APFS large-fetch samples span 805–2,778 ms for the
baseline and 789–1,949 ms for the candidate. One APFS baseline push took 12.65 s:
the existing phase metrics put 12.44 s in the client residual, with apply at
161 ms and staging at 57 ms. That residual is total time less measured server
phases, not a precise attribution to a particular client function. This screen
does not identify that outlier's underlying cause. All samples remain included.

Setup is still substantial: the push fixture's workspace-creation medians were
24.30/19.57 s on APFS and 11.93/10.01 s on Btrfs (baseline/candidate). This patch
does not change workspace creation, so those differences are not evidence of
a creation speedup. The initial trace and the paired phase logs identify that
path as the next place to investigate, including captured-base verification.

Full `go test ./...`, `go vet ./...`, and race tests for changes, client, daemon
and CLI passed on both native hosts. Ten Python harness regression tests passed
locally after the review corrections below. Cleanup covers ordinary errors,
timeouts, Ctrl-C and SIGTERM; it cannot run after SIGKILL or an operating-system
crash, and a filesystem cleanup failure is reported rather than silently ignored.

The [raw reports and logs](benchmarks/2026-09-14-latency-diagnosis.json) include
phase events, native filesystem information, validation output, all samples and
source/binary hashes. The [frozen overlays and driver](benchmarks/2026-09-14-latency-diagnosis-inputs.tar.gz)
reproduce the baseline and candidate from `f64148c`; their README describes the
first diagnostic's helper-file rename. Generated bodies, binaries and raw trace
files are excluded from retained artifacts.

## Review corrections

Both maintained drivers now share `benchmark_campaign`, which owns final status,
failure reporting, SIGTERM handling and scratch lifetime. When cleanup also fails,
the original campaign exception remains primary, with a separate `cleanup_failure`
record and an exception note identifying the remaining scratch path. A cleanup
failure after successful work still fails the campaign.

SIGTERM unwinds through the child-process shutdown before scratch cleanup and
report writing. The previous signal handler is restored on exit; additional
SIGTERM signals during that termination cleanup are ignored. The comparison
driver also writes its active-case record before starting each long measurement.

Regression coverage exercises both CLI entry points with real SIGTERM delivery:
the driver exits with code 143, the detached child is gone, scratch is removed and
partial logs plus a failure report remain. Fault injection covers simultaneous
campaign/cleanup errors, interruption/cleanup errors and cleanup-only errors.
Normal completion and continued diagnostic collection after a failed case remain
covered. These checks were added before the fixes and reproduced the failures.

These corrections change only Python harness code, its tests and this report.
The Go implementation and benchmark sources still match the measured candidate.
Frozen input archives retain the harness that actually ran the recorded campaign;
they have not been rewritten to imply that the later fixes were benchmarked.
The private-input permission walk remains a separate performance experiment.
