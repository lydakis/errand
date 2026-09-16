# Shared checkpoint builder comparison

This report retains the pre-review candidate and its measurements. The direct
strategy below unnecessarily constructed a prior snapshot during loading, so its
ranking is superseded by the [review follow-up](SNAPSHOT_CHECKPOINT_BUILDER_REVIEW.md).
The measured cache-miss gains do not depend on that cache-hit-only asymmetry.

## Scope

This slice compares the committed observation checkpoint (`5866129`) with source
observations collected by the ordinary snapshot builder. The checkpoint remains
an experiment. Production callers use the refactored ordinary builder but do not
load or publish disk checkpoints.

The in-memory representation remains the existing adaptive contiguous/Merkle
engine. Neither candidate persists derived tree nodes or changes the wire format.

## Changes under measurement

- Expand selected paths and ancestors once, then collect native observations in
  the same pass that constructs manifest entries and reads changed bodies. Reserve
  the known result capacity for observation builds (respecting the entry limit),
  and preserve sorted traversal order. Ordinary builds retain their existing
  growth policy after the regression investigation below; both paths avoid a
  redundant final sort.
- Reuse matching hashes after fresh native stat checks. Verify changed entries and
  directory identity/mode before publication; the caller still verifies selection.
- Stop collecting observations when the expanded selection exceeds the checkpoint
  entry ceiling. Prepare the ordinary manifest once. If checkpoint identity is
  unavailable, use the already selected paths instead of repeating selection.
- Validate and own loaded metadata through `manifest.New` once. The update variant
  carries that validated snapshot into index construction instead of validating
  and copying the same loaded entries again. Decoder slices remain separate from
  the immutable snapshot; there is no unchecked constructor.
- Compare reconstructing the prior index and applying edits (`shared-update`)
  with constructing the current index directly (`shared-current`). Both produce a
  ready adaptive index and the existing wire root.

The shared builder checks logical byte and entry limits before hashing, including
when reusing hashes. Its directory checks allow ignored sibling timestamp churn
while preserving native identity, type and mode. Cancellation, atomic replacement,
backdated edits, malformed checkpoints and source-selection checks remain covered.
The entry ceiling includes implicit ancestors. The 64 MiB checkpoint byte ceiling
still applies to the encoded payload; it is not a bound on peak allocation.

## Method

Each native campaign interleaves four variants: frozen ordinary cold preparation,
frozen checkpoint preparation, shared-update and shared-current. Each case has
eight rounds. Rotation plus reversal puts each variant in each execution position
twice. Every round checks identical wire roots and expected hash/reuse/write/cache
status counters. A separate eight-pair comparison checks the ordinary builder in
the frozen and candidate binaries.

Fixtures: 1,000 x 32-byte files; 10,000 x 4-KiB files; 1,000 x 64-KiB files;
50,000 x 128-byte files; and a Git-selected 10,000 x 4-KiB fixture. Files are spread
across directories of 100 entries. Explicit-ignore fixtures add an empty
`.errandignore`. Git fixtures have an initialized index and no `.errandignore`.
Cases cover missing checkpoints, unchanged restarts, one-file edits and batches
of 1,000 changed files. Miss means the advisory checkpoint is removed, not that
filesystem caches are cold. No transfer or application-readiness latency is timed.

The same per-host environment, source fixture and candidate binary apply to both
index strategies. The frozen archive and source digest are checked before its
binary is built. Reports include source/harness/binary identities, toolchain,
filesystem/mount metadata, every raw sample and execution position. `Total` is
measured inside Go and includes preparation plus wire hashing. Python process
polling time is not used for performance claims.

The shared pass combines scan and hashing work in `Phases.Hash`; its `Scan` value
is zero. Loaded-state validation moved into `Load`, so compare total time or
`Load + Index` when evaluating that handoff. Per-phase movement alone is not a
speedup, and the combined comparison does not isolate each change's contribution.

## Final results and decision

Keep the shared observation pass and validated loaded-state handoff. Keep
prior-state updates as the checkpoint default; direct construction has no
consistent advantage across these cases. Do not claim a universal speedup or
promote disk checkpoints into production callers from this experiment alone.

The clearest final gain is 50K initial population: 858.26 to 784.92 ms on Mac mini
and 1,249.63 to 1,076.59 ms on Cabal, with eight wins out of eight on both hosts.
Warm 50K runs are essentially flat. Small APFS results remain variable.

