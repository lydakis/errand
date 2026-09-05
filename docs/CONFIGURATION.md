# Configuration

Run submission and `errand config` share one resolver. For peer selection and
automatic apply, precedence is:

1. Explicit CLI flags.
2. The explicitly selected profile.
3. The selected workspace's `.errand.toml` defaults.
4. Personal defaults.
5. Safe defaults: no automatic apply and no implicit peer.

Profiles can also set a workdir, overriding the caller's relative directory;
an explicit CLI workdir wins. Environment settings use the same layers, with
merge rules described below. Session forwarding and artifact declarations use
list replacement at each layer. Detachment and broad snapshot opt-in remain CLI options.

Personal, runner, and workspace configuration reject unknown keys, including
nested settings. Errors identify the file and key so misspellings cannot
silently change behavior. Peer edits also refuse to rewrite invalid config.

## Personal configuration

`~/.config/errand/config.toml` holds aliases and transport details. When
`XDG_CONFIG_HOME` is set, use `$XDG_CONFIG_HOME/errand/config.toml` instead.

```toml
default_peer = "cabal"
apply_on_success = false

[peers.cabal]
url = "http://cabal:7443"

[peers.mac-mini]
ssh = "mac-mini"
remote_command = "/usr/local/bin/errand"
remote_socket = "/Users/you/.errand/errand.sock"
```

Existing personal syntax is unchanged. An explicit `apply_on_success = false`
is retained through peer edits and appears as a personal choice in provenance.
The resolver reads the personal file once per resolution; malformed TOML is
an error even when CLI flags override its settings.

## Workspace configuration

```toml
# .errand.toml
[workspace]
root = true

[run]
peer = "mac-mini"

[changes]
apply_on_success = false
```

`run.peer` is an optional convenience preference. Its alias must exist in the
caller's personal configuration. Unknown or empty preferences fail with their
source file identified; they never silently fall back to `default_peer`.
`--on` or `--url` overrides the preference. Addresses, SSH commands, and socket
paths remain in personal configuration.

Only configuration accepted from the selected root contributes preferences.
A nested `.errand.toml` does not override a marked ancestor's settings.
Automatic boundary discovery retains its ownership and directory-permission
checks; explicit `--workspace-root` retains its existing shared-root behavior.
A preferred peer does not imply `workspace.root = true` and does not relax
snapshot selection rules.

With `--no-snapshot`, the current directory is the configuration root. Ancestor
markers cannot choose a peer or apply policy for that invocation. Without an
explicit workdir, normal snapshot runs preserve the caller's relative directory;
`--workdir .` or `--workdir=` explicitly selects the workspace root.

`--apply` enables automatic apply; `--no-apply` disables it. Explicit boolean
values are honored: `--apply=false` disables it, and `--no-apply=false` enables
it. Supplying both flag names is an error, regardless of their values.

Workspace configuration should contain no secret values. Keep project commands
and service orchestration in scripts, Make, Just, or Compose.

## Profiles

Define profiles in either personal configuration or the selected workspace's
`.errand.toml`, using the same syntax in both:

```toml
[profiles.build.run]
peer = "cabal"
workdir = "packages/api"       # relative to the selected workspace root

[profiles.build.changes]
apply_on_success = false

[profiles.format.run]
workdir = "."                  # explicitly use the workspace root

[profiles.format.changes]
apply_on_success = true
```

Select one profile explicitly:

```sh
errand --profile build -- go test ./...
errand --profile build --on mac-mini -- go test ./...
errand --profile format -- gofmt -w .
errand config --profile build --json
```

Without `--profile`, no profile is active. Unknown names fail even if other
CLI flags supply all settings. An empty `--profile` value is an error.

When the selected workspace and personal config define the same profile name,
the workspace definition replaces the entire personal definition. Omitted
fields inherit ordinary workspace/personal defaults, not fields from the
shadowed personal profile. An empty workspace profile (`[profiles.build]`)
therefore opts out of all settings in that personal profile. Other personal
profiles remain available by name.

Profiles support `run.peer`, `run.workdir`, `changes.apply_on_success`,
`env.set`, `env.pass`, `session.forward`, `artifacts.paths`, and `caches`. Explicit `false` and empty workdir values override
lower layers. Unsupported keys and incorrect value types are errors when
loading configuration, including in inactive profiles. There is no profile
inheritance, automatic profile selection, command definition, or transport
configuration inside profiles.

