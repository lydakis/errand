# errand — a personal job runner for machines you own

> **Status: frozen for v0** (2026-08-28). Further conceptual elaboration is
> deliberately deferred until milestone 1 replaces `scripts/remote-check`
> in the Atlas workflow and reality reports back.

Run the thing you would have run locally, on another machine you own, and
get the result back. Same argv, same working tree, logs streaming to your
terminal, real exit code — the heat, watts, and minutes spent elsewhere.

Not part of Atlas. If the two ever meet, errand is a client that targets an
Atlas Surface — never the other way around.

## The conceptual center: a remote process transaction

errand's unit of value is not a connection, it is a transaction:

> Capture a workspace snapshot, authorize the caller, execute once —
> without replay on retry — stream an ordered result, retain workspace
> changes safely, clean up what errand owns, and leave an append-only receipt.

Everything below serves that. Without those properties errand is ssh plus
tar; with them it is a tool.

This is also the answer to "why not just ssh?" — ssh gives you a remote
shell and assumes the remote already has your project state: a checkout on
the right branch, your uncommitted changes, no stale artifacts. Every ssh
target is a pet you maintain. Errand ships the workspace with the job, so
targets are **stateless with respect to your projects**: nothing checked
out, nothing drifting, any peer equally valid at any moment. The snapshot
round-trip is what makes errand more than a shell; the transaction
properties are what make the round-trip trustworthy.

### The v0 fidelity contract

"As if you ran it locally" means exactly this in v0, no more:

- Transported faithfully: argv, the workspace snapshot, explicitly declared
  environment values, relative working directory, stdout/stderr in
  daemon-observed order, selected signals, exit status, and explicitly
  attached local TCP forwards.
- Not reproduced: ambient local environment, an interactive terminal
  (no stdin, no PTY — non-interactive only), ambient local network access, or
  the local platform. Results are native to the *target*; a binary built on
  x86 Linux is an x86 Linux binary, and the receipt says so.
- Command means **argv**, executed directly. Shell composition is explicit:
  `errand -- sh -lc 'cargo test && cargo clippy'`.

## Beliefs

- Transport is not authorization. Every request is authorized against an
  explicit, revocable grant, even between your own devices.
- **An errand execution grant is shell access as the job's OS user on that
  machine. Grant it as carefully as SSH access.** Arbitrary commands can
  read and exfiltrate anything that user can read — and, in v0, can also
  interfere with errand's own state (see Receipt trust).
- Honest cleanup: errand promises to remove **all state it owns** —
  workspace, managed processes, temp files, containers. It does not promise
  the job mutated nothing else (see Cleanup).
- The receipt is a diagnostic record, append-only as emitted by the daemon.
  It is honest about its own trust level (see Receipt trust). Never
  overwritten into fiction *by errand*.
- Boring on purpose. One binary, one daemon, one application protocol.
  Files on disk. (One protocol, not one literal socket — the tailnet TCP
  listener and the local Unix socket share one handler stack.)

## Shape: symmetric peers

One Go binary. Every machine that installs it can both delegate and receive;
there is no controller. This is a **symmetric peer-to-peer runner, not a
mesh**: no peer relaying, no gossip, no shared scheduler, no distributed
membership — and none should be accidentally grown. Symmetry describes the
two roles, not self-execution: a client may target other peers, but a runner
rejects connections originating from its own machine. Run local work directly.

Peers are **explicit** in personal config; `errand peers` probes configured
peers' `/v0/info`; `--where` searches only those. Discovery is scoped, not
scanning: `errand peers discover` asks the caller's own tailscaled for the
node list and probes each online node's errand port with an authenticated
`/v0/info` — never arbitrary hosts, never other networks — and prints exact
`peers add` commands for runners that admit the caller. Probing facts is
implied by holding any errand capability (plus network-level access); no
separate probe grant exists in v0.

**Platforms:** Linux and macOS are the v0 targets for both roles; Windows
is a design constraint, not a v0 deliverable — the protocol and job model
assume nothing POSIX-only. The milestone 1 host backend uses a process group
plus an inherited per-job scope marker so it can find descendants that create
new sessions on Linux and macOS. Native cgroup scopes and Windows Job Objects
remain backend-specific follow-on work.

```
errand -- cargo test                  # configured default peer
errand --on cabal -- nix flake check  # named peer (personal alias)
errand --where kvm,x86_64 -- ...      # facts-based selection among peers
```

