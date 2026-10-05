#!/usr/bin/env python3
"""Time the edit, run, inspect loop: saves, job start, first output, results back.

Each case runs the CLI as a user would, from a generated Git checkout of small
files, against configured peers (or an isolated local daemon with --isolated):

  workspace-create  `workspaces create` from a new checkout
  watch-start   `push --watch` until its first receipt
  save          one-file save: until the runner's copy shows it (visible) and
                until the watch reports its receipt
  burst         20 saves 5 ms apart: until the last is visible, and the last receipt
  job-cold      first `-- true` from a checkout with new content (everything ships)
  job-warm      `-- true` again with nothing changed (nothing ships)
  job-edit      one file edited, then `-- true` (one file ships)
  first-output  `-- echo ready`: until "ready" reaches stdout, and until exit
  apply         `--apply -- sh -c 'echo N > out.txt'`: until exit with out.txt applied
  detach        `-d -- true` until the handle returns, then `attach` until it exits
  fetch-apply   `fetch --apply` of a finished detached job that wrote out.txt

Every command records wall time and the client's CPU time; with --isolated the
daemon's CPU time is recorded too. Seeing a save on a configured peer needs
--observe PEER=SSH_HOST: a small Python poller runs there over SSH and watches
the workspace copy under the runner's state directory (default ~/.errand), so
visible times include one network hop back. Without it, saves report receipts only. A sample counts only when the CLI reported the
expected shipping and the job's terminal receipt confirms success, checked
outside the timer.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import re
import resource
import shlex
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import uuid

from benchmark import checked_json, parse_transfer, positive, successful_receipt
from benchmark_push import filesystem_facts
from benchmark_watch import cpu_seconds

JOB = re.compile(r"^errand: job (\S+) \(", re.M)
SAVE_PAUSE_SECONDS = .5
BURST_EDITS = 20
# Reports when edit.txt holds each requested body; standard library only, so it
# runs unchanged on any runner with python3.
POLLER = r"""
import os, sys, time
path, timeout = sys.argv[1], float(sys.argv[2])
print("ready" if os.path.isdir(os.path.dirname(path)) else "missing " + path, flush=True)
for line in sys.stdin:
    want = line[:-1] + "\n"
    deadline = time.monotonic() + timeout
    while True:
        try:
            with open(path) as f:
                if f.read() == want:
                    print("seen", flush=True)
                    break
        except FileNotFoundError:
            pass
        if time.monotonic() > deadline:
            print("timeout", flush=True)
            break
        time.sleep(.002)
