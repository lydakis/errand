# Shared initialization and verified capture

Baseline: `f109142` (`Skip redundant permission work for private merge inputs`).

## Decision

Retain this slice for shared verified initialization and measured capture gains.
Both final variants passed native full tests, vet and race checks on APFS and
Btrfs. APFS many-file and deep-chain capture improved in every pair; Btrfs also
improved many-file and deep-chain capture. The initial APFS deep-tree regression
was rejected and corrected before adoption. The three considered items,
provenance, cohesive materialization policy and genuine depth fixtures, are included.

This is not evidence of uniform end-to-end improvement or bounded tail latency.
Btrfs directory-heavy results vary, and complete-operation signals changed between
campaigns. The final push screen flagged a 5.4% paired slowdown; a separate
higher-iteration follow-up found a 0.1% paired difference with mixed signs.
That weakens the regression signal without establishing equivalence. Retaining
the shared capture improvement accepts this remaining measurement uncertainty.
See the complete tables and all retained attempts below.

## Scope and implementation

Workspace creation, ephemeral job staging and persistent job acquisition all call
`CaptureWorkspaceBaseContext` to preserve their immutable change base before a
command can mutate the workspace. Capture now uses the same materializer as
push/fetch staging and private merge inputs.

Previously capture cloned or copied files in parallel, synchronized them, then
serially walked the destination through the snapshot tar packer into `io.Discard`
to verify bodies and metadata. The new path checks source metadata, builds the
private tree through the shared writer, and verifies each member within its copy
worker before synchronization. It eliminates the second verification traversal
and tar encoding. Fallback byte copies hash in flight instead of rereading the
copied file. Filesystem clones still require a read to verify their contents.

APFS uses `Fclonefileat` from an open source descriptor and an opened destination
parent. Linux uses `FICLONE` between opened files. Both produce independent file
identities with copy-on-write storage sharing. An unsupported clone falls back
to the shared verified byte copier. No hard links or unverified clone shortcut
are used. Cloning remains selected only for initialization capture in this slice;
ordinary transfer/merge callers retain their existing copy strategy.

The internal materialization policy groups destination permissions, optional
cloning, member synchronization and the final tree barrier. Durable trees restore
manifest modes, synchronize children before parents, then complete the tree
barrier. Capture still renames only after this barrier and synchronizes the
publication directory afterward. The existing Darwin batched full flush and
Linux fsync implementation remain unchanged. Private merge inputs retain their
skipped chmod/map work and do not gain durability flushes.

Capture opens sources strictly, without widening their permissions. Private
staging continues to use temporary access widening with restoration after workers
finish. Both adapters share type, size, mode, link-target, file-identity and body
verification. Regular-file metadata is checked against the opened descriptor;
non-file metadata is checked before materialization. Failure leaves no published partial base, and cleanup traverses
restricted private directories safely. Source files are never modified by capture.

Deep paths reuse at most 16 opened parent roots per source/destination adapter.
Flat and one-directory paths bypass this cache. Borrowed handles cannot be
evicted; if every eligible handle is busy, work uses the original root. Cache
misses start from the nearest retained ancestor. Source handles are revalidated
before eviction and close, with leaves checked before ancestors so the chain
ultimately anchors at the caller's root. Rebinding an ancestor rejects capture.
Workers join before handles close and before publication. No raw unconfined
pathname shortcut is introduced.

Directory creation reuses these roots. Finalization reuses retained ancestors
without opening a new cached parent for a directory that it visits only once.
Regular files no longer get a redundant metadata prepass before their descriptor
checks. These refinements address measured APFS path-resolution work and apply
to the shared writer used by capture, push/fetch staging and merge inputs.

Incoming network tar decoding and cache-assisted extraction remain transport
boundaries in `archive.ExtractWith`. This change unifies the subsequent immutable
base initialization; it does not introduce another temporary source tree or
change the wire protocol, selection policy, snapshot representation or conflicts.

## Review follow-ups included

The benchmark driver now records all ordinary Go test files, test fixtures and
module definitions as comparison inputs. It rejects unmatched inputs before
validation/build unless their exact paths are explicitly acknowledged. Exceptions
must correspond to real differences and are recorded alongside per-file hashes.
All Python sources beside the driver are recorded, including imported support
modules. Per-variant Go toolchain settings and Python version are retained; input
and harness identities are checked again after the campaign.

The revised comparison permits differences only in six regression-test files:
`base_test.go`, `base_shared_test.go`, `staging_parallel_test.go`,
`transfer_materialize_test.go`, `changes_test.go` and `materialize_paths_test.go`,
all under `internal/changes`. The first campaign permitted only the first four. The existing capture
fixture helper in `base_test.go` is unchanged. Benchmark sources and their helpers
match. These exceptions cover added contracts and adaptation to internal APIs,
not different benchmark work.