Facts (arch, OS, cores, memory, /dev/kvm, container runtime, nix, disk
free) are measured with `observed_at` timestamps. Client-side they are
selection hints; **the daemon revalidates them as requirements at
admission** against the actual job user — `/dev/kvm` must be openable, a
container runtime must actually respond, not merely sit on PATH.

## Transport and identity

Two transports behind one peer abstraction, each yielding an identity the
daemon authorizes against an explicit, revocable, directed grant:

1. **Tailnet (preferred).** Daemon binds the host's tailnet address.
   Callers are identified via destination-scoped WhoIs; the runner reaches
   tailscaled through whichever provider it discovers: the LocalAPI Unix
   socket (Linux `tailscaled`, or the open-source macOS daemon) or the
   `tailscale` CLI (the only path into the standalone macOS app, which
   cannot scope capabilities and therefore authorizes by allow list only).
   Destination-scoped capabilities require Tailscale 1.100 or newer; older
   or unversioned LocalAPI daemons fail closed. errand stores zero credentials
   in this mode.
2. **SSH.** The client speaks the same versioned HTTP protocol over an SSH
   session (`ssh HOST errand _stdio`), which the runner bridges to the
   daemon's **Unix socket**. Identity there is the kernel's: peer
   credentials (`SO_PEERCRED` / `LOCAL_PEERCRED`) must match the daemon's uid.
   The socket and its state-directory parent are private to that account.
   SSH's `known_hosts` pins the runner and `authorized_keys` controls who may
   log in as the runner account, so errand owns no keys or pairing state.
   Peer configuration may set `remote_command` to an absolute Errand binary
   path when the non-interactive SSH `PATH` does not contain it, and
   `remote_socket` when the runner was started with a non-default config,
   state directory, or socket path.
   Set `listen = "none"` for an SSH-only runner with no Tailscale. The Unix
   socket is the sanctioned loopback route; the TCP listener still refuses
   loopback and self-target connections, which WhoIs cannot identify.

   **The edge is directed** on both transports: on the tailnet from the
   grant's `src` to `dst`, and over SSH through access to the runner account.
   In errand vocabulary the sides are the **caller** (may submit) and the
   **runner** (accepts). A bidirectional relationship is two one-way grants.

No third transport. No tailcat mode: control-plane-less tunnels discard the
identity story errand depends on.

### Tailnet authorization, precisely

Network access and application capability are separate layers. Without an
`ip` grant the connection never reaches errand (denied by Tailscale); with
network access but no errand capability, errand returns 403. Both are
required:

```jsonc
"grants": [{
  "src": ["autogroup:member"],
  "dst": ["tag:errand-runner"],
  "ip":  ["tcp:7443"],
  "app": {
    "lydakis.dev/cap/errand": [
      { "actions": ["submit", "read-own", "kill-own", "forward-own", "manage-caches", "gc-own"] }
    ]
  }
}]
```

The capability carries an **action schema from day one** (not a boolean):
`submit`, `read-own`, `kill-own`, `forward-own`, `manage-caches`, `gc-own`,
and later `read-all`.
Matching grants are additive; errand merges capability objects
deliberately (union of actions).

Authorization is checked on submission **and on every** logs, fetch, forward,
signal, kill, and listing request. Revocation prevents new control, retrieval,
and forwarding; it does not retroactively kill an already-admitted job (that
would be a different guarantee, possible later). Active forwarding requests
end when their transport connection closes.

### Ownership

`read-own` / `kill-own` / `forward-own` need a defined owner principal:

- **Tailnet:** the authenticated Tailscale *user* when WhoIs provides one
  (so a job submitted from the phone can be attached from the laptop),
  otherwise the exact node identity.
- **SSH / local:** the kernel-attested daemon uid on the Unix socket, as
  `local-user:<uid>`. Any SSH session or shell for that uid is the same owner.
- Cross-transport or manually grouped identities are not supported in v0:
  a job submitted over the tailnet is not owned by the same person's local
  uid, and vice versa.

`admission.json` records the user identity and the exact device identity
(tailnet), or the uid and username (SSH/local), even when ownership keys
off only one.

### Job handles are peer-qualified

In a controllerless system a bare ULID doesn't say where the job lives. The
canonical handle is `peer/ulid`:

```
job cabal/01K4Q8ZJ2M...
```