Workspace profiles follow the same boundary and trust rules as workspace
defaults. With `--no-snapshot`, only profiles in the current directory and
personal config are considered; the resulting workdir must be empty or `.`.
Use `--workdir .` to override a profile's nested workdir for such an invocation.
Profiles cannot move the snapshot boundary or enable broad snapshot selection.

## Named caches

Cache bindings use a table of stable names and exact workspace-relative directories:

```toml
[caches]
compiler = "target"

[profiles.clean.caches]
```

The empty profile table clears inherited caches. Personal, workspace, selected
profile, and CLI settings use that precedence, replacing the whole binding list.
Use repeatable `--cache NAME=PATH` for a run override, or `--no-caches` to disable
bindings. `errand config` shows the effective list and its source.

Caches are runner-local and disposable. They are excluded from snapshots and
retained results even when artifact declarations include them. The runner
refuses concurrent reuse of a leased cache until process cleanup completes.
See [named caches](NAMED_CACHES.md) for lifecycle behavior, project identity,
`df`, `gc cache`, and the runner's separate `[named_cache]` budget.

## Artifact declarations

Artifacts add workspace outputs to ordinary change retention, including files
excluded by Git or `.errandignore`. Use the same table in personal config,
workspace config, or an explicitly selected profile:

```toml
[artifacts]
paths = ["reports", "dist/app"]

[profiles.test.artifacts]
paths = ["reports"]
```

Each path names an exact file or directory relative to the workspace root,
including when the command runs in a nested workdir. Directories include their
descendants. Paths must be clean and relative, with no glob syntax, backslashes,
Git metadata, or Errand's reserved `.errand-change-` namespace. Duplicate paths
are errors. Lists are bounded to 10,000 entries, 8 KiB per path, and 256 KiB total.

The highest specified list replaces lower layers: CLI, selected profile,
workspace, personal, then no declarations. `paths = []` clears inherited
declarations; omitting it inherits. Workspace profiles still replace personal
profiles of the same name as a whole.

```sh
errand --artifact reports --artifact dist/app -- make test
errand --no-artifacts -- make test
errand config --profile test --json
```

`--artifact` is repeatable and replaces the configured list. `--no-artifacts`
clears that list; supplying both flag names is an error. It only disables the
extra declarations; ordinary workspace change retention continues.
Submission prints the effective declarations, and `errand config` reports
their source. The immutable job spec and receipt record the paths.

Declarations never upload local ignored files or change the snapshot boundary.
They select outputs from the job's workspace after execution, including failed
commands, subject to the existing byte, entry, and cleanup limits. Missing
outputs are allowed, and symlinks follow the existing safe-link retention rules.
`--no-snapshot` accepts declarations too, using the current directory's config.
Both client and runner must support artifact declarations; older runners refuse
the request rather than executing with a different admission digest.

Retrieve artifacts with ordinary `fetch`, `fetch --output DIR`, or `fetch --apply`.
Workspace-relative paths and the existing clean-or-refuse merge rules are
unchanged. In particular, an ignored local directory wasn't part of the submitted
baseline: a different remote addition at that path can conflict. Use export to
retrieve it separately, or resolve the local collision before applying. Errand
never treats an artifact declaration as permission to overwrite local files.

## Environment settings

Put non-secret literals in `env.set`. Ambient variables may be selected by
personal `env.pass`, an explicitly selected profile, or CLI `--passenv`.
A workspace's top-level `env.pass` must be absent or empty: repository defaults
cannot grant themselves access to the initiating shell's environment.
For example, a workspace can define:

```toml
[env]
set = { CI = "1" }

[profiles.integration.env]
pass = ["NODE_AUTH_TOKEN", "SERVICE_API_TOKEN"]
```

```sh
errand --profile integration -- pnpm test:local
errand --profile integration --env CI=0 -- pnpm test:local
errand config --profile integration --json
errand doctor --profile integration
```

Only the explicitly selected profile contributes settings. Keep sensitive
variable names in the profiles that need them: selecting that profile forwards
their local values to the selected runner. Do not store secret values in
configuration files. Personal `env.pass` authorizes forwarding across runs;
review a workspace profile's variable names before explicitly selecting it.
Docker and database setup remain repository scripts.

