# Run configuration

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