Printed handles (especially for detached jobs) are always peer-qualified. A
configured peer prints its local alias and therefore requires the same alias
on another initiator, or an explicit `--url`. A raw URL retains its scheme and
port in the handle. A bare ULID resolves through `--on`, `--url`, or the
configured default peer. Unknown aliases and conflicting `--on`/`--url`
selectors fail locally rather than being guessed as network hosts.

## Execution backends and isolation — separate axes

Backends provision a toolchain; isolation confines writes. They are not
the same property:

| Backend | Toolchain | Filesystem confinement |
|---|---|---|
| host | target's own tools | none |
| container (rootless OCI) | the image | ordinary writes limited to declared mounts |
| nix devshell | reproducible from flake | **none** |
| Atlas environment (future) | environment's own | strong boundary |

Host and nix jobs are **trusted execution**: they run with the job user's
full powers. The container backend constrains ordinary filesystem writes to
the workspace and declared mounts — it still consumes CPU, memory, disk,
and network, can mutate declared caches, and shares the host kernel. That
is cleanliness for trusted code, not containment of hostile code.

Backend-specific networking sits behind one job-endpoint seam:

```go
type JobEndpoint interface {
    DialTCP(context.Context, uint16) (net.Conn, error)
}
```

The host and nix adapters dial the runner's loopback network while the job is
running. That network is shared: Errand cannot claim the selected job owns the
listening socket, and concurrent host jobs cannot both bind the same port. This
matches the trusted host execution model, where a submission grant already
permits the job to access runner-local services. Container and future Atlas
adapters instead dial loopback inside the job's network environment. The CLI
and tunnel transport do not depend on those runtime details.

Per-project defaults (portable, in the repo):

```toml
# .errand.toml — describes the project, never names your machines
[workspace]
root = true                    # this directory is the snapshot boundary

[changes]
apply_on_success = true        # apply after clean successful completion

[run]
backend = "container"
image   = "rust:1.86"          # receipt records the resolved digest

[env]
pass = ["RUST_BACKTRACE"]      # forwarded from initiator by name only
set  = { CI = "1" }

[[cache]]
name  = "cargo-registry"
path  = "/usr/local/cargo/registry"
scope = "project"              # project | user | peer (opt-in beyond project)
```

Personal aliases and defaults (`cabal`, default peer, addresses, and
`apply_on_success`) live in
`~/.config/errand/config.toml`, never in the repository.
For `--no-snapshot`, the current directory is the configuration root, so an
ancestor marker cannot silently choose an apply policy for an empty workspace.

Environment defaults to **nothing forwarded**: target's PATH, plus `pass`
by name and `set` literals. The initiator's ambient environment is never
shipped silently. Secret-designated values and value-derived hashes are not
written into the receipt. Only names and provenance are retained.

### Cleanup, honestly

When a job ends (success, failure, kill), errand removes everything it
owns: workspace, the inherited per-job process scope, and temporary state.
The host scope finds ordinary descendants even when they call `setsid`; it is
lifecycle containment, not a security boundary against hostile code that
deliberately scrubs the inherited marker. `result.json` records `cleanup_ok`
separately from the exit code, and errand never reports pristine when scope
inspection or cleanup failed.

Persistent state comes in three kinds, not one:

- **Named caches** — errand-owned writable directories with explicit
  lifecycle: declared in config, measured by `errand df`, removed by
  `errand gc cache`. Project-scoped by default.
- **Backend stores** — container image layers, the nix store. Managed by
  the runtime, not by `errand gc`; pruning them is the runtime's own
  command, surfaced but never run implicitly.
- **Job state** — runtime state is removed per the contract above. Receipts
  remain until their owner explicitly applies a bounded `errand gc jobs`
  policy. New job IDs must carry a ULID timestamp from the preceding 24 hours,
  with one hour of allowed future clock skew. The runner durably advances a
  high-water clock that never moves backward across restart. Replay-only
  collection markers expire after 25 hours. Markers for jobs with retained
  workspace changes are scoped to the originating client and retain that minimum
  lifetime; successful local reconciliation acknowledges the marker so it can
  retire. Unacknowledged change markers expire after 30 days, bounding state
  left by lost clients. The markers are minimal and non-secret, and neither
  permits a collected ID to execute again.

## The job transaction

### Receipt format (append-only)

`~/.errand/jobs/<ulid>/`:

```
spec.json        # versioned, immutable redacted request receipt (argv, env
                 # names and provenance, limits, manifest root)
admission.json   # caller user + device identity, authz result, revalidated facts
execution.json   # argv and, unless PATH was caller-supplied, resolved path
events.ndjson    # append-only lifecycle + control events
io.log           # framed, base64-payload stdout/stderr in daemon-observed order
result.json      # written once at terminal completion
scope.json       # transient recovery marker; retained until runtime cleanup
workspace/       # deleted at cleanup
changes/         # immutable workspace-change bundle; retained with the job receipt
```

A derived `status.json` may exist as a convenience cache; it is never the
authoritative record.

### Receipt trust (v0)

Receipts are **append-only as emitted by the daemon**. They are diagnostic
records, not tamper-evident against local administrators or against jobs
running as the same OS user — in v0 the job state lives under the same
account that runs host and nix jobs, so a hostile job could corrupt
receipts, other jobs' state, or the daemon itself. This is accepted for a
personal tool running trusted code, and it is stated rather than hidden.

The upgrade path is real privilege separation, not a shared "errand user":
`errandd` owns job records, peer keys, and configuration; jobs run as a
separate ephemeral worker account that cannot read or modify the daemon
state directory. That lands post-v0 unless adversarial receipt integrity
becomes a requirement — in which case it belongs in milestone 1.

### Invariants

- **At-most-once admission.** The client generates the job ULID and a
  canonical request digest, submitting with `PUT /v0/jobs/<ulid>`.
  During one daemon lifetime, same ID + same digest returns the existing job;
  same ID + different digest returns 409 Conflict. An environment-free receipt
  can reconstruct that identity after restart. A receipt with declared
  environment values cannot do so without persisting a value-derived verifier,
  so every same-ID retry after restart fails closed with 409. Errand never
  automatically reruns a job whose
  execution state is ambiguous (e.g. after a daemon restart between
  "starting" and process creation) — ambiguity is reported, not replayed.
  Network retries therefore cannot duplicate a command; that is the whole
  user-facing promise, and no stronger distributed-systems claim is made.
- **Ordered, resumable logs.** The authoritative stream is framed events
  `{"seq":42,"stream":"stdout","data_b64":"..."}` — SSE is a text
  protocol, so payloads are base64. Ordering is **daemon-observed order**:
  there is no intrinsic total order between two pipes, only the order the
  daemon drained them. Reconnect resumes via `Last-Event-ID`. Plain
  `stdout.log`/`stderr.log` are derived views.
- **Separate outcomes.** `result.json` distinguishes
  `{exit_code, signal, changes_ok, cleanup_ok, logs_complete}` — a job can
  succeed while collection or cleanup fails, and the receipt says which.
- **Signals behave like signals.** Ctrl-C forwards SIGINT; a second Ctrl-C
  or `errand kill --force` escalates. If contact is lost mid-signal, the
  client prints the peer-qualified handle and admits the process may still
  be running.
- **Attachment is local and reversible.** On an interactive terminal, Ctrl-D
  stops local log following and any local port listeners for that attachment,
  prints the reattach command, and exits 0 for successful detachment. It never
  signals the remote job. Non-terminal EOF is ignored so scripts retain
  attached exit-code fidelity unless they explicitly use `--detach`.
- **Durability and reconciliation:** jobs survive *caller* disconnection.
  Before starting a process the daemon persists an unguessable inherited
  scope marker. After a daemon restart it terminates surviving scoped
  processes, removes the workspace and transient scope record, and writes a
  durable `ambiguous` result. It never replays the command and never treats
  the absence of a survivor as evidence that the command did not start. A
  target reboot is reconciled with the same conservative ambiguous outcome.
  If terminal cleanup was incomplete, the retained scope marker and workspace
  are retried on later daemon starts without rewriting the immutable result.

### Attached TCP forwarding (milestone 4.5 amendment, 2026-09-03)

Port forwarding extends an attached job session without becoming durable job
policy or another top-level command:

```text
errand --forward 3000 -- pnpm dev
errand --forward 8080:3000 -- pnpm dev
errand attach --forward 3000 HANDLE
errand attach --forward 8080:3000 HANDLE
```

`--forward [LOCAL:]REMOTE` is repeatable. Omitting `LOCAL` uses the same port
locally. The run form is shorthand for submitting and opening the initial
attached session with those forwards. A job may be submitted without any
forwards and gain or change them on any later attachment. Forward mappings are
local session state: they are not written to `spec.json`, remembered for later
attachments, or displayed by `status`.

