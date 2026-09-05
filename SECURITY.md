# Security Policy

## Supported Versions

Errand is under pre-release v0 development. Security fixes target the current
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

This policy covers the CLI, daemon, HTTP protocol, authorization, snapshots,
archives, caches, receipts, retained changes, local change application, process
cleanup, attached TCP forwarding, configuration and profiles, local access
management, diagnostics, and release packaging and publication workflows.
The product and configuration contracts are in [docs/DESIGN.md](docs/DESIGN.md)
and [docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Threat Model and Security Invariants

The following properties must hold:

- Requests fail closed unless the caller has the required Errand action.
- An active exact `deny_users` login match overrides tailnet capabilities and
  `allow_users`. Saved access edits take effect only after daemon restart;
  tailnet login denials do not revoke SSH or Unix-socket access, deny tagged
  nodes without that login, or terminate existing jobs and streams.
- Job status, logs, changes, signals, forwarding, and collection respect the
  authenticated ownership boundary.
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
  never selected automatically; attachment profiles cannot retarget a job or
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
