"""Workload and receipt contracts for the bounded journal-depth comparison."""
import json
import os
from pathlib import Path
import shutil

from benchmark_observation_journal import JOURNAL_LIMIT, journal_layout, validate_sample
from benchmark_snapshot_integration import benchmark_order, run

VARIANTS = ('checkpoint', 'observation-replacement', 'observation-journal')
DEPTHS = (0, 8, 31)


def fixed_cases(number):
    # Historical six-round design: marginal coverage, not independent orders.
    # unchanged -> one edit -> batch is intentional so dirty paths only expand.
    return [(d, w) for d in benchmark_order(DEPTHS, (5 * number + 1) % 6)
            for w in ('unchanged', 'edit', 'batch')]


def check_receipt(sample, files, edits, variant, depth, replaced):
    expected = dict(CacheStatus='hit', CacheError='', Written=edits > 0, Hashed=edits,
                    Reused=files-edits, JournalRecordsLoaded=depth if variant == VARIANTS[2] else 0,
                    ReplacedBase=replaced)
    if any(sample['Result'].get(k) != v for k,v in expected.items()):
        raise RuntimeError(f"Expected {expected}, got {sample['Result']}")


def cycle_receipt(samples, variant):
    if len(samples) != 33:
        raise RuntimeError('A cycle must contain 32 appends and one compaction')
    for step,s in enumerate(samples):
        expected_depth = step if variant == VARIANTS[2] else 0
        expected_replace = variant == VARIANTS[1] or variant == VARIANTS[2] and step == 32
        if (s['Result']['JournalRecordsLoaded'] != expected_depth or
                s['Result']['ReplacedBase'] != expected_replace or not s['Result']['Written']):
            raise RuntimeError('Cycle history or compaction is incomplete')
    return dict(Total=sum(s['Total'] for s in samples), Wire=sum(s['Wire'] for s in samples),
                cache_bytes=samples[-1]['cache_bytes'], operations=33,
                Result=dict(Written=True, CheckpointBytes=sum(s['Result']['CheckpointBytes'] for s in samples),
                            Phases={p:sum(s['Result']['Phases'][p] for s in samples)
                                    for p in samples[0]['Result']['Phases']}))