The client binds both IPv4 and IPv6 local loopback when the platform supports
them. It binds every requested local port before submission or attachment so a
collision fails without admitting a new job or partially opening a session.
Each accepted TCP connection creates one separately authenticated,
owner-checked, full-duplex request to the runner.
The runner accepts it only with `forward-own` and only while the named job is
running. For the host backend, that job is an ownership and liveness gate, not
a network boundary: the tunnel can reach any runner-loopback service. Staging,
queued, and terminal jobs refuse tunnel connections immediately. The local
listener remains open, so clients may retry after the job starts.

Detaching with Ctrl-D closes the local listeners and their connections without
signaling the job. Ctrl-C retains the normal attached-job meaning: it sends
SIGINT, a second Ctrl-C force-kills, and the forwarding session closes as the
job settles. A client that wants forwarding after detaching invokes `attach`
again with the desired mappings. `--detach --forward` is rejected because no
attached client would remain to own the local listeners.

There is no readiness probe. If the application is not listening, the accepted
local connection closes without a noisy session diagnostic; the listener stays
available for the next connection. Bytes are relayed without application-level
inspection, so HTTP, WebSockets, and development-server hot reload need no
protocol-specific behavior. EOF or reset from either endpoint closes the whole
forwarded connection; independent TCP half-closes are not part of the initial
HTTP tunnel contract. Unexpected forward failures are local session diagnostics
and never rewrite the remote process result. Local bind failure on the initial
run occurs before submission.

The initial surface deliberately excludes port inference, automatic browser
opening, reverse forwards, UDP, Unix sockets, SOCKS, HTTPS termination, public
or LAN binds, background local tunnels, and automatic remote-port allocation.
The ordinary two-hour runtime limit and runner capacity rules are unchanged: a
development server occupies one running slot until it exits or is stopped.

Forward mappings and individual TCP connections are not receipt state. The
daemon does not append an event for every browser or hot-reload connection and
does not retain forwarded bytes. It keeps only the transient connection state
needed to close tunnels when the job or request ends.

### Limits

Limits are **fixed at admission and enforced at the relevant execution
phase**: upload size at admission; workspace expansion at unpack; runtime,
log size, and retained-change size during execution and collection. On hitting the
log cap the daemon terminates the job with `limit_exceeded`, preserving a
complete log up to termination; the fidelity contract promises faithful
process output, not best-effort truncation.

The runtime deadline begins when the child starts and remains unchanged through
workspace-change collection. Collection does not receive a fresh runtime budget after the
process exits.

**Concurrency (milestone 3.5 amendment, 2026-08-30).** A runner executes
up to `max_jobs` jobs at once (default 1; concurrency is an explicit
per-runner choice) with a bounded FIFO admission queue of `max_queued`
waiting jobs (default 8; zero disables waiting). Beyond both, submissions
return `busy`; that boundary survives. Admission order and capacity are
reserved before staging, so upload duration cannot reorder jobs. Once staging
succeeds, a durable queued marker commits the admission and every process
start flows through one FIFO launcher. Launch failures therefore produce the
same durable receipt whether or not a slot was initially free. A signal or
kill accepted before process start settles durably as killed before the
control request succeeds.

Queued jobs appear in listings as `queued`. On daemon restart, a queued marker
without a process-scope record proves the command never started; the daemon
settles that job with a never-started result and does not replay it. A scope
record still means execution may have begun and retains the ambiguous restart
semantics. The runtime limit starts at launch; time spent queued is unbounded.
The queue remains deliberately simple: no priorities, bin-packing, or
cross-peer scheduling, and errand does not referee CPU or memory contention.
Both limits are per-machine choices for the operator who knows its workloads.

## Workspace snapshot

- Snapshot-root discovery precedence is explicit `--workspace-root PATH`, the
  nearest ancestor `.errand.toml` with `[workspace] root = true`, then the
  current directory. The selected root must contain the invocation directory;
  the latter becomes the default remote workdir relative to that root. The
  root marker defines a boundary, not permission to ship it. Automatic
  discovery trusts only caller-owned markers in caller-owned directories that
  are not group- or world-writable; intentionally shared workspaces use the
  explicit flag.
