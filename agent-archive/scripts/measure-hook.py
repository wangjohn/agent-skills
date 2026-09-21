#!/usr/bin/env python3
"""Measure isolated synthetic hook latency; does not install hooks or contact storage.

Usage: python3 scripts/measure-hook.py /absolute/path/to/agent-archive
This measures subprocess startup plus local hook work, not whole-task overhead.
"""
import datetime, json, math, os, pathlib, platform, statistics, subprocess, sys, tempfile, time
binary = str(pathlib.Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix='archive-hook-benchmark-') as tmp:
    root = pathlib.Path(tmp).resolve()
    home, project = root / 'archive', root / 'project'
    home.mkdir(mode=0o700); project.mkdir()
    activated = (datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=1)).isoformat().replace('+00:00','Z')
    cfg = {'schema_version':1,'machine_id':'synthetic-benchmark','harnesses':['codex'],'archive':{'enabled':True,'projects':[{'project_id':'synthetic','root':str(project),'included':True,'activated_at':activated}]}}
    (home / 'config.json').write_text(json.dumps(cfg))
    env = dict(os.environ, AGENT_ARCHIVE_HOME=str(home))
    def run(payload):
        start = time.perf_counter()
        result = subprocess.run([binary,'_hook','--harness','codex'], input=json.dumps(payload),capture_output=True,text=True,env=env,timeout=3)
        elapsed = (time.perf_counter()-start)*1000
        if result.returncode or result.stderr: raise RuntimeError(result.stderr or str(result.returncode))
        return elapsed
    run({'hook_event_name':'SessionStart','source':'startup','session_id':'synthetic','cwd':str(project)})
    if not list((home / 'registrations').glob('*.json')):
        raise RuntimeError('synthetic session did not register; refusing a no-op benchmark')
    event = {'hook_event_name':'Stop','session_id':'synthetic'}
    for _ in range(5): run(event)
    results={}
    for mode in ['enabled','paused']:
        cfg['paused']=mode=='paused';(home / 'config.json').write_text(json.dumps(cfg))
        samples=[run(event) for _ in range(100)]
        results[mode]={'samples':len(samples),'median_ms':round(statistics.median(samples),2),'p95_ms':round(sorted(samples)[math.ceil(.95*len(samples))-1],2),'max_ms':round(max(samples),2)}
    print(json.dumps({'platform':platform.platform(),'scenario':'synthetic stop hook subprocess; no collector/cloud/app integration','results':results},indent=2))
