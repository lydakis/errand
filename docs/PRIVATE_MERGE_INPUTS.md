# Accessible private merge inputs

Baseline: `3a0cf85` (`Copy verified merge inputs directly and diagnose transfer latency`).

This experiment targets the permission work remaining after the direct-copy
slice. It does not change the selected snapshot representation or transfer
protocol. Workspace and job initialization remains the next integration slice.

## Change and guarantees

Previously, apply copied verified base/remote inputs, restored their manifest
permissions, then walked both trees to make them readable again. That last walk
built permission and identity maps whose only remaining consumer closed the
root handles. The merge already obtains logical permissions from its manifests.

Private merge inputs now keep the owner's initial read/write permissions on
files and access permissions on directories. The shared copier skips destination
permission restoration and its directory bookkeeping for these disposable
inputs. There is no second accessibility walk or returned accessibility handle.
Logical output modes still come from the manifests during merge.

Durable staging selects manifest permissions explicitly and retains its member
flushes, directory restoration and publication barrier. Source identity/type/mode
checks, full body hashing, bounded copy workers and restoration of temporarily
widened source access remain shared. Copies remain independent of later staging
edits. The apply journal still synchronizes chosen output before installation.
Live-source capture and export are unchanged.

Coverage retains same-size corruption rejection, restricted source restoration,
copy independence and existing staging fault tests. An apply-level case checks
nested read-only directories, executable/read-only/unreadable files, symlinks,
file bodies and final logical modes. A restricted-input three-way case checks
independent local/remote text edits, local mode preservation and restoration of
the staged base/remote files' unreadable modes.

## Measurement contract

Five alternating baseline/candidate pairs on each native host, with identical
benchmark Go sources, `GOMAXPROCS=2` and `CGO_ENABLED=0`. Every sample has three
iterations. Both variants run the complete Go test suite, vet and race checks
before timing. Compilation, source fixture setup, copied-body verification and
cleanup are excluded from isolated preparation timing. Preparation includes both
base and remote copies and closing their root handles.

The isolated fixtures contain:

- Small: one 256-byte file per tree.
- Batch: 128 files of 4 KiB, grouped into four directories per tree.
- Nested: 1,024 files of 256 bytes, grouped into 32 directories per tree.
- Restricted: 128 files of 256 bytes with file/directory manifest modes zero.
- Large: one 8 MiB file per tree.

Isolated bodies use repeated A/B bytes, so their large-file result is not an
incompressible-storage claim. Complete batch/large fetch uses the existing
deterministic random-body workloads as an additional check.

Complete operations include push, watch, both persistent and ephemeral fetch,
batch fetch and large fetch. Their native-host loopback HTTP benchmarks include
transfer/application work; they do not measure cross-host network latency or
application hot-reload readiness. Timed operation costs are distinct from the
whole benchmark process, which includes fixture setup and cleanup.

The driver records source, production, benchmark and binary hashes, alternating
order, every raw sample, host load, phase logs, actual fixture filesystem and
mount options. Fixtures live in the native workspace filesystem, including on
Cabal, whose ordinary temporary directory can be memory-backed. A failed or
interrupted campaign retains its partial report and failure location.

The benchmark-only adapter normalizes the baseline's access-handle return to the
candidate's error-only API outside timing. It adds no production compatibility
path, and both versions run the same preparation call shape inside timing.

The initial adoption gate required consistent preparation improvement, passing correctness
checks and no unresolved complete-operation regression. A lower median alone
does not establish a complete-operation improvement; paired directions and
spread must also be reported. These five pairs are a bounded screen, not a
precise tail-latency estimate. The results below do not satisfy the original
complete-operation gate. Following review, this slice is retained as a component
improvement with unresolved complete-operation effects explicitly accepted.

## Results

Both primary campaigns completed: 55 samples per variant per host. Both variants
passed `go test ./...`, `go vet ./...` and the driver's seven-package race suite
on both hosts. The ten local Python ordering/timeout/interruption/cleanup tests
also passed. Sources remained unchanged throughout timing.

After review, the restricted-input three-way test was added and the focused
materialization/three-way tests passed locally, both normally and with the race
detector. Comparing the current Go sources, module files and integration harness
against the frozen candidate found only the added test-file change. Production
and benchmark sources remain identical to the measured version; the full source
hash now differs. The frozen reports and their original validation remain intact.

Medians in milliseconds; counts show candidate-faster pairs out of five.
Each host uses its own committed-baseline measurements.