- Automatic selection is limited to Git worktrees. A non-Git directory must
  contain `.errandignore` or use the explicit `--include-all` override.
  Filesystem roots are always refused; the user's home directory requires the
  override even when a policy file exists. The client prints the selected file
  count and total bytes before admission.
- `--no-snapshot` explicitly skips local content inspection and transfer. The
  client still records the invocation directory's identity and the empty
  manifest root so retained changes can be applied only to that originating
  workspace. The job receives an errand-owned workspace, but it starts empty,
  so a non-root `--workdir` is rejected locally.
- Selection uses explicit `.errandignore` rules when present, otherwise Git's
  tracked, untracked, and ignore hierarchy. The effective ignore rules are
  frozen into the request for new-path retention.
- The snapshot is consistent or refused: packing builds a manifest of
  path, type, mode, size, and content hash; files that change mid-pack are
  detected and the snapshot **fails with a retry prompt** rather than
  shipping a mixture of moments. The manifest root hash is recorded in
  `spec.json` (and is the natural basis for later content-addressed sync).
- Git submodules are rejected unless an explicit `.errandignore` policy is
  used to define a recursive filesystem snapshot. A gitlink is never sent as
  a silently empty directory.
- `.git` is **not** shipped by default (size, history disclosure); jobs get
  `ERRAND_GIT_COMMIT` / `ERRAND_GIT_DIRTY`, with an opt-in to include the
  repository database.
- Archive extraction (both directions) rejects absolute paths, `..`,
  symlinks escaping the workspace, and unsafe hardlinks. A strange archive
  must not write outside `workspace/`.

## Workspace changes

Retention (target-side) and application (client-side) are separate. After the
process and its scope settle, the target compares the final workspace with the
submitted manifest and durably retains the minimal roots of every creation,
modification, and deletion. This happens on successful and failed commands and
requires no path declaration. Every submitted path remains eligible so its
modification or deletion is observable. Newly created paths are eligible only
when allowed by the selection policy frozen into the request during snapshot
preparation. The command cannot widen retention by editing ignore files. For
`--no-snapshot`, the baseline and selection policy are empty, so every
representable generated path is retained.

Retention uses the same representable filesystem boundary as snapshots. Git
metadata, Errand apply transactions, and transient nodes such as sockets are
excluded without invalidating other retained changes.

Attached and detached jobs use the same retention path. Completion reports the
retained-change summary and handle without downloading the bundle unless the
job's submission policy requests automatic application. `--apply` applies only
after the remote process and transaction both succeed; `--no-apply` disables
workspace or personal defaults. That policy is independent of observation:
explicit `--detach` and interactive Ctrl-D hand it to a detached local
completion worker. `attach` follows logs and status without choosing or changing
the policy. Interrupted workers retain their pending policy and are resumed on
the next client invocation. A different or non-matching workspace can stage
changes but cannot apply them.

Application is conflict-safe, never silent:

1. Retain the submitted base tree and completed remote tree for every changed
   root.
2. Download both trees into local staging; validate paths, types, and content.
3. Three-way merge the submitted base, current local workspace, and completed
   remote tree entirely outside the working tree.
4. If every selected change merges cleanly, install the complete result through
   same-filesystem renames. A durable local journal rolls back an interrupted
   installation or completes its state record before discarding original files.
5. By default, any conflict leaves the working tree untouched, retains staging,
   and reports the conflicting paths.

`fetch --apply --conflicts` is the explicit exception to step 5. It installs
clean changes and standard text conflict markers while leaving binary and type
conflicts at their local values. It retains the staged base and remote values,
reports every unresolved path, and exits nonzero. Errand provides no conflict
index, resolution commands, or continuation lifecycle.

This is a path-based merge with Git-compatible text merge behavior. It does not
attempt rename detection or maintain an Errand-specific conflict index.
Git is otherwise optional. The client invokes `git merge-file` only when apply
must merge a text file changed on both the local and remote sides; a missing
binary aborts safely before the installation transaction.

Client-side workspace identity and staging records are keyed by runner endpoint plus job
ID. Pending apply journals are never collected. Pre-admission records follow an
explicit local GC cutoff; unresolved submitted records receive a 30-day safety
window before an explicit `gc changes` may retire them.

### Exit status: two layers

- **Process result:** the remote exit code or signal, recorded exactly in
  `result.json`.
- **Transaction result:** whether execution, workspace change retention, and
  cleanup completed.

