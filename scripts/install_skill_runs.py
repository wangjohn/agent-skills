#!/usr/bin/env python3
"""Install private skill-run hooks and an optional macOS R2 upload job."""
import argparse
import json
import os
from pathlib import Path
import plistlib
import shlex
import shutil
import subprocess
import sys

from skill_runs import Store, atomic_json

LABEL = 'com.agent-skills.skill-runs-upload'


def merge_hooks(existing, command):
    result = json.loads(json.dumps(existing))
    hooks = result.setdefault('hooks', {})
    for event in ['UserPromptSubmit', 'PostToolUse', 'Stop', 'Interrupt']:
        groups = hooks.setdefault(event, [])
        # Replace only handlers owned by this installer. Preserve all other hooks.
        for group in groups:
            group['hooks'] = [h for h in group.get('hooks', [])
                              if h.get('statusMessage') != 'Recording private skill-run evidence']
        groups[:] = [g for g in groups if g.get('hooks')]
        handler = {'type': 'command', 'command': command,
                   'statusMessage': 'Recording private skill-run evidence',
                   'timeout': 3 if event == 'Interrupt' else 10}
        if event == 'UserPromptSubmit':
            handler['additionalContextLimit'] = 600
        group = {'hooks': [handler]}
        if event == 'PostToolUse':
            group['matcher'] = '.*'
        groups.append(group)
    return result


def install(home, codex_home, remote=None, rclone=None, launch=False):
    store = Store(home)
    if launch:
        if sys.platform != 'darwin':
            raise ValueError('The LaunchAgent installer requires macOS.')
        if not (remote or store.config.get('remote')):
            raise ValueError('Configure --remote before enabling uploads.')
        if not (rclone or store.config.get('rclone') or shutil.which('rclone')):
            raise ValueError('Install rclone before enabling uploads.')
    runtime = store.home / 'bin/skill_runs.py'
    runtime.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    shutil.copyfile(Path(__file__).with_name('skill_runs.py'), runtime)
    os.chmod(runtime, 0o700)
    if remote:
        store.config['remote'] = remote
    if rclone:
        store.config['rclone'] = str(Path(rclone).resolve())
    atomic_json(store.home / 'config.json', store.config)
    codex_home = Path(codex_home).expanduser()
    codex_home.mkdir(parents=True, exist_ok=True)
    path = codex_home / 'hooks.json'
    existing = json.loads(path.read_text()) if path.exists() else {}
    command = shlex.join([sys.executable, str(runtime), '--home', str(store.home), 'hook'])
    if path.exists():
        backup = path.with_name('hooks.json.skill-runs-backup')
        if not backup.exists():
            shutil.copyfile(path, backup)
    atomic_json(path, merge_hooks(existing, command))
    if launch:
        if sys.platform != 'darwin':
            raise ValueError('The LaunchAgent installer requires macOS.')
        if not store.config.get('remote'):
            raise ValueError('Configure --remote before enabling uploads.')
        binary = store.config.get('rclone') or shutil.which('rclone')
        if not binary:
            raise ValueError('Install rclone before enabling uploads.')
        store.config['rclone'] = str(Path(binary).resolve())
        atomic_json(store.home / 'config.json', store.config)
        path = Path.home() / 'Library/LaunchAgents' / (LABEL + '.plist')
        path.parent.mkdir(parents=True, exist_ok=True)
        job = {'Label': LABEL, 'ProgramArguments': [sys.executable, str(runtime), '--home', str(store.home), 'upload'],
               'RunAtLoad': True, 'StartInterval': 180, 'ProcessType': 'Background',
               'StandardOutPath': str(store.home / 'upload.stdout.log'),
               'StandardErrorPath': str(store.home / 'upload.stderr.log')}
        path.write_bytes(plistlib.dumps(job))
        target = 'gui/%s' % os.getuid()
        subprocess.run(['launchctl', 'bootout', target + '/' + LABEL], capture_output=True)
        subprocess.run(['launchctl', 'bootstrap', target, str(path)], check=True)
    store.db.close()
    return runtime


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--home', default=str(Path.home() / '.local/share/agent-skill-runs'))
    parser.add_argument('--codex-home', default=os.environ.get('CODEX_HOME', str(Path.home() / '.codex')))
    parser.add_argument('--remote', help='rclone remote including bucket and prefix: r2:agent-skill-runs/v1')
    parser.add_argument('--rclone', help='Absolute rclone executable path')
    parser.add_argument('--launch', action='store_true', help='Enable uploads every three minutes and at login')
    args = parser.parse_args()
    runtime = install(args.home, args.codex_home, args.remote, args.rclone, args.launch)
    print('Installed recorder:', runtime)
    print('Review and trust the new hooks in Codex (/hooks in the CLI), then start a new task.')
    print('Uploads:', 'scheduled' if args.launch else 'not scheduled')


if __name__ == '__main__':
    main()
