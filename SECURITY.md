# Security Policy

## Supported Versions

Errand is in early v0 development. Security fixes target the current
`main` branch. Older commits and development snapshots are not separately
supported unless the issue also affects current `main`.

## Reporting a Vulnerability

Please report vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/lydakis/errand/security/advisories/new).
Do not open a public issue for an undisclosed vulnerability.

Include the affected revision, required access, realistic impact, and the
smallest reproduction you can provide. Please avoid including real credentials,
tokens, or unrelated private data.

## System and Scope

Errand runs jobs only on explicitly selected or configured machines. Its
read-only discovery command may probe candidate runners returned by the
caller's own tailscaled node list. Tailnet requests authenticate through
Tailscale identity and are authorized by application capabilities or a
runner-local `allow_users` list, subject to overriding `deny_users` entries.
SSH requests authenticate through SSH and
bridge to a private Unix socket; the daemon grants local requests only when
kernel peer credentials match its OS user. Jobs run directly as the runner's OS
user, transfer workspace snapshots, and can return selected workspace changes
to the originating client.

Local clients can use the private Unix socket directly with `--on local`.
Before sending requests, local clients and setup verify the connected server's
kernel-attested UID matches their effective UID. This also protects custom
socket paths against impersonation by another local user.
All connection paths reach the same daemon, HTTP handlers, job queue, and state.
The daemon accepts tailnet HTTP traffic on its network listener; SSH starts
an `errand _stdio` bridge to the daemon's private Unix socket. They retain
distinct ownership principals: a tailnet user or node and a local OS user
are not interchangeable identities for access to an existing job.

