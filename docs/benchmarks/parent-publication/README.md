# Parent-publication evidence

See [the report](../../PARENT_PUBLICATION.md) for scope, recovery ordering and
interpretation. `runs.json` records the native runner handles and commands.
`inputs/` retains the exact sources, including the common driver, used to build
each variant. Both reference labels execute the same baseline binary. Each host
contains raw individual operation logs, the generated report and summary, and
package/race-test results.

From the repository root:

```sh
python3 scripts/verify_parent_publication.py
```

The verifier checks raw hashes, complete sample membership, rotated execution
positions, generated summaries, paired comparisons, and frozen source hashes.
It pins every baseline Go source/test and module file to Git commit `fd3ce97`,
requiring that commit to be available locally. Across the complete frozen
archives, only `apply_group.go` and `apply_group_parents_test.go` may differ.
All other inputs, including ordinary test helpers and Python scripts, must match.
Current production must match the measured candidate plus the exact before/after
hashes in `review-source.json`. Those post-measurement edits change comments only;
the expanded tests and verifier are not new performance measurements. Use
`--checkout PATH` to verify this reviewed revision after later changes. Tests and
docs are outside the production comparison with the current checkout.

From this directory, verify all retained evidence files with:

```sh
shasum -a 256 -c SHA256SUMS
```

The archives are immutable. Do not regenerate them from later production edits
or attach these timings to an altered candidate.
