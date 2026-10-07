"""Recordable live demo: rent one inexpensive GPU, train, fetch, terminate."""
import datetime as dt
import json
import math
import re
import shlex
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
QUERY = "gpu=a10"


def output(command, timeout=40):
    return subprocess.check_output(command, cwd=ROOT, text=True, timeout=timeout,
                                   stderr=subprocess.PIPE)


def leases():
    return json.loads(output(["errand", "leases", "--on", "mini", "--json"]))


def run(command, timeout=90):
    print("\n$ " + shlex.join(command), flush=True)
    subprocess.run(command, cwd=ROOT, check=True, timeout=timeout)


def main():
    cap_check = (
        'from pathlib import Path; import tomllib; '
        'c=tomllib.loads((Path.home()/".config/errand/errandd.toml").read_text()); '
        'print(c.get("cloud",{}).get("lambda",{}).get("max_price_per_hour",10))'
    )
    cap = float(output(["errand", "--on", "mini", "--no-snapshot", "--",
                        "/opt/homebrew/bin/python3", "-c", cap_check]).strip())
    if not 0 < cap <= 1.50:
        raise SystemExit("Set Mini's Lambda max_price_per_hour to 1.50 and restart before this demo.")
    peers = json.loads(output(["errand", "peers", "--json"]))
    mini = next(p for p in peers if p["name"] == "mini")
    offer = next((o for o in mini["info"].get("offers", []) if o["name"] == "gpu-1x-a10"), None)
    if not offer or not 0 < offer.get("price_per_hour", 999) <= 1.50:
        raise SystemExit("No A10 at $1.50/h or less; refusing a more expensive demo.")
    baseline = leases()
    if any(l["state"] in ("ready", "launching", "releasing") for l in baseline):
        raise SystemExit("Existing cloud lease found; leaving it alone. Run this demo when none are active.")
    previous = {l["id"] for l in baseline}
    price = offer["price_per_hour"]
    print("\033[2J\033[H", end="")
    command = ["errand", "--where", QUERY, "--", "python3", "train.py"]
    handle = None
    process = None
    watchdog = None
    log = []
    result = {"query": QUERY, "price_per_hour": price}
    started = time.monotonic()
    print("$ " + shlex.join(command), flush=True)
    try:
        process = subprocess.Popen(command, cwd=ROOT, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, text=True, bufsize=1)

        def interrupt():
            if process.poll() is None:
                print("Demo time limit reached; cancelling the job and releasing its GPU.", flush=True)
                process.send_signal(signal.SIGINT)

        watchdog = threading.Timer(600, interrupt)
        watchdog.daemon = True
        watchdog.start()
        for line in process.stdout:
            print(line, end="", flush=True)
            log.append(line)
            found = re.search(r"errand: job ([a-z0-9-]+/[0-9A-Z]{26})", line)
            if found:
                handle = found.group(1)
        code = process.wait()
        if code:
            raise RuntimeError(f"Remote training exited {code}")
        if not handle:
            raise RuntimeError("No remote job handle was reported")
        target = ROOT / "results" / handle.split("/")[1]
        run(["errand", "fetch", "--output", str(target), handle, "out"])
        result.update({"job_handle": handle, "result_directory": str(target)})
        print("Checkpoint and metrics are back on this MacBook.", flush=True)
    finally:
        if watchdog:
            watchdog.cancel()
        if process and process.poll() is None:
            process.send_signal(signal.SIGINT)
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        (ROOT / "recordings").mkdir(exist_ok=True)
        (ROOT / "recordings" / "training.log").write_text("".join(log))
        created = [l for l in leases() if l["id"] not in previous and l["where"] == QUERY]
        if len(created) > 1:
            raise RuntimeError("Multiple new matching leases found; cleanup needs inspection")
        for lease in created:
            result["lease_id"] = lease["id"]
            if lease["state"] in ("ready", "launching", "releasing"):
                run(["errand", "leases", "release", "--on", "mini", lease["id"]], timeout=240)
            deadline = time.monotonic() + 180
            while True:
                current = next(l for l in leases() if l["id"] == lease["id"])
                if current["state"] == "released" or (
                    current["state"] == "failed" and "released" in current.get("progress", [])
                ):
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError("GPU termination is not confirmed; inspect errand leases immediately")
                time.sleep(3)
            created_at = dt.datetime.fromisoformat(current["created_at"].replace("Z", "+00:00"))
            if current["state"] == "failed":
                print("Failed launch cleaned up; no active demo lease remains.", flush=True)
                result["failed_lease"] = current
                continue
            released_at = dt.datetime.fromisoformat(current["released_at"].replace("Z", "+00:00"))
            minutes = math.ceil((released_at - created_at).total_seconds() / 60)
            estimate = minutes * price / 60
            result.update({"lease": current, "estimated_cost_upper_bound_usd": estimate,
                           "wall_seconds": time.monotonic() - started})
            print(f"\nGPU terminated. Estimated compute cost: <= ${estimate:.2f} before tax.", flush=True)
        (ROOT / "recordings" / "run.json").write_text(json.dumps(result, indent=2) + "\n")
        if not created:
            print("No demo GPU lease was created.", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (KeyboardInterrupt, RuntimeError, subprocess.SubprocessError) as exc:
        print(f"Demo stopped: {exc}", file=sys.stderr, flush=True)
        sys.exit(1)
