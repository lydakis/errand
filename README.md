# errand

Run the thing you would have run locally, on another machine you own, and
get the result back — same command, same working tree, logs streaming to
your terminal, real exit code. The heat, watts, and minutes are spent
elsewhere.

> **Status: design phase.** There is no code yet. The v0 design is frozen
> and lives in [docs/DESIGN.md](docs/DESIGN.md); implementation starts with
> its milestone 1.

## Why not just ssh?

ssh gives you a remote shell and assumes the remote already has your
project state: a checkout on the right branch, your uncommitted changes,
no stale artifacts. Every ssh target is a pet you maintain.

errand ships the workspace *with* the job, so targets are stateless with
respect to your projects: nothing checked out, nothing drifting, any peer
equally valid at any moment. Around that round-trip it wraps a
transaction ssh doesn't attempt:

- **At-most-once admission** — network retries can never run a command
  twice.
- **Detach and reattach** — submit from a laptop or phone, walk away,
  stream ordered resumable logs later; jobs survive caller disconnect.
- **Declared outputs, applied safely** — build artifacts come back to your
  local tree atomically, with conflict detection, never a silent
  overwrite.
- **Honest cleanup** — errand removes everything it owns; explicitly
  declared caches are the only persistent state.
- **A receipt** — an append-only record of what was asked, who asked, what
  ran, and what happened.

## Shape

One binary, symmetric peers, no controller. Transport and identity come
from your [tailnet](https://tailscale.com) (WhoIs + ACL capability grants —
errand stores zero credentials) or, later, from direct LAN pairing with
pinned device keys. Execution backends: host, rootless container, nix
devshell.

```
errand -- cargo test                  # configured default peer
errand --on buildbox -- nix flake check
errand --where kvm,x86_64 -- ...      # pick a peer by measured facts
errand --detach -- ./long-benchmark   # prints a handle, go to bed
```

Linux and macOS first; Windows is a design constraint, not yet a
deliverable.

## Non-goals

Not a CI system, not interactive (no PTY in v0), not a security boundary
against hostile code, no web UI, no fan-out. The design resists becoming
ansible, nomad, or a scheduler on purpose.

## License

MIT
