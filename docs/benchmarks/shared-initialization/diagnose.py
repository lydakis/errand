from pathlib import Path
import json, os, sys
root=Path(__file__).resolve().parent
sys.path.insert(0,str(root/'candidate/scripts'))
from benchmark_snapshot_integration import run, benchmark_campaign, filesystem, source_digest
out=root/'diagnostics';out.mkdir()
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
report={'scope':'Diagnostic traces, not timing comparisons','versions':{}}
with benchmark_campaign(out,report) as scratch:
 env['TMPDIR']=str(scratch)
 report['filesystem']=filesystem(scratch,env,out,'filesystem')
 for variant in ['baseline','candidate']:
  cwd=root/variant; binary=scratch/f'{variant}.test'
  report['versions'][variant]=source_digest(cwd,production=True)
  run(['go','test','-c','-o',str(binary),'./cmd/errand'],cwd,env,out/f'{variant}-build.txt')
  for case,pattern in [('create','^BenchmarkWorkspaceCreationAndSubmission$/^workspace-create$'),('fetch','^BenchmarkFetchCompletion$/^persistent=false$')]:
   prefix=f'{variant}-{case}';trace=scratch/f'{prefix}.trace';cpu=scratch/f'{prefix}.cpu'
   run([str(binary),'-test.run=^$',f'-test.bench={pattern}','-test.benchtime=3x',f'-test.trace={trace}',f'-test.cpuprofile={cpu}'],cwd,env,out/f'{prefix}.txt',timeout=120)
   profile=scratch/f'{prefix}.syscall'
   run(['go','tool','trace','-pprof=syscall',str(trace)],cwd,env,profile,read_output=False)
   for kind,p in [('syscall',profile),('cpu',cpu)]:
    run(['go','tool','pprof','-top','-cum',str(binary),str(p)],cwd,env,out/f'{prefix}-{kind}.txt')
   print(prefix,flush=True)
