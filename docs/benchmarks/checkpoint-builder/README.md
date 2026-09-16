# Checkpoint builder evidence

See [the report](../../SNAPSHOT_CHECKPOINT_BUILDER.md) for decisions and limits.
These files are measurement evidence, not production dependencies.

The [review follow-up](../../SNAPSHOT_CHECKPOINT_BUILDER_REVIEW.md) uses
`review-fixes-inputs.*` and `review-fixes-{apfs,btrfs}`. Each host retains separate
matrix, caller and same-loop reports plus compressed raw logs. Unlike the earlier
campaign, the matrix and callers run before source-generating diagnostics, so
their Go source inventories match the clean archive directly. The matrix has
sixteen rounds per case; its independent ordinary-builder guard has eight pairs.
Caller and disabled-observation controls have eight pairs.

The Mac `position-control` report interleaves real old/shared comparisons with an
all-legacy negative control in the same executable. `position_control.py` is a
follow-up diagnostic for the review-fixes inputs; copy it beside the source after
extracting that archive to replay it. It changes no production code, records its
own hash, and does not numerically correct or replace the preceding measurements.

The Btrfs `git-control` report uses `git_position_control.py` with the same
interleaved negative-control design on 10,000 Git-selected 4-KiB files. Replay it
against the review-fixes archive by copying this diagnostic beside the extracted
source, then passing a fresh `--output` directory. Its separate script hash and
180 raw samples are retained; it does not replace the separate-binary cold guard.

- `exploratory-inputs.*` and `exploratory-{apfs,btrfs}` retain the first attempted
  campaign. Its final guard mistakenly counted the extracted baseline as candidate
  source. Reports intentionally retain `status: failed`; do not pool them with
  accepted measurements.
- `candidate-inputs.*`, `apfs`, `btrfs`, `summary.json` and
  `candidate-comparison.md` describe the valid candidate before the ordinary-build
  allocation fix. `recheck-*`, `bisect-*`, `same-*`, `same-bisect-btrfs` and
  `growth-btrfs` retain the investigation of its regression signals.
- `final-inputs.*` identify the allocation-policy fix. Its ordinary builds use
  their existing slice-growth behavior; observation builds reserve result capacity.
  Final native reports are in `final-{apfs,btrfs}` and fixed-loop controls in
  `fixed-control-{apfs,btrfs}`. `final-summary.json` summarizes the final matrix.
  `final-campaign-inputs.json` additionally identifies three retained diagnostic
  Go files counted by the matrix inventory after the fixed-loop control; the report
  explains and verifies that wider inventory.

The archives contain the complete measured Go/module and Python harness inputs,
plus the older frozen checkpoint archive required by the comparison driver. They
omit unrelated older benchmark evidence. JSON inventories record source, harness
and archive hashes. Reports and `logs.tar.gz` retain all raw samples and checks.
Documents were completed after measurements; Go and harness identities are checked
independently of documentation.

## Reproduction

Extract the desired input archive into an empty directory and run from that root:

```sh
python3 scripts/benchmark_checkpoint_builder.py --output /tmp/checkpoint-comparison
```

The output directory must not already exist. On the measured Mac mini, jobs used
`DEVELOPER_DIR=/Library/Developer/CommandLineTools` because the default Xcode Git
launcher required a license acknowledgment. No system setting or license was
changed. Both variants within each comparison used the same tool environment.

Diagnostic scripts in this directory are frozen probes for the *candidate*
revision, not general-purpose source rewriters. To replay `recheck.py`,
`bisect_cold.py`, `same_binary_bisect.py` or `growth_bisect.py`, extract
`candidate-inputs.tar.gz`, copy the selected diagnostic script into that checkout,
and run it with `--output` pointing to a new directory. Its source overlays are
retained in its output logs. `same_binary.py` also runs against the final inputs.
Never apply diagnostic overlays to production files.

`summarize.py APFS_REPORT BTRFS_REPORT` emits the descriptive four-way summary.
Ratios are paired by round; medians are not silently pooled across revisions.
