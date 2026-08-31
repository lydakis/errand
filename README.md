# errand

Run the thing you would have run locally, on another machine you own, and
get the result back: same command, same working tree, logs streaming to
your terminal, real exit code. The heat, watts, and minutes are spent
elsewhere.

> **Status: milestone 3.5.** The transactional core works end to end over a
> tailnet, including durable detached jobs, restart reconciliation, and
> content-addressed snapshot reuse. The v0 design is frozen in
> [docs/DESIGN.md](docs/DESIGN.md).

## Quickstart

```
# on a runner (Linux box on your tailnet)
errand serve                  # config: ~/.config/errand/errandd.toml

# on a caller
errand info                   # measured facts: arch, kvm, tools
errand -- python3 -m unittest # runs your working tree over there
errand --no-snapshot -- uname -a # runs in a fresh empty remote workspace
# Ctrl-D detaches without stopping the remote job; Ctrl-C interrupts it
job=$(errand --detach -- make build)
errand ps                     # list your jobs across configured peers
errand ps --json              # the same receipt-backed data for clients
errand attach "$job"          # replay logs and follow to completion
errand gc jobs --dry-run --older-than 30d --keep 500
```

Runner config authorizes callers by tailnet identity (whois): an ACL app
capability or a local `allow_users` list. Destination-scoped capability
checks require Tailscale 1.100 or newer. No keys, no credentials stored.
Runners execute one job at a time by default and queue up to eight more. Set
`max_jobs` and `max_queued` in `errandd.toml` to change those limits;
`max_queued = 0` disables queueing.

Run errand from a Git worktree for automatic snapshot selection. A non-Git
directory requires an explicit `.errandignore` policy or `--include-all`.
Errand always refuses a filesystem root, and snapshotting your home directory
requires `--include-all`. The client prints the selected file count and byte
total before remote admission. Use `--no-snapshot` when a command needs no
local files; Errand then skips local inspection and runs it in an empty remote
workspace. Because that workspace is empty, `--workdir` may only name its root.

For a workspace containing several repositories, place this marker and an
explicit `.errandignore` at the shared root:

```toml
# .errand.toml
[workspace]
root = true
```

Errand discovers the nearest marked ancestor, snapshots from there, and runs
the command in the caller's relative directory. `--workspace-root PATH`
selects the same boundary explicitly. Automatic discovery trusts only a marker
and directory owned by the caller in a directory that is not group- or
world-writable; use the explicit flag for intentionally shared workspaces. A
marker chooses only the boundary; it does not bypass `.errandignore`, Git
selection, home-directory protection, or the filesystem-root refusal.

## Why not just ssh?

ssh gives you a remote shell and assumes the remote already has your
project state: a checkout on the right branch, your uncommitted changes,
no stale artifacts. Every ssh target is a pet you maintain.

errand ships the workspace *with* the job, so targets are stateless with
respect to your projects: nothing checked out, nothing drifting, any peer
equally valid at any moment. Around that round-trip it wraps a
transaction ssh doesn't attempt. The implemented milestones provide:

- **At-most-once admission:** network retries cannot run a command
  twice.
- **Durable execution:** an admitted job survives caller disconnect, with
  ordered logs that the protocol can resume after a dropped connection.
- **Honest cleanup:** errand removes the temporary workspace and inherited
  process scope it owns, then records whether cleanup completed.
- **A receipt:** an append-only record of what was asked, who asked, what
  ran, and what happened.

The remaining v0 work includes explicitly declared named caches, fact-based
peer selection, and direct LAN pairing.

## Shape

One binary, symmetric peers, no controller. Transport and identity come
from your [tailnet](https://tailscale.com) (WhoIs + ACL capability grants;
errand stores zero credentials) or, later, from direct LAN pairing with
pinned device keys. The current execution backend runs directly on the
host. Rootless containers and Nix devshells are planned.

```
errand -- cargo test                   # configured default peer
errand --on buildbox -- cargo test     # named configured peer
```

An attached terminal can detach at any time with Ctrl-D and later resume with
`errand attach HANDLE`. Ctrl-C keeps its Unix meaning: it sends SIGINT to the
remote command, and a second Ctrl-C force-kills it. Interactive detachment
returns 0 for the detach action; it is not the unfinished job's exit status.
Non-terminal EOF is ignored, so scripts remain attached unless they request
`--detach` explicitly.

