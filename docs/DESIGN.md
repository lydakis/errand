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
> without replay on retry — stream an ordered result, return declared
> artifacts safely, clean up what errand owns, and leave an append-only
> receipt.

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
  daemon-observed order, selected signals, exit status.
- Not reproduced: ambient local environment, an interactive terminal
  (no stdin, no PTY — non-interactive only), the local network, the local
  platform. Results are native to the *target*; a binary built on x86 Linux
  is an x86 Linux binary, and the receipt says so.
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
  Files on disk. (One protocol, not necessarily one literal socket — tailnet
  HTTP and LAN mTLS may want separate listeners sharing a handler stack.)

## Shape: symmetric peers

One Go binary. Every machine that installs it can both delegate and receive;
there is no controller. This is a **symmetric peer-to-peer runner, not a
mesh**: no forwarding, no gossip, no shared scheduler, no distributed
membership — and none should be accidentally grown.

Peers are **explicit** in personal config; `errand peers` probes configured
peers' `/v0/info`; `--where` searches only those. No tailnet scanning.
Probing facts is implied by holding any errand capability (plus
network-level access); no separate probe grant exists in v0.

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

Two transports behind one peer abstraction:

1. **Tailnet (preferred).** Daemon binds the host's tailnet address.
   Callers are identified via destination-scoped LocalAPI WhoIs
   (`WhoIsForIP`, constraining the capability map to the address actually
   accessed); milestone 1 requires Tailscale 1.100 or newer and fails
   closed on older or unversioned daemons. errand stores zero credentials
   in this mode.
2. **Local network.** errand owns identity: a pairing ceremony performs an
   authenticated key exchange (PAKE / short-authentication-string — the
   short code is never sent as a bearer secret over the unauthenticated
   channel), is single-use, short-lived, and rate-limited, and pins stable
   device public keys. Connections are mTLS between paired devices.
   `errand peers revoke` removes the key **and closes active sessions**.

   **A pair is a directed edge.** Initiating a pairing grants the
   *initiator* the right to submit to the *responder* — never the reverse.
   This mirrors tailnet authorization, where direction comes from the
   grant's `src` → `dst`; on LAN it comes from who initiated. A
   bidirectional relationship is two one-way pairs, each independently
   revocable. In errand vocabulary the sides of the authority edge are the
   **caller** (may submit) and the **runner** (accepts): each machine keeps
   an authorized-callers list (device keys that may submit to it) and a
   known-runners list (machines it can submit to). Ceremony UX: the runner
   displays the short code (works headless — you are SSH'd in anyway), the
   caller enters it.

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
      { "actions": ["submit", "read-own", "kill-own"] }
    ]
  }
}]
```

The capability carries an **action schema from day one** (not a boolean):
`submit`, `read-own`, `kill-own`, later `read-all`, `manage-caches`.
Matching grants are additive; errand merges capability objects
deliberately (union of actions).

Authorization is checked on submission **and on every** logs, fetch,
signal, kill, and listing request. Revocation prevents further control and
retrieval; it does not retroactively kill an already-admitted job (that
would be a different guarantee, possible later).

### Ownership

`read-own` / `kill-own` need a defined owner principal:

- **Tailnet:** the authenticated Tailscale *user* when WhoIs provides one
  (so a job submitted from the phone can be attached from the laptop),
  otherwise the exact node identity.
- **LAN:** the paired device public key. Device-scoped — no cross-device
  attach on LAN in v0.
- Cross-transport or manually grouped identities are not supported in v0.

`admission.json` records both the user identity and the exact device
identity, even when ownership keys off only one.

### Job handles are peer-qualified

In a controllerless system a bare ULID doesn't say where the job lives. The
canonical handle is `peer/ulid`:

```
job cabal/01K4Q8ZJ2M...
```

Printed handles (especially for detached jobs) are always peer-qualified
and portable across initiators. The CLI accepts a bare ULID only when local
submission history resolves it unambiguously.

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

Per-project defaults (portable, in the repo):

```toml
# .errand.toml — describes the project, never names your machines
[run]
backend = "container"
image   = "rust:1.86"          # receipt records the resolved digest

[env]
pass = ["RUST_BACKTRACE"]      # forwarded from initiator by name only
set  = { CI = "1" }

[[outputs]]
path    = "target/release/atlasctl"
collect = "success"            # success | always   (target-side collection)
apply   = "auto"               # auto | manual      (client-side application)

