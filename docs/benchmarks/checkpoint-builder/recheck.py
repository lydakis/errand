#!/usr/bin/env python3
"""Focused A/A/B recheck of guard outliers; all samples and provenance retained."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import sys
import tarfile
sys.path.insert(0,str(Path.cwd()/'scripts'))
from benchmark_checkpoint_builder import candidate_digest, variant_order, validate_sample, BASELINE, ARCHIVE
from benchmark_snapshot_integration import benchmark_campaign, run, filesystem, source_digest
from snapshot_provenance import harness_inputs

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--output',type=Path,required=True)
a=p.parse_args()
root=Path.cwd();out=a.output.resolve();out.mkdir(parents=True,exist_ok=False)
env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
for k in tuple(env):
 if k.startswith('GIT_'): del env[k]
env.update(GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull)
r=dict(samples=[],source_sha256=candidate_digest(root,out),harness_inputs=harness_inputs(root/'scripts'),script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),platform=platform.platform(),developer_dir=env.get('DEVELOPER_DIR'),rounds=24)
with benchmark_campaign(out,r) as scratch:
 env['TMPDIR']=str(scratch)
 r['filesystem']=filesystem(scratch,env,out,'filesystem')
 r['toolchain']=json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','GOFLAGS','CGO_ENABLED','GOEXPERIMENT'],root,env,out/'toolchain.json'))
 base=scratch/'baseline';base.mkdir()
 archive=root/'docs/benchmarks/snapshot-checkpoint/review-final-inputs.tar.gz'
 assert hashlib.sha256(archive.read_bytes()).hexdigest()==ARCHIVE
 with tarfile.open(archive) as tar: tar.extractall(base,filter='data')
 assert source_digest(base)==BASELINE
 r['binaries']={}
 for label,source in [('baseline',base),('candidate',root)]:
  binary=scratch/f'{label}-probe'
  run(['go','build','-o',str(binary),'./experiments/snapshotcheckpoint/cmd'],source,env,out/f'build-{label}.txt')
  r['binaries'][label]=hashlib.sha256(binary.read_bytes()).hexdigest()
 for case,count,size,git,mode in [('cold-bodies',1000,65536,False,'cold'),('cold-git',10000,4096,True,'cold'),('checkpoint-batch',1000,32,False,'checkpoint')]:
  fixture=scratch/case;fixture.mkdir();paths=[]
  if not git: (fixture/'.errandignore').write_text('')
  for i in range(count):
   f=fixture/f'dir-{i//100:04d}'/f'file-{i:05d}';f.parent.mkdir(exist_ok=True);f.write_bytes((f'body-{i:05d}'.encode()+b'x'*size)[:size]);paths.append(f)
  if git:
   run(['git','init','-q'],fixture,env,out/f'{case}-git-init.txt');run(['git','add','.'],fixture,env,out/f'{case}-git-add.txt')
  def invoke(label,log):
   binary=scratch/('candidate-probe' if label=='candidate' else 'baseline-probe')
   return json.loads(run([str(binary),'-root',str(fixture),'-cache',str(scratch/f'cache-{case}-{label}'),'-mode',mode],root,env,log))
  if mode=='checkpoint':
   for label in ('baseline-a','baseline-b','candidate'): invoke(label,out/f'{case}-seed-{label}.json')
  for pair in range(24):
   if mode=='checkpoint':
    for i,f in enumerate(paths): f.write_bytes((f'edit-{pair}-{i}'.encode()+b'z'*size)[:size])
   run(['sync'],root,env,out/'sync.txt')
   hashes=[]
   for position,label in enumerate(variant_order(('baseline-a','baseline-b','candidate'),pair),1):
    s=invoke(label,out/f'{case}-{pair}-{label}.json');validate_sample(s,count+int(not git),count if mode=='checkpoint' else 0,False,mode=='cold');hashes.append(s['Hash'])
    s.update(case=case,pair=pair,position=position,variant=label);r['samples'].append(s)
   assert len(set(hashes))==1
  print(case+' complete',flush=True)
 r['final_source_sha256']=candidate_digest(root,out);r['final_harness_inputs']=harness_inputs(root/'scripts')
 assert r['source_sha256']==r['final_source_sha256'] and r['harness_inputs']==r['final_harness_inputs']
 assert r['script_sha256']==hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
