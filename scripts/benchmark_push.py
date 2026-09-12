#!/usr/bin/env python3
"""Measure synthetic persistent pushes against an isolated native local daemon."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import shutil
import subprocess
import tempfile
import time


def positive(value):
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def filesystem_facts(path):
    if platform.system() == "Darwin":
        device = subprocess.check_output(["df", "-P", str(path)], text=True).splitlines()[-1].split()[0]
        info = plistlib.loads(subprocess.check_output(["/usr/sbin/diskutil", "info", "-plist", device]))
        return {key: info.get(key) for key in ("FilesystemType", "DeviceIdentifier", "SolidState")}
    return subprocess.check_output(["findmnt", "-T", str(path), "-no", "FSTYPE,SOURCE,OPTIONS"], text=True).strip()


def write_fixture(root, files, directories, file_bytes):
    root.mkdir()
    (root / ".errandignore").write_text("")
    for i in range(files):
        group = root / f"package-{i * directories // files:05d}"
        group.mkdir(exist_ok=True)
        body = (f"file {i:08d}\n" if i % 5 else "shared contents\n").encode()
        (group / f"file-{i:05d}.txt").write_bytes(body.ljust(file_bytes, b"x"))
    (root / "edit.txt").write_text("before\n")


class CommandFailure(RuntimeError):
    def __init__(self, message, details):
        super().__init__(message)
        self.details = details


def output_tail(value):
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    value = value or ""
    return dict(tail=value[-8192:], truncated=len(value) > 8192)


def run(binary, root, env, timeout, *args):
    started = time.monotonic()
    command = [binary, *args]
    try:
        result = subprocess.run(command, cwd=root, env=env, text=True,
                                capture_output=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise CommandFailure(str(error), dict(command=command,
            seconds=time.monotonic() - started, exit_code=None,
            timed_out=isinstance(error, subprocess.TimeoutExpired),
            stdout=output_tail(getattr(error, "stdout", None)),
            stderr=output_tail(getattr(error, "stderr", None)))) from error
    elapsed = time.monotonic() - started
    try:
        if result.returncode:
            raise ValueError(f"command exited {result.returncode}")
        return json.loads(result.stdout), elapsed
    except ValueError as error:
        raise CommandFailure(str(error), dict(command=command, seconds=elapsed,
            exit_code=result.returncode, timed_out=False,
            stdout=output_tail(result.stdout), stderr=output_tail(result.stderr))) from error


def record_failure(report, error, **identity):
    report.setdefault("failures", []).append(dict(identity, error=str(error),
        **(error.details if isinstance(error, CommandFailure) else {})))


def finish_round(futures, report, output, sample, case, started):
    failed = False
    # Join every sibling, even after a failure, so successful measurements survive.
    for name, future in futures:
        try:
            row = future.result()
        except Exception as error:
            failed = True
            record_failure(report, error, workspace=name, sample=sample, case=case)
        else:
            report["samples"].append(row)
            print(json.dumps(row), flush=True)
        save_report(output, report)
    report["rounds"].append(dict(sample=sample, case=case, complete=not failed,
                                 seconds=time.monotonic() - started))
    save_report(output, report)
    if failed:
        raise RuntimeError("push round failed; see report.json failures and daemon.log")


def measure(args, binary, storage, sockets, report):
    # Neither the caller's peer routing nor its receipt/apply state is used.
    env = dict(os.environ, XDG_CONFIG_HOME=str(storage / "config"),
               XDG_STATE_HOME=str(storage / "client-state"))
    config = storage / "config" / "errand" / "errandd.toml"
    config.parent.mkdir(parents=True)
    config.write_text('transport = "local"\nlisten = "none"\n'
                      f'state_dir = {json.dumps(str(storage / "daemon"))}\n'
                      f'socket = {json.dumps(str(sockets / "daemon.sock"))}\n')
    with (args.output / "daemon.log").open("w") as log:
        daemon = subprocess.Popen([binary, "serve", "--config", str(config)], env=env,
                                  stdout=log, stderr=log, cwd=storage)
        try:
            deadline = time.monotonic() + 30
            while not (sockets / "daemon.sock").exists():
                if daemon.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError("daemon failed to start; see daemon.log")
                time.sleep(.05)

            workspaces = []
            for i in range(args.workspaces):
                source = storage / f"source-{i}"
                write_fixture(source, args.files, args.directories, args.file_bytes)
                name = f"bench-{i}"
                try:
                    result, elapsed = run(binary, source, env, args.timeout,
                                          "workspaces", "create", "--on", "local", "--json", name)
                except CommandFailure as error:
                    record_failure(report, error, workspace=name, case="create")
                    raise
                workspaces.append((source, name, result["id"]))
                report["samples"].append(dict(case="create", workspace=name, seconds=elapsed))
                save_report(args.output, report)

            def push(workspace, sample, edit):
                source, name, workspace_id = workspace
                if edit:
                    (source / "edit.txt").write_text(f"edit-{sample}\n")
                selection = ["edit.txt"] if edit else []
                receipt, elapsed = run(binary, source, env, args.timeout, "push", "--on", "local",
                                       "--workspace", name, "--json", "--apply", *selection)
                transfer = receipt["transfer"]
                if transfer["changed_paths"] != int(edit):
                    raise RuntimeError(f"unexpected changed-path count: {receipt}")
                destination = storage / "daemon" / "workspaces" / workspace_id / "data" / "edit.txt"
                if destination.read_bytes() != (source / "edit.txt").read_bytes():
                    raise RuntimeError("applied contents differ from source")
                return dict(case="edit" if edit else "noop", sample=sample,
                            workspace=name, seconds=elapsed, transfer=transfer)

            # Separate workspaces share one daemon and filesystem. Each round
            # finishes before the next; creation and fixtures are outside push timers.
            with ThreadPoolExecutor(max_workers=args.workspaces) as pool:
                for sample in range(args.samples):
                    for edit in (True, False):
                        started = time.monotonic()
                        futures = [(ws[1], pool.submit(push, ws, sample, edit)) for ws in workspaces]
                        finish_round(futures, report, args.output, sample,
                                     "edit" if edit else "noop", started)
        finally:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait()


def save_report(output, report):
    temporary = output / "report.json.tmp"
    temporary.write_text(json.dumps(report, indent=2) + "\n")
    temporary.replace(output / "report.json")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="client and isolated daemon binary")
    parser.add_argument("--output", type=Path, required=True, help="new directory on the filesystem to measure")
    parser.add_argument("--files", type=positive, default=1000)
    parser.add_argument("--directories", type=positive, default=10)
    parser.add_argument("--file-bytes", type=positive, default=1024)
    parser.add_argument("--samples", type=positive, default=3)
    parser.add_argument("--workspaces", type=positive, default=1, help="concurrent pushes to separate workspaces")
    parser.add_argument("--timeout", type=positive, default=300)
    args = parser.parse_args()
    if args.directories > args.files or args.file_bytes < 16:
        parser.error("directories must not exceed files; file-bytes must be at least 16")
    binary = shutil.which(args.binary)
    if binary is None:
        parser.error("binary not found")
    binary = str(Path(binary).resolve())
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=False, mode=0o700)
    report = dict(complete=False, binary_sha256=hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
                  platform=platform.platform(), fixture=dict(files=args.files, directories=args.directories,
                  file_bytes=args.file_bytes, workspaces=args.workspaces), samples=[], rounds=[])
    try:
        report["filesystem"] = filesystem_facts(args.output)
        report["version"] = subprocess.check_output([binary, "version"], text=True).strip()
        # Data stays on the selected disk; AF_UNIX paths stay short on macOS.
        with tempfile.TemporaryDirectory(prefix="data-", dir=args.output) as storage:
            with tempfile.TemporaryDirectory(prefix="err-push-", dir="/tmp") as sockets:
                measure(args, binary, Path(storage), Path(sockets), report)
        report["complete"] = True
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
        print(f"benchmark failed: {error}", flush=True)
    finally:
        save_report(args.output, report)
    return 0 if report["complete"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
