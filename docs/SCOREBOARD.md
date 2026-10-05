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

Build the commit being scored; the client and every daemon it talks to must
run that build. Generated reports stay under ignored `dist/` because they
contain peer facts and job handles.

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

To score a build without touching a runner's installed daemon and its
workspaces, start a scratch daemon of the build beside it, with its own state
and socket, and point a scratch client config at it. Over SSH, on the runner:

```sh
mkdir -p ~/errand-score && cp errand-score ~/errand-score/errand
cat > ~/errand-score/errandd.toml <<CONFIG
transport = "ssh"
state_dir = "$HOME/errand-score/state"
socket = "$HOME/errand-score/errand.sock"
CONFIG
~/errand-score/errand serve --config ~/errand-score/errandd.toml
```

and on the laptop, with `XDG_CONFIG_HOME` pointing at a scratch directory whose
`errand/config.toml` names it:

```toml
[peers.score-cabal]
ssh = "cabal"
remote_command = "/home/you/errand-score/errand"
remote_socket = "/home/you/errand-score/errand.sock"
```

then pass `--on score-cabal --observe score-cabal=cabal:~/errand-score/state`.
An SSH peer starts the remote command for each request over a shared SSH
connection, a cost a tailnet peer does not pay, so record which transport a
row used.

`--files 100000` adds the large checkout; on a slow disk its cold job and
workspace creation take minutes, so raise `--timeout` if a command gets near
600 s. The harness refuses a runner with active jobs at the start, but nothing
reserves it, so keep builds and other benchmarks off the machines while it
runs. Each run leaves its finished jobs on the runner for normal retention and
removes the workspace it created.

## Current main

### Linux cloud host, `154c370`

Client and isolated daemon on one 4-CPU Linux VM with ext4, which cannot clone
files. Creating a file there takes about 200 µs, roughly ten times a laptop
SSD, so job rows overstate what Cabal or a Mac would show. Native and
laptop-to-runner rows will replace these. Summary data:
[`benchmarks/2026-10-05-scoreboard-linux.json`](benchmarks/2026-10-05-scoreboard-linux.json).

Median / p95; rows measured once show one value.

| Case | 1K files | 10K files |
|---|---:|---:|
| workspace-create | 1.3 s | 13.1 s |
| watch-start | 0.42 s | 2.6 s |
| save, visible | 34 / 42 ms | 55 / 90 ms |
| save, receipt | 45 / 55 ms | 78 / 115 ms |
| burst, last save visible / last receipt | 149 / 162 ms | 226 / 247 ms |
| job-cold | 0.60 s | 6.8 s |
| job-warm | 1.60 / 1.85 s | 9.4 / 11.9 s |
| job-edit | 1.81 / 1.87 s | 9.6 / 10.2 s |
| first-output, `ready` printed | 1.43 s | 8.1 s |
| first-output, exit | 1.58 / 1.75 s | 9.9 / 10.1 s |
| apply | 1.65 / 1.87 s | 9.8 / 12.6 s |
| detach-submit | 1.58 / 1.71 s | 8.1 / 9.8 s |
| detach-attach | 159 / 185 ms | 1.68 / 1.92 s |
| fetch-apply | 28 / 37 ms | 28 / 39 ms |

A warm job used 3.0 s of daemon CPU at 1K and 17 s at 10K, against 0.2 s and
0.9 s in the client. A watch save used about 20 and 60 ms of daemon CPU.

At 100K files, measured with `scripts/benchmark_watch.py` on the same build
(Git selection, 30 saves): workspace creation 95 s, watch start 25 s, a save
visible in 210 / 382 ms (one took 3.5 s) with its receipt at 358 / 570 ms. A
20-save burst took 5.0 s to show its last save, in a single receipt.

What this shows on this host:

- Jobs are where users wait. A job that does nothing takes 1.6 s at 1K files
  and over 9 s at 10K before it exits; a save shows on the runner in 34 and
  55 ms.
- Every job, cold or warm, copies the whole workspace a second time as its
  change base, and that copy was 75–80% of the daemon's CPU in profiles of
  both. On a filesystem that cannot clone, each file of it is first created
  for a clone that fails, removed, and created again; skipping the clone after
  the first failure cut a 10K warm job from 8.5 to 6.5 s in a prototype.
  Whether APFS and Btrfs runners pay the same is what the native rows will show.
- Warm jobs are somewhat slower than cold ones. The single cold samples above
  are faster than repeated runs: in interleaved cold/warm pairs on a fresh
  daemon, cold took 0.92 s and warm 1.24 s at 1K (median of 5, warm slower in
  4), and 7.6 and 8.9 s at 10K (median of 3, warm slower in 2). Run-to-run
  spread on this host is wide (0.65–1.6 s at 1K).
- A detached submit returns after most of the staging (8.1 of about 9.8 s at
  10K), so `-d` saves little.
- `fetch --apply` of a finished job is flat at about 28 ms.
- At 100K files a burst of saves costs about 25 times a single save. The
  watch trace (`ERRAND_TRACE_WATCH=1`) shows why: a save landed while the
  previous push was still reading that file, the push stopped with "source
  changed", and the watch then dropped every cached hash and rescanned and
  rehashed all 100K files (4.6 s) instead of the one file that changed.
