# Generic named-cache validation

All named bindings now use one directory-preservation engine. Command, tool,
path, configuration-file, and environment detection have been removed. No
package-manager adapters, injected configuration, implicit installs, or
container/VM backend are involved. Existing `[caches]` declarations are the opt-in.

## Correctness

The full `go test -race -json ./... -count=1 -timeout=8m` suite ran on the trusted
runners with `ERRAND_TEST_CACHE_TOOLS=1`:

| Runner | Job |
| --- | --- |
| Cabal, Linux amd64 | `01M29ADY0M16ZTN11MR3T9HPGM` |
| Mac mini, Darwin arm64 | `01M29ADWAJ7V632FWJMKV0W4X1` |

The final filesystem capability and GC changes also passed race tests for
`internal/namedcache`, `internal/daemon`, and `cmd/errand` on Cabal
`01M29AYXN0TV6H2FGNX03KS6ZY` and Mac mini `01M29AYWX574VSQBFBT6KK5CMR`.
The final dry-run accounting adjustment passed the complete named-cache race
suite on Cabal `01M29B9PWNTXAD1RD9M9KXX1H5` and Mac mini
`01M29B9R0NBFTDEG7GCP1Z02K4`.
The subsequent concurrent-admission and replaced-parent fixes passed the daemon,
named-cache, and CLI race suites with native tool fixtures enabled on Cabal
`01M29C1NYSR238BRKJTW0XWB3M` and Mac mini `01M29C1M3Q4ZH6QWKZ0SD9XT1P`.
The cold-cache filesystem fix passed those same race suites with native tool
fixtures enabled on Cabal `01M29D3THYHJ9B19S5N84T5V5P` and Mac mini
`01M29D3THAVB3P2WH7ZWXJD62B`, including all three Linux cold-publication cases.

Native fixtures use offline local packages and separate install/build and
verification jobs. They cover independent ephemeral jobs, independent persistent
workspaces, and unchanged configuration in an isolated fixture home.

- macOS: npm, pnpm 10.32.1, Bun, uv, and Cargo ran and passed.
- Linux: pnpm 10.32.1, uv, and Cargo ran and passed. Bun was unavailable. The
  runner's npm shim could not run even `npm --version` before job setup, so its
  fixture was explicitly skipped. This is not Linux npm certification.
- Python verification runs the restored environment's interpreter and imports
  its installed package. Absolute console-script shebangs are preserved, not
  relocated. Cargo verification rebuilds after a source change and executes
  the changed output in a later job.

Lifecycle tests cover overlapping jobs, independent cancellation, persistent
workspace final-member publication, unchanged readers, failed commands using
atomic replacement, partial multi-cache publication retries, legacy adoption,
restart, failed release, GC protection, accounting, and eviction without damaging
already-restored files. More than 400 holders do not exceed a record-size limit.
A restored directory's comparison baseline, executable mode, and modification
time are checked against its actual files. Symlink targets and wrapper contents
are tested unchanged, including absolute paths.

Storage and CLI tests cover owner/workspace attribution, unmeasured sizes,
cache exclusion from snapshots and retained artifacts, and unrelated damaged
cache metadata without blocking a job's own cache lifecycle.

Local `go vet ./...`, `CGO_ENABLED=0 go build ./...`, formatting, and
`git diff --check` are part of the final checks. No commit, push, runner binary
replacement, or Fabric source edit is included.

## Fabric workflow and performance

A temporary Go test fixture runs Fabric's source through in-process daemons.
It declares the root, protocol, TypeScript SDK, server, and CLI `node_modules`
directories using ordinary named-cache bindings. Each arm receives its own
fixture-owned pnpm store. Cold seeds are excluded. Ten pairs alternate which arm
runs first:

- Baseline: fresh workspace, offline frozen-lockfile install, then the protocol
  test command.
- Cache: fresh workspace, restored installed directories, then only
  `pnpm test:root tests/unit/protocol.test.ts`.

Both arms use the same native tools on each machine. No Fabric source, scripts,
or runner-wide tool configuration was changed. The runner fixtures contain
selected source copies, and pnpm 10.32.1 is selected only within those fixtures.