| Case | APFS baseline → candidate | Faster pairs | Btrfs baseline → candidate | Faster pairs |
|---|---:|---:|---:|---:|
| inputs-small | 1.603 → 0.832 (-48.1%) | 4/5 | 0.372 → 0.257 (-30.8%) | 5/5 |
| inputs-batch | 18.199 → 16.970 (-6.8%) | 4/5 | 17.293 → 14.326 (-17.2%) | 5/5 |
| inputs-nested | 129.507 → 108.694 (-16.1%) | 5/5 | 125.654 → 100.889 (-19.7%) | 5/5 |
| inputs-restricted | 31.273 → 21.816 (-30.2%) | 5/5 | 23.050 → 17.216 (-25.3%) | 5/5 |
| inputs-large | 57.596 → 50.579 (-12.2%) | 2/5 | 54.887 → 54.679 (-0.4%) | 3/5 |
| fetch-batch | 7703.241 → 7747.399 (+0.6%) | 4/5 | 6406.855 → 6148.711 (-4.0%) | 3/5 |
| fetch-large | 597.107 → 607.221 (+1.7%) | 0/5 | 862.369 → 804.507 (-6.7%) | 2/5 |
| fetch-false | 121.010 → 127.161 (+5.1%) | 1/5 | 91.891 → 94.227 (+2.5%) | 2/5 |
| fetch-true | 259.513 → 237.202 (-8.6%) | 4/5 | 236.498 → 200.359 (-15.3%) | 4/5 |
| watch | 366.100 → 373.437 (+2.0%) | 2/5 | 381.335 → 370.029 (-3.0%) | 3/5 |
| push | 526.088 → 517.396 (-1.7%) | 3/5 | 803.211 → 782.386 (-2.6%) | 3/5 |

Preparation improved in 38/40 small, batch, directory-tree and restricted pairs
across the two hosts. The 1,024-file and restricted cases improved in every pair.
The large-input case does not establish a gain: Btrfs is essentially unchanged,
and APFS improves in only two pairs despite a lower median. Removing per-entry
permission work helps many-file trees; it does not remove body copying/hashing.

Complete-operation results vary substantially. For example, Btrfs watch has one
499.5 ms candidate sample versus baseline samples of 370.7–384.4 ms; that sample
is slower across client, staging and apply phases. It is retained, not discarded.
A lower median is not enough to attribute overall gains to this patch.

The primary APFS run flags large fetch slower in all five pairs (about 6–27 ms),
and ephemeral fetch slower in four. A bounded follow-up checked those two
cases with the same sources, three iterations and five alternating pairs, starting
candidate-first. Its result is reported separately rather than pooled with the
primary campaign.

The paired direction reversed in the follow-up; the large-fetch median remained
0.3% slower:

| Follow-up case | APFS baseline → candidate median | Candidate-faster pairs |
|---|---:|---:|
| Large fetch | 639.517 → 641.521 ms (+0.3%) | 4/5 |
| Ephemeral fetch | 133.714 → 129.658 ms (-3.0%) | 4/5 |

**Decision: retain accessible private merge inputs.** The many-file preparation
gain is consistent on both native filesystems, and correctness/durability checks
pass. Complete-operation performance remains unresolved, including the possibility
of a small APFS regression. The follow-up's reversed paired direction does not
establish equivalence or erase the primary result. Retaining this component gain
accepts that uncertainty; fewer permission operations do not prove an overall
latency bound. There is no basis here to advertise faster overall push/fetch/watch
or a large-file speedup. A stronger overall claim needs a predefined acceptable
regression margin and paired measurements precise enough to assess it.

The complete-path benchmarks spend hundreds of milliseconds to seconds in
transfer, staging and application work. Eliminating a fraction of a millisecond
for a tiny merge input need not produce a measurable overall gain. The shared
initial-population path remains the next roadmap slice; this result does not
justify expanding the current patch into it.

Follow-up evidence: [report](benchmarks/private-merge-inputs/apfs-confirmation/report.json),
[summary](benchmarks/private-merge-inputs/apfs-confirmation/summary.json),
[logs](benchmarks/private-merge-inputs/apfs-confirmation/logs.tar.gz), and
[driver](benchmarks/private-merge-inputs/apfs-confirmation/driver.tar.gz).
The follow-up job was `mac-mini/01M2H827CAZGB8PMRV02W32VAK`.

Evidence: [summary](benchmarks/private-merge-inputs/summary.json),
[Btrfs report](benchmarks/private-merge-inputs/cabal/report.json),
[APFS report](benchmarks/private-merge-inputs/mac-mini/report.json),
[Btrfs logs](benchmarks/private-merge-inputs/cabal/logs.tar.gz),
[APFS logs](benchmarks/private-merge-inputs/mac-mini/logs.tar.gz), and
[frozen inputs](benchmarks/private-merge-inputs/inputs.tar.gz).

Btrfs fixtures used `compress=zstd:3` on Cabal's native home filesystem. APFS
fixtures used the Mac mini's native workspace volume. The reports retain exact
mounts, Go versions, source hashes, process durations and load observations.

Primary jobs: `cabal/01M2H5MYE68GMMQ1V8E301DJZR` and
`mac-mini/01M2H5MYFPZYP3YP1FS3K1CGEC`.

## Reproduction

From the candidate tree, provide a clean baseline checkout with the identical
`internal/changes/apply_materialize_benchmark_test.go` overlaid:

```sh
python3 scripts/benchmark_snapshot_integration.py \
  --baseline /path/to/baseline \
  --output /path/on/native-filesystem/fresh-results \
  --revision '3a0cf85 private accessible merge inputs' \
  --scope merge-inputs --rounds 5
```
