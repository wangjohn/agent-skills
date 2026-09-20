#!/usr/bin/env python3
"""Private, opt-in skill-run capture. Python standard library; rclone for R2."""
import argparse
import contextlib
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import uuid

VERSION = 1
DEFAULT_HOME = Path.home() / '.local/share/agent-skill-runs'
MAX_TEXT = 32000
MAX_TOOLS = 100
SECRET_KEY = re.compile(r'^(authorization|cookie|set-cookie|password|passwd|secret|secret_access_key|access_token|refresh_token|api_key|api_token|token)$', re.I)


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def scrub(value):
    """Best-effort redaction; not a guarantee that proprietary data is removed."""
    if isinstance(value, dict):
        return {k: '[REDACTED]' if SECRET_KEY.fullmatch(k) else scrub(v) for k, v in value.items()}
    if isinstance(value, list):
        return [scrub(v) for v in value]
    if not isinstance(value, str):
        return value
    value = re.sub(r'-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----', '[REDACTED PRIVATE KEY]', value, flags=re.S)
    value = re.sub(r'(?i)(Bearer\s+)[^\s"\']+', r'\1[REDACTED]', value)
    value = re.sub(r'\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[A-Z0-9]{16})\b', '[REDACTED]', value)
    value = re.sub(r'''(?im)(\b(?:[\w-]{0,80}(?:api[_-]?key|api[_-]?token|password|secret|access[_-]?token|refresh[_-]?token)[\w-]{0,80})["']?\s*[:=]\s*)(["']?)([^\s,;"']+)''', r'\1[REDACTED]', value)
    return value


def text_evidence(value, limit=MAX_TEXT):
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    return {'text': scrub(text)[:limit], 'truncated': len(text) > limit,
            'original_characters': len(text)}


def marker_from_output(response):
    if isinstance(response, dict):
        if response.get('exit_code', 0) not in (0, None) or response.get('isError'):
            return None
        for key in ['output', 'text', 'content']:
            found = marker_from_output(response.get(key))
            if found:
                return found
    elif isinstance(response, list):
        for part in response:
            found = marker_from_output(part)
            if found:
                return found
    elif isinstance(response, str):
        for line in response.splitlines():
            if line.startswith('SKILL_RUN_APPLIED '):
                return json.loads(line[len('SKILL_RUN_APPLIED '):])
        try:
            decoded = json.loads(response)
        except ValueError:
            return None
        if isinstance(decoded, (dict, list)):
            return marker_from_output(decoded)
    return None


def atomic_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.tmp-')
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(value, f, indent=2)
            f.write('\n')
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def git_info(cwd):
    def run(*args):
        try:
            p = subprocess.run(['git', '-C', cwd, *args], capture_output=True, text=True, timeout=2)
            return p.stdout.strip() if p.returncode == 0 else None
        except (OSError, subprocess.TimeoutExpired):
            return None
    return {'head': run('rev-parse', 'HEAD'), 'branch': run('branch', '--show-current'),
            'status': scrub(run('status', '--porcelain') or ''),
            'note': 'Working-tree contents and PR input revisions are not automatically snapshotted.'}