| Machine | Baseline median | Cache median | Reduction | Baseline range | Cache range |
| --- | --- | --- | --- | --- | --- |
| Cabal, Linux amd64 | 3.495 s | 2.543 s | 27.2% | 3.427–3.542 s | 2.386–2.689 s |
| Mac mini, Darwin arm64 | 2.677 s | 2.363 s | 11.7% | 2.565–2.740 s | 2.331–2.524 s |
| Local Mac, CPU profiling enabled | 6.223 s | 5.968 s | 4.1% | 5.935–6.668 s | 5.614–6.219 s |

Every measured command succeeded. Benchmark jobs were Cabal
`01M29ARSSM56YTZHYVEZD8QTYH` and Mac mini `01M29ARQKHFWHTKBSNTPQEKJ6V`.
The earlier Linux filesystem-boundary check passed in `01M29APV4C4SGX0JN32NCYBZ49`;
that job's separate benchmark portion failed because its fixture path was wrong,
then succeeded after correcting the path in the benchmark job above. The local profile includes syscall samples from concurrent
file workers, so their cumulative time is not wall time.

The measured tradeoff is visible in phase medians:

| Runner / arm | Admission to command | Command | After command to settlement |
| --- | --- | --- | --- |
| Cabal / baseline | 0.210 s | 2.990 s | 0.220 s |
| Cabal / cache | 0.403 s | 1.717 s | 0.351 s |
| Mac mini / baseline | 0.446 s | 1.339 s | 0.756 s |
| Mac mini / cache | 1.217 s | 0.616 s | 0.379 s |

Restoration adds preparation work but removes installation from the command.
On macOS, moving or recognizing cached directories also reduces settlement work.
Phase medians are calculated separately and need not sum to the total median;
client snapshot/submission overhead is outside these three daemon phases.

The previous three-sample comparison was about 15% slower. After the review fixes,
the expanded interleaved comparison improves on both target platforms for this
Fabric workflow. This is not a full Fabric-suite or concurrent-throughput
benchmark, and does not promise the same gain for every tool or filesystem.
Cross-filesystem copies can cost more than same-filesystem links or clones.

## Review regressions

- A linked reader cannot republish stale directory entries after a sibling
  changes shared bytes or permissions and replaces a different file.
- Private copies still detect in-place writes, and restoration records their
  actual metadata rather than assuming source metadata survived copying.
- An unsupported FIFO reports publication failure while releasing completed
  workspace membership. A subsequent job repairs and publishes successfully.
- A missing persistent cache directory is restored again; a missing published
  generation is a cold cache rather than a permanently broken binding.
- Concurrent admissions restore a missing persistent directory before any
  member can use it, while later members preserve a prepared member's deletion.
- Replacing an ephemeral cache's parent with a regular file reports the cache
  error but releases its holder and removes the workspace. Restore-stage cleanup
  still rejects symlink parents without touching their targets.
- The comparison receipt is saved before exposing a restored directory, and
  interrupted stage cleanup is specific to the job's holder.
- Idle orphan generations are reclaimed without evicting the current cache;
  held trees and dry runs remain protected. Dry-run budget accounting excludes
  the same orphan bytes that actual cleanup removes before choosing evictions.
- Actual file-descriptor exhaustion in an isolated child process does not cause
  GC to delete a healthy cache.
- Linux restores between the normal temporary filesystem and `/dev/shm` fall
  back to private copies, preserve comparison metadata, and publish changes back.
- Cold caches on separate Linux filesystems publish their first installed files
  from ephemeral and persistent workspaces. Later persistent in-place writes
  remain detectable. Older empty linked baselines exercise publication's actual
  hardlink-failure fallback and retain their atomic-replacement contract.

The filesystem tests simulate missing metadata targets and interrupted staging;
they do not constitute an actual power-cut experiment. Mutable cache bytes remain
nontransactional.

## Contract boundaries

The engine preserves directories; it does not understand language environments.
Users and tools own cache placement, invalidation, concurrency discipline, and
absolute-path compatibility. Files may share inodes where hardlinks are used,
so failed jobs do not imply rollback of in-place file writes. Concurrent directory-layout publications
use last-completed-publication wins, with no merge between workspaces or
atomic publication across multiple named caches.

Relative dependency links work when all required relative directories are
preserved. Absolute references remain absolute. This change does not certify
portable Python console scripts, CMake build trees, all language/tool versions,
or every package-manager layout. Those limits are documented in
[NAMED_CACHES.md](NAMED_CACHES.md).