"""


def children_cpu():
    usage = resource.getrusage(resource.RUSAGE_CHILDREN)
    return usage.ru_utime + usage.ru_stime


def write_fixture(root, files, nonce):
    root.mkdir()
    for i in range(files):
        directory = root / f"package-{i // 100:04d}"
        directory.mkdir(exist_ok=True)
        (directory / f"file-{i:06d}.txt").write_text(f"{nonce} file {i}\n")
    (root / "edit.txt").write_text(f"{nonce} before\n")
    # Signing would ask for the user's key, or fail without it.
    git = ["git", "-C", str(root), "-c", "user.email=bench@example.invalid", "-c", "user.name=bench",
           "-c", "commit.gpgsign=false"]
    subprocess.run([*git, "init", "-q"], check=True)
    subprocess.run([*git, "add", "-A"], check=True)
    subprocess.run([*git, "commit", "-qm", "fixture"], check=True)


class Loop:
    def __init__(self, binary, env, timeout, daemon=None):
        self.binary, self.env, self.timeout, self.daemon = binary, env, timeout, daemon

    def run(self, cwd, *args, marker=None):
        """Run the CLI; return timings, CPU and output, or raise on failure."""
        client = children_cpu()
        daemon = cpu_seconds(self.daemon.pid)[0] if self.daemon else None
        started = time.monotonic()
        process = subprocess.Popen([self.binary, *args], cwd=cwd, env=self.env, text=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        watchdog = threading.Timer(self.timeout, process.kill)
        watchdog.start()
        first, lines = None, []
        try:
            for line in process.stdout:
                if marker and first is None and line.strip() == marker:
                    first = time.monotonic() - started
                lines.append(line)
            stderr = process.stderr.read()
            code = process.wait()
        finally:
            watchdog.cancel()
        wall = time.monotonic() - started
        if wall >= self.timeout:
            raise ValueError(f"errand {' '.join(args)} timed out after {self.timeout} s")
        stdout = "".join(lines)
        if code != 0:
            raise ValueError(f"errand {' '.join(args)} exited {code}: {stderr[-2000:]}")
        row = dict(wall_ms=wall * 1000, client_cpu_ms=(children_cpu() - client) * 1000)
        if marker:
            if first is None:
                raise ValueError(f"{marker!r} never reached stdout")
            row["first_output_ms"] = first * 1000
        if self.daemon:
            row["daemon_cpu_ms"] = (cpu_seconds(self.daemon.pid)[0] - daemon) * 1000
        return row, stdout, stderr

    def job(self, cwd, *args, scenario, marker=None):
        """Submit a job; return its timings with the shipping the CLI reported, and its handle."""
        row, _, stderr = self.run(cwd, *args, marker=marker)
        row.update(parse_transfer(stderr, scenario))
        match = JOB.search(stderr)
        if not match:
            raise ValueError("no job handle in the output")
        return row, match.group(1)

    def verify(self, cwd, job):
        successful_receipt(checked_json(self.binary, ["status", "--json", job], cwd, self.env))


class NoOutput(ValueError):
    pass


class Lines:
    """A child process whose stdout lines are timestamped as they arrive."""

    def __init__(self, argv, cwd, env, stdin=None, stderr=subprocess.DEVNULL):
        self.process = subprocess.Popen(argv, cwd=cwd, env=env, text=True, stdin=stdin,
                                        stdout=subprocess.PIPE, stderr=stderr)
        self.lines = queue.Queue()
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()

    def read(self):
        try:
            for line in self.process.stdout:
                self.lines.put((time.monotonic(), line.rstrip("\n")))
        finally:
            self.lines.put((time.monotonic(), None))

    def next(self, timeout):
        try:
            stamp, line = self.lines.get(timeout=timeout)
        except queue.Empty:
            raise NoOutput(f"no output from {self.process.args[0]} within {timeout} s") from None
        if line is None:
            raise ValueError(f"{self.process.args[0]} exited early")
        return stamp, line

    def send(self, line):
        self.process.stdin.write(line + "\n")
        self.process.stdin.flush()

    def stop(self, timeout):
        """Interrupt the process; return its exit code, or None if it had to be killed."""
        self.process.send_signal(signal.SIGINT)
        try:
            code = self.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
            code = None
        self.reader.join(timeout=5)
        return code

    def remaining(self):
        """Lines that arrived but were never read."""
        out = []
        while True:
            try:
                _, line = self.lines.get_nowait()
            except queue.Empty:
                return out
            if line is not None:
                out.append(line)


def observer_command(target, workspace, timeout):
    """Return the poller command for the runner's copy of edit.txt; SSH_HOST None runs it here."""
    host, state = target
    path = f"{state}/workspaces/{workspace}/data/edit.txt"
    if host is None:
        return [sys.executable, "-u", "-c", POLLER, path, str(timeout)]
    # The remote shell expands a leading ~; everything else is quoted.
    remote = '"$HOME"/' + shlex.quote(path[2:]) if path.startswith("~/") else shlex.quote(path)
    return ["ssh", "-T", host, f"python3 -u -c {shlex.quote(POLLER)} {remote} {timeout}"]


def observer(target, workspace, temp, env, timeout):
    """Start a poller that reports when the runner's copy of edit.txt holds a body."""
    lines = Lines(observer_command(target, workspace, timeout), temp, env, stdin=subprocess.PIPE)
    _, status = lines.next(60)
    if status != "ready":
        raise ValueError(f"cannot see the runner's workspace copy ({status}); check the --observe state directory")
    return lines


def applied(line):
    try:
        return json.loads(line).get("status") == "applied"
    except ValueError:
        return False


def burst_receipts(receipt, started, quiet):
    """Read a burst's receipts; return the last one's stamp and how many arrived.

    The watch pushes a burst back to back, so the next receipt can take as long
    as a push. The burst has converged at the first silence longer than `quiet`,
    which grows to twice the wait for the first receipt and twice the longest
    gap between receipts.
    """
    stamp, _ = receipt()
    count, quiet = 1, max(quiet, 2 * (stamp - started))
    while True:
        try:
            following, _ = receipt(quiet)
        except NoOutput:
            return stamp, count
        quiet = max(quiet, 2 * (following - stamp))
        stamp, count = following, count + 1
        if count > BURST_EDITS:
            raise ValueError("a save burst produced more receipts than saves")


