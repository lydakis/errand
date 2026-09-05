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

`errand setup` is idempotent: it keeps an existing config or service
definition unless `--force` is given, enables user-service linger on Linux
(so the runner survives logout), installs a launch agent on macOS, and
restarts the service so preserved configuration edits are active. Before
writing anything, it reserves an idle runner and blocks new admissions until
restart; it refuses while jobs are staging, starting, running, or queued.
Generated services retain the absolute entries from the setup shell's `PATH`
and add the standard system directories, so runner-installed developer tools
remain available to jobs. Setup also links `/usr/local/bin/errand` when it can
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
Set `listen = "none"` in `errandd.toml` for an SSH-only runner. SSH handles
host authentication, keys, jump hosts, and caller access. Jobs submitted over
SSH and the tailnet have separate owners.
Runners execute one job at a time by default and queue up to eight more. Set
`max_jobs` and `max_queued` in `errandd.toml` to change those limits;
`max_queued = 0` disables queueing.

## Storage and garbage collection

`errand df` reports logical storage used by each runner's shared snapshot cache
and the authenticated caller's named caches and job receipts, plus local change records and
download staging. Human output uses readable binary units; `--json` preserves
raw byte and item counts, cache limits, and cache TTL. Capability-based runners
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
