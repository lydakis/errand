# Journal measurement review follow-up

This follow-up changes instrumentation and interpretation, not the Go preparation
or storage algorithms. See [the report](../../../JOURNAL_DEPTH_PROFILE.md).

## Original evidence

The parent `apfs/` and `btrfs/` timing reports, source archives, raw logs and combined
profiles remain byte-identical. `original-SHA256SUMS` preserves their pre-review
manifest (including parent documentation as it then stood). Documentation can now
point to this follow-up; use the current parent manifest to verify the complete
current collection. Do not treat the historical source-match statement as applying
to the corrected Python harness.

The original sparse histories and cycles repeatedly edit one file. Fixed-depth
orders cover all marginal permutations but are coupled by round. These limits do
not invalidate the measured workload or dense-history rejection, and do prevent
claims about arbitrary sparse histories or independent ordering.

`harness-followup.patch` maps original campaign Python sources to this corrected
harness; the supplemental probe's baseline comes from its own original archive.
No Go input changed during review fixes. The next comparison's crossed ordering,
rotating/dispersed edits and competing runtime optimizations are roadmap work,
not measurements claimed by this follow-up.

## New diagnostics and validation

- `apfs/` and `btrfs/`: profile-only native campaigns. Each report contains 72
  profile receipts: four scenarios, three variants, three repetitions, two
  separate instruments. Ordinary comparison arrays are empty and timing_complete
  is false. Profiled Total values must not be added to original timing samples.
- Each CPU invocation requests only `-cpu-profile`; each allocation invocation
  requests only `-heap-profile`. CPU runs retain normal runtime allocation
  sampling. New raw profiles and their independently collected pprof summaries
  live in `profiles.tar.gz` and `profiles/` respectively.
- `inputs.tar.gz`, toolchain/mount/build metadata, native test/race/vet/Python logs,
  report hashes and `job.txt` identify each measured run. Both hosts pass Go test,
  race, vet and 71 Python tests. Post-fetch validation checked exact current
  source/archive identity, the 36 CPU + 36 heap count, empty ordinary samples and
  matching variant roots for every case/repetition/instrument.
- `helper-smoke/`: a separate native Mac mini run with 100-file ignore and Git
  fixtures verifies the shared incompressible treatment loop, logical byte
  receipts and unchanged roots. This is functional validation, not a timing claim.

## Full phase accounting

`apfs-phases.md` and `btrfs-phases.md` are generated from the original retained
samples by `scripts/summarize_journal_depth.py`. They include every recorded phase,
Wire, residual Other, Total, and paired preparation-only ratios. Individual
medians do not add up to the median Total. Reproduce from the repository root:

```sh
python3 scripts/summarize_journal_depth.py docs/benchmarks/journal-depth/apfs/report.json --output /tmp/apfs-phases.md
python3 scripts/summarize_journal_depth.py docs/benchmarks/journal-depth/btrfs/report.json --output /tmp/btrfs-phases.md
```

Run the new diagnostics without repeating the timing campaign:

```sh
python3 scripts/benchmark_journal_depth.py --profiles-only --output dist/journal-profiles-separated
```

On the measured Mac mini, set DEVELOPER_DIR=/Library/Developer/CommandLineTools.
The separate helper smoke command is:

```sh
python3 experiments/snapshotcheckpoint/interference_probe.py --smoke --output dist/interference-helper-smoke
```