def measure_saves(loop, peer, files, samples, seed, temp, record, target):
    repo = temp / f"saves-{peer}-{files}"
    write_fixture(repo, files, f"{seed}:saves")
    name = f"loop-{uuid.uuid4().hex[:12]}"
    row, stdout, _ = loop.run(repo, "workspaces", "create", "--on", peer, "--json", name)
    record("workspace-create", 0, row)
    workspace = json.loads(stdout)["id"]
    watch = poller = cleanup = burst = None
    code, late, log = 0, 0, temp / f"watch-{name}.log"
    try:
        if target:
            poller = observer(target, workspace, temp, loop.env, loop.timeout)
        daemon = cpu_seconds(loop.daemon.pid)[0] if loop.daemon else None
        started = time.monotonic()
        with log.open("w") as stderr:
            watch = Lines([loop.binary, "push", "--on", peer, "--workspace", name, "--apply", "--json", "--watch"],
                          repo, loop.env, stderr=stderr)

        def receipt(timeout=loop.timeout):
            stamp, line = watch.next(timeout)
            row = json.loads(line)
            if row.get("status") not in ("applied", "unchanged"):
                raise ValueError(f"watch failed: {line}")
            return stamp, row

        stamp, _ = receipt()
        result = dict(wall_ms=(stamp - started) * 1000, client_cpu_ms=cpu_seconds(watch.process.pid)[0] * 1000)
        if loop.daemon:
            result["daemon_cpu_ms"] = (cpu_seconds(loop.daemon.pid)[0] - daemon) * 1000
        record("watch-start", 0, result)
        slowest = 0
        for sample in range(samples):
            time.sleep(SAVE_PAUSE_SECONDS)
            body = f"{seed} save {sample}"
            if poller:
                poller.send(body)
            client = cpu_seconds(watch.process.pid)[0]
            daemon = cpu_seconds(loop.daemon.pid)[0] if loop.daemon else None
            started = time.monotonic()
            (repo / "edit.txt").write_text(body + "\n")
            stamp, row = receipt()
            if row["status"] != "applied":
                raise ValueError("a save produced no transfer")
            result = dict(wall_ms=(stamp - started) * 1000)
            slowest = max(slowest, stamp - started)
            if poller:
                seen, status = poller.next(loop.timeout)
                if status != "seen":
                    raise ValueError("the runner's copy never showed the save")
                result["visible_ms"] = (seen - started) * 1000
            result["client_cpu_ms"] = (cpu_seconds(watch.process.pid)[0] - client) * 1000
            if loop.daemon:
                result["daemon_cpu_ms"] = (cpu_seconds(loop.daemon.pid)[0] - daemon) * 1000
            record("save", sample, result)

        time.sleep(SAVE_PAUSE_SECONDS)
        last = f"{seed} burst {BURST_EDITS - 1}"
        if poller:
            poller.send(last)
        client = cpu_seconds(watch.process.pid)[0]
        daemon = cpu_seconds(loop.daemon.pid)[0] if loop.daemon else None
        started = time.monotonic()
        for edit in range(BURST_EDITS):
            (repo / "edit.txt").write_text(f"{seed} burst {edit}\n")
            time.sleep(.005)
        result = {}
        if poller:
            seen, status = poller.next(loop.timeout)
            if status != "seen":
                raise ValueError("the runner's copy never showed the last save of the burst")
            result["visible_ms"] = (seen - started) * 1000
        # The watch coalesces the burst into back-to-back pushes; wait out at
        # least twice the slowest single save before calling it finished.
        stamp, receipts = burst_receipts(receipt, started, max(1, 2 * slowest))
        result.update(wall_ms=(stamp - started) * 1000, receipts=receipts,
                      client_cpu_ms=(cpu_seconds(watch.process.pid)[0] - client) * 1000)
        if loop.daemon:
            result["daemon_cpu_ms"] = (cpu_seconds(loop.daemon.pid)[0] - daemon) * 1000
        burst = result
    finally:
        if watch:
            code = watch.stop(loop.timeout)  # Ctrl-C finishes the in-flight push and exits
            # A push that lands after the burst was measured means it had not converged.
            late = sum(applied(line) for line in watch.remaining())
        if poller:
            poller.process.stdin.close()
            try:
                poller.process.wait(timeout=60)
            except subprocess.TimeoutExpired:
                poller.process.kill()
                poller.process.wait()
        try:
            loop.run(repo, "workspaces", "rm", "--on", peer, name)
        except ValueError as error:
            cleanup = error  # reported below unless an earlier failure is already propagating
    if code is None:
        raise ValueError(f"watch did not stop within {loop.timeout} s and was killed: {log.read_text()[-2000:]}")
    if code:
        raise ValueError(f"watch exited {code}: {log.read_text()[-2000:]}")
    if cleanup:
        raise cleanup
    if late:
        raise ValueError(f"{late} push(es) finished after the burst was measured; it had not converged")
    record("burst", 0, burst)


