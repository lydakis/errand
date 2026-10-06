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
asks the cloud peer again, which hands you the same lease.

Your machine keeps no record of its leases. The cloud peer lists your active
leases, with how to reach them, whenever errand asks it, so a name like
`cabal-7f3a` works from any process and stops working when the lease ends.

A lease is yours, not one device's. Each device you run errand from has its
own SSH key, and a machine reached over SSH lets in only the keys the cloud
peer added. The cloud peer adds a device's key when that device asks for a
machine the lease matches, or names the lease with `--on cabal-7f3a`; the
device waits until it has. Until then the lease is not a peer on that device,
though `errand leases` lists it. No private key leaves its device. All this
needs the cloud peer to answer: if you point the name `cabal` at another
runner, its leases, and job handles such as `cabal-7f3a/01K...`, are no longer
reachable by name, and `errand leases release` cannot reach them either. The
cloud peer still ends them when they go idle or reach their lifetime.

## When errand leases

- Only when no reachable runner of yours matches the requirements. A matching
  runner that is busy queues the job; errand does not rent a second machine.
- Never for `--where '*'`.
- Every reachable cloud peer with a matching offer can supply the machine.
  The cheapest matching offer wins: an offer with `price_per_hour` before one
  without, and equal offers in random order. Neither your default peer nor how
  busy a cloud peer's own runner is plays a part. If a cloud peer turns the
  request down before starting anything (no permission, nothing matches any
  more, or it is at `max_leases`), the next one is asked. A cloud peer that
  does not answer is skipped, so keep offers on an always-on machine. A ready
  or launching lease of yours that already matches is reused, from any of
  your devices.
- An offer's facts are a claim. The lease becomes ready only once the machine
  answers as an errand runner whose measured facts match, and the runner
  rechecks them when the job is admitted.

If a run stops while its lease is still launching, from Ctrl-C or a failure,
it withdraws its request, and the cloud peer cancels the launch unless another
run is waiting for the same lease. Once a lease is ready, only the idle and
lifetime rules below or `errand leases release` end it. A run that stops after
that, with or without a job, leaves the machine up for at most one idle
window. Being given to a run counts as work, so a reused ready lease starts a
full idle window.

## Leases

```sh
errand leases                     # your leases on every cloud peer
errand leases --json
errand leases release cabal-7f3a  # release now; a full lease ID works too
```

The cloud peer logs each lease's launch, readiness, release and any failed
release attempts.

The cloud peer releases a ready lease when its runner has had no staging,
starting, running or queued jobs for the offer's `idle_timeout`, or when the
lease reaches `max_lifetime`, even if a job is still running. A runner it
cannot reach counts as idle. Persistent workspaces and retained results on a
leased machine end with the lease, so fetch what you need first.

