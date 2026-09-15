# Directory materialization experiment

Baseline: `36d2f59`, including the search-only permission correction. Both
variants receive the identical revised capture benchmark. Production continues
to use the baseline until a measured alternative passes the adoption gates.

`grouped.patch` changes only shared file scheduling. It sorts files by parent
directory and schedules batches of at most eight files. Each batch holds one
destination-parent lease; the source adapter and its verification remain intact.
The batch limit preserves parallelism for flat trees. This tests scheduling and
lease reuse, not complete removal of the parent cache or a different durability
policy. Cancellation is checked between members and all workers still join.

`grouped-ordered.patch` is a separate control that keeps the incoming file order
and batches only adjacent members of the same parent. It removes the sorting
step from the first candidate. Deep-chain file order can interact with which
ancestors remain open after directory creation; the follow-up measures that
interaction rather than assuming grouping is the only changed cost.

The comparison keeps source-binding checks, the bound of 16 retained parents per
adapter, byte verification, native cloning, manifest permissions and publication
barriers unchanged. A grouping win must translate to complete operations without
a repeatable regression. A loser remains experiment material, not a runtime flag.

The deep fixture's manifest already groups files by parent. Both patches form
the same batches there; the second is a manifest-dispatch-order control, not a
fixture-insertion-order or different-batch-membership control.

## Measured identities

Both frozen archives were checked: their only production Go difference is
`internal/changes/transfer_materialize.go`, and its diff exactly matches the
corresponding patch. Applying each patch to baseline `36d2f59` yields these
`source_digest(root, production=True)` identities:

| Patch | Candidate production SHA256 | Campaigns under `docs/benchmarks/directory-traversal/` |
|---|---|---|
| `grouped.patch` | `8580bdac49683e83d925ef221a0ffc6eb8934a55c120190eaeb072577c3983b7` | `apfs`, `btrfs` |
| `grouped-ordered.patch` | `04d4dc9c80ce48ce9ca3584e5637b12303bf696c40e1cd39573d66af4af16f40` | `ordered-apfs`, `ordered-btrfs` |

## Measurement

`scripts/benchmark_materialization.py` accepts two frozen source directories. It
requires identical test/fixture/module inputs, validates the changes package,
runs its race tests and vet, and records source, harness, toolchain and binary
identities. Full repository validation is a separate gate for adopting production
changes. It alternates the variants over seven pairs by default.

Capture uses seven iterations per sample and retains output trees until the
sample ends. `testing.B.Loop` avoids a separate calibration invocation. Fixture
files and directories are synchronized before timing; each output's containing
directory is created and synchronized outside timing. Timed capture still
includes verification, cloning/copying, all member flushes and publication.
Keeping outputs removes this benchmark's between-iteration deletion I/O; it
does not establish that deletion I/O caused earlier noisy results.

The driver runs `sync` between benchmark processes. Filesystem caches remain
warm, and unrelated host activity remains possible. Complete command benchmarks
retain their existing setup and cleanup policies; the capture fixture change is
not a claim to have isolated every operation from all filesystem I/O.

Separate deep-chain and wide/deep profiles collect mutex contention and syscall
waits. These include untimed setup and cleanup, and concurrent waits can exceed
elapsed time. Do not add mutex and syscall delay as wall-time components or use
profiled runs as timing samples.

The frozen inputs and native reports are retained under
`docs/benchmarks/directory-traversal/`. Reproduce by extracting `inputs.tar.gz`
and running the candidate's `scripts/benchmark_materialization.py` with its
`--baseline`, `--candidate` and a new `--output` path on the filesystem under test.
The output must be outside both source trees, including through symlink aliases.
Both source directories must remain unchanged throughout the campaign. On macOS,
use `DEVELOPER_DIR=/Library/Developer/CommandLineTools` and `GOFLAGS=-p=1` as in
the recorded launch. Do not run another benchmark on that host concurrently.

Frozen drivers and reports preserve the original campaign. Current drivers add
output-path guards and filesystem mount metadata for subsequent runs; the current
counter driver also records imported harness/toolchain identities and rejects
missing samples or incomplete adapter counters. These checks do not alter the
archived data or fill in historical mount options that were never recorded.
