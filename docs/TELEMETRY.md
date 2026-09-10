# Usage telemetry

Stable releases enable limited usage telemetry by default. It helps answer
whether installations submit jobs, return in later weeks, and use particular features.
It counts participating client installations, not people. One person may use
several clients, and agents or scripts can submit many jobs from one client.

The integration uses PostHog US hosting at `https://us.i.posthog.com`.
The first eligible invocation displays a notice on stderr and sends no events.
Later invocations may send the events below. The notice is remembered in
`telemetry-id.notice` beside personal configuration. If Errand cannot write
the notice or remember it, that invocation sends nothing.
Errand reserves writable notice state before printing, so an unwritable
configuration directory does not produce a notice on every command.

## Enable or disable

To disable telemetry persistently, add this to
`~/.config/errand/config.toml`, or `$XDG_CONFIG_HOME/errand/config.toml`:

```toml
[telemetry]
enabled = false
```

Removing the table restores the default. Set `enabled = true` to explicitly
enable telemetry without waiting for the notice. `ERRAND_TELEMETRY=1` also
explicitly enables it; `ERRAND_TELEMETRY=0` disables it. Both override
personal configuration. Other values disable it. `DO_NOT_TRACK=1` always
prevents sending. Workspace files, profiles, and runner configuration cannot
set telemetry preferences.

Sending is also disabled for development/prerelease versions, builds without
a project token, and detected CI environments. Invalid personal configuration
disables telemetry without changing the command's own configuration handling.
No telemetry network requests or identity/notice reads/writes occur when disabled.

## Inspect the payload

```sh
ERRAND_TELEMETRY_DEBUG=1 errand df
```

This executes the ordinary command and prints its telemetry JSON to stderr,
prefixed with `[telemetry]`. The preview replaces the installation ID with
`preview` and omits the public ingestion token. All other event fields use
the actual values. Preview sends no telemetry and creates no telemetry state,
even if telemetry is enabled. It also works in development builds and CI.
The ordinary command can still contact runners as usual.

## Events and fields

| Event | When | Properties |
| --- | --- | --- |
| `cli_finished` | An included client command returns | Errand operation and `success` or `nonzero_exit` |
| `job_admitted` | The client confirms admission of a new job | Transport and booleans for persistent workspace, caches, artifacts, forwarding, automatic apply, and starting detached |

Both events include the Errand version, OS, CPU architecture, and a random
installation ID. The operation is one of `run`, `peers`, `workspaces`,
`config`, `doctor`, `attach`, `push`, `fetch`, `ps`, `status`, `kill`, `df`,
or `gc`. Help, version, setup, access management, the daemon, and internal
helper commands emit nothing. Subcommand arguments are not collected.

The installation ID is stored with mode 0600 beside personal configuration
in `telemetry-id`. It is unrelated to your hostname, account, repository, or
hardware identifiers. Deleting that file starts a new identity on the next
enabled invocation; it does not delete previously received events.
Unreadable or corrupt identity state disables sending. An incomplete notice
marker also keeps default-on sending disabled, including after a crash while
displaying the notice. Delete the affected file to let Errand recreate it.

No executed command, command arguments, flag values, environment names or
values, repository names, paths, file contents, logs, peer addresses, job IDs,
workspace names, or raw error messages are sent. Error outcomes are coarse
because remote process exit codes overlap Errand's own codes. A detached
submission returning zero does not mean its job later succeeded. No daemon
completion events or cross-machine identity linking are performed.

Feature flags describe settings at admission. They do not prove that a cache
hit occurred, an artifact was produced, or automatic apply later succeeded.
Fetch and push usage is counted at the command level; their apply/export
options are not distinguished in this first version.
Admission properties are top-level event properties: `transport`,
`persistent_workspace`, `caches`, `artifacts`, `forwarding`, `automatic_apply`,
and `start_detached`. They can be filtered and broken down directly in PostHog.

## Delivery and privacy

Events use PostHog's HTTPS capture API. The build fixes the ingestion host
to the project's US region; repository configuration cannot redirect
it. Requests do not follow redirects. Each request has a one-second timeout,
and command exit waits at most 200ms for pending delivery before cancelling.
Admission and exit events send independently, so a slow admission response
does not prevent an exit event from being attempted. Slow networks can still
lose events within this bounded budget; event delivery order is not guaranteed.
Failures are silent and never affect command output or exit status.
There are no retries, background subprocesses, or persistent event queues.

The payload requests no person profiles and disables GeoIP enrichment with
`$process_person_profile = false` and `$geoip_disable = true`.
The PostHog project also has **Discard client IP data** enabled, so client
IPs are removed from stored events. It uses the free plan, whose published
[data retention period is one year](https://posthog.com/pricing).
The random stable ID makes these events pseudonymous, not fully anonymous.
PostHog and network intermediaries can see transport metadata such as the
source IP. Payload settings are not a promise about provider access logs.

Opt-outs, CI exclusions, dropped events, reinstalls, shared machines, and
multiple clients per person limit the measurements. The maintainer's own
test installations should be excluded from adoption dashboards.

## Maintainer verification

1. Verify the US project's IP-discard setting and free-plan retention before
   releasing changes. Update this document if those settings change.
2. Keep the public ingestion `ProjectToken` and regional `Host` defaults in
   `internal/telemetry/telemetry.go`. The public project token can ship in the
   source and binary. Never embed a personal API key. Keeping these defaults
   in source gives release binaries and the source-built Homebrew formula
   the same settings. Source builds using the development version stay off.
3. Verify that the first default-on invocation displays the notice without
   sending; subsequent invocations may send. Test both opt-out mechanisms.
4. Verify an isolated test client's two events in PostHog. Check stored
   properties against the schema above, including absent IP and GeoIP fields.
5. Start with dashboards for weekly active installation IDs, unique IDs with
   `job_admitted`, weekly return submission cohorts, and feature proportions
   among admitted jobs. Label these as participating installations.

The sender implements the [PostHog capture API](https://posthog.com/docs/api/capture)
directly with Go's standard HTTP client. No analytics SDK, automatic capture,
session recording, or feature-flag evaluation is included.
