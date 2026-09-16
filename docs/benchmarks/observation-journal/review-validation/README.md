# Review follow-up validation

These checks validate the post-measurement tests and harness changes. They do not
add timing samples or replace the original native campaign logs.

- Mac mini and Cabal: `go test ./experiments/snapshotcheckpoint/...`,
  `go test -race ./experiments/snapshotcheckpoint/...`, and
  `go vet ./experiments/snapshotcheckpoint/...` all exited 0. The logs and job
  handles are retained in the host directories. Mac mini used
  `DEVELOPER_DIR=/Library/Developer/CommandLineTools`.
- Local: `python3 -m unittest discover -s scripts` exited 0 with 67 tests
  (8.228 seconds). This is a recorded command result, not a retained raw log.
- Both original reports were reanalyzed without new performance measurements.
  Their `review-summary.json` files reproduce both paired comparisons and show
  cumulative ordinary history depths 0–5 on both hosts.

`../review-inputs.json` identifies the reviewed Go and Python sources. No non-test
Go source differs from the measured candidate archive. `../review-followup.patch`
is cumulative from that archive and includes test consolidation, the equal-size
regression, stricter receipt checks, unique setup logs and paired analysis.
