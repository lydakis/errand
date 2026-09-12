# errand

Run your usual commands on another machine, from your laptop. Errand sends
your current working tree, including uncommitted edits, streams logs to your
terminal, and returns the exit code. No remote checkout to maintain.

Let your Mac mini run tests or a coding agent while you keep working,
start a development server elsewhere, or use a GPU machine for training.
Put `errand --` in front of the command you'd normally run:

```sh
errand -- make test
```

<p align="center">
  <img src="docs/assets/errand-astral-projection.png" width="600" alt="Errand: running it elsewhere is a lot like astral projection.">
</p>

<p align="center"><sub>Adapted from Figure 7.1 in Daniel P. Dern's <i>The Internet Guide for New Users</i> (1994).</sub></p>

## Install

Install Errand on both machines. It supports macOS and Linux.

With [Homebrew](https://brew.sh):

```sh
brew install lydakis/errand/errand
```

Or download a binary from [GitHub Releases](https://github.com/lydakis/errand/releases).
To build from source, use the Go version in [go.mod](go.mod):

```sh
go build -trimpath -o errand ./cmd/errand
```

## Quickstart

The machine that runs your commands is the **runner**. For this walkthrough,
connect both machines to the same Tailscale network under your login.

On the runner:

```sh
errand setup
```

Setup installs and starts the runner service. On your laptop, find it:

```sh
errand peers discover
```

Discovery prints the command to add each available new runner. Run the one
for your machine. For example:

```sh
errand peers add mac-mini YOUR_RUNNER_HOSTNAME
```

`mac-mini` is the name you'll use for this runner. The first peer you add
becomes the default.

<details>
<summary>Using SSH instead?</summary>

Use a host or alias you already connect to with SSH, logging in as the user
running Errand:

```sh
errand peers add mac-mini YOUR_SSH_HOST --ssh
```

See [SSH runner setup](docs/OPERATIONS.md#ssh-only-setup) for setup and custom
paths. Setup without Tailscale requires Errand v0.1.1 or newer.

</details>

### Run a command

From your project's Git worktree, use the command you'd normally run:

```sh
errand -- make test
```

Output streams here, and the exit code comes back. Your local files stay
unchanged by default. Choose another runner with `errand --on NAME -- COMMAND`.

By default, each job gets a fresh workspace with your files, including
uncommitted edits. Install the required tools on the runner. Git-ignored dependencies such as
`node_modules` don't travel with your files; install them within the job
when needed.

### Leave it running and come back

If you don't want to keep watching, press **Ctrl-D**. The command keeps
running on the other machine. Later, find its handle and reattach:

```sh
errand ps --last 5
errand attach HANDLE
```

Ctrl-C interrupts the remote command. Reattaching follows logs; it doesn't
bring changed files back. To start detached, use `errand -d -- COMMAND`.

## Let Errand choose a runner

Have several runners? Ask for what your command needs:

```sh
errand --where 'os=linux,go' -- go test ./...
```

Errand checks your configured runners for Linux and Go, then chooses among
the matches based on available job capacity. It prints the selected peer;
detach, attach, and fetch work as usual. See [runner selection](docs/CONFIGURATION.md#automatic-runner-selection)
for other requirements and saved preferences.

## Open a remote development server locally

Start your project's development server on your Mac mini, including its
dependency install in the same job:

```sh
errand --on mac-mini -L 3000 -- sh -c 'pnpm install && pnpm dev'
```

For a server listening on port 3000, open `http://localhost:3000` on your
laptop. The server runs on the mini; the port is available locally while
you're attached.

Each run uses the files sent when it starts; later local edits aren't
continuously synced. See [port forwarding](docs/USAGE.md#attached-sessions-and-forwarding)
for multiple ports and reconnecting.

## Bring changed files back

Fetch stages a job's retained changes locally for inspection. Apply merges
them into the workspace you submitted from:

```sh
errand fetch HANDLE
errand fetch --apply HANDLE
```

Changes are retained even if the command fails. Apply checks for conflicts
with your local edits before changing files. See [fetch and apply](docs/USAGE.md#retained-changes-fetch-and-apply)
for details.

## When you need more

- **Try something in a separate local workspace:** run `errand --on local -- make test`
  without disturbing your current checkout. [Local setup and usage](docs/USAGE.md#local-jobs).
- **Keep working in the same workspace:** create one explicitly and reuse it across
  jobs. [Persistent workspaces](docs/USAGE.md#persistent-workspaces).
- **Collect a test report:** `errand --artifact reports -- make test` retains
  ignored output too. [Artifacts and exporting results](docs/USAGE.md#artifacts-and-caches).
- **Speed up repeat builds:** `errand --cache compiler=target -- cargo test`
  reuses the runner's build cache. [Named caches](docs/NAMED_CACHES.md).
- **Save recurring choices:** use `.errand.toml` and named profiles, then
  inspect them with `errand config`. [Configuration](docs/CONFIGURATION.md).
- **Troubleshoot a connection:** run `errand doctor`.
  [Runner setup and troubleshooting](docs/OPERATIONS.md).

Errand runs trusted code directly on machines you control. Commands must
work without an interactive terminal; for coding agents, use their
noninteractive mode. Use `errand --help` or `errand COMMAND --help` for flags
and examples, or read the [usage guide](docs/USAGE.md).

Errand does not collect usage telemetry.

## License

MIT