def measure(loop, peer, files, samples, save_samples, nonce, temp, record, target):
    repo = temp / f"repo-{peer}-{files}"
    seed = f"{nonce}:{peer}:{files}"
    measure_saves(loop, peer, files, save_samples, seed, temp, record, target)
    write_fixture(repo, files, seed)
    on = ["--on", peer, "--no-apply"]  # a personal apply_on_success must not reach jobs that do not apply

    def expect_out(body):
        if (repo / "out.txt").read_text().strip() != body:
            raise ValueError("out.txt did not come back with the job's contents")
        (repo / "out.txt").unlink()

    row, job = loop.job(repo, *on, "--", "true", scenario="cold")
    loop.verify(repo, job)
    record("job-cold", 0, row)
    for sample in range(samples):
        row, job = loop.job(repo, *on, "--", "true", scenario="cached")
        loop.verify(repo, job)
        record("job-warm", sample, row)

        (repo / "edit.txt").write_text(f"{seed} edit {sample}\n")
        row, job = loop.job(repo, *on, "--", "true", scenario="edit")
        if row["shipped_files"] != 1:
            raise ValueError("a one-file edit did not ship exactly one file")
        loop.verify(repo, job)
        record("job-edit", sample, row)

        row, job = loop.job(repo, *on, "--", "echo", "ready", scenario="cached", marker="ready")
        loop.verify(repo, job)
        record("first-output", sample, row)

        row, job = loop.job(repo, "--on", peer, "--apply", "--", "sh", "-c", f"echo a{sample} > out.txt",
                            scenario="cached")
        loop.verify(repo, job)
        expect_out(f"a{sample}")
        record("apply", sample, row)

        row, job = loop.job(repo, *on, "-d", "--", "true", scenario="cached")
        record("detach-submit", sample, row)
        row, _, _ = loop.run(repo, "attach", job)
        loop.verify(repo, job)
        record("detach-attach", sample, row)

        _, job = loop.job(repo, *on, "-d", "--", "sh", "-c", f"echo f{sample} > out.txt", scenario="cached")
        loop.run(repo, "attach", job)  # fetch is timed once the job has finished
        loop.verify(repo, job)
        row, _, _ = loop.run(repo, "fetch", "--apply", job)
        expect_out(f"f{sample}")
        record("fetch-apply", sample, row)


def summarize(samples):
    groups = {}
    for s in samples:
        groups.setdefault((s["peer"], s["files"], s["case"]), []).append(s)
    out = []
    for (peer, files, case), rows in groups.items():
        entry = dict(peer=peer, files=files, case=case, samples=len(rows))
        for key in ("wall", "visible"):
            if all(f"{key}_ms" in r for r in rows):
                values = sorted(r[f"{key}_ms"] for r in rows)
                entry.update({f"{key}_median_ms": round(statistics.median(values), 1),
                              f"{key}_p95_ms": round(values[min(len(values) - 1, round(0.95 * (len(values) - 1)))], 1),
                              f"{key}_min_ms": round(values[0], 1), f"{key}_max_ms": round(values[-1], 1)})
        for key in ("first_output", "client_cpu", "daemon_cpu"):
            if all(f"{key}_ms" in r for r in rows):
                entry[f"{key}_median_ms"] = round(statistics.median(r[f"{key}_ms"] for r in rows), 1)
        out.append(entry)
    return out