CLI behavior: when the transaction succeeds, the CLI exits as the remote
process did. If the remote process exits 0 but change retention or cleanup
fails, the CLI returns an errand-level nonzero status and prints
`remote_exit=0` — otherwise
`errand -- cargo build && ./target/release/thing` would happily run a stale
binary, which violates local-likeness worse than remapping the status. If
contact is lost before a terminal result, the CLI returns an errand-level
failure and prints the resumable peer-qualified handle. A nonzero remote
exit is returned as-is, with secondary transaction failures reported
separately.

## Execution context receipt

The receipt records the execution context errand controlled or directly
observed — it is **not** an attestation of everything the process
transitively ran, loaded, or fetched (that would be execution tracing, a
different product). Recorded: argv and, unless PATH was caller-supplied, the
top-level resolved executable path; target platform; container image digest;
nix flake lock hash and resolved derivation; and explicitly configured version
probes. Raw PATH and value-derived PATH hashes are never persisted. Nothing
vaguer is promised.

## CLI surface (v0)

```
errand [--on X | --where facts] [-d | --detach] [--apply | --no-apply] -- <cmd...>
errand setup [--max-jobs N] [--allow-user LOGIN]...
             [-f | --force] [-n | --dry-run] [--print-acl]
errand peers [--json]           # configured peers and reachability
errand peers add NAME HOST      # verify first (403 → accurate allow-list remedy), then record
errand peers remove NAME
errand peers discover [-a | --all] # runners on the caller's own tailnet; read-only
errand ps [-a | --all] [-n N | --last N] [--on X] [--json] # N <= 200
errand info [--on X] [--json]  # human-readable fleet facts unless JSON is requested
errand logs <peer/ulid> [-f]    # resumes from last seen seq
errand attach <peer/ulid>
errand fetch [--apply [--conflicts]] <peer/ulid> [path]
errand kill [-f | --force] <peer/ulid>
errand df [--on X] [--json]    # fleet storage; read-own
errand gc cache [--dry-run]     # shared cache policy; manage-caches
errand gc jobs --older-than 30d # caller-owned clean terminal receipts
errand gc changes --older-than 30d # local workspace identities and downloaded staging
errand gc all --older-than 30d [--dry-run] # cache, jobs, and local change state
```

Every GC target accepts `--dry-run`. A dry run uses the same eligibility policy
while leaving all persistent state unchanged, including cache files, receipts,
reconciliation markers, local lock files, permissions, and the admission
clock. Local staging that cannot be sized without widening permissions is
reported as a failed preview and left untouched.

Frequent run options use established short forms: `-d` for `--detach`, `-e`
for `--env`, `-w` for `--workdir`, and SSH-style `-L` for `--forward`. GC uses
`-n` for `--dry-run`, `errand kill` uses `-f` for `--force`, and peer
management follows those same aliases plus `-a` for `discover --all`. Routing,
transport details, workspace mutation, and snapshot-boundary options remain
long-form.

`errand setup` acquires an expiring lease from the local daemon before changing
config or service files. The daemon grants it only while idle and atomically
refuses new admissions until restart, so setup cannot race a newly submitted
job. If an existing local socket cannot be reserved, setup refuses the
restart. Its generated SSH peer block includes the effective `remote_socket`
and an absolute `remote_command` unless setup proved that
`/usr/local/bin/errand` resolves to the installed executable.

Without `--detach`: streams, exits per the two-layer status rule — drop-in
for scripts. With `--detach`: prints the peer-qualified handle and returns.
A terminal user can press Ctrl-D while running or reattached to detach the
local follower and later attach again; Ctrl-C continues to interrupt the
remote process. A detach exit of 0 confirms only the local detach action, not
the eventual job outcome. Detaching never changes a job's `--apply` or
`--no-apply` policy; automatic application continues in a detached local worker.
A bare ULID is resolved through `--on`, `--url`, or the configured default
peer. Alias-qualified handles require the matching peer configuration;
URL-qualified handles retain the concrete scheme, host, and port. An explicit
`--url` may route an alias-qualified handle elsewhere, and the client then
reports the effective URL rather than preserving a misleading alias.

## Protocol

