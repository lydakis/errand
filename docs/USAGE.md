# Using Errand

Run commands from a local workspace on a configured runner. For installation
and the first run, start with the [README](../README.md).

## Command conventions

Frequent execution flags follow familiar CLI conventions: `-d` is
`--detach`, `-e` is `--env`, `-w` is `--workdir`, and SSH-style `-L` is
`--forward`. `errand kill -f` requests `--force`, and GC accepts `-n` for
`--dry-run`; setup and peer mutation use the same `-f` and `-n` forms, while
peer discovery uses `-a` for `--all`. Safety boundaries, transport details,
and mutating workspace options remain long-form so their meaning stays explicit.
Use `--help` at the relevant level, for example `errand peers --help` or
`errand gc jobs --help`.

## Exit status

Errand returns the remote process's exit code. If that code is zero but the
transaction fails, Errand exits 120. A secondary transaction failure is
reported without replacing a nonzero process exit code. Detaching successfully
returns zero for the detach action, before the remote command has finished.

## Workspace selection and run preferences

Run errand from a Git worktree for automatic snapshot selection. A non-Git
directory requires an explicit `.errandignore` policy or `--include-all`.
Errand always refuses a filesystem root, and snapshotting your home directory
requires `--include-all`. The client prints the selected file count and byte
total before remote admission. Use `--no-snapshot` when a command needs no
local files; Errand then skips local content inspection and runs it in an empty
remote workspace. Errand still records the invocation directory's identity so
retained changes can be applied there safely. Because the remote workspace is
empty, `--workdir` may only name its root.

Before submission, Errand shows the full local workspace and command-directory
paths. Job status describes the workdir relative to the runner's workspace,
using `workspace root` for its root; local paths are not remote execution paths.

For a workspace containing several repositories, place this marker and an
explicit `.errandignore` at the shared root:

```toml
# .errand.toml
[workspace]
root = true

[run]
peer = "mac-mini"             # optional; alias lives in personal config

[changes]
apply_on_success = true
```

Errand discovers the nearest marked ancestor, snapshots from there, and runs
the command in the caller's relative directory. `--workspace-root PATH`
selects the same boundary explicitly. Automatic discovery trusts only a marker
and directory owned by the caller in a directory that is not group- or
world-writable; use the explicit flag for intentionally shared workspaces. A
marker chooses only the boundary; it does not bypass `.errandignore`, Git
selection, home-directory protection, or the filesystem-root refusal.
`[changes].apply_on_success` is read only from the selected workspace root.
For `--no-snapshot`, the current directory is the configuration root; Errand
does not climb to an ancestor marker when no local workspace is transferred.
Set `apply_on_success = true` in `~/.config/errand/config.toml` for a personal
default. Explicit `--apply` or `--no-apply` wins over workspace and personal
configuration. Applying is a property of the submitted job, independent of
whether its logs are currently being observed. `--detach --apply` and Ctrl-D
during an applying run hand completion to a detached local worker, which waits
for success and applies into the originating workspace. `attach` remains
observation-only and never chooses or changes that policy. If the worker or
client machine stops, the persisted policy is resumed by the next client
command on that machine.

`[run].peer` chooses a preferred configured peer for new runs. `--on` or
`--url` wins over this preference; an unknown workspace alias fails instead
of silently using the personal default. `errand config` shows the effective
run settings and their sources; `errand config --json` provides the same
information for scripts. Inspection stays local and does not resume automatic
applications. Named profiles in personal or workspace config can supply peer,
workdir, and apply preferences: `errand --profile build -- go test ./...`.
CLI flags override profiles; `errand config --profile build` explains the
result. See [configuration](CONFIGURATION.md) for syntax and precedence.

## Local jobs

Available since v0.1.2. Run a command in a separate
workspace on this machine:

```sh
errand setup --local
errand --on local -- make test
```

The command receives a snapshot with the same file-selection rules as a remote
job. Its ordinary workspace edits stay in the job; your original checkout
changes only through apply. The process still has your account's permissions,
so this is a workspace copy, not a security sandbox.

Start attached, press Ctrl-D if you want to stop watching, then use the
printed `local/JOB_ID` handle:

```sh
errand ps --on local --last 5
errand attach local/JOB_ID
errand fetch local/JOB_ID
errand fetch --apply local/JOB_ID
```