## Measurement contract

Five alternating baseline/candidate pairs on APFS and Cabal/Btrfs, `GOMAXPROCS=2`, three timed iterations per sample. Both variants run
full Go tests, vet and the seven-package race suite before measurement. Both
receive the identical capture benchmark overlay. Frozen trees are independent
of ongoing local edits.

Capture cases keep total logical bytes at 8 MiB: one file, 512 files, 512 separate
directories, and 512 files distributed over a genuinely 32-level directory chain.
The revised campaign adds 128 independent eight-level branches with one file
each, totaling 1,024 directories and the same 8 MiB.
The bodies repeat deterministic bytes, so these component measurements are not
claims about incompressible data. Setup and cleanup are excluded from capture
timing; metadata checks, cloning/copying, hashing, modes, synchronization and
publication are included.

Complete operations cover 1,000-file workspace creation and ephemeral submission,
both fetch modes, ordinary push and watch. Creation/submission include local
client preparation, loopback transport and server work; submission also includes
execution and result handling. The first operation has a cold application cache,
subsequent operations may reuse it. Filesystem page-cache coldness is not forced.
These results do not measure CLI process startup, cross-machine latency or browser
hot reload.

Keep all samples and report medians, paired ratios and faster-pair counts. A
median paired slowdown above 5% with at least four of five slower pairs triggers
investigation before adoption. This is a regression screen, not an equivalence
or tail-latency guarantee. Retention requires measured capture improvement on
both native filesystems and no unexplained repeatable regression above that
screen in affected complete operations. If the signals disagree, preserve the
candidate as an experiment and record the unresolved decision rather than infer
an overall speedup from fewer operations.

## First candidate and diagnosis

The first complete comparison is preserved in
[`benchmarks/shared-initialization`](benchmarks/shared-initialization):
`inputs.tar.gz`, `cabal/` and `apfs-local/`. Both variants passed full tests,
vet and the seven-package race suite on both hosts. The APFS host was the local
Apple M1 Max on AC power, not the Mac mini. The original mini job lost its
outcome after a daemon restart; its incomplete run is not used as evidence.

The first candidate improved APFS 512-file capture from 138.619 to 100.327 ms and
512-directory capture from 196.874 to 166.596 ms. However, the 32-level case
regressed from 442.110 to 893.166 ms, with the candidate slower in all five pairs.
It was rejected unchanged. Btrfs 512-file capture improved from 323.325 to
240.578 ms; complete-operation effects were mixed. These are medians, with all
pairs retained in the reports.

Diagnostic syscall traces in `deep-profile/` attribute the deep APFS cost to
repeated `os.Root` path traversal. The candidate accumulated 1.63 seconds in
`os.rootOpenDir`, compared with 0.257 seconds in the baseline; `Fclonefileat`
itself accumulated only 0.11 seconds. These are aggregate traced syscall delays,
including concurrent work, not additive wall-time components. Profiling is
separate from the comparative timing samples.

The revised implementation above retains bounded parent roots, avoids repeated
file metadata checks, and skips cache insertion for one-shot finalization opens.
The additional wide/deep case guards against improving a single chain while
making many independent deep branches more expensive.

## Retained-parent candidate and correctness correction

The retained-parent candidate completed on Cabal/Btrfs (`revised-cabal/`). Its
local APFS campaign was interrupted before timing when review found the
implicit-directory alias issue below (`revised-apfs-interrupted/`). Inputs and
reproduction instructions are in `revised-inputs.tar.gz`.
The Btrfs job is `cabal/01M2HD8D8S2FRE2VPC936WMQJV`.

The attempted mini run `mac-mini/01M2HD8E7KATR5S3ZGTGHY4BX4` failed baseline
validation before timing: Apple Git reported an unaccepted Xcode license. Its
report and logs are preserved in `revised-mini-failed/`. No license or system
configuration was changed. The local APFS run uses the same frozen source inputs.
The first local revised attempt also stopped before timing: the unchanged
baseline's `TestDetachedInterruptWaitsForForwardingAndDoesNotReportSuccess` missed
its two-second admission deadline under the full race suite. All three isolated
race reruns passed without changing its deadline or source. The failed pre-timing
attempt remains in
`revised-apfs-validation-failed/`. The next local attempt hit Apple's license
error inside Git-dependent baseline tests; that attempt is preserved in
`revised-apfs-git-failed/`. Selecting the already-installed Command Line Tools
with `DEVELOPER_DIR=/Library/Developer/CommandLineTools` made the affected baseline
race tests pass. The final local launch uses that per-process setting for both
variants, with identical frozen code and benchmark inputs. No license acceptance,
system developer-directory change, or test deadline change is part of the work.
No timing samples are discarded.
The Btrfs retained-parent run improved 512-file capture (311.779 to 242.465 ms)
and the 32-level case (426.260 to 350.271 ms). Workspace creation and ephemeral
fetch crossed the investigation screen, at median paired ratios 1.153 and 1.101.
Separate traces and the final complete comparison investigated those signals; the
component improvements do not establish overall improvement.