A nonempty top-level workspace `env.pass` stops run/config/doctor resolution
with a migration hint, even when CLI options would override it. Move those
names to personal configuration or an explicitly selected profile, or remove
the workspace default and use `--passenv`. `pass = []` remains allowed in
workspace defaults to clear inherited forwarding; it never selects a value.

Settings resolve from personal defaults through workspace and selected profile
to CLI overrides. `set` merges by variable name; higher layers replace lower
values, including with an empty string. A specified `pass` list replaces all
inherited forwarding. `pass = []` clears forwarded variables while retaining
literal settings. Omitting `pass` inherits it. A higher-layer literal replaces
forwarding of that name, and a higher-layer forwarded name replaces a literal.
One config layer cannot put the same name in both `set` and `pass`.

Repeated CLI `--passenv NAME` options form a replacement pass list. CLI
`--env NAME=VALUE`/`-e NAME=VALUE` overrides that name, including a `--passenv`
of the same name. The last repeated literal wins. An omitted CLI pass list
inherits config forwarding. Names must be nonempty and contain neither `=`
nor NUL; literal values must be strings without NUL. Unknown environment keys
and incorrect types fail configuration loading, even in inactive profiles.

Every forwarded variable is required, including explicit CLI `--passenv`.
An unset variable stops submission before snapshot preparation, runner contact,
or local submission-state creation. A variable set to an empty string is
available and is forwarded as empty. The client captures values once before
preparing the submission. Runner receipts retain names and `literal`/`passenv`
provenance, not values; commands can still print values into their own logs.

`errand config` lists resolved variable names, kinds, sources, and availability
without showing values, including literal values. Missing variables remain
visible in its successful inspection report. `errand doctor` reports missing
variables as a failed environment check and skips its runner probe. Both
commands accept `--env` and `--passenv` to inspect the same overrides as a run.
JSON contains an `environment` array with `name`, `kind`, `source`, and
`available`; it contains no environment values.

## Inspect without submitting

```sh
errand config
errand config --json
errand config --on cabal --no-apply
errand config --profile build --json
errand config --no-snapshot --json
```

The table and JSON show the selected profile (when active), effective peer, endpoint, configured SSH options,
workspace root, workdir, project label, apply policy, and snapshot mode, with
sources. An empty workdir means the workspace root. SSH URLs are the configured
endpoints, before the client assigns internal transport identities.

Inspection uses the same run defaults and override flags as submission. It
reads configuration and boundary metadata locally, makes no runner connections,
writes no client state, and does not resume pending automatic applications.
It does not hash files or validate a complete job: use `errand peers` to check
runner availability; snapshot policy and remote command validation still happen
when submitting. Peer lifecycle and job-handle commands retain their explicit
targets and personal defaults; a workspace preference only selects new runs.

## Session forwarding

Configure local loopback TCP forwards in personal configuration, the selected
workspace, or an explicitly selected profile:

```toml
[session]
forward = ["3000"]

[profiles.dev.session]
forward = ["8080:3000", "3001"]

[profiles.build.session]
forward = []
```

```sh
errand --profile dev -- pnpm dev
errand config --profile dev --json
errand --profile dev --forward 9000:3000 -- pnpm dev
errand --profile dev --detach --no-forward -- pnpm dev
errand attach --profile dev PEER/JOB
errand attach --no-forward PEER/JOB
```

`session.forward` is an array of strings using the CLI's `[LOCAL:]REMOTE`
syntax. Both ports must be between 1 and 65535. Hostnames, bind addresses, and
duplicate local ports are rejected. Omitting the list inherits lower layers;
`forward = []` explicitly clears it. Each supplied list replaces the whole
previous list: personal defaults, workspace defaults, selected profile, then
CLI. Repeat `--forward`/`-L` to supply multiple CLI mappings. `--no-forward`
clears the list and cannot be combined with either forwarding flag.
`--no-forward=false` leaves configured defaults in effect.

Runs resolve these settings from their selected configuration root, including
`--workspace-root` and `--no-snapshot` behavior. `attach` resolves session
preferences from the current directory's discovered workspace and optionally
`--profile`. Its handle and `--on`/`--url` still determine the target; profile
run targets, environment values, workdirs, and apply settings are not applied
to an existing job. Unknown profiles and malformed configuration still fail.
A later attachment resolves the configuration available at that time. It does
not remember which profile or mappings were used for submission.