class Fixture:
    def __init__(self, source, scratch, output, env, binary, count, size, git):
        self.source, self.out, self.env, self.binary = source, output, env, binary
        self.label = f'{count}-{size}-{"git" if git else "ignore"}'
        self.root = scratch/self.label
        self.root.mkdir()
        self.size, self.count, self.files = size, count, count+int(not git)
        self.paths=[]
        if not git:
            (self.root/'.errandignore').write_text('')
        for i in range(count):
            p=self.root/f'dir-{i//100:04d}'/f'file-{i:05d}'
            p.parent.mkdir(exist_ok=True)
            p.write_bytes((f'body-{i}'.encode()+b'x'*size)[:size])
            self.paths.append(p)
        if git:
            run(['git','init','-q'],self.root,env,output/f'init-{self.label}.txt')
            run(['git','add','.'],self.root,env,output/f'add-{self.label}.txt')
        self.caches={v:scratch/f'cache-{self.label}-{v}' for v in VARIANTS}
        self.templates={v:scratch/f'template-{self.label}-{v}' for v in VARIANTS}
        self.serial=0

    def cache(self, variant):
        return self.caches[variant]/('checkpoint' if variant==VARIANTS[0] else 'observations')

    def invoke(self, variant, tag, flags=()):
        command=[str(self.binary),'-root',str(self.root),'-cache',str(self.caches[variant]),
                 '-mode',variant,*flags]
        return json.loads(run(command,self.source,self.env,self.out/f'{self.label}-{tag}-{variant}.json',timeout=300))

    def mutate(self, edits):
        self.serial+=1
        for i in range(edits):
            self.paths[i].write_bytes((f'edit-{self.serial}-{i}'.encode()+b'z'*self.size)[:self.size])

    def drain(self, tag):
        run(['sync'],self.source,self.env,self.out/f'{self.label}-{tag}-sync.txt')

    def seed(self, tag):
        for v in VARIANTS:
            self.cache(v).unlink(missing_ok=True)
            result=self.invoke(v,tag+'-seed')['Result']
            if (result['CacheStatus']!='missing' or result['CacheError'] or
                    result['Hashed']!=self.files or not result['Written']):
                raise RuntimeError(f'Bad seed: {result}')

    def prepare(self, depth, tag, near_bytes=False):
        self.seed(tag)
        if near_bytes:
            if self.count<50000:
                raise RuntimeError('Near-byte-limit fixture needs 50K files')
            self.mutate(30000)
            first=self.invoke(VARIANTS[2],tag+'-fill-0')
            check_receipt(first,self.files,30000,VARIANTS[2],0,False)
            layout=journal_layout(self.cache(VARIANTS[2]))
            per_file=layout['journal_bytes']/30000
            edits=int((JOURNAL_LIMIT*0.92-layout['journal_bytes'])/per_file)
            self.mutate(edits)
            second=self.invoke(VARIANTS[2],tag+'-fill-1')
            check_receipt(second,self.files,edits,VARIANTS[2],1,False)
            layout=journal_layout(self.cache(VARIANTS[2]))
            if not JOURNAL_LIMIT*0.85<layout['journal_bytes']<JOURNAL_LIMIT*0.97 or layout['records']!=2:
                raise RuntimeError(f'Near-full fixture not admitted: {layout}')
        else:
            for step in range(depth):
                self.mutate(1)
                sample=self.invoke(VARIANTS[2],f'{tag}-fill-{step}')
                check_receipt(sample,self.files,1,VARIANTS[2],step,False)
            layout=journal_layout(self.cache(VARIANTS[2]))
            if layout['records']!=depth:
                raise RuntimeError(f'Wrong prepared history: {layout}')
        for v in VARIANTS[:2]:
            sample=self.invoke(v,tag+'-catchup')
            if sample['Result']['CacheStatus']!='hit' or sample['Result']['CacheError']:
                raise RuntimeError(f'Control catchup failed: {sample}')
        hashes=[self.invoke(v,tag+'-warm') for v in VARIANTS]
        for v,s in zip(VARIANTS,hashes):
            check_receipt(s,self.files,0,v,layout['records'],False)
        if len({s['Hash'] for s in hashes})!=1:
            raise RuntimeError('Prepared roots differ')
        for v in VARIANTS:
            shutil.copyfile(self.cache(v),self.templates[v])
        return layout

    def restore(self):
        for v in VARIANTS:
            shutil.copyfile(self.templates[v],self.cache(v))

    def profile_path(self, directory, scenario, number, variant, kind):
        return directory/f'{self.label}-{scenario}-{number}-{variant}.{kind}'

    def measure(self, scenario, number, edits, depth, replaced=False, profile=None):
        self.restore()
        before=journal_layout(self.cache(VARIANTS[2]))
        if before['records']!=depth:
            raise RuntimeError('History changed before measurement')
        self.mutate(edits)
        self.drain(f'{scenario}-{number}')
        samples=[]
        for position,v in enumerate(benchmark_order(VARIANTS,number),1):
            flags=[]
            if profile:
                directory,kind=profile
                if kind not in ('cpu','heap'):
                    raise ValueError('Choose exactly one profile kind')
                flags=[f'-{kind}-profile',str(self.profile_path(directory,scenario,number,v,kind))]
            sample=self.invoke(v,f'{scenario}-{number}',flags)
            check_receipt(sample,self.files,edits,v,depth,
                          edits>0 and (v==VARIANTS[1] or v==VARIANTS[2] and replaced))
            after=journal_layout(self.cache(v)) if v!=VARIANTS[0] else None
            if v==VARIANTS[2] and edits and not scenario.startswith('byte'):
                validate_sample(sample,self.files,edits,'edit',v,before,after)
            if v==VARIANTS[2]:
                expected=0 if replaced else depth+int(edits>0)
                if after['records']!=expected:
                    raise RuntimeError('Wrong published history')
                if edits:
                    written=after['cache_bytes'] if replaced else after['cache_bytes']-before['cache_bytes']
                    if sample['Result']['CheckpointBytes']!=written:
                        raise RuntimeError('Published bytes do not match growth')
            sample.update(fixture=self.label,scenario=scenario,round=number,position=position,
                          variant=v,edits=edits,setup=before,cache_bytes=self.cache(v).stat().st_size)
            if profile:
                sample['profile_kind']=kind
            samples.append(sample)
        if len({s['Hash'] for s in samples})!=1:
            raise RuntimeError('Timed roots differ')
        return samples

    def cycle(self, number):
        self.seed(f'cycle-{number}')
        samples={v:[] for v in VARIANTS}
        steps=[]
        for step in range(33):
            before=journal_layout(self.cache(VARIANTS[2]))
            if before['records']!=step:
                raise RuntimeError('Cycle lost history')
            self.mutate(1)
            self.drain(f'cycle-{number}-{step}')
            group=[]
            # Every step has all six orders across the six independent cycles.
            for position,v in enumerate(benchmark_order(VARIANTS,number+step),1):
                s=self.invoke(v,f'cycle-{number}-{step}')
                check_receipt(s,self.files,1,v,step,v==VARIANTS[1] or v==VARIANTS[2] and step==32)
                if v==VARIANTS[2]:
                    validate_sample(s,self.files,1,'edit',v,before,journal_layout(self.cache(v)))
                s.update(fixture=self.label,scenario='cycle',round=number,step=step,position=position,
                         variant=v,cache_bytes=self.cache(v).stat().st_size)
                samples[v].append(s)
                group.append(s)
            if len({s['Hash'] for s in group})!=1:
                raise RuntimeError('Cycle roots differ')
            steps.extend(group)
        return [dict(cycle_receipt(samples[v],v),fixture=self.label,scenario='cycle',round=number,variant=v)
                for v in VARIANTS],steps

    def interference(self, number, chunk=None, scenario='preceding-writer'):
        # Same preparation and source for every treatment. Drain prior writes
        # before each trial; vary only the preceding write and its drain policy.
        samples=[]
        noise=self.root.parent/f'noise-{self.label}'
        if chunk is None:
            chunk=b'x'*(4<<20)
        for position,treatment in enumerate(benchmark_order(('quiet','writer','writer-synced'),number),1):
            self.drain(f'interference-{number}-{treatment}-before')
            if treatment!='quiet':
                with noise.open('wb') as stream:
                    for _ in range(16): stream.write(chunk)
                    stream.flush()
                    if treatment=='writer-synced': os.fsync(stream.fileno())
                if treatment=='writer-synced': self.drain(f'interference-{number}-after')
            sample=self.invoke(VARIANTS[0],f'interference-{number}-{treatment}')
            check_receipt(sample,self.files,0,VARIANTS[0],0,False)
            sample.update(fixture=self.label,scenario=scenario,round=number,position=position,
                          variant=treatment,cache_bytes=self.cache(VARIANTS[0]).stat().st_size,
                          logical_write_bytes=0 if treatment=='quiet' else 16*len(chunk))
            samples.append(sample)
        if len({s['Hash'] for s in samples})!=1:
            raise RuntimeError('Interference roots differ')
        return samples