Review also found that an implicit directory's 0700 restoration could overwrite
an explicit directory alias's manifest mode on case-insensitive APFS. A capture
probe returned success with incorrect directory permissions in 3 of 20 runs;
the old final pack would have rejected that tree. The finalizer now flushes
implicit directories without resetting their creation permissions. Explicit
members still restore their manifest modes. A deterministic finalizer test and
an APFS capture regression cover this boundary. The focused race suite passes.

The separate Btrfs traces in `btrfs-diagnostics/` used the permission-corrected
candidate. Around 96% of aggregate syscall delay during creation was in fsync
for both variants. Total traced fsync delay was 73.27 seconds for baseline and
70.79 seconds for candidate, including concurrent workers and untimed setup.
The capture-related stacks were similar (24.03 and 24.69 seconds), rather than
showing the APFS-style directory traversal explosion. This does not prove
creation equivalence: traced sample latency still differed, and aggregate waits
are not elapsed time. Ephemeral fetch's traced timed mean reversed direction,
90.93 to 85.93 ms. The decision also uses the final unprofiled paired run;
the creation profile points at durability waits rather than path traversal.
The whole-process fetch trace includes untimed job/capture work, so it cannot
attribute the timed fetch change.

## Final comparison

The permission-corrected inputs are frozen in `final-inputs.tar.gz`. The final
APFS launch selects `DEVELOPER_DIR=/Library/Developer/CommandLineTools` and
`GOFLAGS=-p=1` for both variants. The latter serializes validation/build packages;
timed benchmark binaries still run with `GOMAXPROCS=2` and `CGO_ENABLED=0`.
Both final variants passed full Go tests, vet and the seven-package race suite
on both native hosts. The local Python harness suite passed all 48 tests.
The production source, tests, benchmark fixtures and harness at measurement time
matched the frozen final candidate. The subsequent permission correction below
changes that source; these tables remain measurements of the frozen candidate,
not a fresh benchmark of the corrected tree. Its production SHA256 is
`5963f543a2b7e2e7557d5bcf15d8893b7de592ede8cf305a813f112ef7f8c828`.
All settings are retained with the results. The final Btrfs job is
`cabal/01M2HEXAS9QKN1VRQ4QHQ8E510`; the local APFS reproduction root is
`/private/tmp/errand-init-final-cto3l0vy`. Both full campaigns and the focused
Btrfs push follow-up are complete.

### Final paired results

All times are medians in milliseconds. Paired ratio is the median of each
candidate/baseline pair; it need not equal the ratio of the two medians. The
Btrfs directory-heavy cases are particularly variable, so both views matter.

#### Local Apple M1 Max / APFS

| Case | Baseline | Candidate | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 19.179 | 18.654 | 0.977 | 4/5 |
| capture-512-files | 136.045 | 82.339 | 0.637 | 5/5 |
| capture-512-directories | 195.640 | 156.966 | 0.787 | 5/5 |
| capture-32-deep | 441.322 | 192.514 | 0.443 | 5/5 |
| capture-8-deep-wide | 241.536 | 250.832 | 1.027 | 1/5 |
| workspace-create | 1114.071 | 1032.094 | 0.930 | 5/5 |
| ephemeral-job | 1336.318 | 1300.682 | 0.973 | 4/5 |
| fetch-false | 134.888 | 135.351 | 1.006 | 2/5 |
| fetch-true | 271.373 | 273.966 | 0.999 | 3/5 |
| watch | 441.065 | 432.841 | 0.985 | 3/5 |
| push | 794.173 | 804.262 | 1.012 | 1/5 |

#### Cabal / Btrfs

| Case | Baseline | Candidate | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| capture-1-files | 37.133 | 37.315 | 0.993 | 4/5 |
| capture-512-files | 300.846 | 251.214 | 0.838 | 5/5 |
| capture-512-directories | 555.613 | 621.925 | 1.203 | 2/5 |
| capture-32-deep | 451.205 | 354.335 | 0.781 | 4/5 |
| capture-8-deep-wide | 523.904 | 640.247 | 0.948 | 3/5 |
| workspace-create | 1907.820 | 1631.598 | 0.896 | 5/5 |
| ephemeral-job | 743.277 | 789.894 | 1.065 | 2/5 |
| fetch-false | 93.259 | 90.239 | 0.968 | 3/5 |
| fetch-true | 205.606 | 201.455 | 0.979 | 3/5 |
| watch | 425.543 | 389.664 | 0.916 | 3/5 |
| push | 875.644 | 905.865 | 1.054 | 0/5 |