Leases are recorded in the cloud peer's state directory before anything is
acquired, and each record keeps how to release its machine: the release
command, or the Lambda API key file. After a restart, the cloud peer keeps
watching ready leases and releases any launch the restart interrupted. A
failed release is retried every idle check until it succeeds. Changing or
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
from [Lambda](#lambda) or from [provider commands](#provider-commands).

Configuring a cloud peer trusts it the way adding peers does: it names the
machines your jobs, workspace snapshots and `--passenv` values go to. Only
add cloud peers you control.

Callers need the `lease` action in the errand capability, separate from
`submit` because leases can cost money. Leasing also needs `submit`, since the
leased machine admits the caller for every action. `allow_users` grants every
action. A caller whose `submit` is taken away is no longer shown where its
leases are, and they end at its next request to the cloud peer.
Until then the machine still admits it, at most until `idle_timeout` or
`max_lifetime`.

```jsonc
"app": { "lydakis.dev/cap/errand": [{ "actions": ["submit", "read-own", "kill-own", "forward-own", "lease"] }] }
```

## Lambda

A `[cloud.offers.lambda]` table rents [Lambda Cloud](https://lambda.ai)
instances. For each lease the cloud peer launches an instance named
`errand-<lease id>`, waits for it to boot, and then connects over SSH. It
sends errand, the runner's config and any keys as one archive and runs a
fixed install script from it
([`internal/cloud/lambda_install.sh`](../internal/cloud/lambda_install.sh)),
which starts the runner as a system service. Releasing terminates the
instance, and a lease counts as released only once Lambda lists its instance
as terminated. Lambda accepts one launch per account every 12 seconds, so leases
launched together take turns.

Clients reach the machine one of two ways:

- **Over SSH** (the default). The runner listens on no port, and jobs reach
  it over SSH on port 22, as they reach any SSH peer. Each machine you run
  errand from makes an SSH key of its own and sends the public half with the
  lease request; the leased machine admits only that key. The lease carries
  the machine's host key, so errand checks it without a `known_hosts` entry.
- **Over your tailnet**, when `tailscale_auth_key_file` is set. The machine
  joins as `errand-<lease id>`, and the runner listens only on the tailnet.

```toml
[[cloud.offers]]
name = "h100"
gpu = "H100 PCIe"
vram = 80
cpus = 26
price_per_hour = 2.49

[cloud.offers.lambda]
instance_type = "gpu_1x_h100_pcie"
regions = ["us-east-1", "us-west-1"]     # preference order; omit for any
api_key_file = "/home/you/.config/errand/lambda-api-key"
# file_systems = ["datasets"]            # Lambda filesystems to attach (one region)
# errand_binary = "/path/to/linux-amd64/errand"
# tailscale_auth_key_file = "/home/you/.config/errand/tailscale-lease-key"
# allow_users = ["you@github"]           # with Tailscale: extra logins the machine admits
```

Lambda offers default to `os = "linux"` and `arch = "amd64"`. The machine
runs the cloud peer's own errand executable when the cloud peer is
`linux/amd64` too. Otherwise, for example a cloud peer on a Mac or an `arm64`
GH200 offer, `errand_binary` must name a Linux build of errand for the offer's
architecture. errand checks this before renting anything.

Set up once:

1. Create a Lambda API key and save it alone in `api_key_file`, owned by the
   user errand runs as, with mode 600. errand refuses files other users can
   read.
2. Restart the cloud peer with `errand setup`.

That is all for SSH. The cloud peer needs `ssh`, and so do the machines you
run errand from. On first use the cloud peer makes an SSH
key in its state directory and adds the public half to your Lambda account
as `errand-<fingerprint>`; it installs errand and watches the runner with
it. For each lease it also makes a fresh SSH host key and hands it to the
instance through cloud-init, then refuses any other host key, so no
connection to the machine can be intercepted. Clients get the same host key
with the lease. The host key's private half is in that lease's Lambda launch
data.

To use your tailnet instead, do these too before restarting:

1. Create a reusable, ephemeral, pre-approved auth key tagged
   `tag:errand-lease` and save it in `tailscale_auth_key_file`, with the same
   ownership and mode. The key goes to the machine over SSH, never in
   Lambda's launch metadata. Ephemeral nodes leave the tailnet on their own
   once terminated.
2. Let your devices reach the tag in your tailnet policy:

   ```jsonc
   "tagOwners": { "tag:errand-lease": ["autogroup:admin"] },
   "grants": [{ "src": ["autogroup:member"], "dst": ["tag:errand-lease"], "ip": ["tcp:7443"] }]
   ```

Over the tailnet, the leased runner admits the tailnet login that asked for
the lease, plus `allow_users`. A lease asked for over SSH or the cloud peer's local socket has
no tailnet login, so without `allow_users` it is refused before anything is
rented. The cloud peer must be able to read the runner's `/v0/info` to see
when it is idle. If the cloud peer is signed in as you, that already works. If
it is a tagged node, add a login that can reach it to `allow_users` or grant
it the errand capability on `tag:errand-lease`. Lease peer names are
MagicDNS names, so MagicDNS must be on.

Lambda bills from launch until the instance is terminated. Shutting the
machine down from inside does not stop billing, so errand relies on the cloud
peer: it terminates on release, after `idle_timeout`, and at `max_lifetime`,
and keeps retrying a failed termination. `max_leases` caps how many instances
can run at once. These limits hold only while the cloud peer runs; they are not
a spending cap, and errand sets none on the Lambda account. If the cloud peer is
gone for good, terminate leftovers named `errand-*` in the Lambda console. Leases keep using the `api_key_file` they
were made with, so keep that file, and the key in it, until they are released:
a different key may belong to another account, so errand keeps retrying the
release of any lease whose machine the key cannot see.

When no listed region has capacity, the lease fails at once with a message
saying so. errand does not wait for capacity. With `file_systems`, the machine
can only launch in the region that holds them, so that is the one region
errand tries; the file systems must all be in that region.

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
booted. With or without it, clients offer the key they sent as
`ERRAND_LEASE_SSH_KEY`, so the machine can admit them by it. A target that
breaks these rules fails the launch as soon as acquire exits, and its machine
is released then.

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

- Lambda is the only built-in provider; others use commands.
- NVIDIA GPUs only, through `nvidia-smi`.
- Only Lambda filesystems outlive a lease. errand does not rent more machines when your runners are full.