class Store:
    def __init__(self, home):
        self.home = Path(home).expanduser().resolve()
        if any((p / '.git').exists() for p in [self.home, *self.home.parents]):
            raise ValueError('Run storage must be outside every Git working tree.')
        self.home.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(self.home, 0o700)
        self.db = sqlite3.connect(self.home / 'state.sqlite3', timeout=2)
        self.db.execute('CREATE TABLE IF NOT EXISTS turns (session TEXT, turn TEXT, body TEXT, PRIMARY KEY(session,turn))')
        self.db.execute('CREATE TABLE IF NOT EXISTS previous (session TEXT PRIMARY KEY, run TEXT)')
        self.db.execute('CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT)')
        self.db.execute('INSERT OR IGNORE INTO settings VALUES (?, ?)', ('machine', str(uuid.uuid4())))
        self.db.commit()
        self.machine = self.db.execute('SELECT value FROM settings WHERE key="machine"').fetchone()[0]
        cfg = self.home / 'config.json'
        self.config = json.loads(cfg.read_text()) if cfg.exists() else {}

    @contextlib.contextmanager
    def transaction(self):
        self.db.execute('BEGIN IMMEDIATE')
        try:
            yield
            self.db.commit()
        except Exception:
            self.db.rollback()
            raise

    def load(self, session, turn):
        row = self.db.execute('SELECT body FROM turns WHERE session=? AND turn=?', (session, turn)).fetchone()
        return json.loads(row[0]) if row else None

    def save(self, state):
        self.db.execute('INSERT OR REPLACE INTO turns VALUES (?,?,?)', (state['session_id'], state['turn_id'], json.dumps(state)))

    def emit(self, record, event_id=None):
        event_id = event_id or str(uuid.uuid4())
        record = dict(record, schema_version=VERSION, event_id=event_id, machine_id=self.machine)
        key = '%s/%s/%s.json.gz' % (self.machine, record['created_at'][:10], event_id)
        dest = self.home / 'outbox' / key
        # A completed Stop retry must not recreate an already uploaded object.
        if dest.exists() or (self.home / 'archive' / key).exists():
            return key
        dest.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd, temp = tempfile.mkstemp(dir=dest.parent, prefix='.tmp-')
        try:
            with os.fdopen(fd, 'wb') as f:
                f.write(gzip.compress(json.dumps(record, ensure_ascii=False).encode()))
                f.flush()
                os.fsync(f.fileno())
            os.replace(temp, dest)
        finally:
            if os.path.exists(temp):
                os.unlink(temp)
        return key

    def allowed(self, cwd):
        excluded = self.config.get('excluded_roots', [])
        return not any(Path(cwd).resolve() == Path(p).expanduser().resolve() or
                       Path(p).expanduser().resolve() in Path(cwd).resolve().parents for p in excluded)

    def start(self, event):
        session, turn = identity(event)
        if not self.allowed(event.get('cwd', '.')):
            return {}
        with self.transaction():
            state = self.load(session, turn)
            if state is None:
                state = {'session_id': session, 'turn_id': turn, 'run_id': str(uuid.uuid4()),
                         'created_at': now(), 'cwd': event.get('cwd'), 'model': event.get('model'),
                         'request': text_evidence(event.get('prompt', '')),
                         'skills': [], 'tools': [], 'dropped_tools': 0, 'closed': False}
                previous = self.db.execute('SELECT run FROM previous WHERE session=?', (session,)).fetchone()
                if previous:
                    # A subsequent user turn may be feedback, but is not labeled as such automatically.
                    self.emit({'kind': 'followup', 'created_at': now(), 'session_id': session,
                               'turn_id': turn, 'related_run_id': previous[0],
                               'classification': 'next_user_message_unclassified',
                               'message': state['request']}, event_id=str(uuid.uuid5(uuid.NAMESPACE_URL, session + ':' + turn + ':followup')))
                    self.db.execute('DELETE FROM previous WHERE session=?', (session,))
                self.save(state)
        command = shlex.join([sys.executable, str(Path(__file__).resolve()), 'signal', '--skill'])
        message = ('Private skill-run recording is configured by the user. When you APPLY a skill to this turn, '
                   'record it once by running ' + command + ' /absolute/path/to/SKILL.md . '
                   'Do not mark skills merely read for research, comparison, or editing. Mark every applied skill, '
                   'including a skill carried over from a previous turn. For non-filesystem skills, use '
                   '--skill-uri URI --name NAME instead of --skill PATH. The recorder is observational; '
                   'do not stop the task if recording fails. Do not read credentials or add secrets to records.')
        return {'hookSpecificOutput': {'hookEventName': 'UserPromptSubmit', 'additionalContext': message}}

    def mark(self, session, turn, path=None, uri=None, name=None):
        with self.transaction():
            state = self.load(session, turn)
            if not state or state['closed']:
                raise ValueError('No open captured turn; the UserPromptSubmit hook must run first.')
            if path:
                p = Path(path).expanduser().resolve()
                if p.name != 'SKILL.md' or not p.is_file():
                    raise ValueError('--skill must point to an existing SKILL.md.')
                raw = p.read_bytes()
                content = raw.decode('utf-8')
                match = re.search(r'^name:\s*[\'"]?([^\n\'"]+)', content, re.M)
                skill = {'name': match.group(1).strip() if match else p.parent.name,
                         'path': str(p), 'sha256': hashlib.sha256(raw).hexdigest(),
                         'snapshot': text_evidence(content, 128000), 'version': git_info(str(p.parent))['head']}
            else:
                if not uri or not name:
                    raise ValueError('A non-filesystem skill needs both --skill-uri and --name.')
                skill = {'name': name, 'uri': uri, 'sha256': None, 'snapshot': None,
                         'version': None, 'gap': 'Non-filesystem skill content was not captured.'}
            skill.update({'evidence': 'agent_reported_applied', 'marked_at': now()})
            if not any((s.get('path'), s.get('uri')) == (skill.get('path'), skill.get('uri')) for s in state['skills']):
                state['skills'].append(skill)
            if 'repository_at_start' not in state:
                state['repository_at_start'] = git_info(state['cwd']) if state.get('cwd') else None
            self.save(state)
        return state['run_id']

    def tool(self, event):
        session, turn = identity(event)
        # A read-only command emits the marker; the trusted hook performs private writes.
        # This works inside a workspace sandbox without giving the agent archive access.
        command = event.get('tool_input', {})
        command = command.get('command', command.get('cmd', '')) if isinstance(command, dict) else ''
        try:
            argv = shlex.split(command)
        except ValueError:
            argv = []
        if len(argv) >= 5 and argv[1:3] == [str(Path(__file__).resolve()), 'signal']:
            marker = marker_from_output(event.get('tool_response'))
            if marker:
                self.mark(session, turn, marker.get('skill'), marker.get('skill_uri'), marker.get('name'))
        with self.transaction():
            state = self.load(session, turn)
            if not state or state['closed'] or not state['skills']:
                return
            tool_id = event.get('tool_use_id')
            if tool_id and any(t['id'] == tool_id for t in state['tools']):
                return
            if len(state['tools']) >= MAX_TOOLS:
                state['dropped_tools'] += 1
            else:
                state['tools'].append({'id': tool_id, 'name': event.get('tool_name'), 'at': now(),
                                       'input': text_evidence(scrub(event.get('tool_input'))),
                                       'output': text_evidence(scrub(event.get('tool_response')))})
            self.save(state)

    def finish(self, event, interrupted=False):
        session, turn = identity(event)
        with self.transaction():
            state = self.load(session, turn)
            if not state or state['closed']:
                return
            if not state['skills']:
                self.db.execute('DELETE FROM turns WHERE session=? AND turn=?', (session, turn))
                return
            state['closed'] = True
            evidence = transcript_metadata(event.get('transcript_path'), turn)
            record = dict(state, kind='run', finished_at=now(),
                          outcome='interrupted' if interrupted else 'turn_finished',
                          response=text_evidence(event.get('last_assistant_message') or ''),
                          transcript=evidence,
                          repository_at_end=git_info(state['cwd']) if state.get('cwd') and not interrupted else None,
                          capture={'skill_use': 'agent_reported_not_independently_verified',
                                   'tools': 'supported PostToolUse events after first marker; bounded excerpts',
                                   'dropped_tools': state['dropped_tools'],
                                   'redaction': 'best_effort_v1_not_a_privacy_guarantee',
                                   'replay': 'partial: earlier context, source inputs, images and artifacts are not bundled',
                                   'supported_agents': 'Codex root turns; subagent use not automatically attributed'})
            self.emit(record, state['run_id'])
            # Keep only a tombstone; private content lives once in the immutable bundle.
            self.save({k: state[k] for k in ['session_id', 'turn_id', 'run_id', 'closed', 'created_at']})
            self.db.execute('INSERT OR REPLACE INTO previous VALUES (?,?)', (session, state['run_id']))

    def status(self):
        pending = list((self.home / 'outbox').glob('*/*/*.json.gz'))
        archive = list((self.home / 'archive').glob('*/*/*.json.gz'))
        states = [json.loads(r[0]) for r in self.db.execute('SELECT body FROM turns')]
        sync_file = self.home / 'upload-status.json'
        return {'home': str(self.home), 'machine_id': self.machine,
                'upload_configured': bool(self.config.get('remote')), 'pending_uploads': len(pending),
                'archived_uploads': len(archive),
                'open_skill_turns': sum(not s['closed'] and bool(s.get('skills')) for s in states),
                'upload_status': json.loads(sync_file.read_text()) if sync_file.exists() else None}


