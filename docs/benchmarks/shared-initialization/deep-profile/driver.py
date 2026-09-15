import os, subprocess, tempfile
from pathlib import Path
root=Path('/private/tmp/errand-init-ajg1gkz5')
output=Path(tempfile.mkdtemp(prefix='errand-init-deep-profile-',dir='/private/tmp'))
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0',TMPDIR=str(output))
for variant in ['baseline','candidate']:
 binary=output/f'{variant}.test'
 subprocess.run(['go','test','-c','-o',str(binary),'./internal/changes'],cwd=root/variant,env=env,check=True,timeout=120)
 with (output/f'{variant}.txt').open('w') as log:
  subprocess.run([str(binary),'-test.run=^$','-test.bench=^BenchmarkCaptureWorkspaceBase$/^32-deep$','-test.benchtime=1x',f'-test.trace={output}/{variant}.trace',f'-test.cpuprofile={output}/{variant}.cpu'],cwd=root/variant,env=env,stdout=log,stderr=subprocess.STDOUT,check=True,timeout=60)
 with (output/f'{variant}.syscall').open('wb') as profile:
  subprocess.run(['go','tool','trace','-pprof=syscall',str(output/f'{variant}.trace')],env=env,stdout=profile,check=True,timeout=60)
 for kind in ['syscall','cpu']:
  with (output/f'{variant}-{kind}.txt').open('w') as text:
   subprocess.run(['go','tool','pprof','-top','-cum',str(binary),str(output/f'{variant}.{kind}')],env=env,stdout=text,stderr=subprocess.STDOUT,check=True,timeout=60)
print(output)
