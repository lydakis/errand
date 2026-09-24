#!/usr/bin/env python3
"""Compare one-shot push and watch using verified files on an isolated daemon."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import re
import signal
import shutil
import subprocess
import tempfile
import threading
import time

from benchmark_push import filesystem_facts, positive, run, save_report, write_fixture


def cpu_seconds(pid):
    if platform.system() == "Linux":
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        ticks = os.sysconf("SC_CLK_TCK")
        return (int(fields[11]) + int(fields[12])) / ticks, 1 / ticks
    value = subprocess.check_output(["ps", "-p", str(pid), "-o", "time="], text=True).strip()
    days, _, value = value.rpartition("-")
    parts = [float(p) for p in value.split(":")]
    resolution = 10 ** -len(value.rsplit(".", 1)[1]) if "." in value else 1
    return (int(days or 0) * 86400) + sum(n * 60**i for i, n in enumerate(reversed(parts))), resolution


def write_edit(args, path, body):
    if args.save_mode == "atomic":
        temporary = path.with_name(path.name + ".save")
        temporary.write_text(body)
        temporary.replace(path)
    else:
        path.write_text(body)


def wait_absent(path, timeout=60):
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        if not path.exists():
            return time.monotonic()
        time.sleep(.002)
    raise RuntimeError(f"deletion delivery timed out: {path.name}")


def apply_change(args, root, remote_root, sample, prefix):
    """Make one measured source change; return its start time and a delivery waiter."""
    if args.change == "create":
        name, body = f"created-{prefix}-{sample}.txt", f"{prefix}-{sample}\n"
        started = time.monotonic()
        write_edit(args, root / name, body)
        return started, lambda: wait_contents(remote_root / name, body), name
    if args.change == "delete":
        name = f"victim-{prefix}-{sample}.txt"
        started = time.monotonic()
        (root / name).unlink()
        return started, lambda: wait_absent(remote_root / name), name
    body = f"{prefix}-{sample}\n"
    started = time.monotonic()
    write_edit(args, root / "edit.txt", body)
    return started, lambda: wait_contents(remote_root / "edit.txt", body), "edit.txt"


TRACE = re.compile(r"errand watch: prepare=(\S+) reason=(\S+) dirty=(\d+) entries=(\d+) elapsed_us=(\d+) err=(\S+)")


def summarize_trace(log_path):
    """Count watch preparation modes and fallback reasons from ERRAND_TRACE_WATCH output."""
    rows = []
    for line in log_path.read_text(errors="replace").splitlines():
        match = TRACE.search(line)
        if match:
            mode, reason, dirty, entries, elapsed, err = match.groups()
            rows.append(dict(mode=mode, reason=reason, dirty=int(dirty), entries=int(entries),
                             elapsed_ms=int(elapsed)/1000, error=err == "true"))
    summary = {}
    for row in rows:
        key = f"{row['mode']}:{row['reason']}"
        item = summary.setdefault(key, dict(count=0, elapsed_ms=[]))
        item["count"] += 1
        item["elapsed_ms"].append(row["elapsed_ms"])
    return dict(preparations=rows, by_reason=summary)


def tree_contents(root):
    result = {}
    for directory, names, files in os.walk(root):
        names[:] = [name for name in names if name not in (".git", "ignored")]
        for name in files:
            path = Path(directory) / name
            result[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def wait_contents(path, expected, timeout=60):
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        try:
            if path.read_text() == expected:
                return time.monotonic()
        except FileNotFoundError:
            pass
        time.sleep(.002)
    raise RuntimeError(f"file delivery timed out: {path.name}")


def measure_mutagen(args, root, storage, report):
    if not args.mutagen:
        return
    binary = str(Path(args.mutagen).resolve())
    # A short, unique data path keeps this daemon separate from user sessions
    # and avoids the Unix socket pathname limit.
    with tempfile.TemporaryDirectory(prefix="errand-mutagen-") as state:
        env = dict(os.environ, MUTAGEN_DATA_DIRECTORY=state)
        target = storage / "mutagen-destination"
        target.mkdir()
        def command(*parts):
            return subprocess.check_output([binary, *parts], env=env, text=True, stderr=subprocess.STDOUT, timeout=60)
        report["mutagen"] = command("version").strip()
        try:
            command("sync", "create", "--name", "bench", "--no-global-configuration", "--mode", "one-way-safe", "--ignore", "ignored/", "--ignore-vcs", str(root), str(target))
            command("sync", "flush", "bench")
            for sample in range(args.samples):
                # Let the preceding cycle settle before starting a warm edit.
                command("sync", "flush", "bench")
                started, check, _ = apply_change(args, root, target, sample, "mutagen")
                delivered = check()
                report["samples"].append(dict(mode="mutagen", sample=sample, delivery_seconds=delivered-started))
        finally:
            command("daemon", "stop")


def measure(args, storage, socket_dir, report):
    binary = str(Path(args.binary).resolve())
    env = dict(os.environ, XDG_CONFIG_HOME=str(storage / "config"), XDG_STATE_HOME=str(storage / "client"))
    config = storage / "config" / "errand" / "errandd.toml"
    config.parent.mkdir(parents=True)
    config.write_text('transport = "local"\nlisten = "none"\n'
                      f'state_dir = {json.dumps(str(storage / "daemon"))}\n'
                      f'socket = {json.dumps(str(socket_dir / "daemon.sock"))}\n')
    with (args.output / "daemon.log").open("w") as log:
        daemon = subprocess.Popen([binary, "serve", "--config", str(config)], env=env, cwd=storage, stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 30
            while not (socket_dir / "daemon.sock").exists():
                if daemon.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("daemon failed to start")
                time.sleep(.01)
            root = storage / "source"
            write_fixture(root, args.files, max(1, args.files // 100), 1024)
            (root / ".errandignore").write_text("ignored/\n")
            if args.change == "delete":
                for prefix in ("rsync", "mutagen", "once", "watch"):
                    for sample in range(args.samples):
                        (root / f"victim-{prefix}-{sample}.txt").write_text(f"victim {prefix} {sample}\n")
            if args.selection == "git":
                (root / ".errandignore").unlink()
                (root / ".gitignore").write_text("ignored/\n")
                git = ["git", "-C", str(root), "-c", "user.name=bench", "-c", "user.email=bench@example.invalid"]
                subprocess.run([*git, "init", "-q"], check=True, capture_output=True)
                if args.git_tracking != "none":
                    tracked = ["."] if args.git_tracking == "all" else [
                        ".gitignore", "edit.txt", *sorted(p.name for p in root.glob("package-*") if int(p.name.split("-")[1]) % 2 == 0),
                        *sorted(p.name for p in root.glob("victim-*"))]
                    subprocess.run([*git, "add", "--", *tracked], check=True, capture_output=True)
                    subprocess.run([*git, "commit", "-q", "-m", "fixture"], check=True, capture_output=True)
            (root / "ignored").mkdir()
            ws, _ = run(binary, root, env, 60, "workspaces", "create", "--on", "local", "--json", "bench")
            remote = storage / "daemon" / "workspaces" / ws["id"] / "data" / "edit.txt"
            rsync = shutil.which("rsync")
            if rsync:
                target = storage / "rsync-destination"
                target.mkdir()
                command = [rsync, "-a", "--checksum", "--delete", "--exclude=ignored/", "--exclude=.git/", str(root)+"/", str(target)+"/"]
                subprocess.run(command, check=True, capture_output=True)
                for sample in range(args.samples):
                    _, check, _ = apply_change(args, root, target, sample, "rsync")
                    started = time.monotonic()
                    subprocess.run(command, check=True, capture_output=True)
                    elapsed = time.monotonic()-started
                    check()  # already delivered; raises only if rsync missed it
                    report["samples"].append(dict(mode="rsync-checksum", sample=sample, seconds=elapsed))
                report["rsync"] = subprocess.check_output([rsync, "--version"], text=True).splitlines()[0]
            measure_mutagen(args, root, storage, report)
            push = ["push", "--on", "local", "--workspace", "bench", "--apply", "--json", "edit.txt"]
            once = push[:-1] if args.change != "edit" else push
            for sample in range(0 if args.skip_once else args.samples):
                _, check, _ = apply_change(args, root, remote.parent, sample, "once")
                receipt, elapsed = run(binary, root, env, 60, *once)
                check()
                report["samples"].append(dict(mode="once", sample=sample, seconds=elapsed, transfer=receipt["transfer"]))
                save_report(args.output, report)

            rows = queue.Queue()
            started = time.monotonic()
            with (args.output / "watch.log").open("w") as watch_log:
                watch_env = dict(env, ERRAND_TRACE_WATCH="1") if args.trace else env
                watch_paths = [push[-1]] if args.change == "edit" else []
                watcher = subprocess.Popen([binary, *push[:-1], "--watch", *watch_paths], cwd=root, env=watch_env,
                                           text=True, stdout=subprocess.PIPE, stderr=watch_log)
                def read_rows():
                    try:
                        for line in watcher.stdout:
                            rows.put((time.monotonic(), json.loads(line)))
                    finally:
                        rows.put((time.monotonic(), None))
                reader = threading.Thread(target=read_rows, daemon=True)
                reader.start()
                def next_row():
                    stamp, row = rows.get(timeout=60)
                    if row is None or row.get("status") not in ("applied", "unchanged"):
                        raise RuntimeError(f"watch failed: {row}")
                    return stamp, row
                try:
                    stamp, _ = next_row()
                    report["watch_start_seconds"] = stamp - started
                    for sample in range(args.samples):
                        time.sleep(args.pause_seconds)
                        started, check, _ = apply_change(args, root, remote.parent, sample, "watch")
                        delivered = check()
                        stamp, receipt = next_row()
                        check()
                        row = dict(mode="watch", sample=sample, seconds=stamp-started, delivery_seconds=delivered-started, transfer=receipt["transfer"])
                        report["samples"].append(row)
                        print(json.dumps(row), flush=True)
                        save_report(args.output, report)

                    before, resolution = cpu_seconds(watcher.pid)
                    started = time.monotonic()
                    for sample in range(100):
                        (root / "ignored" / "build").write_text(str(sample))
                    time.sleep(args.idle_seconds)
                    elapsed = time.monotonic() - started
                    report["idle"] = dict(seconds=elapsed, cpu_seconds=cpu_seconds(watcher.pid)[0]-before, cpu_resolution_seconds=resolution,
                                          unexpected_receipts=rows.qsize())
                    if not rows.empty():
                        raise RuntimeError("idle or ignored churn produced a transfer")

                    started = time.monotonic()
                    for sample in range(20):
                        write_edit(args, root / "edit.txt", f"burst-{sample}\n")
                        time.sleep(.005)
                    delivered = wait_contents(remote, "burst-19\n")
                    receipts = []
                    while True:
                        stamp, receipt = next_row()
                        receipts.append(receipt)
                        if remote.read_text() == "burst-19\n" and rows.empty():
                            break
                    time.sleep(.4)
                    while not rows.empty():
                        stamp, receipt = next_row()
                        receipts.append(receipt)
                    report["burst"] = dict(edits=20, receipts=len(receipts), seconds=stamp-started,
                                           delivery_seconds=delivered-started, transfer=receipts[-1]["transfer"])
                    if len(receipts) > 20 or remote.read_text() != "burst-19\n":
                        raise RuntimeError("save burst did not converge with bounded transfers")
                finally:
                    watcher.send_signal(signal.SIGINT)
                    try:
                        code = watcher.wait(timeout=60)
                    except subprocess.TimeoutExpired:
                        watcher.kill(); watcher.wait(); raise
                    reader.join(timeout=5)
                    if reader.is_alive():
                        raise RuntimeError("watch output reader did not finish")
                    watcher.stdout.close()
                    report["watch_exit_code"] = code
                if code:
                    raise RuntimeError(f"watch failed to stop cleanly: {code}")
                # Include receipts emitted while draining an in-flight push on
                # shutdown. A temporarily empty queue does not establish quiescence.
                while not rows.empty():
                    stamp, receipt = rows.get_nowait()
                    if receipt is None:
                        continue
                    if receipt.get("status") not in ("applied", "unchanged"):
                        raise RuntimeError(f"watch failed during shutdown: {receipt}")
                    receipts.append(receipt)
                    report["burst"].update(receipts=len(receipts), seconds=stamp-started, transfer=receipt["transfer"])
                if len(receipts) > 20:
                    raise RuntimeError("burst exceeded one transfer per save")
                if tree_contents(root) != tree_contents(remote.parent):
                    raise RuntimeError("watch destination tree differs")
                report["whole_tree_verified"] = True
                if args.trace:
                    report["watch_trace"] = summarize_trace(args.output / "watch.log")
        finally:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill(); daemon.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--mutagen", help="optional Mutagen binary; uses an isolated daemon")
    parser.add_argument("--output", required=True, type=Path, help="new directory on the filesystem to measure")
    parser.add_argument("--files", type=positive, default=1000)
    parser.add_argument("--samples", type=positive, default=5)
    parser.add_argument("--idle-seconds", type=positive, default=3)
    parser.add_argument("--selection", choices=("explicit", "git"), default="explicit")
    parser.add_argument("--save-mode", choices=("inplace", "atomic"), default="inplace")
    parser.add_argument("--git-tracking", choices=("none", "all", "mixed"), default="none",
                        help="with --selection git: commit no files, every file, or half the packages")
    parser.add_argument("--change", choices=("edit", "create", "delete"), default="edit",
                        help="measured change: edit edit.txt, create a new root file, or delete a root file")
    parser.add_argument("--skip-once", action="store_true", help="skip one-shot push samples")
    parser.add_argument("--trace", action="store_true", help="record watch preparation modes and fallback reasons")
    parser.add_argument("--pause-seconds", type=float, default=0, help="idle interval before each measured watch save")
    args = parser.parse_args()
    if args.pause_seconds < 0:
        parser.error("--pause-seconds must be nonnegative")
    if args.git_tracking != "none" and args.selection != "git":
        parser.error("--git-tracking requires --selection git")
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=False, mode=0o700)
    report = dict(complete=False, platform=platform.platform(), filesystem=filesystem_facts(args.output),
                  binary_sha256=hashlib.sha256(Path(args.binary).read_bytes()).hexdigest(), files=args.files,
                  selection=args.selection, save_mode=args.save_mode, pause_seconds=args.pause_seconds,
                  git_tracking=args.git_tracking, change=args.change, samples=[])
    try:
        with tempfile.TemporaryDirectory(prefix="storage-", dir=args.output) as storage, tempfile.TemporaryDirectory(prefix="errand-watch-") as sockets:
            measure(args, Path(storage), Path(sockets), report)
        report["complete"] = True
    finally:
        save_report(args.output, report)


if __name__ == "__main__":
    main()