def identity(event):
    session, turn = event.get('session_id'), event.get('turn_id')
    if not session or not turn:
        raise ValueError('Hook event lacks session_id or turn_id; capture cannot be attributed.')
    return str(session), str(turn)


def transcript_metadata(path, turn):
    """Optional, allowlisted metadata only. Never copy system prompts or reasoning."""
    result = {'adapter': 'codex-jsonl-v1', 'model': None, 'effort': None,
              'usage': None, 'status': 'unavailable'}
    if not path or not Path(path).is_file():
        return result
    active = False
    try:
        # Bound work for Stop hooks. Never scan a huge session on every turn.
        with open(path, 'rb') as f:
            size = f.seek(0, 2)
            f.seek(max(0, size - 4 * 1024 * 1024))
            if f.tell():
                f.readline()
            for line in f:
                try:
                    item = json.loads(line)
                except (ValueError, UnicodeDecodeError):
                    continue
                p = item.get('payload', {})
                if not isinstance(p, dict):
                    continue
                if item.get('type') == 'turn_context':
                    active = p.get('turn_id') == turn
                    if active:
                        result.update(model=p.get('model'), effort=p.get('effort'), status='matched_turn')
                if item.get('type') == 'token_usage_record' and p.get('turn_id') == turn:
                    result['usage'] = p.get('turn_token_usage')
    except OSError:
        result['status'] = 'unreadable'
    return result


