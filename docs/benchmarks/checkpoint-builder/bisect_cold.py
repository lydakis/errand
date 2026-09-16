#!/usr/bin/env python3
"""Cold-path bisection with per-process allocation/GC evidence and frozen overlays."""
import argparse,hashlib,json,os,platform,sys,tarfile
from pathlib import Path
sys.path.insert(0,str(Path.cwd()/'scripts'))
from benchmark_checkpoint_builder import candidate_digest, variant_order, BASELINE, ARCHIVE, validate_sample
from benchmark_snapshot_integration import benchmark_campaign,run,source_digest,filesystem
from snapshot_provenance import harness_inputs
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--output',type=Path,required=True);a=p.parse_args()
root=Path.cwd();out=a.output.resolve();out.mkdir(parents=True,exist_ok=False)
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
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
 old_body=old[begin:end]
 start=current.index('func buildBoundedContext(');finish=current.index('\nfunc expandPaths(',start)
 legacy=current[:start]+old_body+'\n'+current[finish:]
 begin=current.index('func buildSelectedContext(');end=current.index('\n// Pack writes',begin)
 body=current[begin:end]
 ordinary=body.replace('func buildSelectedContext(', 'func buildOrdinarySelectedContext(').replace(', observed *ObservedBuild','')
 ordinary=ordinary.replace('''\t\tentry, reused, err := observed.lookup(rel, fi)
\t\tif err != nil {
\t\t\treturn m, err
\t\t}
''','').replace('''\t\t\tvar sum string
\t\t\tif reused {
\t\t\t\tsum = entry.SHA256
\t\t\t} else {
\t\t\t\tsum, err = builder.hash(ctx, abs, fi)
\t\t\t}''','\t\t\tsum, err := builder.hash(ctx, abs, fi)').replace('''\t\tif err := observed.record(e, fi, reused); err != nil {
\t\t\treturn m, err
\t\t}
''','')
 assert 'observed.' not in ordinary and 'reused' not in ordinary
 no_hooks=current.replace('return buildSelectedContext(ctx, root, expandPaths(paths), maxBytes, maxEntries, builder, nil)','return buildOrdinarySelectedContext(ctx, root, expandPaths(paths), maxBytes, maxEntries, builder)')+'\n'+ordinary
 a1=body.index('\tif len(paths) != 0 {');a2=body.index('\tvar bytes int64',a1)
 old_alloc=body[:a1]+body[a2:]
 old_alloc=old_alloc.replace('// expandPaths already sorted and deduplicated the traversal.', 'sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })')
 no_alloc=current[:begin]+old_alloc+current[end:]
 def instrument(text):
  text=text.replace('"os/signal"','"os/signal"\n"runtime"')
  text=text.replace('start := time.Now()','var before, after runtime.MemStats\n runtime.ReadMemStats(&before)\n start := time.Now()',1)
  text=text.replace('result.State = nil','runtime.ReadMemStats(&after)\nresult.State = nil')
  text=text.replace('Wire, Total time.Duration','Wire, Total time.Duration\nAllocBytes, Mallocs, NumGC uint64')
  text=text.replace('time.Since(start)}','time.Since(start), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, uint64(after.NumGC-before.NumGC)}')
  return text
 variants={'frozen':(base,old),'candidate':(root,current),'old-loop':(root,legacy),'no-hooks':(root,no_hooks),'old-allocation':(root,no_alloc)}
 r['overlays']={};binaries={}
 for name,(source,body) in variants.items():
  go=out/f'{name}-snapshot.go';go.write_text(body)
  main=out/f'{name}-main.go';main.write_text(instrument((source/'experiments/snapshotcheckpoint/cmd/main.go').read_text()))
  overlay=out/f'{name}-overlay.json';overlay.write_text(json.dumps({'Replace':{str(source/'internal/snapshot/snapshot.go'):str(go),str(source/'experiments/snapshotcheckpoint/cmd/main.go'):str(main)}}))
  binary=scratch/f'{name}-probe';run(['go','build','-overlay',str(overlay),'-o',str(binary),'./experiments/snapshotcheckpoint/cmd'],source,env,out/f'build-{name}.txt');binaries[name]=binary
  r['overlays'][name]=dict(snapshot_sha256=hashlib.sha256(go.read_bytes()).hexdigest(),main_sha256=hashlib.sha256(main.read_bytes()).hexdigest(),binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest())
 fixture=scratch/'fixture';fixture.mkdir();(fixture/'.errandignore').write_text('')
 for i in range(1000):
  f=fixture/f'dir-{i//100:04d}'/f'file-{i:05d}';f.parent.mkdir(exist_ok=True);f.write_bytes((f'body-{i:05d}'.encode()+b'x'*65536)[:65536])
 for pair in range(20):
  run(['sync'],root,env,out/'sync.txt');hashes=[]
  for position,name in enumerate(variant_order(variants,pair),1):
   s=json.loads(run([str(binaries[name]),'-root',str(fixture),'-mode','cold'],root,env,out/f'{pair}-{name}.json'));validate_sample(s,1001,0,False,True);hashes.append(s['Hash']);s.update(pair=pair,position=position,variant=name);r['samples'].append(s)
  assert len(set(hashes))==1
 r['final_source_sha256']=candidate_digest(root,out);r['final_harness_inputs']=harness_inputs(root/'scripts')
 assert r['final_source_sha256']==r['source_sha256'] and r['final_harness_inputs']==r['harness_inputs']