Final tables show median milliseconds. Ratios are medians of within-round ratios,
not ratios of the displayed medians. Each case has eight rounds. All 1,440 matrix
samples and 180 fixed-loop control samples passed root/counter and provenance
checks. Full ranges, phase medians and positions are in
[final-summary.json](benchmarks/checkpoint-builder/final-summary.json).

| Host | Fixture | Case | Frozen checkpoint | Shared update | Update / frozen | Wins | Shared current | Current / update |
|---|---|---|---:|---:|---:|---:|---:|---:|
| apfs | 1000-32-ignore | miss | 44.48 | 51.58 | 1.148 | 3/8 | 57.30 | 1.164 |
| apfs | 1000-32-ignore | unchanged | 44.84 | 42.57 | 0.997 | 4/8 | 54.59 | 1.051 |
| apfs | 1000-32-ignore | edit | 52.34 | 57.47 | 1.102 | 4/8 | 47.71 | 0.830 |
| apfs | 1000-32-ignore | batch | 48.10 | 55.71 | 0.953 | 5/8 | 40.22 | 0.859 |
| apfs | 10000-4096-ignore | miss | 213.95 | 207.37 | 0.966 | 6/8 | 186.49 | 0.953 |
| apfs | 10000-4096-ignore | unchanged | 84.80 | 70.29 | 0.907 | 7/8 | 95.21 | 1.287 |
| apfs | 10000-4096-ignore | edit | 83.11 | 91.27 | 1.109 | 2/8 | 91.10 | 1.012 |
| apfs | 10000-4096-ignore | batch | 100.49 | 101.30 | 0.978 | 4/8 | 111.16 | 1.005 |
| apfs | 1000-65536-ignore | miss | 72.86 | 66.97 | 0.979 | 4/8 | 80.61 | 1.097 |
| apfs | 1000-65536-ignore | unchanged | 33.96 | 31.77 | 0.989 | 4/8 | 37.78 | 1.148 |
| apfs | 1000-65536-ignore | edit | 31.51 | 35.92 | 1.037 | 2/8 | 34.54 | 0.901 |
| apfs | 1000-65536-ignore | batch | 79.41 | 86.02 | 1.201 | 3/8 | 88.24 | 1.014 |
| apfs | 50000-128-ignore | miss | 858.26 | 784.92 | 0.925 | 8/8 | 805.93 | 1.031 |
| apfs | 50000-128-ignore | unchanged | 267.19 | 268.13 | 0.996 | 5/8 | 251.38 | 0.987 |
| apfs | 50000-128-ignore | edit | 280.56 | 275.58 | 1.002 | 4/8 | 290.33 | 1.033 |
| apfs | 50000-128-ignore | batch | 289.19 | 288.07 | 0.997 | 4/8 | 297.87 | 1.018 |
| apfs | 10000-4096-git | miss | 399.45 | 361.49 | 0.899 | 6/8 | 397.03 | 1.087 |
| apfs | 10000-4096-git | unchanged | 291.53 | 263.41 | 0.897 | 5/8 | 288.11 | 1.069 |
| apfs | 10000-4096-git | edit | 267.43 | 253.31 | 0.962 | 5/8 | 265.82 | 1.063 |
| apfs | 10000-4096-git | batch | 317.58 | 292.82 | 0.960 | 6/8 | 306.48 | 1.009 |
| btrfs | 1000-32-ignore | miss | 26.38 | 24.11 | 0.909 | 8/8 | 24.10 | 1.005 |
| btrfs | 1000-32-ignore | unchanged | 13.87 | 13.78 | 0.992 | 4/8 | 13.93 | 1.011 |
| btrfs | 1000-32-ignore | edit | 16.17 | 16.19 | 1.000 | 4/8 | 16.34 | 1.007 |
| btrfs | 1000-32-ignore | batch | 30.82 | 28.90 | 0.946 | 7/8 | 27.72 | 0.956 |
| btrfs | 10000-4096-ignore | miss | 499.23 | 465.05 | 0.952 | 6/8 | 412.12 | 0.868 |
| btrfs | 10000-4096-ignore | unchanged | 115.38 | 118.69 | 1.004 | 4/8 | 116.73 | 0.971 |
| btrfs | 10000-4096-ignore | edit | 132.01 | 129.73 | 0.982 | 6/8 | 127.47 | 0.987 |
| btrfs | 10000-4096-ignore | batch | 165.97 | 165.61 | 1.058 | 3/8 | 165.22 | 0.986 |
| btrfs | 1000-65536-ignore | miss | 272.19 | 269.54 | 0.962 | 5/8 | 292.68 | 1.087 |
| btrfs | 1000-65536-ignore | unchanged | 13.93 | 13.68 | 0.987 | 5/8 | 13.63 | 0.992 |
| btrfs | 1000-65536-ignore | edit | 16.39 | 16.35 | 0.991 | 5/8 | 16.74 | 1.013 |
| btrfs | 1000-65536-ignore | batch | 264.70 | 265.84 | 1.019 | 4/8 | 248.58 | 0.966 |
| btrfs | 50000-128-ignore | miss | 1249.63 | 1076.59 | 0.857 | 8/8 | 1093.06 | 1.021 |
| btrfs | 50000-128-ignore | unchanged | 527.04 | 524.72 | 1.000 | 4/8 | 517.24 | 0.971 |
| btrfs | 50000-128-ignore | edit | 598.82 | 598.60 | 0.989 | 5/8 | 605.95 | 1.017 |
| btrfs | 50000-128-ignore | batch | 619.08 | 617.98 | 0.992 | 4/8 | 616.26 | 1.004 |
| btrfs | 10000-4096-git | miss | 698.33 | 665.97 | 0.907 | 6/8 | 688.73 | 1.028 |
| btrfs | 10000-4096-git | unchanged | 293.57 | 300.29 | 1.006 | 2/8 | 313.81 | 1.027 |
| btrfs | 10000-4096-git | edit | 317.59 | 315.60 | 1.026 | 3/8 | 325.07 | 1.039 |
| btrfs | 10000-4096-git | batch | 409.68 | 402.43 | 0.974 | 5/8 | 388.70 | 0.962 |

