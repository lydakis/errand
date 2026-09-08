# Operating Errand

Errand runs directly on machines you own. Install it using the
[quickstart](../README.md#quickstart), then use this guide for setup, access,
diagnostics, and storage maintenance.

## Peers and discovery

`errand peers` shows runner status, capacity, platform, and capabilities. `errand peers
add NAME HOST` probes the runner with an authenticated `/v0/info` *before*
writing anything: a 403 prints your tailnet login and tells you to add it to
the runner's `allow_users`, then restart through `errand setup`; an unreachable
host suggests running setup there (or `--no-verify` to record an offline runner). HOST may be a
MagicDNS name, `host:port`, an `http://` URL, or an ssh_config host with
`--ssh` (plus `--remote-command` when errand is not on that host's login
PATH, and `--remote-socket` for a non-default daemon socket). `errand peers
discover` is read-only and scoped to your own tailnet:
it asks tailscaled for the node list, probes each online node's errand
port, and prints exact `peers add` commands for runners that admit you,
flagging ones already configured (by name or IP) and ones that refused you.
It never scans arbitrary hosts and never writes config.
Use `errand peers --on NAME` or `--url URL` to query one runner.
`errand peers --json` always returns an array of peer records, including
`name`, `target`, `default`, and `status`. Reachable peers include the complete
runner response under `info`, including CPU count, version, capacity, and tool
paths. Failed peers remain in the array with a `detail` message and no `info`.
The command exits nonzero if any selected peer cannot be queried.

## Runner setup

`errand setup` defaults to both SSH and Tailscale for new runners.
`errand setup --ssh` saves an SSH-only preference; `errand setup --tailscale`
saves a Tailscale-only preference. `errand setup --local` enables only local
Unix-socket jobs. These flags update `transport` in the runner
config without requiring `--force`. Plain setup respects that saved setting.
All modes install and start the same platform service, with user-service
linger on Linux and a launch agent on macOS. Setup preserves unrelated config
values and existing service definitions unless `--force` is given. Before
writing anything, it reserves an idle runner and blocks new admissions until
restart; it refuses while jobs are staging, starting, running, or queued.
Generated services retain the absolute entries from the setup shell's `PATH`
and add the standard system directories, so runner-installed developer tools
remain available to jobs. When SSH is enabled, setup also links `/usr/local/bin/errand` when it can
(so SSH callers find it on the non-interactive PATH); otherwise its client
snippet includes the required absolute `remote_command`. It then probes the
daemon over its own socket. `-n` or `--dry-run` shows every decision;
`--print-acl` emits the tailnet grant for capability-based fleets.

## Access

Runner access can use a tailnet URL or SSH. Tailnet callers are authorized by
WhoIs identity through an ACL app capability or `allow_users`; an allow-listed
login receives full runner access unless its exact login is in `deny_users`.
A denial overrides both grant sources. The daemon uses
a tailscaled LocalAPI socket when available and falls back to the `tailscale`
CLI, including for the standalone macOS app. CLI-based WhoIs cannot provide
destination-scoped capabilities, so that path requires `allow_users`.

Manage the saved access lists locally on the runner:

```sh
errand access
errand access add -n friend@example.com
errand access add friend@example.com
errand access remove friend@example.com
errand access deny friend@example.com # override tailnet grants
errand access undeny friend@example.com # restore any remaining grants
errand setup                         # restart to activate saved changes
```

Use `--config PATH` before the login for a custom runner config, and pass the
same path to `setup`. `--json` is available on all access commands. These
commands edit an existing local file; they do not contact or restart a peer.
`remove` only edits the allowlist. `deny` overrides tailnet grants after restart;
SSH access remains separate. Saved edits do not cancel jobs or existing
streams. Real edits preserve other TOML setting values but reformat the file
and remove comments.
See [runner access configuration](CONFIGURATION.md#runner-access) for the
full contract.

## Diagnostics

Run `errand doctor` to check this installation, any configured local runner,
and the peer selected for your next invocation. `--on cabal` overrides the peer
as usual; `--profile NAME` and `--json` are also supported. A client-only machine
skips local runner checks. An installed service that stopped or a configured
socket that disappeared is an error. Use `--config PATH` to include a custom
local runner configuration alongside the other checks. Doctor checks SSH
readiness, reports next steps, and submits no job or configuration changes.
See [doctor checks](CONFIGURATION.md#diagnose-the-selected-runner) for scope
and exit codes.

## Local-only setup

Available in the next release after v0.1.1. To run jobs on your laptop in
separate workspaces without enabling remote access to its runner:

```sh
errand setup --local
errand --on local -- make test
errand doctor --on local
```

This installs the same background service with `transport = "local"`. Only
its private Unix socket accepts jobs, restricted to the daemon's OS user.
It opens no TCP listener, refuses `errand _stdio` SSH bridging, and requires
neither Tailscale nor SSH. It does not install the SSH PATH shim. Plain setup,
including after an upgrade, preserves local-only mode. Your remote default
peer stays unchanged, and a failed remote run never falls back to local.
`peers` and `ps` include the installed local runner alongside personal aliases.

On an existing remote runner, `setup --local` switches that service to local
access after the normal idle-runner check. Saved remote listener addresses
and authorization policy remain available if you explicitly enable a remote
transport later. Use `--dry-run` to preview the switch. A differing service
definition is rejected before any changes: inspect it, then use
`setup --local --force` if you want setup to replace it. Setup only reports
success after the running daemon confirms local-only mode.
This does not disable OS SSH login: anyone who can already run commands as your account can use
its local socket too. A job runs with your account's permissions, even though
its working directory is a separate snapshot.

Setup and the built-in `local` target use the same default runner config,
`$XDG_CONFIG_HOME/errand/errandd.toml` when set, otherwise
`~/.config/errand/errandd.toml`, to determine the socket path. If your service
uses `setup --config PATH`, put its effective absolute socket path in a personal peer table, then select that alias:

```toml
# ~/.config/errand/config.toml
[peers.sandbox]
socket = "/absolute/path/to/errand.sock"
```

```sh
errand --on sandbox -- make test
```

Earlier setup versions always used `~/.config/errand/errandd.toml`. If that
file exists but the XDG location is empty, setup stops before changing anything,
including with `--force`. Use `errand setup --config ~/.config/errand/errandd.toml`
to keep the existing configuration and transport settings; use a personal socket
alias as above when the config is outside the current default location.

For job lifecycle and apply behavior, see [local jobs](USAGE.md#local-jobs).

## SSH-only setup

Install Errand on both machines and enable SSH access to the runner account.
On the runner, install the service as that account:

```sh
errand setup
```

For a default installation, setup saves `transport = "both"`. If Tailscale
is unavailable, it reports why and writes `listen = "none"` for now. After
installing or connecting Tailscale, rerun `errand setup`: it enables the
listener and records the discovered identity provider. On first activation,
it grants the runner's tailnet owner access only if no explicit allowlist or
capability policy is already configured. Existing denials are preserved.

To explicitly use SSH alone, run `errand setup --ssh`. It saves
`transport = "ssh"` and skips Tailscale discovery. SSH login to the runner's
OS account must be enabled separately; setup configures Errand's bridge,
not the operating system's SSH server. For Tailscale alone, use
`errand setup --tailscale`; Tailscale must be available and authenticated.
That mode disables the SSH bridge and local job operations, while retaining
the private socket for health checks and setup.

Edit `transport` in `~/.config/errand/errandd.toml` anytime, then rerun setup:
`"both"`, `"ssh"`, `"tailscale"`, and `"local"` are the supported values. A custom config
uses `errand setup --config PATH`. Existing configs without `transport` keep
their legacy behavior: `listen = "none"` means SSH-only; any other listener
permits both. Set `transport = "both"` to opt a legacy SSH-only runner into
later tailnet discovery.

An explicit `--ssh`, `--tailscale`, or `--local` also repairs an invalid saved transport.
Use `--force` (`-f`) only if you want to regenerate the rest of the config
and service definitions too; `--dry-run` (`-n`) previews either operation.

Setup does not remove an enabled tailnet listener during an outage: it stops
before making changes. Pending tailnet access in `"both"` mode can remain
pending across reruns. Explicit tailnet options also require Tailscale;
a running installation must meet Errand's version requirement. Mode changes
and first-time tailnet activation preserve unrelated config values, including
queue limits, cache settings, custom sockets, and access policy, but reformat
the TOML and remove comments. Unchanged configs remain byte-for-byte intact.
`--force` regenerates config and service definitions and replaces custom settings.

On the client, register the SSH host or ssh_config alias:

```sh
errand peers add buildbox YOUR_SSH_HOST --ssh
```

SSH must log in as the user running the daemon. If Errand is not on the
non-interactive SSH PATH, add `--remote-command /absolute/path/to/errand`
to the registration command. For custom daemon sockets, also pass
`--remote-socket /absolute/path/to/errand.sock`.

## SSH and runner capacity

An SSH peer uses the same HTTP protocol over `ssh HOST errand _stdio`. The
daemon accepts the bridge through a private Unix socket and verifies that it
runs as the daemon's OS user. Configure an ssh_config alias and an absolute
binary path when non-interactive SSH does not include Errand on `PATH`:

```toml
[peers.cabal]
ssh = "cabal"
remote_command = "/usr/local/bin/errand"
remote_socket = "/srv/errand/errand.sock"
```

`errand setup` always prints the effective `remote_socket`, so the SSH peer
remains correct when setup uses a custom config, state directory, or socket.
Set `transport = "ssh"` in `errandd.toml` for an SSH-only runner. SSH handles
host authentication, keys, jump hosts, and caller access. Jobs submitted over
SSH and the tailnet have separate owners.
Runners execute one job at a time by default and queue up to eight more. Set
`max_jobs` and `max_queued` in `errandd.toml` to change those limits;
`max_queued = 0` disables queueing.

## Storage and garbage collection

`errand df` reports logical storage used by each runner's shared snapshot cache
and the authenticated caller's named caches and job receipts. Each runner also
reports aggregate change records and download staging for its OS account,
including results it fetched while acting as a client. Only counts and byte
totals are exposed, never filenames or contents. This uses the service's
`XDG_STATE_HOME`, or its account's default state directory.

The `local` row combines Unix-connected runner storage and the invoking
client's fetched changes, counting shared change storage only once. Without a
local runner it shows fetched changes alone. Older runners that do not report
fetched-change usage show `-` for that column; upgrade the runner to include it
in its total. Human output uses readable binary units; `--json` preserves
raw byte and item counts and cache limits. Capability-based runners
must grant `read-own` to use `errand df`; `manage-caches` remains required only
for `errand gc cache`. Job receipt collection uses the separate `gc-own` action.
The frozen design's ACL example includes the complete action set.

GC always names its target. Bare `errand gc` only prints usage:

```text
errand gc cache
errand gc cache --dry-run
errand gc jobs --older-than 30d --keep 500
errand gc jobs --dry-run --older-than 30d
errand gc changes --older-than 30d
errand gc all --older-than 30d --keep 500
errand gc all --dry-run --older-than 30d --keep 500
```

Job GC only removes the caller's clean `exited` or `killed` receipts. Clean
means cleanup, logs, and workspace-change retention all completed with no
transaction error.
Active, queued, ambiguous, incomplete, and actively replayed receipts are
protected. When both retention bounds are present, a receipt must be older
than the cutoff and outside the newest `keep` receipts to be removed. `all` is
client-side composition of the separately authorized cache and job endpoints
plus local change-state collection. `gc changes` removes old local workspace
identity records, verified downloads, and interrupted download staging;
pending apply transactions are always protected. Records whose submission never began follow
the requested `--older-than` boundary. Unresolved submitted jobs remain
protected for 30 days, after which an explicit local GC may retire abandoned
state.
`--dry-run` is available for every GC target. It applies the same selection
policy and reports the space it can inspect without changing cache, receipt,
reconciliation, local-change, lock, permission, or admission-clock state.
Local staging that cannot be inspected without widening permissions is reported
as a failed preview and left untouched.
After non-dry job GC, the client replays the runner's durable, owner-scoped
collection markers, so a lost deletion response does not strand local records.
New job IDs must carry a ULID timestamp from the preceding 24 hours, with one
hour of allowed future clock skew. The runner durably advances a high-water
clock that never moves backward across restart. Replay-only collection markers
expire after 25 hours. Markers for jobs with retained changes are scoped to the
originating client and retain that minimum lifetime; after the client reconciles
its local state, it acknowledges the marker so it can retire. Unacknowledged
change markers expire after 30 days, bounding abandoned client state. The
markers are small and non-secret, and they never permit a collected ID to
execute again.

## Upgrades

See [runner upgrades](RELEASING.md#runner-upgrades) for Homebrew upgrades,
service restarts, and preserving an existing installation. Setup owns the
runner service; do not also start it through `brew services`.
