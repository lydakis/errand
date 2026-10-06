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

Errand runs jobs only on explicitly selected or configured machines, or on a
machine a configured cloud peer leases for the caller (see below). Its
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

A runner with a `[cloud]` section is a cloud peer. When `--where` matches no
reachable runner, the client asks configured cloud peers for a lease. The
cloud peer rents a machine through a Lambda account or operator-written
provider commands, installs errand on it, and reports how to reach it. Jobs
then go from the client to the leased machine directly, as to any runner; the
cloud peer never relays jobs. Leases belong to the ownership principal that
asked for them, not to one device. The cloud peer watches each leased machine
and releases it by its idle and lifetime rules.

Windows runners are experimental and take remote requests over the tailnet
only; SSH bridging is not supported there. On
Windows, the local socket identifies peers by their process token's user SID
instead of a numeric UID, and jobs run in a Job Object instead of a process
group.

This policy covers the CLI, daemon, HTTP protocol, authorization, snapshots,
archives, caches, receipts, retained changes, local change application, process
cleanup, attached TCP forwarding, configuration and profiles, local access
management, diagnostics, cloud peers and leases (the lease endpoints, lease
records, provider commands, the Lambda provider and what it installs on
leased machines), the experimental Windows runner, and release packaging and
publication workflows. The product and configuration contracts are in
[docs/DESIGN.md](docs/DESIGN.md), [docs/CONFIGURATION.md](docs/CONFIGURATION.md)
and [docs/CLOUD.md](docs/CLOUD.md).

## Threat Model and Security Invariants

The following properties must hold:

- The Errand CLI and daemon do not collect or send usage analytics.

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
  On Windows that scope is a Job Object the job's processes cannot leave,
  and which ends them when the daemon exits.
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
  is never treated as permission to replay execution. A client that stops
  hearing a running job's heartbeat reports the job's state as unknown and
  does not resubmit it.
- On Windows, the runner refuses to start a `.bat` or `.cmd` program with an
  argument that `cmd.exe` would reinterpret (`"`, `%`, `^`, `&`, `|`, `<`,
  `>` or a line break), so argv cannot become a different command.
- Output that Errand captures from helper processes it runs itself, such as
  provider commands and `ssh`, is size-limited.
- Attached TCP forwarding requires the appropriate action and ownership of a
  running job, and creates only client-local loopback listeners.
- Peer discovery probes only online nodes returned by the caller's own tailnet
  at the fixed Errand port. It must not scan arbitrary hosts or write client
  configuration.

Cloud peers and leases:

- Leasing needs both the `lease` and `submit` actions on the cloud peer, and
  happens only for `--where` requirements no reachable runner matches. A
  `--where '*'` request never rents. Clients ask only configured peers (and
  the local runner) for offers and leases, never discovered or unconfigured
  hosts.
- Lease requests and device key admissions must be `application/json` and
  are refused when they carry an `Origin` header, so a web page cannot start
  a rental.
- A lease belongs to the ownership principal that requested it: a tailnet
  user, a tagged node, or the cloud peer's local OS user for requests over
  SSH or the socket. Listing, reading, admitting a device key to,
  withdrawing and releasing a lease all require that principal. A caller
  without `submit` is shown no lease targets, progress or errors.
- Any request from a principal that has lost `submit` on the cloud peer ends
  all of that principal's leases, whatever the request asked for.
- A leased machine admits only its owner and the cloud peer. A Lambda
  machine on the tailnet allows the tailnet login that asked for the lease
  plus the offer's `allow_users`; a Lambda lease requested without a tailnet
  login and without `allow_users` is refused before anything is rented. For
  a machine reached over SSH, the cloud peer adds the public key of each of
  the owner's devices that asks for the lease, and a Lambda machine admits
  only those keys and the cloud peer's own. A provider command receives the
  first device's public key and decides how its machine admits it. No
  private key leaves its device.
- A lease that carries an SSH host key is reached only with that key pinned,
  by the client and by the cloud peer. The Lambda provider generates a fresh
  host key for every lease. Without a host key, the user's own SSH
  configuration and `known_hosts` apply.