Detached jobs, `ps`, `attach`, `kill`, workspace-root discovery, declared
output collection and conflict-safe application, snapshot-cache inspection,
and cache and receipt GC are implemented. Planned v0 commands still include
fact-based peer selection, named-cache management, and pairing.

Declare outputs in the selected workspace's `.errand.toml`:

```toml
[[outputs]]
path = "dist/app"
collect = "success" # success | always
apply = "auto"      # auto | manual
```

The originating attached run downloads every collected output into the local
Errand state directory. `apply = "auto"` replaces the declared local path only
when it is unchanged from the pre-submission baseline. A later `attach` stages
only; `errand fetch --apply HANDLE [PATH]` or `errand attach --apply HANDLE`
explicitly applies outputs. The
optional path selects one declared output. A different machine can fetch and
inspect outputs, but cannot apply them without the original machine's local
baseline record. Output paths must be clean workspace-relative paths and may
not enter `.git` metadata. Local baseline and download records are scoped by
both runner endpoint and job ID, so one peer can never reuse another peer's
apply state.

## Job state and control

`errand ps` shows admission and process start times, process duration, source
snapshot, workdir, command, and terminal outcome. Git sources are shown as a
short commit plus `+dirty` when applicable; `errand ps --json` retains the full
commit and manifest digests. Running durations are measured on the runner, so
caller and runner clock offsets do not distort them.

The remote job states have deliberately narrow meanings:

- `staging`: admitted and preparing its workspace, but the command has not
  started.
- `queued`: staging is complete and the job is waiting for a running slot. Its
  process start and duration remain blank.
- `running`: the command has started and has not reached a terminal result.
- `exited`: the command reached a terminal result, including ordinary nonzero
  exits and signals forwarded to or raised by the process.
- `killed`: Errand terminated the job because of a user request or enforced
  limit.
- `ambiguous`: Errand cannot prove a clean terminal outcome, commonly after
  restart reconciliation or a receipt persistence failure. It never silently
  replays the command.

`detached` is a local attachment condition, not a remote job state. Detaching
with Ctrl-D leaves the job unchanged. Ctrl-C sends SIGINT, and a second Ctrl-C
sends SIGKILL. `errand kill HANDLE` requests graceful SIGTERM termination;
`errand kill --force HANDLE` sends SIGKILL. Staging and queued jobs can be
cancelled durably before they start. A submission is rejected as busy only
when both the configured running slots and bounded queue are full.

Capability-based runners must grant `manage-caches` to use `errand caches` and
`errand gc cache`. Job receipt collection uses the separate `gc-own` action.
The frozen design's ACL example includes the complete action set.

GC always names its target. Bare `errand gc` only prints usage:

```text
errand gc cache
errand gc jobs --older-than 30d --keep 500
errand gc jobs --dry-run --older-than 30d
errand gc outputs --older-than 30d
errand gc all --older-than 30d --keep 500
```

Job GC only removes the caller's clean `exited` or `killed` receipts. Clean
means cleanup, logs, and outputs all completed with no transaction error.
Active, queued, ambiguous, incomplete, and actively replayed receipts are
protected. When both retention bounds are present, a receipt must be older
than the cutoff and outside the newest `keep` receipts to be removed. `all` is
client-side composition of the separately authorized cache and job endpoints
plus local output-state collection. `gc outputs` removes old local baseline
records, verified downloads, and interrupted download staging; pending apply
transactions are always protected. Records whose submission never began follow
the requested `--older-than` boundary. Unresolved submitted jobs remain
protected for 30 days, after which an explicit local GC may retire abandoned
state.
After non-dry job GC, the client replays the runner's durable, owner-scoped
collection markers, so a lost deletion response does not strand local records.
New job IDs must carry a ULID timestamp from the preceding 24 hours, with one
hour of allowed future clock skew. The runner durably advances a high-water
clock that never moves backward across restart. Replay-only collection markers
expire after 25 hours. Markers for jobs with declared outputs are scoped to the
originating client and retain that minimum lifetime; after the client reconciles
its local state, it acknowledges the marker so it can retire. Unacknowledged
output markers expire after 30 days, bounding abandoned client state. The
markers are small and non-secret, and they never permit a collected ID to
execute again.

Linux and macOS first; Windows is a design constraint, not yet a
deliverable.

## Non-goals

Not a CI system, not interactive (no PTY in v0), not a security boundary
against hostile code, no web UI, and no arbitrary-host discovery or scheduler
fan-out. `errand ps` only queries explicitly configured peers. The design
resists becoming ansible, nomad, or a scheduler on purpose.

## License

MIT
