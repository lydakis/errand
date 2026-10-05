# Whole-loop scoreboard

One place for what a user waits on across the whole loop, measured on one
commit of `main` the same way on every machine. A component change is judged by
whether it moves these rows, not by its own microbenchmark.

## What is measured

`scripts/benchmark_loop.py` runs the CLI as a user would, from a generated Git
checkout of small files (100 per directory, every file tracked), against
configured peers or an isolated local daemon. For each checkout size:

| Case | Timed until |
|---|---|
| workspace-create | `workspaces create` returns |
| watch-start | `push --watch` reports its first receipt |
| save | one-file save: the runner's copy shows it (visible), and the watch reports its receipt |
| burst | 20 saves 5 ms apart: the last one is visible, and the last receipt (the backlog is the gap) |
| job-cold | first `-- true` from new content; everything ships |
| job-warm | `-- true` again; nothing ships |
| job-edit | `-- true` after a one-file edit; one file ships |
| first-output | `-- echo ready`: until `ready` reaches stdout, and until exit |
| apply | `--apply -- sh -c 'echo N > out.txt'` until exit, with `out.txt` applied locally |
| detach-submit, detach-attach | `-d -- true` until the handle returns, then `attach` until it exits |
| fetch-apply | `fetch --apply` of a finished detached job that wrote `out.txt` |

Saves run with 0.5 s of idle before each one. A sample counts only when the
CLI reported the expected shipping (all, none or one file), the job's terminal
receipt confirms success, and applied files hold the job's contents; those
checks run outside the timer. Each row reports median, p95, minimum and
maximum, the client's CPU per command (the watch process's CPU per save), and
the daemon's CPU when it is isolated. Jobs run cold first, then warm, so cold
and warm are separate rows.

Visible times on a configured peer need `--observe PEER=SSH_HOST[:STATE_DIR]`:
a small Python poller runs on the runner over SSH and reads the workspace's
copy of the edited file under the runner's state directory (default
`~/.errand`). Its report crosses the network once, so a visible time includes
one hop back to the client. Without `--observe`, saves report receipts only.
With `--isolated` the poller runs locally.

`scripts/benchmark_watch.py` stays the detailed same-host save benchmark: it
adds the one-shot `push`, rsync and Mutagen comparisons, idle CPU and
whole-tree verification.

## Running it

Build the commit being scored and install the same build on every runner;
the client and daemons must match. Generated reports stay under ignored
`dist/` because they contain peer facts and job handles.

```sh
git rev-parse --short HEAD
CGO_ENABLED=0 go build -trimpath -o dist/errand-score ./cmd/errand

# Same machine, on each of the laptop, Cabal and the Mac mini:
python3 scripts/benchmark_loop.py --binary dist/errand-score --isolated \
  --files 1000 --files 10000 --output dist/score/$(hostname -s)-local

# Laptop to runners, with the runners idle:
python3 scripts/benchmark_loop.py --binary dist/errand-score \
  --on cabal --observe cabal=cabal --on mac-mini --observe mac-mini=mac-mini \
  --files 1000 --files 10000 --output dist/score/laptop-to-runners
```

`--files 100000` adds the large checkout; on a slow disk its cold job and
workspace creation take minutes, so raise `--timeout` if a command gets near
600 s. The harness refuses a runner with active jobs at the start, but nothing
reserves it, so keep builds and other benchmarks off the machines while it
runs. Each run leaves its finished jobs on the runner for normal retention and
removes the workspace it created.

## Current main

Pending.
