#!/usr/bin/env python3
"""Old and new cold loops in one executable, with identical-binary A/A controls."""
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
 legacy_function=old_body.replace('func buildBoundedContext(', 'func BuildLegacyBenchmarkContext(').replace(', builder *Builder','').replace('\n\tselected :=','\n var builder *Builder\n\tselected :=',1)
 mixed=current+'\n'+legacy_function+'\n'+ordinary+'\n'+old_alloc.replace('func buildSelectedContext(', 'func buildOriginalAllocationSelectedContext(')+'\n'+(body[:a1]+body[a2:]).replace('func buildSelectedContext(', 'func buildGrowthSelectedContext(')
 mixed+='\nfunc BuildNoHooksBenchmarkContext(ctx context.Context, root string, paths []string, maxBytes int64, maxEntries int) (proto.Manifest, error) { return buildOrdinarySelectedContext(ctx, root, expandPaths(paths), maxBytes, maxEntries, nil) }\nfunc BuildOldAllocationBenchmarkContext(ctx context.Context, root string, paths []string, maxBytes int64, maxEntries int) (proto.Manifest, error) { return buildOriginalAllocationSelectedContext(ctx, root, expandPaths(paths), maxBytes, maxEntries, nil, nil) }\n'
 mixed+='\nfunc BuildGrowthBenchmarkContext(ctx context.Context, root string, paths []string, maxBytes int64, maxEntries int) (proto.Manifest, error) { return buildGrowthSelectedContext(ctx, root, expandPaths(paths), maxBytes, maxEntries, nil, nil) }\n'
 checkpoint=(root/'experiments/snapshotcheckpoint/checkpoint.go').read_text()
 needle='m, err := snapshot.BuildBoundedContext(ctx, root, paths, -1, -1)'
 assert checkpoint.count(needle)==1
 checkpoint=checkpoint.replace(needle, 'build := snapshot.BuildBoundedContext\n switch os.Getenv("ERRAND_BENCH_LOOP") { case "legacy-a", "legacy-b": build = snapshot.BuildLegacyBenchmarkContext; case "no-hooks": build = snapshot.BuildNoHooksBenchmarkContext; case "old-allocation": build = snapshot.BuildOldAllocationBenchmarkContext; case "growth": build = snapshot.BuildGrowthBenchmarkContext }\n m, err := build(ctx, root, paths, -1, -1)')
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
 variants=('legacy-a','legacy-b','shared','no-hooks','old-allocation','growth')
 fixture=scratch/'fixture';fixture.mkdir();(fixture/'.errandignore').write_text('')
 for i in range(1000):
  f=fixture/f'dir-{i//100:04d}'/f'file-{i:05d}';f.parent.mkdir(exist_ok=True);f.write_bytes((f'body-{i:05d}'.encode()+b'x'*65536)[:65536])
 for pair in range(30):
  run(['sync'],root,env,out/'sync.txt');hashes=[]
  for position,name in enumerate(variant_order(variants,pair),1):
   s=json.loads(run([str(binary),'-root',str(fixture),'-mode','cold'],root,dict(env,ERRAND_BENCH_LOOP=name),out/f'{pair}-{name}.json'));validate_sample(s,1001,0,False,True);hashes.append(s['Hash']);s.update(pair=pair,position=position,variant=name);r['samples'].append(s)
  assert len(set(hashes))==1
 r['final_source_sha256']=candidate_digest(root,out);r['final_harness_inputs']=harness_inputs(root/'scripts')
 assert r['final_source_sha256']==r['source_sha256'] and r['final_harness_inputs']==r['harness_inputs']
