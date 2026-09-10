# Service upgrades

Status: runtime and advisory-warning slice for review. This is not yet a safe
Homebrew migration from a running released 0.2.1 daemon. Do not release this
slice as a complete upgrade fix until the first-migration test below passes.

## Incident and existing lifecycle

On September 10, 2026 UTC, an Errand job upgraded a macOS runner from 0.2.0
to 0.2.1. The upgrade and formula tests succeeded. Homebrew cleanup removed
the 0.2.0 Cellar directory while its daemon continued running. The daemon
answered through its Unix socket. Application Firewall logs subsequently
reported an unresolved executable path, UNIX error 2, and a default drop
of incoming TCP connections. This was not a 0.2.1 daemon crash.

The private recovery report and firewall evidence were inspected. Addresses,
account identities, and private configuration are intentionally excluded here.
An idle `errand setup` recovered the service without changing its saved
configuration or service definition. Independent real jobs then succeeded.

The previous lifecycle tests changed a Homebrew-style opt symlink but did not
remove the old executable. A stable symlink makes the next start find new
bytes; it does not preserve the identity of an already-running executable.
Setup's idle lease prevents interruption of active jobs, but cannot prevent
Homebrew from unlinking the executable before setup runs.

## Implemented slice

`serve` copies its executable into `STATE_DIR/runtime/SHA256/errand` and
re-executes it before opening listeners. The runtime root and generation
directories must be private directories; the executable is a regular file
with mode 0500. Publication cannot replace an existing generation. Reuse
checks the content hash without staging another copy. Relative state paths
resolve to absolute runtime paths, and startup compares file identities using
the opened executable, so removal of its installation path after publication
does not prevent re-execution. A damaged runtime fails startup instead of silently
replacing bytes that another daemon may be using.

The service manager still launches the installed CLI with the same arguments,
configuration, and environment. Re-execution preserves its PID. Existing
launchd/systemd definitions need no rewrite. An ordinary package upgrade can
remove a candidate daemon's original installation while its runtime executable
remains present. Jobs continue on that daemon. George selected explicit idle
`errand setup` to adopt the installed version; there is no automatic replacement
or drain mode.

Runtime generations are retained. They are not snapshot or job caches and
are not removed by `errand gc`. This costs one binary per distinct build per
state directory. Collection needs a separate ownership design before deleting
files that other foreground or managed daemons may still execute. Each new
generation requires writable space for one binary; reuse reads and verifies
the existing generation without needing another binary-sized allocation.

Keep the state directory outside package-manager installations, on a filesystem
that permits execution and hard links. A `noexec` mount cannot host the runtime.
Startup reports preparation or execution failures rather than falling back to
an installation that package cleanup can remove.

Normal completion and errors remove temporary `.install-*` copies. A forced
process kill or machine crash can leave one behind. There is no automatic
age-based sweep: another startup may still be copying. For manual cleanup,
stop every foreground and managed daemon using that state directory and prevent
new starts before removing abandoned `.install-*` files. Retained generation
directories remain outside this cleanup procedure.

Run, attach, and fetch emit one advisory warning to stderr when a bounded info
probe reports a different daemon version. Run and attach perform their local
environment/forwarding checks first. The probe adds a round trip, bounded to
two seconds; failed probes do not gate the requested operation. There is no
version ordering or compatibility negotiation.

The warning identifies the invoking CLI and running daemon separately. It asks
the operator to check `errand version` on the runner before choosing setup.
The runner's installed binary version is unknown to the remote client: a
client/server mismatch alone is not evidence that setup will change anything.

## First migration remains a release blocker

A running 0.2.1 process has no runtime-copy startup step. Installing this code
cannot relocate its existing execution. Restarting it during an active job
would violate the job contract. Waiting for an upgrade job to finish inside a
synchronous post-install restart can deadlock on that very job. A background
restart does not protect the executable from immediate Homebrew cleanup.

