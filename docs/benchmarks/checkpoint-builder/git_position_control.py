#!/usr/bin/env python3
"""Git cold-path comparison with interleaved identical-code negative controls."""
import argparse,hashlib,json,os,platform,sys,tarfile
from pathlib import Path
sys.path.insert(0,str(Path.cwd()/'scripts'))
from benchmark_checkpoint_builder import candidate_digest, variant_order, BASELINE, ARCHIVE, validate_sample
from benchmark_snapshot_integration import benchmark_campaign,run,source_digest,filesystem
from snapshot_provenance import harness_inputs
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--output',type=Path,required=True);a=p.parse_args()
root=Path.cwd();out=a.output.resolve();out.mkdir(parents=True,exist_ok=False)
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
for key in tuple(env):
 if key.startswith('GIT_'): del env[key]
env.update(GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull)
r=dict(samples=[],source_sha256=candidate_digest(root,out),harness_inputs=harness_inputs(root/'scripts'),script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),platform=platform.platform())
with benchmark_campaign(out,r) as scratch:
 env['TMPDIR']=str(scratch)
 r['filesystem']=filesystem(scratch,env,out,'filesystem')
 base=scratch/'baseline';base.mkdir()
 archive=root/'docs/benchmarks/snapshot-checkpoint/review-final-inputs.tar.gz'
 assert hashlib.sha256(archive.read_bytes()).hexdigest()==ARCHIVE
 with tarfile.open(archive) as tar:tar.extractall(base,filter='data')
 assert source_digest(base)==BASELINE
 current=(root/'internal/snapshot/snapshot.go').read_text();old=(base/'internal/snapshot/snapshot.go').read_text()
 begin=old.index('func buildBoundedContext(');end=old.index('\n// Pack writes',begin)
 old_body=old[begin:end].replace('func buildBoundedContext(', 'func BuildLegacyBenchmarkContext(').replace(', builder *Builder','')
 old_body=old_body.replace('\n\tselected :=', '\n var builder *Builder\n\tselected :=',1)
 mixed=current+'\n'+old_body
 checkpoint=(root/'experiments/snapshotcheckpoint/checkpoint.go').read_text()
 needle='m, err := snapshot.BuildBoundedContext(ctx, root, paths, -1, -1)'
 assert checkpoint.count(needle)==1
 checkpoint=checkpoint.replace(needle, 'build := snapshot.BuildBoundedContext\n if os.Getenv("ERRAND_BENCH_LEGACY") == "1" { build = snapshot.BuildLegacyBenchmarkContext }\n m, err := build(ctx, root, paths, -1, -1)')
 def instrument(text):
  text=text.replace('"os/signal"','"os/signal"\n"runtime"')
  text=text.replace('start := time.Now()','var before, after runtime.MemStats\n runtime.ReadMemStats(&before)\n start := time.Now()',1)
  text=text.replace('result.State = nil','runtime.ReadMemStats(&after)\nresult.State = nil')
  text=text.replace('Wire, Total time.Duration','Wire, Total time.Duration\nAllocBytes, Mallocs, NumGC uint64')
  text=text.replace('time.Since(start)}','time.Since(start), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, uint64(after.NumGC-before.NumGC)}')
  return text
 go=out/'mixed-snapshot.go';go.write_text(mixed)
 main=out/'mixed-main.go';main.write_text(instrument((root/'experiments/snapshotcheckpoint/cmd/main.go').read_text()))
 cp=out/'mixed-checkpoint.go';cp.write_text(checkpoint)
 overlay=out/'overlay.json';overlay.write_text(json.dumps({'Replace':{str(root/'internal/snapshot/snapshot.go'):str(go),str(root/'experiments/snapshotcheckpoint/cmd/main.go'):str(main),str(root/'experiments/snapshotcheckpoint/checkpoint.go'):str(cp)}}))
 binary=scratch/'mixed-probe';run(['go','build','-overlay',str(overlay),'-o',str(binary),'./experiments/snapshotcheckpoint/cmd'],root,env,out/'build.txt')
 r['overlays']={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in (go,main,cp)}
 r['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
 variants=('legacy-a','legacy-b','shared')
 fixture=scratch/'fixture';fixture.mkdir()
 for i in range(10000):
  f=fixture/f'dir-{i//100:04d}'/f'file-{i:05d}';f.parent.mkdir(exist_ok=True);f.write_bytes((f'body-{i:05d}'.encode()+b'x'*4096)[:4096])
 run(['git','init','-q'],fixture,env,out/'git-init.txt')
 run(['git','add','.'],fixture,env,out/'git-add.txt')
 r['fixture']='10000 x 4096, Git selected'
 r['scope']='Same executable, interleaved all-legacy negative control and mixed comparison'
 for pair in range(30):
  for control in (('all-legacy','mixed') if pair%2==0 else ('mixed','all-legacy')):
   run(['sync'],root,env,out/'sync.txt');hashes=[]
   for position,name in enumerate(variant_order(variants,pair),1):
    legacy='0' if control=='mixed' and name=='shared' else '1'
    s=json.loads(run([str(binary),'-root',str(fixture),'-mode','cold'],root,dict(env,ERRAND_BENCH_LEGACY=legacy),out/f'{control}-{pair}-{name}.json'))
    validate_sample(s,10000,0,False,True);hashes.append(s['Hash'])
    s.update(pair=pair,position=position,variant=name,control=control,legacy=legacy)
    r['samples'].append(s)
   assert len(set(hashes))==1
 r['final_source_sha256']=candidate_digest(root,out);r['final_harness_inputs']=harness_inputs(root/'scripts')
 assert r['final_source_sha256']==r['source_sha256'] and r['final_harness_inputs']==r['harness_inputs']
