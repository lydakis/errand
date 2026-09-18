# Apply follow-up evidence

See [the report](../../APPLY_FOLLOWUP.md) for interpretation and adoption decisions.
`runs.json` identifies each source, command and runner job. `inputs/` contains
immutable sources. `driver.py` is the driver used for the secondary campaigns.

Verify reports from the repository root:

```sh
python3 scripts/verify_apply_followup.py docs/benchmarks/apply-followup/{apfs,btrfs,apfs-exchange,btrfs-exchange,btrfs-historical-small,btrfs-flat-repeat}
```

Verify file hashes from this directory with `shasum -a 256 -c SHA256SUMS`.
The verifier also checks all marked decision tables in `docs/APPLY_FOLLOWUP.md`,
including paired reductions and faster-pair counts. It compares the current
checkout's production sources with the frozen `parents` archive plus the exact
before/after hashes in `review-source.json`. Use `--checkout PATH` when verifying
another checkout. A later source change must not silently inherit these timings:
the review edits are explicitly unbenchmarked, and the original six campaigns,
source archives, and raw logs remain unchanged.
Exchange production code is deliberately absent from the checkout; only its
isolated patch and frozen experiment sources are retained.