The inspected Homebrew source at commit
`c1016b595ee8ffc5624a8e63071fe235abbbba5c` selects old kegs in
`Formula#eligible_kegs_for_cleanup` and removes them in `Cleanup#cleanup_keg`.
`FormulaInstaller#post_install` catches failures; failing that hook is not a
reliable cleanup veto. Homebrew also honors `.keepme` references to existing
paths through `Keg#keepme_refs`. This may provide a migration retention
mechanism, but this slice does not write markers into package-managed kegs.

Before adopting retention, prove it runs before automatic cleanup, works in
Homebrew's install sandbox and with custom service paths, survives a failed
migration, and releases only its own reference after every protected process
has stopped. Do not turn stale marker cleanup into another way to unlink a
live executable. Do not require cleanup environment variables or a rescue
terminal as the normal upgrade procedure.

## Acceptance

The ordinary runtime suite re-executes a subprocess after removing its installed
executable, verifies PID preservation, and checks that reuse needs no writes.
The ordinary local-runner end-to-end test uses a relative state directory and
asserts that the live daemon executes a retained runtime before exercising jobs
and transfers. Unix-socket health alone is not evidence of re-execution.

The opt-in service test uses two builds of this candidate source, labeled
`live-v1` and `live-v2`. It removes the old Cellar directory while a gated job
runs, refuses busy setup, completes the job, and switches to the new daemon
through idle setup. It compares configuration and service-definition bytes.
This proves candidate-to-candidate behavior, not a released-version migration.

With `ERRAND_TEST_NETWORK_PEER` set to a trusted peer, that peer submits the
gated job over real Tailscale TCP, attaches, and retrieves its retained output.
It also probes before deletion, after deletion while the job is active, and
after the new daemon starts. macOS network acceptance requires the existing
Application Firewall to be enabled; the test never changes it. Use a peer
with two available job slots so a waiting job cannot starve its own probe.

Before release, add a disposable Homebrew migration from the actual released
0.2.1 binary/service layout with automatic cleanup enabled. Require an active
job to finish and results to remain remotely retrievable, then complete idle
setup and cleanup of the old installation without configuration drift. Repeat
with an idle service and migration failure. Keep Linux/systemd coverage and
macOS/launchd coverage, including the real macOS firewall path.

## Validation recorded for this slice

- Review fixes passed runtime regressions for relative paths, reuse with
  read-only runtime directories, and re-execution after installation removal.
  A temporary Go source overlay that published the runtime but skipped execution
  made the ordinary local-runner test fail at its executable-path assertion.
  The overlay did not modify the checkout.
- macOS launchd plus real Cabal-to-Mac-mini TCP passed with Application
  Firewall enabled. The final run deleted the active candidate's Cellar,
  retrieved the remote job's retained output, and adopted the next candidate.
  Isolated services and temporary state were removed.
- Linux systemd lifecycle and runtime race tests passed on Cabal with its
  existing user-bus location supplied to the test process.
- Race tests for the changed CLI, client, setup, and runtime packages passed
  on macOS and Linux. `go vet ./...` and `git diff --check` passed.
- The first broad race runs caught advisory probes occurring before local
  forward binding. The fix moved probes after local checks; the existing
  forwarding regression and changed-package suites then passed.
- No production service, firewall setting, package installation, or published
  release was changed. No actual released-0.2.1 Homebrew migration was tested.

## Herdr comparison

Herdr distinguishes server/client version differences from endpoint protocol
generation differences in its [status implementation](https://github.com/herdrdev/herdr/blob/master/src/cli/status.rs).
Its [installation documentation](https://herdr.dev/docs/install/#update) leaves
compatible servers running across package-manager upgrades and excludes those
upgrades from experimental live handoff. Its [updater](https://github.com/herdrdev/herdr/blob/master/src/update.rs)
directs Homebrew installations back to Homebrew. These support advisory
visibility and explicit adoption, but do not establish a solution to macOS
Cellar deletion and Application Firewall identity. Errand does not copy Herdr's
protocol negotiation or live-handoff machinery.
