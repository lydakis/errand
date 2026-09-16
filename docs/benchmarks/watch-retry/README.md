# Watch source retry validation

The original Linux CI failure stopped `push --watch` after a source changed
during hashing. Retrying based on watcher generations depended on when native
notifications arrived. Resetting that budget on every edit also let unrelated
edits hide invalid source files and storage failures indefinitely.

This change retries explicitly classified source mutations without consuming
the three-retry budget for other source errors. Mixed mutation/storage errors
remain bounded. Exhausted source errors cannot fall through to the network
reconnect loop. Preparation is invalidated before resampling; the existing
50 ms delay is unchanged.

Regression coverage includes mutation with and without native notifications,
unsupported files during unrelated edits, alternating and joined disk/mutation
failures, and same-size edits detected while copying a transfer source.

## Measurement method

All three benchmarks use 10,000 files of 1 KiB each. They warm the workspace,
then measure ordinary push/apply, watched single-file edits, and watched bursts
of 400 writes spaced 5 ms apart. Every completed operation verifies destination
contents. Burst time includes the writer; `settle-ms/op` measures delivery after
the final write. CPU is process user plus system time, including the in-process
client and daemon. These are local HTTP transfer benchmarks on each runner,
not inter-machine latency or command startup measurements.

Baseline and candidate use the same benchmark code and Go compiler. Pairs
alternate execution order. Each benchmark process measures three operations
after Go's one-operation calibration. Fixtures and state reside on the runner
workspace volume, not Linux `/tmp` (which is tmpfs on the measured machine).
The saved report retains individual samples so medians and ranges can be checked.
Small differences should not be interpreted as statistically established gains.

The first comparison used `f109142` with four pairs on each machine. During
measurement, the checkout independently advanced to `001c997`; a second
comparison uses that baseline with two pairs on each machine. The original
processes used frozen snapshots, so the two comparisons are kept separate.
Another task started on Cabal near the end of the first campaign; treat that
Linux campaign as exploratory. Two overlapping follow-up attempts were stopped
and excluded. The final Linux campaign ran in an agreed idle window.

## Results on the updated baseline

Go 1.27.1 on macOS 27.0 arm64/APFS and Linux 7.1.9 x86_64/Btrfs.
Values are medians of two samples per variant, each measuring three operations.

| Measurement | APFS baseline | APFS candidate | Btrfs baseline | Btrfs candidate |
| --- | ---: | ---: | ---: | ---: |
| Push/apply | 593 ms | 595 ms | 1,038 ms | 825 ms |
| Watched edit | 425 ms | 420 ms | 439 ms | 451 ms |
| Burst, including writer | 3,148 ms | 3,166 ms | 2,932 ms | 2,908 ms |
| Delivery after final burst write | 744 ms | 765 ms | 776 ms | 751 ms |
| Watched edit CPU | 129 ms | 126 ms | 298 ms | 307 ms |
| Burst CPU | 1,471 ms | 1,477 ms | 2,939 ms | 2,902 ms |

No material regression was observed in these scenarios. Btrfs ordinary-push
samples varied substantially: baseline 855–1,220 ms, candidate 795–855 ms.
Do not interpret the median reduction as an established speedup. The earlier
exploratory campaign's 8% higher candidate push median did not repeat.
The limited sample size also cannot rule out small changes.

Keep the existing retry delay: the measurements do not justify adding adaptive
backoff or trading final-delivery latency for fewer attempts. These results do
not generalize to every Linux filesystem, network, or workload. Individual
samples and candidate file hashes are in [results.json](results.json).

Validation passed on the measured candidate: full `go test -race ./...` and
`go vet ./...` on Linux; race tests for archive, snapshot, changes, client, CLI,
and daemon packages plus full vet on macOS. Formatting and diff checks passed.

## Reproduce

From the repository root containing this patch:

```sh
python3 docs/benchmarks/watch-retry/compare.py 001c997 --rounds 4 \
  --output /tmp/watch-benchmark-results.json
```

The script replaces the six changed existing production files in the baseline
binary through a Go overlay. New classification code remains present but unused
there. Use the specified revision with the matching patch checkout; this is not
a general comparison of arbitrary revisions. Run on an otherwise idle machine
and record the filesystem and Go version. The output may contain local paths;
the checked-in report contains only metrics and source hashes.
