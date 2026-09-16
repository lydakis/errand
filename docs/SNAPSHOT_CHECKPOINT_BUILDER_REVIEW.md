# Shared builder review follow-up

## Scope

This follows the [shared-builder experiment](SNAPSHOT_CHECKPOINT_BUILDER.md).
It fixes the three verified review findings and includes the related fingerprint,
admission, test-coverage and cold-path measurement work. It changes no transfer
protocol, durability policy or adaptive/Merkle representation. Disk checkpoints
remain experimental.

## Changes

- Reused symlinks take their target from the matching observation, just as reused
  files take their hash. Replacement between stat and target resolution can no
  longer combine a new target with the old stamp. The regression was observed
  failing before the fix; misses still read the current target.
- Checkpoint loading validates metadata in both strategies. Only prior-state
  updates construct the prior immutable snapshot. Direct construction uses the
  same canonical metadata validator without retaining an unused snapshot.
- `BuildPaths` owns the sorted paths and implicit ancestors before cache
  admission. Git selections exceeding the expanded ceiling skip checkpoint I/O.
  Explicit collection replaces the old zero-limit sentinel and retroactive
  disabled/status updates. Unsupported empty and nonempty selections use ordinary
  hashing and leave existing checkpoint bytes untouched.
- Each observed entry computes one native fingerprint. The ordinary builder's
  lookup returns a nil pointer instead of returning an unused manifest entry by
  value. Disabled observation builds retain ordinary slice-growth behavior.
- Loaded checkpoint state and persisted checkpoint records have distinct types.
  The benchmark CLI documents its current mode and the historical zero Scan phase.

## Measurement contract

The frozen comparator remains `5866129`. The corrected candidate is identified by
[review-fixes-inputs.json](benchmarks/checkpoint-builder/review-fixes-inputs.json).
Its complete reproducible Go/module and Python inputs are retained in the adjacent
archive. The main matrix uses sixteen balanced rounds per case and the existing
eight-pair ordinary-builder guard. Actual command paths use eight alternating
baseline/candidate pairs with three benchmark iterations per invocation. These
are native-filesystem loopback HTTP paths, not measurements of cross-host network
latency or browser readiness. Watch measurements exercise the existing retained
session path; workspace creation and ephemeral submission include cold setup.

A same-binary fallback control compares ordinary hashing with explicitly disabled
observations. The old/shared-loop diagnostic runs last, so its generated Go files
cannot contaminate the preceding source inventories. Reports preserve raw samples,
source/harness/binary identities, native mounts and correctness checks.

Native jobs: Mac mini `01M2KP3CJF0QK5B7X0FB5W7193`; Cabal
`01M2KP3DMGQZPC22N69HA0DA0X`.

## Validation

The complete local `go test ./...` and `go vet ./...` passed. Focused tests cover
symlink observation binding, regular reuse and limits, structural transitions,
empty selection, admission before cache loading, unsupported fallback, and equal
metadata rejection with and without prior-state restoration. The checkpoint and
snapshot Python harness suites passed (18 tests).

A private integration overlay reproduced replacement between stat and symlink
resolution through the complete builder. After the fix, the manifest retained the
coherent cached target and ordinary packing rejected the changed link. No probe
hooks were added to the production builder. Native campaigns run focused race
checks before timing; caller controls additionally validate the client, daemon and
CLI packages on their host.

## Roadmap disposition

Included now: symlink binding, fair strategy construction, expanded-count admission,
one fingerprint per entry, fallback coverage, and actual-caller cold controls.

Next slice: unify native reuse rules with the in-memory builder and strengthen
observation publication ownership before adding callers. Continue the planned
derived-index restoration and changed-record comparison using frozen inputs.

Before production adoption: executable filesystem eligibility, cache ownership and
directory lifetime, early encoded-byte admission/bounded codec work, mount aliases,
and selection guards through freezing. The current 64 MiB limit still bounds the
encoded payload rather than peak allocation. These gates remain open.

## Corrected results

Keep the shared pass and retain prior-state updates as the prototype default.
Removing the unused prior snapshot makes the strategy comparison fairer, but
direct construction still has no consistent advantage across hosts and workloads.
For 50K entries its current/update paired ratios are 1.010/1.018/1.014 on APFS
(unchanged/edit/batch), and 1.030/0.996/0.985 on Btrfs. Larger batches sometimes
favor direct construction; that is not a general default-selection rule.

The 50K initial-population improvement survives the corrections with 16/16 wins
on both hosts. Warm changes are smaller and mixed. Times below are median ms;
ratios are medians of within-round ratios, not ratios of displayed medians.

| Host | 50K case | Frozen checkpoint | Shared update | Paired ratio | Wins |
|---|---|---:|---:|---:|---:|
| APFS | Initial population | 875.10 | 813.89 | 0.926 | 16/16 |
| APFS | Unchanged | 271.05 | 265.08 | 0.968 | 11/16 |
| APFS | One edit | 287.87 | 275.11 | 0.996 | 8/16 |
| APFS | 1,000 edits | 304.58 | 278.90 | 0.965 | 12/16 |
| Btrfs | Initial population | 1246.20 | 1075.81 | 0.862 | 16/16 |
| Btrfs | Unchanged | 532.33 | 533.78 | 0.995 | 9/16 |
| Btrfs | One edit | 607.95 | 619.69 | 1.013 | 4/16 |
| Btrfs | 1,000 edits | 636.74 | 624.07 | 0.984 | 10/16 |