Both shared strategies execute the same miss path. Differences between their
miss timings are run/cache variability, not different initial-population code.

### Ordinary-builder controls and remaining limits

| Host | Fixture | Frozen cold ms | Candidate cold ms | Paired ratio | Candidate wins |
|---|---|---:|---:|---:|---:|
| apfs | 1000-32-ignore | 75.48 | 76.92 | 1.045 | 1/8 |
| apfs | 10000-4096-ignore | 194.96 | 191.43 | 0.998 | 5/8 |
| apfs | 1000-65536-ignore | 90.18 | 85.97 | 0.973 | 5/8 |
| apfs | 50000-128-ignore | 732.28 | 733.60 | 1.000 | 5/8 |
| apfs | 10000-4096-git | 420.03 | 368.98 | 0.932 | 8/8 |
| btrfs | 1000-32-ignore | 20.54 | 20.83 | 1.007 | 2/8 |
| btrfs | 10000-4096-ignore | 456.67 | 403.44 | 0.889 | 6/8 |
| btrfs | 1000-65536-ignore | 238.05 | 256.73 | 1.077 | 1/8 |
| btrfs | 50000-128-ignore | 918.33 | 921.19 | 1.001 | 4/8 |
| btrfs | 10000-4096-git | 743.83 | 799.42 | 1.076 | 2/8 |

The final same-executable 64-KiB control compares the fixed loop against two
identical old-loop labels over 30 balanced rounds. Its ratio uses the mean of
the two old-loop timings within each round:

| Host | Old A ms | Old B ms | Fixed shared ms | Paired ratio | Shared wins |
|---|---:|---:|---:|---:|---:|
| apfs | 72.19 | 78.11 | 83.96 | 1.045 | 11/30 |
| btrfs | 247.88 | 246.91 | 244.54 | 0.996 | 15/30 |

Cabal’s controlled loop comparison returns to parity after restoring ordinary
slice growth. However, the separate-binary matrix still reports 1.077 for its
64-KiB cold guard and 1.076 for its Git cold guard. The Mac same-executable
control is 1.045 while its corresponding separate-binary body guard is 0.973.
These differences are retained and have not been causally assigned to binary
layout, scheduling, GC or filesystem behavior. They prevent a blanket
regression-free claim. Stronger actual-caller/process controls belong before
broad production performance claims; the larger checkpoint-miss win does not
settle every cold-path effect.

## Cold-builder regression investigation

The first valid matrix and a 24-round A/A/B recheck found cold-builder slowdown
signals, particularly for 1,000 x 64-KiB files on Cabal. Those measurements are
retained separately in the [candidate comparison](benchmarks/checkpoint-builder/candidate-comparison.md)
and [recheck summary](benchmarks/checkpoint-builder/recheck-summary.json).