- Provider-command and Lambda targets reach a runner by `url` or `ssh`
  only; a lease cannot name a local socket.
- An offer's facts are a claim. A lease becomes ready only once its machine
  answers as an errand runner whose measured facts match the request, and
  the client checks them again before using it.
- Values from the caller (`where`, login, SSH public key) reach provider
  commands only as environment variables, never as arguments or shell text,
  and an SSH key must parse as one public key. The Lambda provider sends
  everything it installs as file contents in one archive over SSH, so no
  value is quoted into a shell or unit file.
- The Lambda API key and the tailnet auth key are read only from regular
  files owned by the cloud peer's user that no other user can read (mode
  `600` on Unix; on Windows, readable only by that user, SYSTEM and
  Administrators). Errors name these files by what they hold, not by path.
  The tailnet auth key goes to the machine only over its pinned SSH
  connection, never in Lambda launch metadata or a command line, and the
  install removes it afterwards.
- The errand build installed on a Lambda machine is checked to be a Linux
  errand build for the machine's architecture before anything is rented. A
  downloaded release must match the release's published checksums, and the
  machine receives the private copy that was checked.
- A lease is recorded in the cloud peer's state directory, with how to
  release it, before anything is acquired. While the cloud peer runs, every
  ready lease is released once its runner has been idle for the offer's
  `idle_timeout` (an unreachable runner counts as idle), or at its
  `max_lifetime` even with a job running, and a failed release is retried
  until it succeeds. A restart releases launches it interrupted. Removing an
  offer only stops new leases. A Lambda lease counts as released only once
  Lambda lists its instance as terminated.
- A cloud peer holds at most `max_leases` active leases across all callers.
  Unless `max_price_per_hour` is `0`, the Lambda provider never launches a
  type whose price, as Lambda lists it right before the launch, is above
  that cap.

## Reportable Findings and Severity

Report authentication or authorization bypasses, cross-owner access or control,
unexpected command execution without an equivalent execution grant, secret
disclosure, replay of an admitted job, unsafe archive or path handling, writes
outside protected roots, unsafe local change application, or bypasses of
documented resource and forwarding boundaries. For cloud peers, that includes
starting a lease without the `lease` and `submit` actions or from a browser
page, seeing or using another principal's lease, a leased machine admitting
anyone but its owner and the cloud peer, a provider or Lambda secret leaving
the cloud peer other than as documented, and a lease outliving its idle and
lifetime rules while the cloud peer runs.

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
- A `lease` grant intentionally lets the caller spend money on the cloud
  peer's provider account, within `max_leases` and, for Lambda,
  `max_price_per_hour`. These limits hold only while the cloud peer runs;
  they are not a spending cap on the provider account.
- Configuring a cloud peer trusts it as much as a configured peer: it chooses
  the machines that receive jobs, workspace snapshots and `--passenv`
  values. The cloud peer has root on the Lambda machines it leases, and it
  logs into them with its own key to install and watch them.
- A device key admitted to a leased machine reached over SSH gets a shell
  as the machine's login, the same authority as `submit` there. On Lambda
  images that login can use `sudo`.
- Taking `submit` away on the cloud peer does not reach into machines
  already leased. The owner's leases end at its next request to the cloud
  peer, and until then a leased machine still admits it, at most until
  `idle_timeout` or `max_lifetime`. Taking only `lease` away leaves existing
  leases to their idle and lifetime rules.
- Provider commands are operator configuration. They run as the cloud peer's
  user with its full environment, and separating successive owners of one
  machine is their responsibility. The bundled `docs/cloud/static-pool.sh`
  frees a claim without stopping jobs still running or removing files.
- Anyone with access to the Lambda account can read a lease's launch data,
  which holds that machine's SSH host private key, and can control or
  impersonate its machines. Downloaded release builds are trusted as far as
  the GitHub release and its checksum file are. A Lambda machine reached over
  the tailnet installs Tailscale from `tailscale.com` if its image lacks it.
- `errand serve --insecure-no-auth` is an explicitly dangerous, test-only mode.
  A finding that requires this flag alone is outside the supported security
  model. Secure operation must remain fail-closed when the flag is absent.
