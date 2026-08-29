# errand

Run the thing you would have run locally, on another machine you own, and
get the result back: same command, same working tree, logs streaming to
your terminal, real exit code. The heat, watts, and minutes are spent
elsewhere.

> **Status: milestone 1.** The transactional core works end to end over a
> tailnet: snapshot, at-most-once admission, host-backend execution,
> framed resumable logs, receipts, faithful exit codes. The v0 design is
> frozen in [docs/DESIGN.md](docs/DESIGN.md).

## Quickstart

```
# on a runner (Linux box on your tailnet)
errand serve                  # config: ~/.config/errand/errandd.toml

# on a caller
errand info                   # measured facts: arch, kvm, tools
errand -- python3 -m unittest # runs your working tree over there
```

Runner config authorizes callers by tailnet identity (whois): an ACL app
capability or a local `allow_users` list. Destination-scoped capability
checks require Tailscale 1.100 or newer. No keys, no credentials stored.

Run errand from a Git worktree for automatic snapshot selection. A non-Git
directory requires an explicit `.errandignore` policy or `--include-all`.
Errand always refuses a filesystem root, and snapshotting your home directory
requires `--include-all`. The client prints the selected file count and byte
total before remote admission.

## Why not just ssh?

ssh gives you a remote shell and assumes the remote already has your
project state: a checkout on the right branch, your uncommitted changes,
no stale artifacts. Every ssh target is a pet you maintain.

errand ships the workspace *with* the job, so targets are stateless with
respect to your projects: nothing checked out, nothing drifting, any peer
equally valid at any moment. Around that round-trip it wraps a
transaction ssh doesn't attempt. Milestone 1 provides:

- **At-most-once admission:** network retries cannot run a command
  twice.
- **Durable execution:** an admitted job survives caller disconnect, with
  ordered logs that the protocol can resume after a dropped connection.
- **Honest cleanup:** errand removes the temporary workspace and inherited
  process scope it owns, then records whether cleanup completed.
- **A receipt:** an append-only record of what was asked, who asked, what
  ran, and what happened.

The remaining v0 work includes a detach and reattach CLI, declared output
transfer with conflict detection, and explicitly declared persistent
caches.

## Shape

One binary, symmetric peers, no controller. Transport and identity come
from your [tailnet](https://tailscale.com) (WhoIs + ACL capability grants;
errand stores zero credentials) or, later, from direct LAN pairing with
pinned device keys. The current execution backend runs directly on the
host. Rootless containers and Nix devshells are planned.

```
errand -- cargo test                   # configured default peer
errand --on buildbox -- cargo test     # named configured peer
```

Planned v0 commands include fact-based peer selection and detached jobs;
their exact command-line interface is not implemented yet.

Linux and macOS first; Windows is a design constraint, not yet a
deliverable.

## Non-goals

Not a CI system, not interactive (no PTY in v0), not a security boundary
against hostile code, no web UI, no fan-out. The design resists becoming
ansible, nomad, or a scheduler on purpose.

## License

MIT