We compared source overlays rather than explaining the result from code shape.
Removing observation hooks did not remove the slowdown. A same-executable
comparison still found an 8.3% median paired slowdown on Cabal (26 of 30 rounds),
while the Mac mini result was approximately parity. The next same-executable
bisection restored the previous allocation/sorting behavior separately from
removing the hooks. Restoring allocation/sorting narrowed the Linux difference;
removing the hooks did not.

A final isolation retained the shared loop and removed only eager allocation.
Its paired ratio to the mean of two legacy-loop controls was 0.995, versus 1.041
for the eager-allocation shared loop in that campaign. All variants still had a
median of ten GC cycles. This supports preserving ordinary slice growth; it does
not establish a specific GC, allocator or hardware-cache explanation.

The final source therefore reserves manifest capacity only for observation
builds. Ordinary builds retain their existing growth policy. The redundant final
sort remains removed because traversal is already sorted. Final verification
repeats the same-executable control and the full workload matrix on both hosts.
The diagnostic overlays never entered production code.

## Production gates

Disk checkpoints remain gated on executable filesystem eligibility, controlled
cache ownership/lifetime, mount-alias handling, selection guards through freezing,
and end-to-end measurements across push, both fetch modes, watch restart, workspace
creation and ephemeral submission. Persisted derived-index restoration and
changed-record publication are later measured alternatives. Large-file deltas
remain roadmap step 4.

## Validation and retained inputs

The final source passed `go test ./...` and `go vet ./...` locally. Both final
native campaigns passed snapshot/manifest/checkpoint tests, race tests, vet and
all five Python harness tests. A full Linux suite/vet run also passed for the
preceding candidate; its output is retained separately and is not relabeled as
a full-suite run of the final allocation policy.

The initial exploratory campaign failed its final provenance check because it
counted the extracted baseline as candidate source. That guard was corrected and
covered by a regression test; those failed reports remain separate. The first
Mac setup attempt hit the Xcode Git license launcher. Using the already installed
Command Line Tools through a per-job `DEVELOPER_DIR` fixed it without accepting a
license or changing system configuration.

[final-inputs.json](benchmarks/checkpoint-builder/final-inputs.json) identifies the
clean final source (`180f6a412606723f1e57449b4de03ee3b57dde69f153f67d0f5e6acf63abf2d2`).
The fixed-loop control ran first and left three diagnostic Go overlay files in
its output directory. The matrix’s conservative whole-tree Go inventory includes
those files, although its explicit package build/test commands do not compile
them. [final-campaign-inputs.json](benchmarks/checkpoint-builder/final-campaign-inputs.json)
records their paths and hashes. Reconstructing the clean archive plus those three
files exactly reproduces the matrix digest
`e0f9a20093231a4fc8b7d3d0e59491a516ae7eb759a7b499f5ce1c45136cdc27`
on both hosts. Both start/end inventories match; source evidence is not rewritten.
The Python harness inventories also match the frozen inputs. Final documentation
was completed after measurement.

| Campaign | Mac mini job | Cabal job |
|---|---|---|
| Final control and matrix | `01M2KKC3N3HZRJ3GSTVM9BRKF5` | `01M2KKC3MRGQT3AD6JW20QKA9D` |
| Valid candidate before allocation fix | `01M2KJ1CJK5FNB8729YHPMEEHA` | `01M2KJ1CJKFRT4C639EGT1KP3V` |
| Separate-binary A/A/B recheck | `01M2KJGFP4T98KC375S95FWKH8` | `01M2KJGFNFER1FF2ZWBYZSEYW0` |
| Source-overlay bisection | `01M2KJTB0XD19CJ31RZNAEFYX1` | `01M2KJR3J7ZXCRMK2HB24AB481` |
| Same-executable comparison before fix | `01M2KJZ2YECA9V9AKR1VB6PJQM` | `01M2KJZ2YEBW0SFESPXHHHW8XB` |
| Same-executable bisection | n/a | `01M2KK2PJNK3RKEBBCX33385WE` |
| Growth-only isolation | n/a | `01M2KK6XW3E9VESEHYCEJD59GD` |

The [evidence README](benchmarks/checkpoint-builder/README.md) explains reproduction
and which frozen revision each diagnostic script expects. Tests retain distinct
contracts: shared limits and cancellation, unchanged reuse, structural changes
across the adaptive threshold, cache recovery/publication and selection races.