A detached run with any effective forwards is rejected before submission;
use `--no-forward` or a profile with `forward = []`. Ctrl-D closes the current
session's listeners and connections. Mappings are never persisted in job specs
or receipts. See [attached TCP forwarding](DESIGN.md#attached-tcp-forwarding-milestone-45-amendment-2026-09-03)
for the transport and lifecycle contract.

`config` and `doctor --json` expose the effective `forward` list with its
source. Inspection validates syntax but does not bind ports or test forwarding
permission, remote port availability, or application readiness. Actual run and
attach commands bind all requested local ports before contacting the runner.

## Runner access

`errand access` manages the runner's saved `allow_users` and `deny_users`
arrays. Run it locally as the runner's OS user, using the config file that
its service loads:

```sh
errand access list --config /path/to/errandd.toml
errand access add --config /path/to/errandd.toml --dry-run friend@example.com
errand access add --config /path/to/errandd.toml friend@example.com
errand access remove --config /path/to/errandd.toml friend@example.com
errand access deny --config /path/to/errandd.toml friend@example.com
errand access undeny --config /path/to/errandd.toml friend@example.com
errand setup --config /path/to/errandd.toml
```

Omit `list` for the default operation. Without `--config`, the path is
`~/.config/errand/errandd.toml`, matching the service installed by `errand setup`,
even when `XDG_CONFIG_HOME` is set. Use `--config PATH` for a runner configured
at another location. Personal aliases, workspace settings, and profiles do
not select a runner config. There is no `--on` or remote edit operation.

List output includes the file path, saved allowlist and denylist, capability
name, and listen setting. It describes saved configuration only, not effective live
authorization. Adding an allowlist login grants full runner access once
activated, unless denied, including command execution as the daemon's OS user.
Logins must be exact,
nonempty strings without whitespace, control characters, or wildcards.

Edits require an existing regular file; missing files and symlinks are
refused. Run `errand setup` first to create a new runner config. Adding an
existing login or removing an absent one is a no-op. Removal clears all
copies of a login. `-n`/`--dry-run` previews the before/after selected list;
previews and no-ops leave the file byte-for-byte unchanged. A real edit
atomically replaces the file with mode `0600`, preserving other TOML values,
including unknown settings, but reformatting the file and removing comments.

No access command contacts tailscaled, edits tailnet grants, restarts a
service, submits a job, or resumes pending automatic applications. Restart
the runner with `errand setup --config PATH` to activate saved edits; setup
refuses to restart while jobs are active. An allowlist removal is not a
complete revocation: capability grants and SSH access remain independent.

`deny LOGIN` adds an exact tailnet login to `deny_users`. Once activated, the
runner refuses requests from that login before applying capability grants or
`allow_users`. Denial wins when a login is present in both lists; the runner
still starts. Existing grants are preserved. `undeny LOGIN` removes all copies
of the denial and restores whatever authorization those grants provide; it
does not add a grant. `add` and `remove` edit only `allow_users`. All four
mutations support dry runs and idempotent edits.

This is a tailnet login policy. It does not deny tagged nodes without that
login, Unix-socket callers, or SSH access to the runner account. Saved denials
do not affect the running daemon until restart, and do not cancel jobs or
terminate existing streams. The test-only `--insecure-no-auth` flag bypasses
all authorization, including denials.

All operations accept `--json`. Listing emits `path`, `allow_users`, `deny_users`,
`capability`, `listen`, and an `activation` reminder. Mutations emit
`operation`, `login`, `dry_run`, `path`, `field`, `before`, `after`, `changed`,
`written`, and `activation`. `field` identifies `allow_users` or `deny_users`.
`changed` describes whether the desired array differs, while `written` is true
only after a successful write. Pass options before the login.

## Diagnose the selected runner

```sh
errand doctor
errand doctor --on cabal
errand doctor --profile build --json
errand doctor --url ssh://my-runner --no-snapshot
errand doctor --config /absolute/path/errandd.toml --json
```

Doctor checks this installation, any configured local runner, and the selected
peer in one report. It resolves the same run settings as `errand config`;
`--on`, `--url`, and profiles retain their usual precedence. `--config PATH`
explicitly includes that local runner configuration without changing the peer.

A machine with no outbound peer skips the peer probe. Unknown aliases, empty
workspace peer preferences, malformed TOML, invalid boundaries, and missing
required environment variables remain errors. Local installation and runner
checks continue independently when run configuration fails. Conversely, a
healthy peer cannot hide a local runner failure.

Healthy human output summarizes passed checks and identifies skipped checks.
Warnings and failures retain their diagnostic details and next steps. Use
`--json` for the complete report, or `--help` for the diagnostic scope.

### Local installation and runner checks

The executable and this shell's PATH are always inspected. Local runner checks
activate when there is a saved runner configuration, an installed or loaded
setup-managed user service, an existing default/configured socket, or an
explicit `--config PATH`. Inaccessible or dangling files are not treated as
absent. A client-only machine reports `local.runner: skipped` with the reason
“Local runner not configured.” If service status is unavailable and no saved
configuration, service definition, or socket exists, the skip explains that
service-manager status could not be established.

For a configured local runner, doctor loads the same runner settings and
defaults as `serve`, then checks:

- Setup's systemd user service or launch agent, plus Linux linger. An installed
  service that is stopped or cannot be queried is an error. Manually managed
  runners can still pass without a setup-managed service definition.
- The configured Unix socket, its private `0600` permissions, and a bounded
  info request that verifies protocol compatibility and caller access. A
  configured socket that disappeared or no longer answers is an error.
- Tailscale identity-provider readiness and listener resolution, skipped when
  `listen = "none"`. The backend must be `Running`; cached identity and IP
  information from a stopped or logged-out backend does not establish readiness.
- Setup's `/usr/local/bin/errand` path for non-interactive SSH callers. A missing
  installation there produces a warning with an absolute `remote_command` hint.

Run doctor as the daemon's user when checking a runner. Use the service's actual
`--config` path for custom configuration. Doctor cannot infer arbitrary service
names, custom service definitions, or `serve` CLI overrides such as `--state-dir`;
put those settings in the runner configuration to inspect them. Socket paths
must be absolute to avoid assuming the service's working directory. Service
status alone does not establish daemon health; the socket probe checks that.

Each service command, identity query, and local info probe has a four-second
deadline; local diagnosis uses a twenty-second context. Service-manager output
is never copied into reports because it may contain environment values.

### Selected peer checks

After resolving run settings and checking required environment variables,
doctor makes one logical `GET /v0/info` probe to the selected peer with a
four-second deadline. Configured SSH aliases retain their `remote_command` and
`remote_socket` settings. The displayed endpoint remains the configured URL.
No other peers are discovered or probed.

For SSH peers, a separate four-second check first verifies non-interactive SSH
and resolves `remote_command` (or the existing `ERRAND_SSH_COMMAND`/`errand`
fallback) with the remote shell's `command -v`. It does not execute the resolved
binary. This check reuses Errand's existing SSH control connection when available,
without creating a control socket or its cache directory. A fresh connection
uses batch authentication and requires an already trusted host key; it never
prompts or adds or updates host keys. Missing binaries get a PATH or
`remote_command` hint; connection failures get SSH authentication/connectivity
hints. On success, the ordinary info probe exercises the actual bridge and its
configured socket. The SSH portion can therefore take up to eight seconds.
Restricted SSH shells must support the command-resolution check.

The peer probe distinguishes connection failures, refused info access, and
responses that do not match the current Errand protocol. A reachable busy peer
produces a warning. Local Unix-socket access does not establish another
machine's tailnet grants; run doctor from the initiating machine to check its
selected peer. Info access does not prove permission to submit, and admission
or capacity can change after the check.

Doctor does not select or hash snapshot files, validate job command
availability, submit jobs, change grants or configuration, restart services,
acquire restart leases, or resume pending automatic applications.

Exit codes are `0` when no check failed (warnings and unconfigured skips are
allowed), `1` when any local or peer check failed or report output failed, and
`2` for invalid command usage. Help exits `0`. With `--json`, reports go to
stdout and include `ok`, `checks`, `scope`, and the resolved `effective` run
configuration when available. `info` belongs to the selected peer; `local_info`
is the local daemon's response, and `socket_path` identifies that local socket.
Local check names have a `local.` prefix. Each check has `name`, `status`
(`ok`, `warning`, `error`, or `skipped`), `detail`, and an optional `hint`.
Usage errors go to stderr without a JSON report.