[[cache]]
name  = "cargo-registry"
path  = "/usr/local/cargo/registry"
scope = "project"              # project | user | peer (opt-in beyond project)
```

Personal aliases and defaults (`cabal`, default peer, addresses) live in
`~/.config/errand/config.toml`, never in the repository.

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
  lifecycle: declared in config, listed by `errand caches`, removed by
  `errand gc`. Project-scoped by default.
- **Backend stores** — container image layers, the nix store. Managed by
  the runtime, not by `errand gc`; pruning them is the runtime's own
  command, surfaced but never run implicitly.
- **Job state** — always removed per the contract above (receipts are kept;
  they are files you can delete).

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
workspace/       # deleted at cleanup
out/             # staged outputs (retention: see open questions)
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
  `{exit_code, signal, outputs_ok, cleanup_ok, logs_complete}` — a job can
  succeed while collection or cleanup fails, and the receipt says which.
- **Signals behave like signals.** Ctrl-C forwards SIGINT; a second Ctrl-C
  or `errand kill --force` escalates. If contact is lost mid-signal, the
  client prints the peer-qualified handle and admits the process may still
  be running.
- **v0 durability scope:** jobs survive *caller* disconnection. Daemon
  crash / target reboot reconciliation is out of v0; until it exists the
  guarantee is limited to network disconnect, and reconciliation, when it
  comes, recovers via deterministic systemd unit names and never assumes
  absence of evidence means the command did not start.

### Limits

Limits are **fixed at admission and enforced at the relevant execution
phase**: upload size at admission; workspace expansion at unpack; runtime,
log size, and output size during execution and collection. One running job
per peer — a second submission returns `busy`, no queue and no `queued`
state. On hitting the log cap the daemon terminates the job with
`limit_exceeded`, preserving a complete log up to termination — the
fidelity contract promises faithful output, not best-effort truncation.

## Workspace snapshot

- Automatic selection is limited to Git worktrees. A non-Git directory must
  contain `.errandignore` or use the explicit `--include-all` override.
  Filesystem roots are always refused; the user's home directory requires the
  override even when a policy file exists. The client prints the selected file
  count and total bytes before admission.
- Manifest is `.errandignore`, initialized from git ignore rules but
  allowing explicit include/exclude — ignored files are sometimes required
  (generated sources), tracked files are sometimes unwanted.
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

## Outputs

Collection (target-side) and application (client-side) are separate:

- `collect = success | always` — when the target gathers the output into
  `out/`. Failure artifacts (test reports, crash dumps, traces) use
  `collect = "always"`; they are usually the point.
- `apply = auto | manual` — whether an attached client applies it to the
  local tree. Detached jobs can't apply anything (the initiating process is
  gone): outputs stage on the target, and a later `attach --apply` or
  `fetch --apply` applies them after baseline validation. Attaching from a
  different or non-matching workspace only stages locally, never
  overwrites.

Application is conflict-safe, never silent:

1. Record pre-run hashes of destination paths.
2. Download into local staging; validate paths and types.
3. Apply atomically only if destinations are unchanged since the run began.
4. On conflict: leave staged, report, let the user resolve.

### Exit status: two layers

- **Process result:** the remote exit code or signal, recorded exactly in
  `result.json`.
- **Transaction result:** whether execution, required output
  collection/application, and cleanup completed.

CLI behavior: when the transaction succeeds, the CLI exits as the remote
process did. If the remote process exits 0 but a required `apply = auto`
output fails to apply (or cleanup fails), the CLI returns an errand-level
nonzero status and prints `remote_exit=0` — otherwise
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
errand [--on X | --where facts] [--detach] -- <cmd...>
errand peers                    # configured peers, probed facts, reachability
errand peers pair | revoke      # LAN identity ceremony
errand ps [--all]
errand logs <peer/ulid> [-f]    # resumes from last seen seq
errand attach [--apply] <peer/ulid>
errand fetch [--apply] <peer/ulid> [path]
errand kill [--force] <peer/ulid>
errand caches | gc              # errand-owned caches only
```

Without `--detach`: streams, exits per the two-layer status rule — drop-in
for scripts. With `--detach`: prints the peer-qualified handle and returns
— the phone workflow.

## Protocol

Versioned HTTP+JSON; `PUT /v0/jobs/<ulid>` (idempotent), SSE with event
IDs for `GET /v0/jobs/<ulid>/logs?follow=1`, `GET /v0/info` for facts.
Curl-debuggable; the version prefix lets daemon and CLI drift during
upgrades without lying to each other.

## Non-goals

- Not a CI system: no triggers, no pipelines, no DAGs.
- Not interactive: no stdin, no PTY, no full-screen programs in v0.
- Not a security boundary against hostile code — see the grant-equals-shell
  belief and Receipt trust. Containment of hostile workloads is Atlas's
  job.
- No web UI. No wake-on-LAN, offline queueing, or store-and-forward.
- No multi-peer fan-out (`--all` is the first step down the ansible path).
- No tailnet scanning for discovery; peers are explicit config.
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
   state with a TTL and size limit, listed and pruned via `errand gc`. The
   job's workspace still dies with the job, so the cleanup contract is
   unchanged; what persists is cache with an explicit lifecycle. This is
   what makes a tight edit-run loop cheap instead of a full re-ship per
   run.
4. Output collection and conflict-safe application (`collect`/`apply`,
   staging, `fetch --apply`); failure-artifact retention.
5. Rootless container backend + named-cache model.
6. LAN pairing with PAKE and pinned device identities.
7. Nix backend.
8. Facts-based `--where` selection with admission-time revalidation.

Milestone 1 alone replaces `scripts/remote-check` in the Atlas workflow and
proves the shape on a real daily need.

## Open questions (carried into implementation, none blocking)

1. macOS receiving story: launchd for `errand serve`; which backends make
   sense there (no systemd scopes, no rootless podman by default)?
2. Privilege separation (`errandd` vs worker account): post-v0, unless
   adversarial receipt integrity gets promoted into the North Star.
3. Retention: receipts (age out vs `errand gc --jobs`?) and staged outputs
   on the target (how long does a detached job's `out/` survive?).
