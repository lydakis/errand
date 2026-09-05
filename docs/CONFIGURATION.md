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
merge rules described below. Forwarding, detachment, and broad snapshot opt-in
remain CLI options.

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
`env.set`, and `env.pass`. Explicit `false` and empty workdir values override
lower layers. Unsupported keys and incorrect value types are errors when
loading configuration, including in inactive profiles. There is no profile
inheritance, automatic profile selection, command definition, or transport
configuration inside profiles.

Workspace profiles follow the same boundary and trust rules as workspace
defaults. With `--no-snapshot`, only profiles in the current directory and
personal config are considered; the resulting workdir must be empty or `.`.
Use `--workdir .` to override a profile's nested workdir for such an invocation.
Profiles cannot move the snapshot boundary or enable broad snapshot selection.

## Environment settings

Personal config, workspace config, and named profiles accept the same
environment section. Put non-secret literals in `set` and names of variables
from the initiating shell in `pass`:

```toml
[env]
set = { CI = "1" }

[profiles.integration.env]
pass = ["NODE_AUTH_TOKEN", "BLUE_API_KEY"]
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
configuration files. Docker and database setup remain repository scripts.

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

## Runner access

`errand access` manages the runner's saved `allow_users` array. Run it locally
as the runner's OS user, using the config file that its service loads:

```sh
errand access list --config /path/to/errandd.toml
errand access add --config /path/to/errandd.toml --dry-run friend@example.com
errand access add --config /path/to/errandd.toml friend@example.com
errand access remove --config /path/to/errandd.toml friend@example.com
errand setup --config /path/to/errandd.toml
```

Omit `list` for the default operation. Without `--config`, the path is
`~/.config/errand/errandd.toml`, matching the service installed by `errand setup`,
even when `XDG_CONFIG_HOME` is set. Use `--config PATH` for a runner configured
at another location. Personal aliases, workspace settings, and profiles do
not select a runner config. There is no `--on` or remote edit operation.

List output includes the file path, saved allowlist, capability name, and
listen setting. It describes saved configuration only, not effective live
authorization. Adding a login grants full runner access once activated,
including command execution as the daemon's OS user. Logins must be exact,
nonempty strings without whitespace, control characters, or wildcards.

Edits require an existing regular file; missing files and symlinks are
refused. Run `errand setup` first to create a new runner config. Adding an
existing login or removing an absent one is a no-op. Removal clears all
copies of a login. `-n`/`--dry-run` previews the before/after allowlist;
previews and no-ops leave the file byte-for-byte unchanged. A real edit
atomically replaces the file with mode `0600`, preserving other TOML values,
including unknown settings, but reformatting the file and removing comments.

No access command contacts tailscaled, edits tailnet grants, restarts a
service, submits a job, or resumes pending automatic applications. Restart
the runner with `errand setup --config PATH` to activate saved edits; setup
refuses to restart while jobs are active. An allowlist removal is not a
complete revocation: capability grants and SSH access remain independent.

All operations accept `--json`. Listing emits `path`, `allow_users`,
`capability`, `listen`, and an `activation` reminder. Mutations emit
`operation`, `login`, `dry_run`, `path`, `before`, `after`, `changed`,
`written`, and `activation`. `changed` describes whether the desired array
differs, while `written` is true only after a successful write. Pass options
before the login.

## Diagnose the selected runner

```sh
errand doctor
errand doctor --on cabal
errand doctor --profile build --json
errand doctor --url ssh://my-runner --no-snapshot
```

Doctor resolves the same run settings and accepts the same override flags as
`errand config`. It then makes one logical `GET /v0/info` probe to the selected
runner, with a four-second deadline. Configured SSH aliases retain their
`remote_command` and `remote_socket` settings. The displayed endpoint remains
the configured URL. No other peers are discovered or probed.

The configuration check reports resolver errors such as malformed TOML,
unknown profiles, missing peers, and invalid workspace boundaries. When
resolution fails, the runner check is skipped. Required environment variables
are checked next, and missing variables also skip the probe. The runner check distinguishes
connection failures, refused info access, and responses that do not match the
current Errand protocol, with next steps for each. A reachable busy runner
produces a warning; it is still a successful diagnostic check.

Doctor reads local configuration and workspace metadata and probes the runner.
It does not select or hash snapshot files, validate command availability,
submit jobs, change grants or configuration, restart services, or resume
pending automatic applications. Successful info access establishes only the
ability to read runner info; a caller can have that access without permission
to submit. Admission and capacity can also change after the check.

Exit codes are `0` for successful checks (including busy warnings), `1` for
configuration, probe, or report-output failures, and `2` for invalid command
usage. Help exits `0`. With `--json`, diagnostic reports go to stdout and
include `ok`, `checks`, `scope`, the resolved `effective` configuration when
available, and `info` after a successful probe. Each check has `name`,
`status` (`ok`, `warning`, `error`, or `skipped`), `detail`, and an optional
`hint`. Usage errors go to stderr without a JSON report.