This policy covers the CLI, daemon, HTTP protocol, authorization, snapshots,
archives, caches, receipts, retained changes, local change application, process
cleanup, attached TCP forwarding, configuration and profiles, local access
management, diagnostics, and release packaging and publication workflows.
The product and configuration contracts are in [docs/DESIGN.md](docs/DESIGN.md)
and [docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Threat Model and Security Invariants

The following properties must hold:

- Usage telemetry in stable releases defaults on after a first-invocation
  notice that sends no events. Explicit personal opt-in can bypass the notice;
  personal opt-out and `DO_NOT_TRACK=1` prevent sending.
  Workspace files, profiles, and runner configuration cannot enable or redirect
  it. Its fixed event schema excludes commands, arguments, paths, file contents,
  environment data, logs, peer addresses, and raw errors. Preview never sends
  telemetry; delivery failures never change job behavior or CLI exit status.
  See [usage telemetry](docs/TELEMETRY.md) for the complete data contract.

- Requests fail closed unless the caller has the required Errand action.
- The runner's saved `transport` preference selects `both`, `ssh`,
  `tailscale`, or `local`. SSH-only mode disables the network listener. Tailscale-only
  mode rejects SSH bridging and local job operations, while retaining
  same-user Unix-socket access to `/v0/info` and `/v0/setup/quiesce` for health
  checks and safe setup restarts. Local-only mode permits same-user socket
  jobs, disables the network listener, and refuses Errand SSH bridging.
  Setup refuses differing service definitions before local-only configuration
  changes unless explicitly replaced with `--force`, and verifies the running
  daemon reports local-only mode before declaring success.
  Plain setup preserves local-only mode; enabling remote access requires
  an explicit transport change.
- Setup preserves existing authorization policy when changing transports
  unless the operator explicitly requests a configuration rewrite with `--force`.
  Unavailable Tailscale may defer first-time activation on a new default
  runner, but must not silently disable an established tailnet listener.
  Setup validates its planned configuration and checks for active jobs before
  rewriting the configuration or restarting a managed service; dry runs do
  neither.
- An active exact `deny_users` login match overrides tailnet capabilities and
  `allow_users`. Saved access edits take effect only after daemon restart;
  tailnet login denials do not revoke SSH or Unix-socket access, deny tagged
  nodes without that login, or terminate existing jobs and streams.
- Storage inspection (`read-own`) exposes aggregate fetched-change counts and
  bytes for the runner OS account, alongside shared cache usage. It does not
  expose fetched filenames, contents, workspace paths, or job access across
  ownership principals.
- Job status, logs, changes, signals, forwarding, and collection respect the
  authenticated ownership boundary.
- Persistent workspaces are explicitly created and owner-scoped. Creation and
  use require `submit`, reads require `read-own`, and removal requires `gc-own`.
  Durable membership tracks each job and prevents deletion during use.
  Cleanup and signals must target the individual job's process group and marker,
  never processes selected by shared workspace or cache directory alone.
  Restart verifies the boot and group leader's birth identity before signalling.
  A recorded group from a previous boot is gone; missing or ambiguous identity
  within the same boot must leave cleanup unresolved. Escaped descendants
  require a visible inherited marker. Concurrent jobs share mutable files and
  cache contents; they do not provide isolation from each other.
  The last member alone releases shared cache leases and removes their bindings.
  Recovery must confirm process cleanup before releasing each job's lease; an
  unreadable receipt must not cause leased workspace files to be deleted.
  Each job records a durable workspace reference before lease publication.
  Corrupt lease metadata protects that job's runtime state without preventing
  unrelated jobs from starting or cleaning up. Bulk workspace I/O must not hold
  the admission mutex needed by status and cancellation.
  Persistent files are not subject to ordinary job/cache GC and remain until
  explicit workspace removal. They are not isolated from other processes
  running as the same OS user.
- Push staging and application require `submit` and the authenticated workspace
  owner. Mutation endpoints accept immutable workspace IDs, not names. Uploads
  are bounded and validated outside the live tree, and cache paths are excluded.
  Transfer state lives outside working trees and binds application receipts to
  destination directory identity. Local push and checkpoint-aware fetch require
  the recorded originating checkout. New conflicts leave selected files untouched
  unless the caller explicitly requests conflict materialization. Transfers do
  not isolate concurrent user commands. Transfer collection requires `gc-own`,
  respects ownership, and protects accepted source checkpoints and pending applies.
- Job identifiers, manifests, archives, cache addresses, and change bundles
  cannot escape their intended state, workspace, staging, or destination roots.
- Archive extraction validates paths, types, symlink targets, declared hashes,
  and size limits before trusting transferred content.
- Retained changes are verified and applied only through the explicit,
  conflict-safe application path. They cannot silently widen their destination.
- No initiator environment value or credential is forwarded implicitly. Jobs
  receive a small runner-side allowlist (`PATH`, `HOME`, `USER`, `LOGNAME`,
  `LANG`, and `TMPDIR`) plus values explicitly declared by the caller. Receipt
  metadata does not retain the declared values. Ambient variable forwarding
  requires personal configuration, an explicitly selected profile, or CLI
  `--passenv`; nonempty top-level workspace `env.pass` is rejected.
  Configuration and doctor diagnostics expose names and provenance, not values.
- Workspace defaults and explicitly selected profiles may choose personally
  configured peer aliases, apply preferences, and session forwards. They cannot
  define peer transports or bypass snapshot-boundary protections. Profiles are
  never selected automatically. A workspace preference for built-in `local`
  requires personal opt-in (`default_peer = "local"` or `[peers.local]`),
  an explicitly selected profile choosing local, or CLI `--on local`.
  Attachment profiles cannot retarget a job or
  apply run environment, workdir, or apply preferences to it.
- Retries cannot execute an admitted job twice. Ambiguous state is reported and
  is never treated as permission to replay execution.
- Attached TCP forwarding requires the appropriate action and ownership of a
  running job, and creates only client-local loopback listeners.
- Peer discovery probes only online nodes returned by the caller's own tailnet
  at the fixed Errand port. It must not scan arbitrary hosts or write client
  configuration.

## Reportable Findings and Severity

Report authentication or authorization bypasses, cross-owner access or control,
unexpected command execution without an equivalent execution grant, secret
disclosure, replay of an admitted job, unsafe archive or path handling, writes
outside protected roots, unsafe local change application, or bypasses of
documented resource and forwarding boundaries.

Assess severity from realistic reachability and additional authority gained.
Unauthenticated execution, cross-owner access, credential disclosure, or writes
outside a protected client or runner boundary are high-impact findings.

## Intended Authority and Accepted Limitations

- A `submit` grant intentionally provides shell-equivalent authority as the
  runner's job OS user. The caller chooses the executable and argv. Direct
  execution of those values is expected behavior, not command injection.
- Host jobs run trusted code and are not isolated from the runner account.
  Errand is not a containment boundary for hostile workloads.
- Transport preferences govern Errand's connection paths, not OS account
  access. Local-only and Tailscale-only modes do not disable the host's SSH
  service or revoke shell access. A same-account SSH login can run a local
  client, access the socket directly, or change the daemon configuration.
  Local-only mode prevents Errand from enabling remote entry points; it
  cannot distinguish local code from code launched through an existing shell
  login under the same UID.
- For host jobs, forwarding's job check is an ownership and liveness gate, not
  a network-isolation boundary. The tunnel can reach any service listening on
  the runner's shared IPv4 or IPv6 loopback at the selected port.
- In v0, jobs and daemon state share an OS user. Receipts are diagnostic and
  append-only as emitted by Errand, but are not tamper-evident against a local
  administrator or a hostile job with the same OS authority.
- Resource consumption inherent in an authorized arbitrary command is not
  independently reportable unless it bypasses an Errand-enforced limit or
  crosses another caller's boundary.
- `errand serve --insecure-no-auth` is an explicitly dangerous, test-only mode.
  A finding that requires this flag alone is outside the supported security
  model. Secure operation must remain fail-closed when the flag is absent.
