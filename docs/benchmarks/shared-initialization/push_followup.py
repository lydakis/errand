from pathlib import Path
import json, os, sys, time
root=Path(__file__).resolve().parent
sys.path.insert(0,str(root/'candidate/scripts'))
from benchmark_snapshot_integration import run, benchmark_campaign, filesystem, source_digest, parse_sample, benchmark_order
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs
out=root/'push-followup';out.mkdir()
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
roots={v:root/v for v in ['baseline','candidate']}
expected=json.loads((root/'validated-source.json').read_text())
report={'validation_job':'cabal/01M2HEXAS9QKN1VRQ4QHQ8E510','scope':'Focused push follow-up after five-pair regression screen; same validated code, nine iterations per sample','versions':{},'rounds':5,'cases':{'push':[]},'orders':[]}
with benchmark_campaign(out,report) as scratch:
 env['TMPDIR']=str(scratch)
 report['fixture_filesystem']=filesystem(scratch,env,out,'filesystem')
 import platform
 report['platform']=platform.platform()
 report['comparison_inputs']={v:comparison_inputs(p) for v,p in roots.items()}
 report['allowed_input_differences']=require_matching_inputs(report['comparison_inputs'],['internal/changes/'+n for n in ['base_test.go','base_shared_test.go','staging_parallel_test.go','transfer_materialize_test.go','changes_test.go','materialize_paths_test.go']])
 report['harness_inputs']=harness_inputs(root/'candidate/scripts')
 report['toolchains']={}
 for v,p in roots.items():
  actual=source_digest(p)
  if actual!=expected[v]:raise RuntimeError(f'{v}: differs from validated source')
  report['versions'][v]={'source_sha256':actual,'samples':[]}
  report['toolchains'][v]=json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','CGO_ENABLED','GOFLAGS','GOTOOLCHAIN','GOEXPERIMENT'],p,env,out/f'{v}-toolchain.json'))
  run(['go','test','-c','-o',str(scratch/f'{v}.test'),'./cmd/errand'],p,env,out/f'{v}-build.txt')
 if report['toolchains']['baseline']!=report['toolchains']['candidate']:raise RuntimeError('toolchains differ')
 for n in range(5):
  order=benchmark_order(roots,n);report['orders'].append(order)
  for v in order:
   started=time.monotonic();load=os.getloadavg()
   raw=run([str(scratch/f'{v}.test'),'-test.v','-test.run=^$','-test.bench=^BenchmarkPushPhases$','-test.benchtime=9x','-test.benchmem'],roots[v],env,out/f'{n}-{v}.txt',timeout=300)
   s=parse_sample(raw,n);s.update(case='push',elapsed=time.monotonic()-started,load_before=load,load_after=os.getloadavg())
   report['versions'][v]['samples'].append(s)
   (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
  print(f'Push pair {n+1}/5',flush=True)
 for v,p in roots.items():
  if source_digest(p)!=expected[v] or comparison_inputs(p)!=report['comparison_inputs'][v]:raise RuntimeError('source changed')
 if harness_inputs(root/'candidate/scripts')!=report['harness_inputs']:raise RuntimeError('harness changed')
