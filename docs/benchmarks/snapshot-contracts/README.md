# Snapshot contract evidence

See [the report](../../SNAPSHOT_CONTRACTS.md) for scope and conclusions.

`baseline-inputs.*` freezes the Go/module sources at `001c997`.
`candidate-inputs.*` freezes the measured Go/module sources and Python harnesses,
including the baseline archive required to replay the comparison. Extract the
candidate archive into an empty directory, change to that directory, then run:

```sh
python3 scripts/benchmark_snapshot_contracts.py --output /tmp/snapshot-contracts
```

The output must not already exist. The Mac mini uses
`DEVELOPER_DIR=/Library/Developer/CommandLineTools`. The harness validates source
and helper identities, builds each executable once, runs native focused race
tests and alternates baseline/candidate order. Reports retain raw samples and
native toolchain/filesystem information. Timing excludes remote job setup.

`superseded-candidate-inputs.*` preserves the first ownership implementation,
which was stopped after a value-copy consumption regression was found. Failed
preflight and stopped attempts are not pooled with final measurements.

`apfs` and `btrfs` contain the final complete reports and compressed raw logs.
`summary.json` is generated with:

```sh
python3 docs/benchmarks/snapshot-contracts/summarize.py \
  docs/benchmarks/snapshot-contracts/apfs/report.json \
  docs/benchmarks/snapshot-contracts/btrfs/report.json
```

It reports all eight paired rounds and the final six caller pairs separately.
The latter checks sensitivity to possible early Cabal contention; it neither
discards the original data nor corrects individual samples. The report documents
the overlap and its timestamp limits.

## Review follow-up

`review-inputs.*` freezes the review cleanup and its focused harness, with the
pre-review candidate archive as the control. To replay after extraction:

```sh
python3 scripts/benchmark_contract_review.py --output /tmp/contract-review
```

On the mini, set `DEVELOPER_DIR=/Library/Developer/CommandLineTools` when invoking
the harness. It runs full tests with packages serialized, vet and focused race
tests before collecting eight alternating pairs for unchanged/edited checkpoint
comparison and first/retained watch preparation. Each timing sample records wall
timestamps, and identical comparison benchmarks are installed in both variants.
The comparison microbenchmark covers 10K records and excludes fixture creation,
verification, checkpoint I/O and transfer latency.

`review-preflight-*` retains the initial failed attempts, which collected no timing
samples: the mini selected the unacknowledged Xcode installation; Cabal's concurrent
full-suite run exceeded the active-writer test deadline. `review-preflight-inputs.*`
preserves that harness. Those attempts are excluded from the timing summary.

`review-apfs` and `review-btrfs` contain the completed follow-up reports and raw
logs. Regenerate their descriptive summary with:

```sh
python3 docs/benchmarks/snapshot-contracts/summarize.py \
  docs/benchmarks/snapshot-contracts/review-apfs/report.json \
  docs/benchmarks/snapshot-contracts/review-btrfs/report.json \
  > docs/benchmarks/snapshot-contracts/review-summary.json
```

The shared summarizer also emits the last six pairs for inspection; the follow-up
conclusion uses all eight. No timing samples from failed attempts are included.