APFS has no case crossing the stated screen. The wide/deep case is slightly
slower (1.027 paired ratio, four slower pairs), and ordinary push is also slightly
slower (1.012, four slower pairs); neither result proves equivalence. Many-file,
wide/shallow and deep-chain capture improved in all five pairs. Workspace creation
improved in all five pairs.

The final Btrfs create/fetch results reversed the earlier slowdowns. Ordinary
push crossed the screen at 1.054, slower in all five pairs. Its median staging
phase stayed similar (162.5 to 161.0 ms), while apply and client phases increased.
A five-pair follow-up uses nine timed iterations per sample against exactly the
same already-validated source hashes. All original samples remain retained.
Follow-up job: `cabal/01M2HGKJ53XGWVHN739MKX3WSG`.

### Btrfs push follow-up

Five alternating pairs with nine timed pushes per sample used the same source
hashes and toolchain settings as `final-cabal/`. Full validation was reused from
that exact native run; benchmark binaries were rebuilt and all samples retained
in `push-followup/`. The driver and expected validated source identities are
retained alongside the reports.

| Case | Baseline ms | Candidate ms | Paired ratio | Faster pairs |
|---|---:|---:|---:|---:|
| push, nine iterations | 772.567 | 782.127 | 1.001 | 2/5 |

The >5% push slowdown did not reproduce in this follow-up. The original five
slower pairs remain part of the evidence, rather than being replaced by this run.
A small difference, filesystem variability and longer-tail effects remain
unresolved. Directory-heavy Btrfs results also remain inconclusive. No claim that
all operations improved or that push latency is equivalent follows from the data.

## Reproduction and next checks

### Permission correction after the review

The review reproduced a source-access regression on APFS: a directory with mode
0100 containing a readable file captured successfully 100/100 times on the
committed baseline but failed 100/100 on the shared candidate. The installed Go
implementation opens intermediate `os.Root` directories with read access, although
pathname traversal only needs search permission. This also affects extracted
workspaces because extraction restores manifest permissions before capture.

The correction keeps the normal retained-parent path and, only after a permission
error, walks from the pinned root with native search descriptors. Darwin uses
`O_SEARCH`; Linux uses `O_PATH`. Each component rejects symlinks, open files are
verified by descriptor and content, and the parent binding is rechecked from the
root before accepting the operation. Traversal uses bounded handles and never
widens source permissions. Underlying filesystem errors are preserved. Darwin
search descriptors also allow synchronization if a parallel explicit directory
alias has already removed read permission during finalization.

Regression coverage exercises ordinary and deep paths, symlinks below restricted
parents, APFS aliases, and source confinement/rebinding. Deterministic tests also
verify cloned output before synchronization regardless of filesystem support,
the busy-cache fallback, and eviction-time identity rejection. These are the
considered items included with the correctness fix.

Validation after the correction passed full Go tests and vet on local macOS/APFS
and Cabal/Linux/Btrfs, plus race checks for changes, archive, client, daemon and
CLI packages. The APFS search-only regression passed 20 repeated runs. A final
guard prevents permission fallback from masking a retained-parent verification
failure; the changes package's race tests and vet were rechecked on both hosts.
The final regression asserts source and captured permissions before the output
oracle temporarily widens its own private input for verification.

The [directory materialization follow-up](DIRECTORY_MATERIALIZATION.md) now
controls deferred cleanup I/O, extends wide/deep measurements and compares the
current scheduler with two directory-batched alternatives. Both alternatives
regress deep APFS capture; the current implementation is retained. Timer
exclusion alone does not exclude delayed cleanup writes from a later flush.
The original tables here remain unchanged; the follow-up measures the corrected
`36d2f59` baseline with the revised fixture policy and makes no cross-campaign
speedup claim.

### Reproduce the frozen measurements

`inputs.tar.gz`, `revised-inputs.tar.gz` and `final-inputs.tar.gz` preserve each
candidate, its benchmark overlay and launch script against `f109142`. Each has a
README. `summarize.py REPORT...` prints every case from complete reports without
filtering. `diagnose.py` reproduces the Btrfs traces from the final input root;
`push_followup.py` plus `validated-source.json` reproduce the focused follow-up.
Place those scripts/files beside the reconstructed `baseline/` and `candidate/`
directories before running them.
On APFS use the final launch environment stated above. Keep outputs on the
filesystem under test and run paired timing campaigns without concurrent load
from another benchmark on that host.

Step 3 remains persisted incremental snapshot state, followed by large-file body
transfer in step 4. Keep the new depth fixtures and warm push/fetch checks in
subsequent comparisons. Longer directory-heavy Btrfs measurements and durability
wait attribution should precede further directory-handle or flush-policy tuning.
