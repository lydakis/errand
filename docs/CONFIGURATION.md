# Configuration

Run submission and `errand config` share one resolver. For peer selection and
automatic apply, precedence is:

1. Explicit CLI flags.
2. The explicitly selected profile.
3. The selected workspace's `.errand.toml` defaults.
4. Personal defaults.
5. Safe defaults: no automatic apply and no implicit peer.

Profiles can also set a workdir, overriding the caller's relative directory;
an explicit CLI workdir wins. Environment variables, forwarding, detachment,
and broad snapshot opt-in remain CLI options.

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

Profiles support only `run.peer`, `run.workdir`, and
`changes.apply_on_success`. Explicit `false` and empty workdir values override
lower layers. Unsupported keys and incorrect value types are errors when
loading configuration, including in inactive profiles. There is no profile
inheritance, automatic profile selection, command definition, or transport
configuration inside profiles.

Workspace profiles follow the same boundary and trust rules as workspace
defaults. With `--no-snapshot`, only profiles in the current directory and
personal config are considered; the resulting workdir must be empty or `.`.
Use `--workdir .` to override a profile's nested workdir for such an invocation.
Profiles cannot move the snapshot boundary or enable broad snapshot selection.

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
