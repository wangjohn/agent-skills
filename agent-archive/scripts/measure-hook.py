#!/usr/bin/env python3
"""Measure isolated synthetic hook latency; does not install hooks or contact storage.

Usage: python3 scripts/measure-hook.py /absolute/path/to/agent-archive [--event stop|subagentstop|all]
This measures subprocess startup plus local hook work, not whole-task overhead.
`stop` drives a Codex Stop hook; `subagentstop` drives a Claude SubagentStop
hook, which is the path that stages a child capture candidate. `all` (the
default) reports both.
"""
import datetime, json, math, os, pathlib, platform, statistics, subprocess, sys, tempfile, time
binary = str(pathlib.Path(sys.argv[1]).resolve())
argv = sys.argv[2:]
event_arg = 'all'
if argv:
    if argv[0] == '--event' and len(argv) > 1:
        event_arg = argv[1]
    elif argv[0].startswith('--event='):
        event_arg = argv[0].split('=', 1)[1]
    else:
        raise SystemExit('usage: measure-hook.py BINARY [--event stop|subagentstop|all]')
if event_arg not in ('stop', 'subagentstop', 'all'):
    raise SystemExit('unknown --event %r; expected stop, subagentstop, or all' % event_arg)
scenarios = ['stop', 'subagentstop'] if event_arg == 'all' else [event_arg]


def measure(scenario):
    """One scenario in its own archive home, so state from another cannot leak in."""
    harness = 'codex' if scenario == 'stop' else 'claude'
    with tempfile.TemporaryDirectory(prefix='archive-hook-benchmark-') as tmp:
        root = pathlib.Path(tmp).resolve()
        home, project = root / 'archive', root / 'project'
        home.mkdir(mode=0o700); project.mkdir()
        activated = (datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=1)).isoformat().replace('+00:00','Z')
        cfg = {'schema_version':1,'machine_id':'synthetic-benchmark','harnesses':[harness],'archive':{'enabled':True,'projects':[{'project_id':'synthetic','root':str(project),'included':True,'activated_at':activated}]}}
        (home / 'config.json').write_text(json.dumps(cfg))
        env = dict(os.environ, AGENT_ARCHIVE_HOME=str(home))
        def run(payload):
            start = time.perf_counter()
            result = subprocess.run([binary,'_hook','--harness',harness], input=json.dumps(payload),capture_output=True,text=True,env=env,timeout=3)
            elapsed = (time.perf_counter()-start)*1000
            if result.returncode or result.stderr: raise RuntimeError(result.stderr or str(result.returncode))
            return elapsed
        run({'hook_event_name':'SessionStart','source':'startup','session_id':'synthetic','cwd':str(project)})
        if not list((home / 'registrations').glob('*.json')):
            raise RuntimeError('synthetic session did not register; refusing a no-op benchmark')
        if scenario == 'stop':
            event = {'hook_event_name':'Stop','session_id':'synthetic'}
            def check():
                pass
        else:
            # A real SubagentStop names the child's own transcript. Point at a
            # small synthetic file so the hook does the work it does live:
            # stage a candidate and record the parent's link. The hook never
            # reads the transcript; the background collector does.
            child = root / 'child.jsonl'
            stamp = datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00','Z')
            child.write_text(''.join(json.dumps({'type':role,'timestamp':stamp,'sessionId':'synthetic','agentId':'synthetic-agent','message':{'role':role,'content':'benchmark'}})+'\n' for role in ('user','assistant')))
            event = {'hook_event_name':'SubagentStop','session_id':'synthetic','agent_id':'synthetic-agent','agent_transcript_path':str(child)}
            def check():
                if not list((home / 'subagent-candidates').glob('*.json')):
                    raise RuntimeError('SubagentStop staged no candidate; refusing a no-op benchmark')
        for _ in range(5): run(event)
        check()
        results={}
        for mode in ['enabled','paused']:
            cfg['paused']=mode=='paused';(home / 'config.json').write_text(json.dumps(cfg))
            samples=[run(event) for _ in range(100)]
            results[mode]={'samples':len(samples),'median_ms':round(statistics.median(samples),2),'p95_ms':round(sorted(samples)[math.ceil(.95*len(samples))-1],2),'max_ms':round(max(samples),2)}
        return results


descriptions = {
    'stop': 'synthetic Codex stop hook subprocess; no collector/cloud/app integration',
    'subagentstop': 'synthetic Claude SubagentStop hook subprocess staging a child candidate; no collector/cloud/app integration',
}
report = {'platform':platform.platform(),'scenarios':{name:{'scenario':descriptions[name],'results':measure(name)} for name in scenarios}}
print(json.dumps(report,indent=2))
