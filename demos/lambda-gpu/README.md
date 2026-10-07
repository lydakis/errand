# A MacBook borrows a GPU

This Errand 0.8 demo rents a Lambda A10 through the Mini and trains a small
character-level nanoGPT on tiny Shakespeare. Training stops at 800 steps or
about 60 seconds. The before/after loss uses the same held-out batches.

## Requirements

Use Errand 0.8 or newer with a [Lambda cloud runner](../../docs/CLOUD.md)
configured on a trusted machine. The GPU image needs CUDA-enabled PyTorch;
this demo does not install it. The tested image used PyTorch 2.7.0 and CUDA 12.8.

The optional `price_cap.py` and `demo.py` helpers use a macOS runner named
`mini` with Homebrew Python at `/opt/homebrew/bin/python3`. The cap helper
expects no existing explicit `max_price_per_hour`, temporarily sets it to
1.50, and restarts that runner. Use it only when the runner is idle. For a
different setup, configure your own hourly cap and run the training command
below directly.

## Before recording

From the Errand checkout root, change to this folder and prepare the temporary
$1.50/hour cap:

```sh
cd demos/lambda-gpu
python3 price_cap.py prepare
clear
```

## On camera

Run the actual training command:

```sh
errand --where gpu=a10 -- python3 train.py
```

`.errand.toml` selects this folder as the workspace, retains `out`, disables
automatic application of remote changes, and makes Python output unbuffered.
`.errandignore` excludes earlier results, recordings, and Python caches from
uploads. No extra run flags are needed.

Copy the job handle printed by Errand to fetch the checkpoint and metrics:

```sh
errand fetch --output results/take-1 JOB_HANDLE out
```

List the leases, then release the demo GPU using its peer name or lease ID:

```sh
errand leases
errand leases release LEASE_PEER
```

Replace `JOB_HANDLE` and `LEASE_PEER` with the actual values shown by Errand.
Release the lease even if training fails or you interrupt it. A completed job
can leave the GPU rented until its idle timeout or explicit release.

## After recording

Once GPU termination is confirmed, restore Mini's previous configuration:

```sh
python3 price_cap.py restore
```

Provisioning can take several minutes. At the tested $1.29/hour rate, five
minutes costs about $0.11 before tax. The price cap limits the hourly rate;
it is not a total spending limit.

`python3 demo.py` is an optional automated test with a ten-minute cancellation
timer, output fetching, and verified lease cleanup. The recording commands above
run the CLI directly.

Training source is `train.py`; the vendored model is `vendor/model.py` from
[karpathy/nanoGPT](https://github.com/karpathy/nanoGPT), pinned at
`3adf61e154c3fe3fca428ad6bc3818b27a3b8291`. Its MIT license is in `vendor/LICENSE`.
Dataset: [tiny Shakespeare](https://github.com/karpathy/char-rnn/blob/master/data/tinyshakespeare/input.txt).
