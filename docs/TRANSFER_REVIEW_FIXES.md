# Transfer review fixes, 2026-09-12

The subsequent [final benchmark round](TRANSFER_FINAL_BENCHMARKS.md) reruns the
completed implementation alongside rsync and Mutagen on both runners.

All four required review findings are fixed. This pass also removes repeated
metadata work, but does not establish a uniform end-to-end speedup. Current
clients are assumed to upgrade together.

## Behavior and shared implementation

- Known dirty files invalidate their cached hashes even when stat evidence is
  identical. Structural events invalidate descendant hashes; overflow and failed
  preparation discard cached hashes before rebuilding.
- Watch reselects after registering control watches, closing the startup gap
  where a newly tracked, previously ignored directory could lack a watch.
- Every push apply publishes a local generation token under the checkout transfer
  lock. A running watch observes that token before its unchanged shortcut. An
  ordinary push of B followed by a local restoration of remembered A now delivers
  A. Unchanged events still make no network requests. The token is advisory and
  uses atomic rename without fsync; existing pending requests and receiver
  receipts remain the durable recovery authority.
- Workspace creation verifies selection before advertising cache fingerprints,
  and retains its later checks.
- Watch reuses the prepared delta when accepting an apply receipt instead of
  calculating that delta again. The shared helper still validates the delta,
  baseline and accepted paths.
- Receiver source expansion runs outside the workspace apply lock. Staging
  validates the checkpoint under that lock. A regression additionally verifies
  that retrying an unstarted stage rejects a changed checkpoint, while applying
  and completed attempts retain their existing recovery behavior.
- Incremental preparation avoids one redundant Git metadata probe. Selection
  evidence and post-preparation/post-freeze Git checks remain.

These changes retain content verification, source quotas, conflicts and durable
apply recovery. The shared staging changes also serve persistent fetch; this
pass does not demonstrate a fetch speedup.

## Measurements

The local 10K-entry accepted-source metadata benchmark compares recomputation and
delta reuse inside one binary, with 20 iterations per variant on M1 Max:

| Path | Time per operation | Allocated bytes per operation |
|---|---:|---:|
| Recompute delta | 7.28 ms | 8,715,934 |
| Reuse prepared delta | 5.64 ms | 4,332,810 |

That is 23% less time and 50% fewer allocated bytes for this metadata operation,
not for a complete transfer.

The 10K-file phase fixture ran on each runner with both endpoints on that host.
Baseline means combine two three-iteration runs. Final means combine two
three-iteration follow-ups after the interleaved experiment:

| Host / operation | Pre-fix baseline | Final follow-up |
|---|---:|---:|
| Mac mini / watch | 409 ms | 414 ms |
| Mac mini / push | 544 ms | 568 ms |
| Mac mini / persistent fetch | 222 ms | 231 ms |
| Cabal / watch | 465 ms | 403 ms |
| Cabal / push | 846 ms | 926 ms |
| Cabal / persistent fetch | 43 ms | 45 ms |

Mac mini uses Darwin arm64/APFS; Cabal uses Linux amd64/Btrfs. Cabal watch improved
in these samples, but ordinary push was slower on both hosts. Host activity and
small samples prevent attributing every difference to code. These results do
not establish absence of performance regressions or reliable tail bounds.

An earlier baseline/candidate/candidate/baseline experiment measured watch means
of 409 to 383 ms on Mac mini and 465 to 394 ms on Cabal. That intermediate
candidate checked the remote checkpoint on unchanged events. The final version
uses the cheaper local generation token and separately verifies zero network
requests on unchanged events. Keep the experiments distinct; the interleaved
results are not exact-final-binary evidence.

The final local binary also completed five-save, 10K-file comparisons on M1 Max
with APFS. These medians include destination-content verification for delivery
and the final durable receipt separately:

| Selection / save | Watch delivery | Watch receipt | Push completion | rsync checksum completion |
|---|---:|---:|---:|---:|
| Explicit / in-place | 451 ms | 587 ms | 960 ms | 1,316 ms |
| Git / atomic | 966 ms | 1,103 ms | 1,204 ms | 1,336 ms |

Both reports verified all selected file contents, observed one receipt for the
20-save burst, no unexpected idle receipts, and clean watcher shutdown. The
three-second idle probes measured zero CPU at 0.01-second resolution; they do not
establish sustained zero resource use. Rsync was Apple openrsync protocol 29 with
`-a --checksum --delete`, not default quick-check rsync. These are different save
and selection workloads, not a before/after comparison. They show that Git and
atomic saves still fall well short of near-instant watch.

## Broader harness and remaining work

The comparison harness now supports Git selection, atomic editor saves, and a
pause before each save to exercise preparation expiry. It drains receipts through
watch shutdown, compares all selected file contents at completion, and reports
CPU-counter resolution, using `/proc` ticks on Linux. Whole-tree verification
covers file contents; it is not a directory-mode or symlink contract test.

```sh
python3 scripts/benchmark_watch.py --binary /absolute/path/to/errand \
  --files 10000 --samples 5 --selection git --save-mode atomic \
  --idle-seconds 3 --output /absolute/path/to/new-result-directory
# Add --pause-seconds 31 to exercise preparation expiry before measured saves.
go test ./internal/changes -run '^$' \
  -bench '^BenchmarkAcceptedSource$' -benchtime=20x -count=1
go test ./cmd/errand -run '^$' \
  -bench '^Benchmark(PushPhases|WatchPhases|FetchCompletion)$' \
  -benchtime=3x -count=2
```

Git-driven selection and atomic saves still take full reconciliation. Preparation
expiry is checked only when work arrives; there is no idle reconciliation timer.
The existing 4096-attempt bound, history scans and complete durable manifests
remain. Retention/index changes need a separate recovery-aware design. Real
laptop-to-runner latency, long histories and sustained idle resource use remain
unmeasured in this slice. Mutagen was not rerun; prior comparison results remain
in [the preceding report](TRANSFER_PERFORMANCE.md).

## Validation and evidence

Each required finding was reproduced before its fix, then passed regression
coverage. A separate review found the unstarted-stage retry edge case; its
regression failed before the final check and passed afterward. The reviewer
confirmed the correction. Scoped suites passed on Darwin and Linux, with focused
race checks and `go vet`. The final staging correction received targeted tests
on both platforms; the full scoped suites preceded that last correction.

Raw interleaved logs are under `/private/tmp/errand-review-abba-mini/results` and
`/private/tmp/errand-review-abba-cabal/results`. Final phase runs are retained as
Errand jobs `mac-mini/01M2CC7PPB7D7CPG1SZR12SXH4` and
`cabal/01M2CC4X0BNGYE8WFH6MMPXEH0`. The Cabal timing binary precedes only the
unstarted-stage retry check, which is outside the measured new-attempt path.
The pre-fix source archive is `/private/tmp/errand-before-review-fixes.tar`.
Final local reports are `/private/tmp/errand-review-final-explicit/report.json`
and `/private/tmp/errand-review-final-git-atomic/report.json`. Their binary SHA-256
is `584bcda14223b8d36d4722b0326934513228411349875947f34aec2400e915f4`.
