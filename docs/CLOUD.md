# Cloud peers (experimental)

A cloud peer is a runner that can lease machines it does not have. It
advertises **offers**, machine shapes such as "1x H100 80 GB, linux/amd64".
When `--where` asks for something none of your runners has, errand asks the
cloud peer for a **lease**. The leased machine runs errand like any other
runner, and your job runs on it directly. The cloud peer never relays jobs.

```console
$ errand --where gpu=h100 -- python train.py
errand: where skipped cabal: requires 1 GPU matching h100 (has none)
errand: no runner of yours matches gpu=h100; leasing h100 from cabal
errand: cabal: launching h100
errand: cabal: waiting for errand on the machine
errand: cabal: ready after 2m41s
errand: lease cabal-7f3a ready (linux/amd64, 26 cpu, 1x NVIDIA H100 80GB HBM3 (80 GiB))
errand: selected cabal-7f3a for gpu=h100 (0/1 slots, 0 staging, 0 queued)
```

The lease is then a peer named after the cloud peer and the end of the lease
ID. While it exists, `--on cabal-7f3a`, job handles such as
`cabal-7f3a/01K...`, `ps`, `attach`, `fetch`, `-L`, persistent workspaces and
named caches all work as they do with any runner. The next `--where gpu=h100`
selects it directly.

Your machine keeps no record of its leases. The cloud peer lists your active
leases, with how to reach them, whenever errand asks it, so a name like
`cabal-7f3a` works from any process and stops working when the lease ends. A
lease reached over SSH is listed only on the machine whose key it admits. All
this needs the cloud peer to answer: if you point the name `cabal` at another
runner, its leases, and job handles such as `cabal-7f3a/01K...`, are no longer
reachable by name, and `errand leases rm` cannot reach them either. The cloud
peer still ends them when they go idle or reach their lifetime.

## When errand leases

- Only when no reachable runner of yours matches the requirements. A matching
  runner that is busy queues the job; errand does not rent a second machine.
- Never for `--where '*'`.
- The first configured cloud peer (by name) with a matching offer is used, and
  its first matching offer. A ready or launching lease of yours that already
  matches is reused, if you asked for it from the same machine.
- An offer's facts are a claim. The lease becomes ready only once the machine
  answers as an errand runner whose measured facts match, and the runner
  rechecks them when the job is admitted.

A run holds the lease it was given until its job is admitted there. If the
run ends with no job admitted, whether from Ctrl-C, a failure before
submitting, or the leased runner refusing the job, it withdraws its request.
The cloud peer releases the lease once no run it was given still holds it; a
ready lease is released at its next check unless a job is running on it. Once
a job is admitted, or may have been because its answer was lost, the lease is
left to the idle rule below. Being given to a run counts as use, so a reused
lease starts a full idle window.

## Leases

```sh
errand leases                    # your leases on every cloud peer
errand leases --json
errand leases rm cabal-7f3a      # release now; a full lease ID works too
```

The cloud peer logs each lease's launch, readiness, release and any failed
release attempts.

The cloud peer releases a ready lease when its runner has had no staging,
starting, running or queued jobs for the offer's `idle_timeout`, or when the
lease reaches `max_lifetime`, even if a job is still running. A runner it
cannot reach counts as idle. Persistent workspaces and retained results on a
leased machine end with the lease, so fetch what you need first.

Leases are recorded in the cloud peer's state directory before anything is
acquired, and each record keeps its release command. After a restart, the
cloud peer keeps watching ready leases and releases any launch the restart
interrupted. A failed release is retried every idle check until it succeeds.
These limits hold only while the cloud peer runs; they are not a spending
cap. If the cloud peer is gone for good, release leftover machines yourself. Changing or
removing an offer, or the whole `[cloud]` section, only stops new leases:
existing ones still end on time and are released the way they were made, and
`errand leases` still lists them.

## Configure a cloud peer

Any runner becomes a cloud peer by adding offers to its `errandd.toml`, then
restarting it with `errand setup`:

```toml
[cloud]
max_leases = 2            # active leases across all callers (default 2)
acquire_timeout = "15m"   # acquire plus waiting for errand (default 15m)

[[cloud.offers]]
name = "h100"
os = "linux"
arch = "amd64"
cpus = 26
gpu = "H100 80GB"         # matched by gpu=MODEL; gpus defaults to 1
vram = 80                 # GiB per GPU
price_per_hour = 2.49     # USD, optional; shown when errand leases it
acquire = ["/home/you/cloud/provider.sh", "acquire", "gpu_1x_h100"]
release = ["/home/you/cloud/provider.sh", "release"]
idle_timeout = "20m"      # default 20m
max_lifetime = "12h"      # default 12h
```

`tools = ["python3", "docker"]` declares tools an offer's machines have, and
`os` may be `linux`, `darwin` or `windows`. An offer gets its machines either
from [provider commands](#provider-commands).

Configuring a cloud peer trusts it the way adding peers does: it names the
machines your jobs, workspace snapshots and `--passenv` values go to. Only
add cloud peers you control.

Callers need the `lease` action in the errand capability, separate from
`submit` because leases can cost money. Leasing also needs `submit`, since the
leased machine admits the caller for every action. `allow_users` grants every
action. A caller whose `submit` is taken away is no longer shown where its
leases are, and they end the next time it asks the cloud peer about them.
Until then the machine still admits it, at most until `idle_timeout` or
`max_lifetime`.

```jsonc
"app": { "lydakis.dev/cap/errand": [{ "actions": ["submit", "read-own", "kill-own", "forward-own", "lease"] }] }
```

## Provider commands

A provider is two commands. errand runs them as the runner's user, with the
runner's environment plus:

| Variable | acquire | release |
| --- | --- | --- |
| `ERRAND_LEASE_ID` | yes | yes |
| `ERRAND_OFFER` | yes | yes |
| `ERRAND_LEASE_WHERE` | yes | no |
| `ERRAND_LEASE_LOGIN` | yes, the caller's tailnet login or empty | no |
| `ERRAND_LEASE_SSH_KEY` | yes, the caller's SSH public key | no |
| `ERRAND_LEASE_STATE` | no | the JSON acquire printed, or empty |

**acquire** creates the machine and exits 0 once it exists. Each stderr line is
shown to the waiting client. Its last stdout line must be a JSON object naming
how to reach the machine's runner, with the same fields as a remote personal
peer: `url`, or `ssh` with optional `remote_command` and `remote_socket`.
Local sockets are refused. Any other fields, such as an instance ID, are kept and passed to
release. Stdout is limited to 16 KiB. Name the machine after `ERRAND_LEASE_ID` so release can find it even
when acquire was interrupted before printing anything. With `ssh`, a
`host_key` field (`"ssh-ed25519 AAAA..."`) makes errand accept only that host
key for the machine, which saves a `known_hosts` entry for a machine that just
booted.

**release** destroys the machine. It must succeed when run twice, and when
`ERRAND_LEASE_STATE` is empty.

The machine itself must run errand where clients can reach it: on your
tailnet, or over SSH. For a rented VM on your tailnet, that usually means boot configuration that installs Tailscale with an ephemeral,
tagged auth key, installs errand, and runs `errand setup`. Grant your devices
the errand capability on that tag, and grant the cloud peer at least one
action there so it can read `/v0/info` to watch for idle machines. Shutting
the machine down at its own `max_lifetime` from boot configuration protects
you if the cloud peer is gone for good.

[`cloud/static-pool.sh`](cloud/static-pool.sh) is a complete provider that
leases runners you already have, such as a borrowed GPU box, one owner at a
time. Its release frees the claim but does not stop jobs still running on the
runner, so give its offer a `max_lifetime` longer than any job and release
leases with work running only when that work may continue next to the next
owner's.

## Current limits

- There is no built-in provider yet; machines come from provider commands.
- NVIDIA GPUs only, through `nvidia-smi`.
- Nothing on a leased machine outlives the lease. errand does not rent more machines when your runners are full.
