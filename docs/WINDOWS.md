# Windows runner (experimental)

A Windows PC can be an Errand runner: send jobs to it from a Mac or Linux
machine over Tailscale. Running the Errand client on Windows is not supported
yet.

## Requirements

- Windows 10 version 1803 or later, or Windows 11, on amd64 or arm64.
- [Tailscale](https://tailscale.com/download/windows) 1.100 or later, signed in
  to the same tailnet as your client.
- The runner runs while you are signed in to Windows. Locking the screen is
  fine; signing out stops it until you sign in again.

## Install

Download `errand_VERSION_windows_amd64.zip` (or `_arm64`) from
[GitHub Releases](https://github.com/lydakis/errand/releases) and unzip
`errand.exe` somewhere permanent, such as `%LOCALAPPDATA%\Programs\errand`.
The binary is not signed yet, so a browser download shows a SmartScreen
warning. In PowerShell, from that folder:

```powershell
.\errand.exe setup
```

Setup needs no administrator rights. It:

- writes the runner config to `%USERPROFILE%\.config\errand\errandd.toml`;
- copies `errand.exe` into `%USERPROFILE%\.errand\runtime` and registers a
  Task Scheduler task named `errand` that starts that copy when you sign in;
- starts the task and checks that the runner answers.

New Windows runners listen on the tailnet only (`transport = "tailscale"`),
because SSH callers are not supported on Windows yet. The runner log is
`%LOCALAPPDATA%\errand\errand.log`.

To upgrade, replace `errand.exe` and run `errand.exe setup` again. The running
task uses its own copy, so replacing the file never fails because the runner
is using it.

## Send jobs from your Mac

On the client, add the PC by its tailnet name and run something:

```sh
errand peers add winpc winpc.example.ts.net
errand --on winpc -- cargo test
errand --on winpc -- pwsh -NoProfile -Command 'Get-ChildItem'
```

Jobs get the runner's `PATH`: your user and system `PATH` as Windows stores
them. The task does not inherit the `PATH` of the shell that ran setup, so
sign out and back in after installing a tool that changes `PATH`, or run setup
again. Programs are found the way Windows finds them, with
`PATHEXT`, so `cargo` runs `cargo.exe` and `npm` runs `npm.cmd`.

## How Windows jobs differ

- **Shell features.** Errand runs the program directly. For pipes, redirection
  or globbing, run a shell yourself: `-- pwsh -c '...'` or `-- cmd /c '...'`.
- **Batch files.** `cmd.exe` reinterprets the arguments of `.bat` and `.cmd`
  programs. Errand refuses to run one with an argument containing `"`, `%`,
  `^`, `&`, `|`, `<`, `>` or a line break, instead of running a different
  command.
- **Stopping.** Each job runs in a Windows Job Object. Ctrl-C on the client or
  `errand kill` ends the job's whole process tree at once. Jobs
  don't get a chance to shut down cleanly yet.
- **File modes.** NTFS has no POSIX permissions. Files keep the modes they
  had on your Mac, so exec bits survive a round trip. New files come back as
  `0644`. Only the read-only attribute is carried from Windows.
- **File names.** Windows can't hold some names that are valid on macOS and
  Linux, such as `CON`, `aux.c`, `a:b`, or names ending in a dot or space.
  Case-only differences (`README` and `readme`) collide.
- **Symlinks.** Creating symlinks needs Developer Mode turned on in Windows
  settings.
- **Line endings.** Errand copies bytes exactly. If your repository uses
  `core.autocrlf=true`, files on the runner have whatever your checkout has.

## Not yet supported

- The Errand client on Windows.
- SSH transport to a Windows runner.
- [Named caches](NAMED_CACHES.md) (`[caches]` in `.errand.toml`); the runner
  refuses jobs that declare one.
- Graceful Ctrl-C (a console break before the hard stop).
- Recovering jobs that were running when the runner stopped.
- Running while nobody is signed in.