Versioned HTTP+JSON: `PUT /v0/jobs/<ulid>` is idempotent;
`GET /v0/jobs` returns the caller's bounded newest-first listing, while
`GET /v0/jobs?active=1` filters active jobs before applying that bound;
`GET /v0/jobs/<ulid>` returns status plus the durable non-secret request
details used by `errand status`; SSE with event IDs powers
`GET /v0/jobs/<ulid>/logs?from=<sequence>`; the signal and kill routes control owned
jobs and return `204 No Content` on success; `POST /v0/snapshot/diff` negotiates missing snapshot blobs;
`GET /v0/storage` reports caller-visible storage, including the snapshot cache;
`POST /v0/cache/gc` prunes the snapshot cache;
`POST /v0/jobs/gc` applies bounded owner-scoped receipt retention;
`GET /v0/jobs/<ulid>/changes` transfers the immutable retained workspace-change bundle;
`POST /v0/jobs/<ulid>/ports/<port>/connect` carries one authenticated
full-duplex TCP connection in its request and response bodies;
`GET /v0/change-reconciliation` pages through durable owner- and client-scoped
collection markers so local change GC can reconcile after a lost deletion
response; `POST /v0/change-reconciliation/ack` releases the change hold after that
reconciliation while preserving the replay-prevention lifetime; and
`GET /v0/info` returns facts. A negotiated blob disappearing before
submission returns the machine-readable `snapshot_cache_miss` error code so
the client can retry the same job ID with a complete snapshot. Curl-debuggable;
the route prefix is the request-protocol version; receipt and change-bundle
versions apply only to their persisted formats. During pre-release v0 development, mixed daemon and CLI
versions are not a compatibility contract.

## Non-goals

- Not a CI system: no triggers, no pipelines, no DAGs.
- Not interactive: no stdin, no PTY, no full-screen programs in v0.
- No reverse port forwarding, UDP forwarding, public listeners, or persistent
  service exposure. Attached TCP forwards are client-initiated and ephemeral.
- Not a security boundary against hostile code — see the grant-equals-shell
  belief and Receipt trust. Containment of hostile workloads is Atlas's
  job.
- No web UI. No wake-on-LAN, offline queueing, or store-and-forward.
- No arbitrary-host discovery or scheduler-style fan-out. `errand ps` may
  query the caller's finite, explicit configured-peer set; partial failure is
  reported with a nonzero exit status.
- No arbitrary-host scanning; discovery is limited to the caller's own
  tailnet node list and the errand port, and never writes config by itself.
- No execution tracing / attestation; the receipt claims only what errand
  observed.

## Build order

1. **Transactional core:** tailnet transport, one explicit peer, host
   backend, non-interactive jobs — with at-most-once admission (ID +
   digest, 409 on mismatch), consistent-or-refused snapshots, safe archive
   extraction, framed resumable logs, two-layer exit status, hard limits
   with busy semantics, full receipt format. The boring correctness goes
   first, not last.
2. Disconnect/reattach (`--detach`, `attach`, `ps`) and daemon-restart
   reconciliation.
3. **Content-addressed snapshot cache** (amendment, 2026-08-28): the
   manifest already carries per-file hashes, so sync becomes negotiation —
   the client sends the manifest, the runner replies with the hashes it is
   missing, the client ships only those blobs, and the runner materializes
   the workspace from its cache. The cache is errand-owned persistent
   state with a TTL and size limit, listed and pruned via `errand gc cache`. The
   job's workspace still dies with the job, so the cleanup contract is
   unchanged; what persists is cache with an explicit lifecycle. This is
   what makes a tight edit-run loop cheap instead of a full re-ship per
   run.
4. Automatic workspace-change retention, staging, success-only apply defaults,
   clean-or-refuse merging, explicit conflict materialization, and
   failure-artifact retention.
   **Milestone 4.5 amendment:** attached TCP forwarding over the existing
   authenticated transport, with host-loopback support and a backend-owned
   job-endpoint seam for later isolated runtimes.
5. Rootless container backend + named-cache model.
6. SSH transport: Unix-socket listener with peer-credential identity,
   `ssh://` peers with ControlMaster sharing, and a tailnet identity provider
   abstraction that also supports macOS runners.
7. Nix backend.
8. Facts-based `--where` selection with admission-time revalidation.

Milestone 1 alone replaces `scripts/remote-check` in the Atlas workflow and
proves the shape on a real daily need.

## Open questions (carried into implementation, none blocking)

1. macOS receiving story: launchd for `errand serve`; which backends make
   sense there (no systemd scopes, no rootless podman by default)?
2. Privilege separation (`errandd` vs worker account): post-v0, unless
   adversarial receipt integrity gets promoted into the North Star.