def upload(store):
    remote = store.config.get('remote')
    rclone = store.config.get('rclone') or shutil.which('rclone')
    if not remote or not re.fullmatch(r'[A-Za-z0-9_-]+:[A-Za-z0-9][A-Za-z0-9/_.-]*', remote):
        raise ValueError('Configure a bucket/prefix remote such as r2:agent-skill-runs/v1.')
    if not rclone:
        raise ValueError('rclone is not installed or configured.')
    # OS lock releases even if the process crashes. Never overlap scheduled uploads.
    import fcntl
    with (store.home / 'upload.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        uploaded = 0
        for p in sorted((store.home / 'outbox').glob('*/*/*.json.gz')):
            key = p.relative_to(store.home / 'outbox')
            cmd = [rclone, 'copyto', str(p), remote.rstrip('/') + '/' + key.as_posix(),
                   '--immutable', '--retries', '3', '--low-level-retries', '3', '--contimeout', '10s', '--timeout', '30s']
            try:
                result = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
                if result.returncode:
                    raise RuntimeError('rclone exited %s; %s' % (result.returncode, scrub(result.stderr)[-2000:]))
            except (OSError, subprocess.TimeoutExpired, RuntimeError) as exc:
                atomic_json(store.home / 'upload-status.json', {'at': now(), 'ok': False, 'error': scrub(str(exc)), 'uploaded': uploaded})
                raise RuntimeError('Upload failed; queued records were retained. See status.') from exc
            dest = store.home / 'archive' / key
            dest.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            os.replace(p, dest)
            uploaded += 1
        atomic_json(store.home / 'upload-status.json', {'at': now(), 'ok': True, 'uploaded': uploaded})


def pull(store):
    remote = store.config.get('remote')
    rclone = store.config.get('rclone') or shutil.which('rclone')
    if not remote or not rclone:
        raise ValueError('Configure R2 and rclone before downloading.')
    subprocess.run([rclone, 'copy', remote, str(store.home / 'cache'), '--immutable',
                    '--include', '*.json.gz'], check=True, timeout=300)


def records(store):
    seen = set()
    for folder in ['outbox', 'archive', 'cache']:
        for p in sorted((store.home / folder).glob('*/*/*.json.gz')):
            with gzip.open(p, 'rt') as f:
                record = json.load(f)
            if record['event_id'] not in seen:
                seen.add(record['event_id'])
                yield record


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--home', default=os.environ.get('SKILL_RUNS_HOME', str(DEFAULT_HOME)))
    commands = parser.add_subparsers(dest='command', required=True)
    commands.add_parser('hook')
    signal = commands.add_parser('signal', help='Emit a read-only skill-use marker for the tool hook')
    signal_group = signal.add_mutually_exclusive_group(required=True)
    signal_group.add_argument('--skill')
    signal_group.add_argument('--skill-uri')
    signal.add_argument('--name')
    mark = commands.add_parser('mark')
    mark.add_argument('--session', required=True)
    mark.add_argument('--turn', required=True)
    group = mark.add_mutually_exclusive_group(required=True)
    group.add_argument('--skill')
    group.add_argument('--skill-uri')
    mark.add_argument('--name')
    for cmd in ['status', 'upload', 'pull']:
        commands.add_parser(cmd)
    ls = commands.add_parser('list')
    ls.add_argument('--skill')
    show = commands.add_parser('show')
    show.add_argument('run_id')
    feedback = commands.add_parser('feedback')
    feedback.add_argument('run_id')
    feedback.add_argument('--file', type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == 'signal':
            print('SKILL_RUN_APPLIED ' + json.dumps({'skill': args.skill, 'skill_uri': args.skill_uri, 'name': args.name}))
            return 0
        store = Store(args.home)
        if args.command == 'hook':
            event = json.load(sys.stdin)
            kind = event.get('hook_event_name')
            output = {}
            if kind == 'UserPromptSubmit':
                output = store.start(event)
            elif kind == 'PostToolUse':
                store.tool(event)
            elif kind in ['Stop', 'Interrupt']:
                store.finish(event, interrupted=kind == 'Interrupt')
            print(json.dumps(output))
        elif args.command == 'mark':
            print(store.mark(args.session, args.turn, args.skill, args.skill_uri, args.name))
        elif args.command == 'status':
            print(json.dumps(store.status(), indent=2))
        elif args.command == 'upload':
            upload(store)
        elif args.command == 'pull':
            pull(store)
        elif args.command == 'list':
            for r in records(store):
                if r['kind'] == 'run' and (not args.skill or any(s['name'] == args.skill for s in r['skills'])):
                    print(json.dumps({k: r.get(k) for k in ['run_id', 'created_at', 'model', 'outcome']} |
                                     {'skills': [s['name'] for s in r['skills']]}))
        elif args.command == 'show':
            for r in records(store):
                if r.get('run_id') == args.run_id or r.get('related_run_id') == args.run_id:
                    print(json.dumps(r, indent=2))
        elif args.command == 'feedback':
            store.emit({'kind': 'feedback', 'created_at': now(), 'related_run_id': args.run_id,
                        'message': text_evidence(args.file.read_text())})
    except Exception as exc:
        if args.command == 'hook':
            # Observational hooks must never block or restart the task.
            print(json.dumps({'systemMessage': 'Skill-run capture failed (%s); the task can continue.' % type(exc).__name__}))
            return 0
        print(scrub(str(exc)), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
