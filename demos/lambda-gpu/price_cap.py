"""Temporarily cap the Mini at $1.50/h; restore its exact prior configuration."""
import subprocess
import sys
import time


def call(code):
    return subprocess.check_output(
        ["errand", "--on", "mini", "--no-snapshot", "--",
         "/opt/homebrew/bin/python3", "-c", code], text=True,
        stderr=subprocess.PIPE, timeout=30,
    ).strip()


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("prepare", "restore"):
        raise SystemExit("Usage: python3 price_cap.py prepare|restore")
    action = sys.argv[1]
    code = r'''
from pathlib import Path
import subprocess, shutil, tomllib
p=Path.home()/'.config/errand/errandd.toml'
backup=p.with_name('errandd.toml.before-gpu-demo')
raw=p.read_text()
cfg=tomllib.loads(raw)
line='max_price_per_hour = 1.50\n'
if ACTION == 'prepare':
    if backup.exists():
        assert raw.replace(line,'') == backup.read_text(), 'Runner config changed; inspect existing backup'
    else:
        assert 'max_price_per_hour' not in cfg['cloud']['lambda'], 'An explicit cap already exists; inspect it'
        shutil.copy2(p,backup)
    if line in raw:
        print('already prepared')
        raise SystemExit(0)
    p.write_text(raw.replace('[cloud.lambda]\n','[cloud.lambda]\n'+line))
else:
    assert backup.exists(), 'No demo backup exists'
    assert raw.replace(line,'') == backup.read_text(), 'Runner config changed; refusing to overwrite it'
    p.write_text(backup.read_text())
p.chmod(0o600)
status=Path('/tmp/errand-demo-cap.status')
status.unlink(missing_ok=True)
label='dev.lydakis.errand-demo-cap'
script='sleep 2; /opt/homebrew/bin/errand setup >/tmp/errand-demo-cap.log 2>&1; echo $? >/tmp/errand-demo-cap.status; launchctl remove '+label
subprocess.run(['launchctl','submit','-l',label,'--','/bin/sh','-c',script],check=True)
print('restarting')
'''.replace("ACTION", repr(action))
    response = call(code)
    if response != "already prepared":
        deadline = time.monotonic() + 60
        while True:
            try:
                status = call("from pathlib import Path; p=Path('/tmp/errand-demo-cap.status'); print(p.read_text().strip() if p.exists() else 'pending')")
                if status == "0":
                    break
                if status != "pending":
                    raise RuntimeError("Runner setup failed; inspect /tmp/errand-demo-cap.log on Mini")
            except subprocess.SubprocessError:
                pass  # A brief disconnect while the service restarts is expected.
            if time.monotonic() >= deadline:
                raise RuntimeError("Mini restart not confirmed; inspect its runner before renting")
            time.sleep(2)
    if action == "restore":
        call("from pathlib import Path; (Path.home()/'.config/errand/errandd.toml.before-gpu-demo').unlink(); Path('/tmp/errand-demo-cap.status').unlink(missing_ok=True); Path('/tmp/errand-demo-cap.log').unlink(missing_ok=True)")
    print("Mini ready: demo cap $1.50/h." if action == "prepare" else "Mini ready: original Lambda configuration restored.")


if __name__ == "__main__":
    main()
