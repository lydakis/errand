# Deferred expiry reconciliation for watch, 2026-09-25

A watch preparation older than 30 s forced full selection before the next edit
could be delivered. So any edit after a pause paid a whole-tree rescan, plus
full Git enumeration for Git-selected sources. That rescan is the watch's
safety net for lost native events on files that received no hint. The
[realistic baseline](WATCH_REALISTIC_BASELINE.md) measured the explicit case
at 400–490 ms against a 307 ms fast path at 10K files.

## Change (`ed21cad`)

When an expired preparation still has verifying selection evidence and only
content hints for known files, the watch refreshes and delivers those files
first. It then records the full reconciliation as owed and schedules the
immediately following cycle, which runs full selection with the usual hash
reuse. That cycle pushes anything a lost event missed, or ends as a silent
"unchanged" pass. Expired cycles with no hints, or with structural hints,
still go straight to full selection. Trace reasons are `expired-deferred` and
`expired-followup`.

`TestWatchPrepareDeliversHintsBeforeExpiredReconciliation` edits one hinted
file and one unhinted file (a simulated lost event). It checks that the first
cycle is incremental and delivers the hinted edit, that a follow-up is
scheduled, and that the follow-up's full reconciliation finds the unhinted edit.

The safety net's latency changes: a change hidden by a lost event now arrives
one cycle after the next hinted edit instead of with it. With no edits at
all, neither version scans (no idle timer).

## Results

Watch matrix at 10K files, with a 31 s pause before each of five measured
saves. Three rounds, back-to-back pairs with alternating order, against
`193b592`. Median visible delivery in ms:

| Case | Baseline | Candidate | Paired ratio (range) | Candidate faster |
|---|---:|---:|---:|---:|
| explicit-inplace-edit, after 31 s idle | 263 | 155 | 0.563 (0.551–0.619) | 3/3 |
| git-tracked-inplace-edit, after 31 s idle | 495 | 174 | 0.346 (0.341–0.383) | 3/3 |

Every measured save produced exactly one `expired-deferred` preparation followed
by one `expired-followup` full preparation. An edit after a pause now takes as
long as an edit without one: 154 and 173 ms in the no-pause runs of the
[staging report](WATCH_STAGING.md).

Raw reports: [benchmarks/2026-09-25-deferred-expiry-linux.json](benchmarks/2026-09-25-deferred-expiry-linux.json).

## Not covered

A second save that lands while the follow-up full cycle runs waits for that
cycle (~115 ms explicit, ~330 ms Git at 10K) before its own delivery. That
cost moved from the first edit to a possible second edit; it was not added.
This run does not measure it.