Exit codes, retained artifacts, named caches, explicit `-d`, and `--apply` /
`--no-apply` follow the same rules as remote jobs. Each new invocation starts
a new job workspace; attaching resumes observation, not execution in a
completed workspace. Nothing automatically falls back to local execution.

For a local development server, connect directly to its listening port.
Local host jobs share your machine's ports; if you use forwarding, choose a
different local port, such as `-L 8080:3000`, to avoid binding the server's port.
See [local-only setup](OPERATIONS.md#local-only-setup) and
[local target configuration](CONFIGURATION.md#local-targets).

## Persistent workspaces

Ordinary runs use a fresh, ephemeral workspace. To keep files across jobs,
create a persistent workspace explicitly, then select it on each run:

```sh
errand workspaces create --on mac-mini experiment
errand --on mac-mini --workspace experiment -- make test
errand --on mac-mini --workspace experiment -- make test
errand workspaces --on mac-mini
errand ps --on mac-mini --workspace experiment --last 5
errand df --on mac-mini --verbose
errand workspaces rm --on mac-mini experiment
```

Creation uploads your current working files using the usual snapshot selection
and configuration rules. `create --no-snapshot` starts empty instead. A missing
workspace name on a run is an error; it never creates a workspace implicitly.
Names start with a letter and contain up to 64 letters, digits, hyphens, or
underscores; names that look like workspace IDs are reserved. They are scoped
to your authenticated identity on that runner.

Subsequent runs use the files already there, at the same runner-side path. They
do not upload your latest local edits. Each command gets its own job handle,
logs, exit status, and retained results. Detaching and attaching work as usual.
The workspace survives failed commands and daemon restarts; interrupted commands
are not replayed. Multiple jobs can use the same workspace, within the runner's
normal concurrency and queue limits. For example, keep a development server
running while another job runs tests. Killing or finishing one job leaves the
others running. Removal is refused until every job has finished cleanup,
including queued jobs.

Jobs share the actual files and cache directories. Errand does not serialize
file writes or determine whether commands are read-only; use separate workspaces
when commands need independent files. `workspaces` and `df --verbose` show every
job currently holding the workspace.

Job handles still support `fetch`, `fetch --apply`, `fetch --apply --conflicts`,
and `fetch --output`. Each job's retained result is an immutable comparison with
the workspace's **creation snapshot**, including accumulated edits from earlier
jobs. Applying it uses the originating directory and the conflict-safe merge rules
described below. Fetching an old result neither updates
nor deletes the live workspace. Use `--no-apply` when you want to keep iterating
there without applying job results locally. With concurrent writers, a retained
result may include a sibling's edits. Collection checks the files it reads and
can fail if they change during capture; it is not an atomic workspace snapshot.
For a stable final result, wait for writers to finish, then run a final command
such as `errand --workspace experiment -- true` and fetch that job.

### Send local edits into a persistent workspace

Fetch addresses a job's fixed result. Push addresses the continuing workspace:

```sh
errand fetch --apply mac-mini/JOB_ID
# Edit locally, then inspect the staged transfer or apply it remotely.
errand push --on mac-mini --workspace experiment
errand push --on mac-mini --workspace experiment --apply
errand --on mac-mini --workspace experiment -- make test
```

Plain push freezes and stages the selected local snapshot without changing the
runner's working files. `--apply` merges it into the workspace. Both directions
use clean-or-refuse three-way merging; `--apply --conflicts` explicitly permits
conflict markers and clean sibling changes. Existing markers are ordinary file
contents. There is no transfer `--force`, background synchronization, or implicit
fetch before push. Concurrent commands remain caller-managed writers.

Push requires `--workspace NAME` and the originating checkout on the machine that
created it. Job handles are fetch targets, not push targets. Workspace names are
resolved to immutable IDs before transfer; deletion and recreation cannot redirect
an in-flight push.

Push supports `--on`, `--url`, `--profile`, `--workspace-root`, `--include-all`,
`--json`, and an optional complete changed `PATH` to limit application. Staging
uploads the selected snapshot, as fetching downloads the retained bundle. Push
uses the normal snapshot
selection rules and the workspace's fixed cache bindings. Artifact declarations
retain remote outputs; they do not opt ignored local files into an upload.
Changing snapshot policy requires a new workspace. Apply is explicit even when
run configuration specifies `changes.apply_on_success = true`.

For persistent workspaces, fetch/apply tracks the last accepted
remote source, and push/apply tracks the last accepted local source independently.
Destination-only edits never become the merge base. A partially conflicted apply
advances clean paths while keeping the old base for conflicted paths. A later
job returning to the creation state can therefore undo previously accepted remote
changes, even when its creation-relative result is empty. Omitting an artifact
from a later job's retention policy does not delete an earlier fetched artifact.
Plain fetch and `--output` continue to expose that job's immutable retained result.
There is no direct live-workspace fetch command.

Repeating push after an uncertain apply response completes that exact request
before accepting another snapshot, including when repeated without `--apply`.
The CLI identifies the recovered push; run push again to send newer local edits.
Completed retries do not overwrite later job edits. If the runner collected a
stage before receiving its apply request, the client uploads the frozen source
again. After an ordinary staged push, `push --apply` uses the acknowledged stage
without another upload, or prepares a new transfer if local files changed. Repeating a successful
fetch/apply with the same job and selection replays its recorded outcome; use a
new job result to fetch subsequent remote work.

`df` includes local transfer storage under changes and remote transfer storage
under its workspace (`df --verbose` shows a transfers column). Use
`gc changes --older-than 7d` locally or
`gc changes --on mac-mini --older-than 7d` on a runner to collect old staging and
unreferenced source bodies. `--dry-run` previews collection. Accepted checkpoints,
creation source bodies, pending applications, and the latest apply receipt stay
protected so an uncertain response remains retryable. Removing a remote
workspace removes its remote transfer state, while local state remains available
for retained job results. Transfer staging is bounded by the existing change
byte limit and 4,096 attempts per relationship; collect old staging when full.
Definitively rejected workspace creation removes its local origin snapshot.
An uncertain creation outcome preserves that snapshot; check `workspaces` before
retrying. GC preserves damaged transfer state, continues collecting healthy
relationships, and reports partial progress with a nonzero exit status.
`df` reports incomplete inventory instead of waiting for a busy local transfer;
`gc changes --dry-run` counts such relationships as protected.
Concurrent inventory and dry-run reads do not mark one another as active transfers.
Uploads to the same workspace wait for the preceding upload to finish; commands
remain independent. `df` includes temporary upload bytes in workspace transfer
storage, including an upload whose target workspace has just been removed.

Whole source snapshots and retained source bodies use the workspace byte limit;
individual deltas and attempt staging use the change byte limit. An unchanged
large source file therefore does not consume the delta allowance. Historical
source bodies still consume storage, and pinned bodies cannot be reclaimed by GC.

Profiles and configuration still provide peer, environment, workdir, forwarding,
and apply preferences. Persistent workspace selection itself requires the
explicit `--workspace` flag. Creation freezes ignore rules and named-cache
bindings; restored cache directories stay live inside the persistent tree. Ignored
files generated inside the tree survive for later jobs, but returning them still
requires artifact declarations. Per-run artifact flags can override retention.
Creation-time artifacts are inherited during reuse. Explicit flags or a selected
profile can override them; later ambient config changes do not replace the
workspace's defaults. Explicit conflicting cache bindings are rejected.
Cache paths are real directories shared by members of that workspace. Changed
trees are saved when the final successful member returns. Independent workspaces
can use the same named caches concurrently, with the normal responsibility to
coordinate writes to shared hardlinked files. Errand does not rewrite embedded
absolute paths or tool configuration. See [named caches](NAMED_CACHES.md).

`df` includes persistent workspace storage. Job and cache GC never collect the
persistent tree. `workspaces rm` explicitly removes an idle workspace and its
creation snapshot, leaving existing job receipts and retained results available
under their normal retention rules. Listing, creation, and removal accept
`--json`; all accept `--on` or `--url` and work with local, SSH, and tailnet peers.

`df --verbose` (`-v`) breaks workspace storage into working files, the creation
base, transfer data, and metadata. Named caches and job storage are listed separately, so
shared caches are never charged again to each workspace. Sizes are logical
regular-file bytes, not unique disk allocation; filesystem clones can share
physical blocks. Cache sizes reflect the last lease release, not live growth.

`ps --workspace NAME` filters jobs before the runner's listing limit. It keeps
the usual active-only default; add `--all` (`-a`) or `--last N` (`-n N`) for
history. The filter accepts an ID too, including after workspace removal while
receipts remain. Names select the current workspace: recreating a name does
not include jobs from the previous workspace. Across peers, missing names
match no jobs. `--json` includes each job's `workspace_id`.

## Attached sessions and forwarding

An attached terminal can detach at any time with Ctrl-D and later resume with
`errand attach HANDLE`. Ctrl-C keeps its Unix meaning: it sends SIGINT to the
remote command, and a second Ctrl-C force-kills it. Interactive detachment
returns 0 for the detach action; it is not the unfinished job's exit status.
Non-terminal EOF is ignored, so scripts remain attached unless they request
`--detach` explicitly.

`--forward [LOCAL:]REMOTE` opens TCP listeners on IPv4 and IPv6 local loopback for the
attached session. It is repeatable and can be added when initially running a
job or on any later `attach`. Omitting `LOCAL` uses the remote port locally.
Ctrl-D closes the attachment's listeners and active connections without
stopping the job. Forwarding is not remembered, and `--detach --forward` is
rejected because no attached client would remain to own the listener. Host
jobs share the runner's loopback network, so Errand cannot prove that a
listening service belongs to the selected host job. Capability-based runners
require the separate `forward-own` action. Forwarded connections carry bytes
in both directions while open; EOF or reset on either side closes that
connection.

## Retained changes, fetch, and apply

After every command, Errand compares the final remote workspace with the
submitted snapshot and retains every snapshot-representable created, modified,
or deleted path. No declaration is required. Every submitted path is compared.
New paths are retained only when allowed by the ignore policy frozen during
snapshot selection, so a remote command cannot widen retention by rewriting
`.errandignore` or `.gitignore`. Git metadata, Errand apply transactions, and
transient filesystem nodes such as sockets are excluded.
Failed commands retain their changes too,
which makes reports, traces, and partial build products available for
inspection. A `--no-snapshot` job starts from an empty baseline, so every file
it creates is retained.

A run reports retained changes and the job handle but does not download them
unless `--apply` or `apply_on_success` requests automatic application after a
completely successful transaction. The policy survives explicit `--detach`
and interactive Ctrl-D; failed commands never apply automatically. The
originating client records background application progress and errors, which
`errand status HANDLE` reports alongside the changed paths. `errand attach
HANDLE` follows logs and status only; it never downloads or applies workspace
changes. `errand fetch HANDLE [PATH]`
stages all changes or any retained path. Applying a selected path requires a
retained change root or an ancestor of one; `errand fetch --apply HANDLE [PATH]`
performs a three-way merge from the submitted snapshot, the current local
workspace, and the completed remote workspace. Errand applies the result only
when the entire selected merge is clean. On conflict, the working tree remains
untouched and the staged base and remote trees remain available.
`errand fetch --apply --conflicts HANDLE [PATH]` explicitly chooses a
non-clean state: clean changes are applied, text conflicts receive standard
conflict markers, and binary or type conflicts keep their local values. It
exits nonzero, reports unresolved paths, and keeps the base and remote values
in staging for manual resolution. Deletions use the same merge rules;
fetching only deleted paths returns the retained `bundle.json` tombstone
metadata because no file exists to return. A different machine can fetch
and inspect changes, but cannot apply them without the originating machine's
workspace identity record. Change records are scoped by runner endpoint and
job ID, so one peer cannot reuse another peer's apply state.

Use `errand fetch --output DIR HANDLE [PATH]` (short form `-o DIR`) to copy
retained remote files into a directory of your choice. For example,
`errand fetch -o ./results HANDLE reports/test.html` creates
`./results/reports/test.html`. Paths stay relative to the submitted workspace;
the export contains remote values, without the staging metadata or base tree.
The parent directory must exist and `DIR` must not exist, even as an empty
directory or symlink. Errand verifies the selected contents before publishing
the complete directory without overwriting an existing destination.
Exports work for failed jobs and from a different client machine. They do not
merge into the originating workspace or mark changes as applied, and cannot
be combined with `--apply` or `--conflicts`. Selecting a deleted path has no
remote value to export; use plain fetch to inspect its deletion metadata.

## Artifacts and caches

To retain generated outputs that Git or `.errandignore` excludes, declare exact
workspace-relative files or directories:

```toml
# .errand.toml
[artifacts]
paths = ["reports", "dist/app"]
```

Or use `errand --artifact reports -- make test`; repeat `--artifact` for multiple
paths. `--no-artifacts` clears configured declarations. Declarations select
remote outputs without uploading local ignored files. Failed jobs retain their
outputs too; missing outputs are allowed. Fetch, export, and apply use the same
retained bundle and conflict rules. See [artifact configuration](CONFIGURATION.md#artifact-declarations)
for profiles, precedence, and limits.

To reuse disposable build data on a runner, use `--cache compiler=target` or
configure `[caches]` with `compiler = "target"`. `--no-caches` clears configured
bindings. Cache contents stay on the runner and are excluded from both input
snapshots and fetched results. Every binding uses the same directory-preservation
mechanism. For example, `[caches]` with `dependencies = "node_modules"` lets a
separate `errand -- pnpm install` supply installed files to a later
`errand -- pnpm test`. Monorepos need explicit bindings for nested installed
directories too. See [named caches](NAMED_CACHES.md) for concurrency, hardlink
behavior, path-portability limits, recovery, and cleanup.

Git is not required for non-Git snapshots, running jobs, status, logs, or plain
fetches. Applying changes needs `git merge-file` on the client only when both
the local and remote sides changed the same text file and a true three-way text
merge is required. Missing Git fails that apply safely before installation.

## Job state and control

Bare `errand ps` queries every explicitly configured peer, merges the results
newest-first by job ULID, and shows active jobs only. The active filter is
applied by each runner before its bounded receipt window, so retained terminal
jobs cannot hide a long-running job. `--all` includes terminal receipts;
`--last N` includes all states and applies one global limit after merging.
`--on` and `--url` explicitly narrow either view to one runner. Bare
`errand peers` and `errand df` follow the same all-configured-peers rule.
`df` groups local runner storage and fetched changes into one `local` row.
`df --verbose` (`-v`) adds individual workspace, named-cache, and job storage
tables. `df --verbose --json` includes the corresponding `details` object;
runners must return the requested inventory details.
Remote rows also include changes fetched by that runner's OS account. See [storage maintenance](OPERATIONS.md#storage-and-garbage-collection)
for accounting and cleanup details.
These read-only fleet commands share target selection, concurrent querying,
partial-failure reporting, and exit semantics. Commands that mutate runner
state remain explicitly single-runner.

Interactive terminals show wrapped job cards with complete commands. Piped or
redirected output uses a compact plain table for tools such as `grep` and
`head`. Both forms show admission and process start times, process duration,
project, source snapshot, command, and terminal outcome. `WORKDIR` appears only
when a listed job runs below its workspace root. Git sources are shown as a
short commit plus `+dirty` when applicable; `errand ps --json` retains exact
structured project, workdir, commit, and manifest metadata, including
truncation flags. Running durations are measured on the runner, so caller and
runner clock offsets do not distort them.

`errand status HANDLE` shows one job's complete human-readable execution view:
runner, state, command, timing, process and transaction outcomes, retained-log
availability, retained workspace changes, and relevant next
commands. `errand status --json HANDLE` emits the same owner-visible data as
structured JSON. `ps` remains the multi-job listing; `attach`, `fetch`, and
`kill` remain the actions on a selected job.

Client-side workspace identities, downloads, and apply transactions are
stored under `$XDG_STATE_HOME/errand`, or `~/.local/state/errand` when
`XDG_STATE_HOME` is unset. `XDG_STATE_HOME` must be absolute and can point to a
writable private location in restricted agent environments. Errand avoids
changing an already-secure directory. It attempts to tighten excess read access
but only refuses to proceed when ownership, missing owner access, or permissions
writable by another user would make the state untrustworthy.

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

## Choose a runner by requirements

For commands that can run on several configured machines, use `--where`:

```sh
errand --where 'os=linux,kvm' -- ./run-vm-tests
errand --where 'go' -- go test ./...
```

Errand filters for the required capabilities, chooses by available job capacity,
and prints the selected peer. The returned job handle works with `attach`,
`fetch`, and other job commands as usual. Use `--on` when you want a specific
machine, including when continuing a persistent workspace.

Requirements also work in workspace defaults and profiles. See
[automatic runner selection](CONFIGURATION.md#automatic-runner-selection)
for syntax, selection rules, local opt-in, and retry behavior.
