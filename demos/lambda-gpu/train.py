"""One-minute, from-scratch character-level nanoGPT training on a real GPU."""
import json
import math
import time
from dataclasses import asdict
from pathlib import Path

import torch

from vendor.model import GPT, GPTConfig


def main():
    if not torch.cuda.is_available():
        raise SystemExit("This demo requires CUDA; refusing to train on CPU.")
    torch.manual_seed(1337)
    torch.set_num_threads(4)
    torch.backends.cuda.matmul.allow_tf32 = True
    torch.backends.cudnn.allow_tf32 = True
    gpu = torch.cuda.get_device_name(0)
    print(f"GPU: {gpu} | PyTorch {torch.__version__} | CUDA {torch.version.cuda}", flush=True)
    text = Path("data/shakespeare.txt").read_text()
    chars = sorted(set(text))
    encode = {c: i for i, c in enumerate(chars)}
    data = torch.tensor([encode[c] for c in text], dtype=torch.long, device="cuda")
    split = int(len(data) * 0.9)
    train, valid = data[:split], data[split:]
    config = GPTConfig(block_size=128, vocab_size=len(chars), n_layer=4,
                       n_head=4, n_embd=128, dropout=0.0, bias=False)
    model = GPT(config).cuda()
    optimizer = torch.optim.AdamW(model.parameters(), lr=1e-3, weight_decay=0.01)
    offsets = torch.arange(config.block_size, device="cuda")

    def batch(source):
        starts = torch.randint(len(source) - config.block_size - 1, (32,), device="cuda")
        indices = starts[:, None] + offsets
        return source[indices], source[indices + 1]

    # Compare before/after on precisely the same held-out batches.
    held_out = [batch(valid) for _ in range(8)]

    @torch.no_grad()
    def evaluate():
        model.eval()
        loss = sum(model(x, y)[1].item() for x, y in held_out) / len(held_out)
        model.train()
        return loss

    before = evaluate()
    print(f"Shakespeare: {len(text):,} characters | {len(chars)} tokens | 4-layer nanoGPT", flush=True)
    print(f"Before training: held-out loss {before:.3f}", flush=True)
    start = time.monotonic()
    trace = []
    for step in range(1, 801):
        x, y = batch(train)
        _, loss = model(x, y)
        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
        optimizer.step()
        if step == 1 or step % 100 == 0:
            elapsed = time.monotonic() - start
            value = loss.item()
            trace.append({"step": step, "loss": value, "elapsed_seconds": elapsed})
            print(f"step {step:4d}/800 | train loss {value:.3f} | {elapsed:5.1f}s", flush=True)
        if time.monotonic() - start >= 60:
            break
    torch.cuda.synchronize()
    seconds = time.monotonic() - start
    after = evaluate()
    improvement = 100 * (1 - after / before)
    print(f"After training:  held-out loss {after:.3f} ({improvement:.1f}% lower)", flush=True)
    print(f"Trained {step:,} steps / {step * 32 * 128:,} tokens in {seconds:.1f}s", flush=True)
    if not math.isfinite(after) or not after < before:
        raise SystemExit("Training did not improve held-out loss.")
    model.eval()
    prompt = "THE KING:\n"
    with torch.no_grad():
        context = torch.tensor([[encode[c] for c in prompt]], device="cuda")
        sample = model.generate(context, 160, temperature=0.7, top_k=20)[0].tolist()
    sample_text = "".join(chars[i] for i in sample)
    print("\nSample from this tiny model (brief training, rough text):", flush=True)
    print(sample_text, flush=True)
    out = Path("out")
    out.mkdir(exist_ok=True)
    torch.save({"model": model.state_dict(), "config": asdict(config), "chars": chars}, out / "checkpoint.pt")
    metrics = {"gpu": gpu, "torch": torch.__version__, "cuda": torch.version.cuda,
               "steps": step, "training_seconds": seconds, "tokens": step * 32 * 128,
               "validation_loss_before": before, "validation_loss_after": after,
               "improvement_percent": improvement, "trace": trace, "sample": sample_text,
               "nanogpt_commit": "3adf61e154c3fe3fca428ad6bc3818b27a3b8291"}
    (out / "metrics.json").write_text(json.dumps(metrics, indent=2) + "\n")
    (out / "sample.txt").write_text(sample_text + "\n")
    print("Saved out/checkpoint.pt, metrics.json, and sample.txt", flush=True)


if __name__ == "__main__":
    main()
