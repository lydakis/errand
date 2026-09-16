#!/usr/bin/env python3
"""Supplement the journal screen with deterministic incompressible preceding writes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import sys
import tarfile

sys.path.insert(0,str(Path(__file__).resolve().parents[2]/'scripts'))
from benchmark_checkpoint_builder import candidate_digest
from benchmark_derived_index import summarize
from benchmark_snapshot_integration import benchmark_campaign, filesystem, run
from journal_depth_cases import Fixture
from snapshot_provenance import harness_inputs


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--smoke',action='store_true',help='Use 100-file fixtures to validate the treatment loop')
    args=parser.parse_args()
    root,out=Path.cwd(),args.output.resolve()
    out.mkdir(parents=True,exist_ok=False)
    env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
    for key in tuple(env):
        if key.startswith('GIT_'): del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull)
    chunk=random.Random(0).randbytes(4<<20)
    report=dict(scope='Diagnostic unchanged preparation after controlled preceding writes',
                source_sha256=candidate_digest(root,out),harness_inputs=harness_inputs(root/'scripts'),
                probe_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                chunk_sha256=hashlib.sha256(chunk).hexdigest(),logical_write_bytes=64<<20,
                rounds=6,smoke=args.smoke,samples=[],platform=platform.platform(),python=platform.python_version(),gomaxprocs=2)
    paths=[*root.rglob('*.go'),root/'go.mod',root/'go.sum',*(root/'scripts').glob('*.py'),Path(__file__).resolve()]
    with tarfile.open(out/'inputs.tar.gz','w:gz') as tar:
        for path in sorted(p for p in paths if not p.is_relative_to(out)):
            tar.add(path,arcname=str(path.relative_to(root)),recursive=False)
    report['archive_sha256']=hashlib.sha256((out/'inputs.tar.gz').read_bytes()).hexdigest()
    with benchmark_campaign(out,report) as scratch:
        env['TMPDIR']=str(scratch)
        report['filesystem']=filesystem(scratch,env,out,'filesystem')
        expected='apfs' if platform.system()=='Darwin' else 'btrfs'
        if report['filesystem'].lower()!=expected: raise RuntimeError('Native filesystem required')
        report['toolchain']=json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','GOFLAGS',
                                           'CGO_ENABLED','GOTOOLCHAIN','GOEXPERIMENT'],root,env,out/'toolchain.json'))
        report['mount']=run(['findmnt','-T',str(scratch),'-n','-o','FSTYPE,OPTIONS']
                            if platform.system()=='Linux' else ['mount'],root,env,out/'mount.txt')
        binary=scratch/'probe'
        run(['go','build','-o',str(binary),'./experiments/snapshotcheckpoint/cmd'],root,env,out/'build.txt')
        report['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
        shapes=((100,32,False),(100,32,True)) if args.smoke else ((50000,128,False),(10000,4096,True))
        for count,size,git in shapes:
            f=Fixture(root,scratch,out,env,binary,count,size,git)
            f.seed('interference')
            for number in range(6):
                report['active']=dict(fixture=f.label,round=number)
                report['samples'].extend(f.interference(
                    number,chunk,scenario='incompressible-preceding-writer'))
            print(f'{f.label}: incompressible screen complete',flush=True)
        report['summary']=summarize(report['samples'],('quiet','writer','writer-synced'),'quiet')
        report.pop('active',None)
        report['final_source_sha256']=candidate_digest(root,out)
        report['final_harness_inputs']=harness_inputs(root/'scripts')
        if (report['source_sha256']!=report['final_source_sha256'] or
                report['harness_inputs']!=report['final_harness_inputs'] or
                report['probe_sha256']!=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()):
            raise RuntimeError('Inputs changed')


if __name__=='__main__':
    main()
