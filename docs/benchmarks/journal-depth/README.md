# Fixed-depth journal experiment evidence

[Review corrections and separate CPU/heap profiles](review-followup/README.md)
are retained separately. The original native campaign files below remain frozen.

Decision, protocol and interpretation: [JOURNAL_DEPTH_PROFILE.md](../../JOURNAL_DEPTH_PROFILE.md).
All paired medians, ranges, wins and interference treatments: [RESULTS.md](RESULTS.md).

Measured 2026-09-16 on native APFS (Mac mini, Darwin arm64) and Btrfs (Cabal,
Linux amd64), Go 1.27.1, GOMAXPROCS=2. The Btrfs mount uses compress=zstd:3.
Baseline commit: `9156978a9138e7dbb41eeb67138e33b04160d9ba`.
This compares three experimental modes of the same candidate binary, not a
before/after production release. Each host retains its exact candidate inputs.

- `apfs/job.txt`, `btrfs/job.txt`: successful primary Errand job handles.
- `HOST/report.json`: all fixed-depth/byte-limit samples, cycle aggregates and
  steps, interference treatments, separate profile receipts, summaries and input
  identities. Exactly 378 samples (including 18 cycle aggregates), 594 cycle
  steps, 36 interference treatments and 36 profiled samples per primary report.
- `HOST/inputs.tar.gz`: exact measured Go/module/Python/workflow sources. Both
  hosts have the same Go digest and script inventory. Archive and binary hashes
  differ legitimately across hosts; the reports record each exact value.
- `HOST/raw-logs.tar.gz`: individual JSON probe receipts, history setup and
  synchronization logs. No fixture contents are needed to regenerate inputs.
- `HOST/profiles.tar.gz`: 36 raw CPU and 36 raw heap profiles. Each named scenario
  and variant has three fresh-process repetitions. These are diagnostic samples,
  not additions to timing comparisons.
- `HOST/profiles/*-top.txt`: pprof CPU cumulative and allocation-space reports,
  each aggregating the three profiles for one scenario/variant.
  `btrfs/profiles/byte-append-expanded-cpu-top.txt` is an additional local pprof
  rendering of those same retained CPU profiles with nodecount=150, to expose
  hierarchy-validation costs below the native top-45 cutoff. No rerun was used.
- Native `tests.txt`, `race.txt`, `vet.txt`, `python-tests.txt`, toolchain and
  mount/filesystem logs accompany each primary report. Go/race/vet pass; Python
  runs 70 passing tests. Some unit tests intentionally print failed fake jobs;
  unittest's final OK and campaign completion establish their outcome.
- `HOST/interference/`: separate 36-sample incompressible-writer follow-up with
  exact inputs, its script and chunk hashes, job handle, toolchain/mount data and
  individual logs. Never pool these later samples with primary timing rounds.

Both primary reports and supplemental reports were independently checked after
fetch: successful status, exact counts and matching roots, recomputed paired
summaries, exact 33-step cycle sums and source/archive identities. The working
Go/Python inputs matched the archived inputs at the original handoff. Source identity is
`35ae2096ae3740ec2e1863b43478e807b2d94eab426a4b068fa867a4f83e0c43`.
The primary campaign does not import the supplemental script; that script was
added afterward and is included in each supplemental archive instead.

`SHA256SUMS` covers every retained file except itself. Verify from this directory:

```sh
shasum -a 256 -c SHA256SUMS
```

For further profile analysis, unpack `profiles.tar.gz` to a scratch directory and
pass the three corresponding `.cpu` files to `go tool pprof -top -cum` (or `.heap`
files with `-top -alloc_space`). Raw profiles contain symbol mappings. No fixture,
runner or new benchmark run is needed. Source-level listings require the matching
archived source and binary; aggregate top reports do not.

## Preflight notes

An initial remote preflight (`mac-mini/01M2MAGEBE6D0HS1T0A9W9BMWM`) stopped before
measurement because Errand snapshots omit `.git`; querying `git rev-parse HEAD`
was invalid. The driver now records the baseline explicitly and archives exact
inputs. This failed preflight supplies no performance samples.

A complete 100-file smoke run (`mac-mini/01M2MAHPZJNGXKB6VVAYMJ6VGC`) verified the
harness before the final independently balanced depth-order schedule. It is not
part of the retained performance evidence. The final schedule is unit-tested and
used by both retained native campaigns. Native validation repeated all tests.