def start_daemon(binary, env, storage, sockets, log):
    config = storage / "config" / "errand" / "errandd.toml"
    config.parent.mkdir(parents=True)
    config.write_text('transport = "local"\nlisten = "none"\n'
                      f'state_dir = {json.dumps(str(storage / "daemon"))}\n'
                      f'socket = {json.dumps(str(sockets / "daemon.sock"))}\n')
    daemon = subprocess.Popen([binary, "serve", "--config", str(config)], env=env,
                              stdout=log, stderr=log, cwd=storage)
    deadline = time.monotonic() + 30
    while not (sockets / "daemon.sock").exists():
        if daemon.poll() is not None or time.monotonic() >= deadline:
            daemon.kill()
            raise ValueError("daemon failed to start; see daemon.log")
        time.sleep(.05)
    return daemon


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", default="errand")
    parser.add_argument("--on", action="append", default=[], help="configured peer; repeat for another peer")
    parser.add_argument("--isolated", action="store_true",
                        help="measure a private local daemon started from --binary instead of configured peers")
    parser.add_argument("--files", type=positive, action="append", help="checkout size; repeat for several (default 1000)")
    parser.add_argument("--samples", type=positive, default=5, help="samples per job case")
    parser.add_argument("--save-samples", type=positive, default=30, help="one-file saves per size")
    parser.add_argument("--observe", action="append", default=[], metavar="PEER=SSH_HOST[:STATE_DIR]",
                        help="watch PEER's workspace copy over SSH to time when saves become visible")
    parser.add_argument("--timeout", type=positive, default=600, help="client timeout per command, seconds")
    parser.add_argument("--output", type=Path, required=True, help="new directory for report.json")
    args = parser.parse_args()
    if args.isolated == bool(args.on):
        parser.error("choose --on PEER or --isolated")
    if len(set(args.on)) != len(args.on):
        parser.error("duplicate peer selection")
    targets = {}
    for value in args.observe:
        peer, _, where = value.partition("=")
        host, _, state = where.partition(":")
        if peer not in args.on or not host:
            parser.error(f"--observe {value}: expected PEER=SSH_HOST[:STATE_DIR] for a peer given with --on")
        targets[peer] = (host, state or "~/.errand")
    binary = shutil.which(args.binary)
    if binary is None:
        parser.error(f"binary not found: {args.binary}")
    binary = str(Path(binary).resolve())
    sizes = args.files or [1000]
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex
    report = {"schema_version": 1, "started_at": datetime.now(timezone.utc).isoformat(), "run_seed": nonce,
              "client_sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
              "caller": {"system": platform.system(), "arch": platform.machine(), "os_release": platform.release()},
              "isolated": args.isolated, "files": sizes, "requested_samples": args.samples,
              "requested_save_samples": args.save_samples, "observe": args.observe,
              "peers": {}, "samples": [], "complete": False}

    def save():
        report["summary"] = summarize(report["samples"])
        temporary = output / "report.json.tmp"
        temporary.write_text(json.dumps(report, indent=2) + "\n")
        temporary.replace(output / "report.json")

    daemon = None
    try:
        # A socket path must stay short, so the temporary tree lives in the system temp directory.
        with tempfile.TemporaryDirectory(prefix="errand-loop-") as directory:
            temp = Path(directory).resolve()
            # Isolate local receipt and apply state; configured peers keep the user's config.
            env = dict(os.environ, XDG_STATE_HOME=str(temp / "state"))
            peers = args.on
            if args.isolated:
                env["XDG_CONFIG_HOME"] = str(temp / "config")
                (temp / "sockets").mkdir()
                with (output / "daemon.log").open("w") as log:
                    daemon = start_daemon(binary, env, temp, temp / "sockets", log)
                report["daemon_filesystem"] = filesystem_facts(temp)
                peers = ["local"]
                targets = {"local": (None, str(temp / "daemon"))}
            report["client_version"] = subprocess.check_output([binary, "version"], env=env, text=True).strip()
            loop = Loop(binary, env, args.timeout, daemon)
            for peer in peers:
                before = checked_json(binary, ["peers", "--json", "--on", peer], temp, env)
                info = before[0]["info"]
                if any(info.get(key, 0) for key in ("running_jobs", "starting_jobs", "staging_jobs", "queued_jobs")):
                    raise ValueError(f"{peer} has active jobs; benchmark an idle runner")
                report["peers"][peer] = {"before": before}
                for files in sizes:
                    def record(case, sample, row):
                        row = dict(peer=peer, files=files, case=case, sample=sample, **row)
                        report["samples"].append(row)
                        save()
                        print(json.dumps(row), flush=True)
                    measure(loop, peer, files, args.samples, args.save_samples, nonce, temp, record, targets.get(peer))
            report["complete"] = True
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
        print(f"benchmark stopped: {error}", file=sys.stderr)
    finally:
        if daemon:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
        save()
    for entry in report["summary"]:
        extra = "".join(f", {label} {entry[key]:.0f} ms" for key, label in (
            ("visible_median_ms", "visible"), ("visible_p95_ms", "visible p95"), ("first_output_median_ms", "first output"))
            if key in entry)
        print(f"{entry['peer']} {entry['files']} {entry['case']}: median {entry['wall_median_ms']:.0f} ms"
              f" (p95 {entry['wall_p95_ms']:.0f}){extra}")
    print(f"Report: {output / 'report.json'}")
    return 0 if report["complete"] else 1


if __name__ == "__main__":
    sys.exit(main())