All fixtures, phase medians, ranges and positions are in the
[matrix summary](benchmarks/checkpoint-builder/review-fixes-matrix-summary.json).
The two shared strategies have identical miss paths; differences between their
miss measurements reflect run variation rather than different algorithms.

### Actual caller controls

These exercise the ordinary production paths, which do not use disk checkpoints.
They do not show a uniform production slowdown. They also do not demonstrate a
universal speedup: the Btrfs ephemeral-job paired median is 3.0% slower. Eight
pairs characterize these fixtures, not every repository or machine state.

| Host | Operation | Frozen ms | Candidate ms | Paired ratio | Wins |
|---|---|---:|---:|---:|---:|
| APFS | Workspace creation | 539.64 | 542.47 | 1.001 | 4/8 |
| APFS | Ephemeral job | 516.66 | 520.66 | 1.006 | 2/8 |
| APFS | Push | 485.00 | 485.88 | 1.004 | 2/8 |
| APFS | Watch | 309.09 | 308.38 | 1.001 | 4/8 |
| APFS | Fetch ephemeral | 101.28 | 102.08 | 1.001 | 4/8 |
| APFS | Fetch persistent | 200.93 | 201.77 | 1.014 | 2/8 |
| Btrfs | Workspace creation | 1742.87 | 1652.55 | 0.925 | 5/8 |
| Btrfs | Ephemeral job | 712.28 | 724.71 | 1.030 | 2/8 |
| Btrfs | Push | 797.21 | 781.07 | 0.986 | 5/8 |
| Btrfs | Watch | 387.42 | 375.32 | 0.961 | 5/8 |
| Btrfs | Fetch ephemeral | 88.62 | 89.08 | 0.991 | 5/8 |
| Btrfs | Fetch persistent | 209.32 | 203.97 | 0.985 | 5/8 |

The disabled-observation same-binary control measures 33.11 to 33.15 ms on APFS
(paired ratio 1.002) and 347.41 to 363.66 ms on Btrfs (1.044). Collection disabled
uses the ordinary allocation policy, but these samples do not justify claiming
zero fallback overhead. Details are in the
[caller summary](benchmarks/checkpoint-builder/review-fixes-callers-summary.json).

### Isolated cold controls and negative control

The original same-executable 64-KiB control gives 0.995 on Btrfs (16/30 wins).
On APFS it gives 1.128 (13/30), with a strong execution-position effect. First
positions are commonly around 65–75 ms while later positions approach 90–105 ms,
including the two labels executing identical old code.

A follow-up interleaves two groups in one executable: every label runs the old
loop in the negative control; the other group runs the real old/shared comparison.
The original statistic (median shared / arithmetic mean of old A and old B within
each round) reports 1.054 for the all-old control and 1.042 for the real comparison.
Identical code can therefore produce an apparent slowdown under that statistic.
The geometric mean of the real group's paired ratios to the geometric old-pair
mean is 0.998, versus 1.032 in the all-old group. These are additional descriptive
statistics, not adjustments to previous measurements.

This does not establish the underlying scheduler, cache or runtime cause, and it
does not prove all cold workloads regression-free. It does show that the original
APFS scalar alone cannot attribute a slowdown to the shared loop. No cold-control
ratio was used to divide away a checkpoint or caller result. The diagnostic job is
`mac-mini/01M2KQHCBDM94BVRSY28NE73SF`; its script, raw samples and overlays are retained
under [position-control](benchmarks/checkpoint-builder/review-fixes-apfs/position-control/report.json).

The separate-binary Btrfs Git cold control still reports 1.092 (2/8 wins), with
the extra time concentrated in preparation/hash rather than Git selection. The
50K ordinary cold guard is 1.040 (3/8). These remain visible in the matrix summary.

A final Btrfs Git diagnostic repeats the 10K-file, 4-KiB fixture with old and
shared loops in the same executable, interleaving identical-code negative
controls. The mixed comparison gives 0.976 (17/30 wins), versus 0.994 (16/30)
for identical code. Geometric paired means are 0.978 and 0.998 respectively.
The earlier 9.2% signal does not reproduce in this controlled comparison; this
does not identify its cause or invalidate the separate-binary samples.
Job `cabal/01M2KR7T1CBXTJQ6DXV98H0HXX` retains all 180 samples, fixture and
overlays under [git-control](benchmarks/checkpoint-builder/review-fixes-btrfs/git-control/report.json).

The evidence supports keeping this slice for review: the shared implementation
preserves the repeatable initial-population gain, and the corrected strategy
comparison does not warrant switching defaults. Warm and ordinary-call results
remain mixed. No blanket regression-free or end-to-end speedup claim follows.
