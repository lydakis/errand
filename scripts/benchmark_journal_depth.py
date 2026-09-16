#!/usr/bin/env python3
"""Fixed journal histories, complete cycles and separate diagnostic profiles."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile

from benchmark_checkpoint_builder import candidate_digest
from benchmark_derived_index import summarize
from benchmark_snapshot_integration import benchmark_campaign, filesystem, run
from journal_depth_cases import Fixture, VARIANTS, fixed_cases
from snapshot_provenance import harness_inputs


def profile_cases(fixture, output, env, repetitions, report):
    profiles=output/'profiles'
    profiles.mkdir()
    for scenario,depth,edits,replace in (('profile-edit-d0',0,1,False),
                                        ('profile-edit-d31',31,1,False),
                                        ('byte-profile-append',2,1,False),
                                        ('byte-profile-compact',2,30000,True)):
        near=scenario.startswith('byte')
        if near and fixture.count<50000: continue
        fixture.prepare(depth,scenario,near_bytes=near)
        for kind in ('cpu','heap'):
            for number in range(repetitions):
                report['profile_samples'].extend(fixture.measure(
                    scenario+'-'+kind,number,edits,depth,replace,(profiles,kind)))
        for variant in VARIANTS:
            stem=f'{fixture.label}-{scenario}'
            for kind,flags in (('cpu',['-top','-cum','-nodecount=150']),
                               ('heap',['-top','-alloc_space','-nodecount=45'])):
                paths=[str(fixture.profile_path(profiles,scenario+'-'+kind,i,variant,kind))
                       for i in range(repetitions)]
                run(['go','tool','pprof',*flags,str(fixture.binary),*paths],
                    fixture.source,env,profiles/f'{stem}-{variant}-{kind}-top.txt')
        print(f'{fixture.label}: {scenario} profiles complete',flush=True)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--rounds',type=int,default=6)
    parser.add_argument('--profile-repetitions',type=int,default=3)
    parser.add_argument('--smoke',action='store_true')
    parser.add_argument('--profiles-only',action='store_true',
                        help='Collect separate CPU/heap diagnostics without repeating timing comparisons')
    args=parser.parse_args()
    if args.rounds<6 or args.rounds%6 or args.profile_repetitions<0:
        parser.error('rounds must be a positive multiple of six; profile repetitions cannot be negative')
    if args.profiles_only and not args.profile_repetitions:
        parser.error('profiles-only requires positive profile repetitions')
    source,out=Path.cwd(),args.output.resolve()
    out.mkdir(parents=True,exist_ok=False)
    env=dict(os.environ,GOMAXPROCS='2',CGO_ENABLED='0')
    for key in tuple(env):
        if key.startswith('GIT_'): del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull)
    report=dict(scope='Fresh-process complete preparation; no delivery claim; warm OS caches',
                source_sha256=candidate_digest(source,out),harness_inputs=harness_inputs(source/'scripts'),
                rounds=args.rounds,smoke=args.smoke,profile_repetitions=args.profile_repetitions,
                profiles_only=args.profiles_only,profile_layout='separate-cpu-and-heap',
                platform=platform.platform(),python=platform.python_version(),gomaxprocs=2,
                samples=[],cycle_steps=[],interference=[],profile_samples=[])
    # Errand snapshots intentionally omit .git. Pin the baseline separately from
    # the exact dirty-source archive rather than querying Git on the runner.
    report['baseline_commit']='9156978a9138e7dbb41eeb67138e33b04160d9ba'
    # Retain the exact Go/module/Python/workflow input set, including uncommitted
    # experiment additions. No later working-tree reconstruction is needed.
    archive=out/'inputs.tar.gz'
    paths=[*source.rglob('*.go'),source/'go.mod',source/'go.sum',*(source/'scripts').glob('*.py'),
           *(source/'.github/workflows').glob('*.yml')]
    with tarfile.open(archive,'w:gz') as tar:
        for path in sorted(p for p in paths if not p.is_relative_to(out)):
            tar.add(path,arcname=str(path.relative_to(source)),recursive=False)
    report['archive_sha256']=hashlib.sha256(archive.read_bytes()).hexdigest()
    with benchmark_campaign(out,report) as scratch:
        env['TMPDIR']=str(scratch)
        report['filesystem']=filesystem(scratch,env,out,'filesystem')
        expected='apfs' if platform.system()=='Darwin' else 'btrfs'
        if report['filesystem'].lower()!=expected: raise RuntimeError('Native APFS/Btrfs required')
        report['toolchain']=json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','GOFLAGS',
                                           'CGO_ENABLED','GOTOOLCHAIN','GOEXPERIMENT'],source,env,out/'toolchain.json'))
        report['mount']=run(['findmnt','-T',str(scratch),'-n','-o','FSTYPE,OPTIONS']
                            if platform.system()=='Linux' else ['mount'],source,env,out/'mount.txt')
        binary=scratch/'probe'
        run(['go','build','-o',str(binary),'./experiments/snapshotcheckpoint/cmd'],source,env,out/'build.txt')
        report['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
        run(['python3','-m','unittest','discover','-s','scripts'],source,env,out/'python-tests.txt')
        run(['go','test','-p=1','./experiments/snapshotcheckpoint/...'],source,env,out/'tests.txt')
        run(['go','test','-race','-p=1','./experiments/snapshotcheckpoint/...'],source,
            dict(env,CGO_ENABLED='1'),out/'race.txt')
        run(['go','vet','./experiments/snapshotcheckpoint/...'],source,env,out/'vet.txt')
        shapes=[(50000,128,False),(10000,4096,True)] if not args.smoke else [(100,32,False)]
        if args.profiles_only:
            shapes=shapes[:1]
        fixtures=[]
        for count,size,git in shapes:
            f=Fixture(source,scratch,out,env,binary,count,size,git)
            fixtures.append(f)
            if args.profiles_only:
                continue
            for number in range(args.rounds):
                previous=None
                for depth,workload in fixed_cases(number):
                    report['active']=dict(fixture=f.label,round=number,depth=depth,workload=workload)
                    if depth!=previous:
                        f.prepare(depth,f'fixed-{number}-d{depth}')
                        previous=depth
                    edits={'unchanged':0,'edit':1,'batch':min(1000,count)}[workload]
                    report['samples'].extend(f.measure(f'fixed-d{depth}-{workload}',number,edits,depth))
                print(f'{f.label}: fixed histories round {number+1}/{args.rounds}',flush=True)
                (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
            if not git:
                for number in range(args.rounds):
                    report['active']=dict(fixture=f.label,scenario='cycle',round=number)
                    aggregates,steps=f.cycle(number)
                    report['samples'].extend(aggregates)
                    report['cycle_steps'].extend(steps)
                    if count>=50000:
                        f.prepare(2,f'near-{number}',near_bytes=True)
                        report['samples'].extend(f.measure('byte-near-full-append',number,1,2))
                        report['samples'].extend(f.measure('byte-compaction',number,30000,2,True))
                    print(f'{f.label}: lifecycle round {number+1}/{args.rounds}',flush=True)
                    (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
            # Every source is now unchanged relative to the replacement control.
            for number in range(args.rounds):
                report['active']=dict(fixture=f.label,scenario='interference',round=number)
                report['interference'].extend(f.interference(number))
            print(f'{f.label}: interference screen complete',flush=True)
        report['summary']=summarize(report['samples'],VARIANTS)
        report['framed_summary']=summarize(report['samples'],VARIANTS,VARIANTS[1])
        report['interference_summary']=summarize(report['interference'],('quiet','writer','writer-synced'),'quiet')
        report['timing_complete']=not args.profiles_only
        if args.profile_repetitions:
            # Profiles run only after all uninstrumented timing comparisons.
            report['active']=dict(scenario='profiles')
            profile_cases(fixtures[0],out,env,args.profile_repetitions,report)
        report.pop('active',None)
        report['final_source_sha256']=candidate_digest(source,out)
        report['final_harness_inputs']=harness_inputs(source/'scripts')
        if report['source_sha256']!=report['final_source_sha256'] or report['harness_inputs']!=report['final_harness_inputs']:
            raise RuntimeError('Campaign inputs changed')


if __name__=='__main__':
    main()
