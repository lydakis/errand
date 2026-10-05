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
maximum, the client's CPU per command (for the watch, its process tree's CPU,
including the Git helpers it has reaped), and the daemon's CPU when it is
isolated. Jobs run cold first, then warm, so cold
and warm are separate rows. Configured peers run with a client config holding
only their entries from the user's, so personal environment, forwards, caches,
artifacts and `apply_on_success` do not change the timed commands. The timed
commands and fixture commits run without the user's global and system Git
configuration.

A burst ends at the first silence longer than twice the slowest push seen so
far: the slowest single save, the wait for the burst's first receipt, or the
longest gap between its receipts, and at least one second. A push that lands
after that fails the run instead of being left out of the row.

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

### Native machines and laptop to runner, `154c370`

Five setups, each from `scripts/benchmark_loop.py --files 1000 --files 10000`
with the default samples (5 per job case, 30 saves):

- **MacBook**, **Mac mini**, **Cabal**: `--isolated` on each machine, so
  client and daemon share it. MacBook Pro (MacBookPro18,4), Apple M1 Max,
  32 GB, APFS, in interactive use during the run (load average about 8).
  Mac mini (Mac18,5), Apple M6, 12 cores, 16 GB, APFS. Cabal, Intel Core
  i7-4578U (2 cores, 4 threads), 15 GB, Btrfs (zstd:3) on dm-crypt, idle.
- **Laptop → Cabal**, **Laptop → Mac mini**: the client on the MacBook, a
  scratch daemon of this build on each runner over the tailnet (`transport =
  "tailscale"` on port 7543, beside the installed runner), with direct tailnet
  connections on the same LAN. Visible times on Cabal come from `--observe`
  over SSH; the Mac mini accepts no SSH key from the laptop, so its saves
  report receipts only.

The Mac mini's isolated run used an empty `HOME`, because its Git config
signs commits with a passphrase-protected key that the fixture commit cannot
use. The binaries were built on the MacBook (`CGO_ENABLED=0`, cross-compiled
for Cabal). Summary data:
[`benchmarks/2026-10-05-scoreboard-native.json`](benchmarks/2026-10-05-scoreboard-native.json).

Median / p95; rows measured once show one value.

1K files:

| Case | MacBook | Mac mini | Cabal | Laptop → Cabal | Laptop → Mac mini |
|---|---:|---:|---:|---:|---:|
| workspace-create | 1.49 s | 767 ms | 2.42 s | 2.04 s | 1.56 s |
| watch-start | 939 ms | 520 ms | 686 ms | 1.38 s | 1.22 s |
| save, visible | 189 / 208 ms | 135 / 152 ms | 134 / 219 ms | 228 / 296 ms | – |
| save, receipt | 319 / 376 ms | 204 / 224 ms | 197 / 322 ms | 292 / 411 ms | 370 / 460 ms |
| burst, last save visible / last receipt | 501 / 608 ms | 946 ms / 1.02 s | 381 / 442 ms | 502 / 569 ms | – / 707 ms |
| job-cold | 1.90 s | 581 ms | 715 ms | 1.51 s | 1.31 s |
| job-warm | 1.54 / 1.55 s | 503 / 514 ms | 677 / 704 ms | 1.21 / 1.36 s | 1.05 / 1.14 s |
| job-edit | 1.50 / 1.56 s | 501 / 515 ms | 694 / 839 ms | 1.18 / 1.49 s | 946 / 981 ms |
| first-output, `ready` printed | 989 ms | 362 ms | 547 ms | 1.25 s | 713 ms |
| first-output, exit | 1.55 / 1.57 s | 506 / 517 ms | 651 / 999 ms | 1.39 / 2.04 s | 920 ms / 1.01 s |
| apply | 1.68 / 1.77 s | 590 / 596 ms | 799 ms / 1.09 s | 1.40 / 1.53 s | 1.27 / 1.35 s |
| detach-submit | 656 / 731 ms | 299 / 306 ms | 524 / 852 ms | 982 ms / 1.04 s | 816 / 849 ms |
| detach-attach | 847 / 867 ms | 200 / 214 ms | 108 / 122 ms | 124 / 180 ms | 231 / 320 ms |
| fetch-apply | 127 / 138 ms | 67 / 69 ms | 82 / 83 ms | 230 / 259 ms | 357 / 422 ms |

10K files:

| Case | MacBook | Mac mini | Cabal | Laptop → Cabal | Laptop → Mac mini |
|---|---:|---:|---:|---:|---:|
| workspace-create | 11.6 s | 5.48 s | 16.0 s | 14.8 s | 9.70 s |
| watch-start | 4.20 s | 1.99 s | 6.49 s | 6.95 s | 2.96 s |
| save, visible | 195 / 247 ms | 153 / 163 ms | 155 / 358 ms | 281 / 379 ms | – |
| save, receipt | 324 / 366 ms | 229 / 239 ms | 239 / 453 ms | 359 / 526 ms | 370 / 469 ms |
| burst, last save visible / last receipt | 541 / 694 ms | 1.06 / 1.13 s | 558 / 641 ms | 1.13 / 1.23 s | – / 837 ms |
| job-cold | 10.1 s | 3.96 s | 8.34 s | 8.50 s | 4.95 s |
| job-warm | 7.53 / 8.61 s | 3.08 / 3.15 s | 6.78 / 7.54 s | 7.88 / 9.17 s | 4.15 / 5.14 s |
| job-edit | 7.37 / 7.61 s | 3.14 / 3.24 s | 6.94 / 7.73 s | 7.87 / 8.91 s | 4.09 / 4.21 s |
| first-output, `ready` printed | 5.21 s | 2.17 s | 5.75 s | 6.88 s | 3.07 s |
| first-output, exit | 7.47 / 7.63 s | 3.06 / 3.12 s | 6.57 / 6.95 s | 7.72 / 8.55 s | 3.95 / 4.67 s |
| apply | 7.47 / 8.25 s | 3.21 / 3.66 s | 7.01 / 7.63 s | 7.98 / 8.91 s | 4.30 / 5.41 s |
| detach-submit | 4.78 / 5.08 s | 2.16 / 2.22 s | 6.34 / 8.67 s | 6.48 / 7.79 s | 3.13 / 3.50 s |
| detach-attach | 2.69 / 2.94 s | 963 / 977 ms | 845 / 880 ms | 821 / 924 ms | 903 ms / 1.04 s |
| fetch-apply | 137 / 140 ms | 72 / 83 ms | 76 / 211 ms | 221 / 705 ms | 445 / 465 ms |

A warm 10K job used 5.3 s of daemon CPU on the Mac mini, 4.0 s on Cabal and
12.8 s on the busy MacBook, and 1.0–2.2 s in the client. From the laptop, the
client alone used 2.1–2.2 s of CPU for a warm 10K job, against 4.1 s of wall
time to the Mac mini. A watch save used 30–70 ms of daemon CPU.

What the native rows show:

- Jobs are still where users wait. From the laptop, a job that does nothing
  takes 4.1 s to exit on the Mac mini and 7.9 s on Cabal at 10K files; a save
  is visible on Cabal in 281 ms and acknowledged in 359 ms.
- On APFS and Btrfs a warm job is faster than the single cold sample, unlike
  ext4: at 10K, 3.1 against 4.0 s on the Mac mini, 6.8 against 8.3 s on
  Cabal and 7.5 against 10.1 s on the MacBook. A warm no-op job still costs
  74–81% of a cold one.
- The runner's hardware matters more than the network for jobs. The same
  laptop runs a 10K no-op job in 4.1 s on the Mac mini and 7.9 s on Cabal;
  Cabal's isolated rows are close to its laptop rows.
- From the laptop a save is acknowledged in 290–370 ms, within about 50 ms
  of the MacBook's own isolated rows, so the source side sets most of a
  save's time.
  The network raises `fetch --apply` from 70–140 ms to 220–450 ms.
- Bursts are slower on macOS sources. The last save of a burst is visible
  after 0.5 s on the MacBook and about 1 s on the Mac mini, against 0.38 and
  0.56 s on Cabal and 0.15 and 0.23 s on the Linux cloud host.
- `-d` returns after 75–82% of a no-op job from the laptop (3.1 of 4.1 s to
  the Mac mini and 6.5 of 7.9 s to Cabal at 10K), as on the cloud host.

### Linux cloud host, `154c370`

Client and isolated daemon on one 4-CPU Linux VM with ext4, which cannot clone
files. Creating a file there takes about 200 µs, roughly ten times a laptop
SSD, so job rows overstate what Cabal or a Mac would show. The native and
laptop-to-runner rows above are the ones to use for Macs and Cabal. Summary
data:
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
  On APFS and Btrfs warm jobs are faster than cold ones (see the native rows).
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
