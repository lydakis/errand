# errand

Run a command on another machine you own, with your current working tree.
Logs stream to your terminal, the exit code comes back, and changed files
can be fetched or applied locally. No remote checkout to maintain.

<p align="center">
  <img src="docs/assets/errand-astral-projection.png" width="600" alt="Errand: running it elsewhere is a lot like astral projection.">
</p>

<p align="center"><sub>Adapted from Figure 7.1 in Daniel P. Dern's <i>The Internet Guide for New Users</i> (1994).</sub></p>

One binary for macOS and Linux. Connect through Tailscale or SSH; each machine
can send jobs, receive them, or both. Commands run directly on the runner, so
install the tools your project needs there. Errand is for trusted code on
machines you control. Interactive programs requiring a PTY are not supported.

## Install

With [Homebrew](https://brew.sh):

```sh
brew install lydakis/errand/errand
errand version
```

Or download a macOS/Linux binary from [GitHub Releases](https://github.com/lydakis/errand/releases).
To build from source, use the Go version in [go.mod](go.mod):

```sh
go build -trimpath -o errand ./cmd/errand
```

## Quickstart

Install Errand on both machines. For this example, both should be connected
to the same Tailscale network under your login.

On the machine that will run jobs:

```sh
errand setup
```

Setup installs and starts the runner service. On your laptop, add that
machine using its Tailscale hostname, then run a command from a Git worktree:

```sh
errand peers add buildbox YOUR_RUNNER_HOSTNAME
errand --on buildbox -- make test
```

Errand sends the selected workspace, including uncommitted changes, streams
the logs, and returns the command's exit code. Git-ignored files are excluded
by default. Your local files stay unchanged unless you request application.
The first peer you add becomes the default, so subsequent commands can use
`errand -- make test`.

For SSH, use `errand peers add buildbox YOUR_SSH_HOST --ssh`. See
[runner setup and access](docs/OPERATIONS.md) for other logins, custom paths,
and SSH-only runners.

## Everyday use

Run in the background and reconnect later:

```sh
job=$(errand -d -- make build)
errand ps
errand status "$job"
errand attach "$job"
```

While attached, Ctrl-D detaches and leaves the job running. Ctrl-C interrupts
the remote command. `errand kill HANDLE` stops a job you have detached from.

Bring changed files back:

```sh
errand fetch HANDLE                 # download into local staging
errand fetch --apply HANDLE         # merge into the originating workspace
errand fetch -o ./results HANDLE    # export remote files to a new directory
errand --apply -- gofmt -w .        # apply automatically after clean success
```

Ordinary workspace changes are retained automatically, including on failed
jobs. Apply checks for local conflicts before changing your files. Attach
only follows logs; it does not fetch or apply results.

Retain ignored outputs, or reuse a build cache on the runner:

```sh
errand --artifact reports -- make test
errand fetch -o ./test-results HANDLE reports
errand --cache compiler=target -- cargo test
```

Artifacts add files to the results you can fetch. Caches stay on the runner
for later jobs and are excluded from uploads and results. Here `compiler`
is a name you choose and `target` is its workspace-relative directory.

Reach a remote development server through localhost:

```sh
errand -L 3000 -- pnpm dev
```

Save recurring choices in `.errand.toml`:

```toml
[profiles.test.run]
peer = "buildbox"

[profiles.test.artifacts]
paths = ["reports"]
```

```sh
errand --profile test -- make test
errand config --profile test        # explain effective settings and sources
errand doctor                      # check installation, runner, and connection
```

Profiles are explicitly selected. Personal settings live in
`~/.config/errand/config.toml`; CLI flags override configuration. See the
[configuration guide](docs/CONFIGURATION.md) for precedence, environment
forwarding, workspace roots, and all supported settings.

## Learn more

- [Usage guide](docs/USAGE.md): command conventions, snapshots, sessions, fetch, and apply.
- [Operations](docs/OPERATIONS.md): setup, access, SSH, diagnostics, and cleanup.
- [Named caches](docs/NAMED_CACHES.md): reuse, ownership, leases, and removal.
- [Design](docs/DESIGN.md): admission, receipts, failure recovery, and trust boundaries.
- [Performance](docs/PERFORMANCE.md): measurements and reproducible benchmarks.
- [Releases and upgrades](docs/RELEASING.md): packaging, publication, and runner migration.

Use `errand --help` or `errand COMMAND --help` for flags and examples.

## License

MIT
