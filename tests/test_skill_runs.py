import gzip
import json
import shlex
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import skill_runs as sr
from install_skill_runs import install, merge_hooks


class SkillRunTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.store = sr.Store(self.root / 'private')
        self.skill = self.root / 'sample/SKILL.md'
        self.skill.parent.mkdir()
        self.skill.write_text('---\nname: sample\ndescription: Test\n---\nRead carefully.\n')
        self.event = {'session_id': 'session', 'turn_id': 'turn', 'cwd': str(self.root),
                      'model': 'test-model', 'prompt': 'Review the new schema.'}

    def tearDown(self):
        self.store.db.close()
        self.temp.cleanup()

    def capture(self):
        self.store.start(self.event)
        run = self.store.mark('session', 'turn', self.skill)
        self.store.finish(dict(self.event, last_assistant_message='Keep the PR schema.'))
        return run

    def test_capture_snapshot_output_and_idempotent_stop(self):
        self.store.start(self.event)
        run = self.store.mark('session', 'turn', self.skill)
        self.store.mark('session', 'turn', self.skill)
        self.store.tool(dict(self.event, tool_use_id='call', tool_name='Bash',
                             tool_input={'command': 'python tests.py'}, tool_response={'exit_code': 0}))
        self.skill.write_text('changed later')
        self.store.finish(dict(self.event, last_assistant_message='Keep the PR schema.'))
        self.store.finish(self.event)
        records = list(sr.records(self.store))
        self.assertEqual(len(records), 1)
        r = records[0]
        self.assertEqual(r['run_id'], run)
        self.assertEqual(len(r['skills']), 1)
        self.assertIn('Read carefully', r['skills'][0]['snapshot']['text'])
        self.assertEqual(r['response']['text'], 'Keep the PR schema.')
        self.assertEqual(r['tools'][0]['id'], 'call')
        self.assertEqual(r['capture']['skill_use'], 'agent_reported_not_independently_verified')

    def test_no_marker_does_not_archive_unrelated_turn(self):
        self.store.start(self.event)
        self.store.tool(dict(self.event, tool_input={'command': 'cat /a/SKILL.md'}))
        self.store.finish(self.event)
        self.assertEqual(list(sr.records(self.store)), [])
        self.assertIsNone(self.store.load('session', 'turn'))

    def test_read_only_signal_is_captured_by_tool_hook(self):
        self.store.start(self.event)
        argv = [sys.executable, str(Path(sr.__file__).resolve()), 'signal', '--skill', str(self.skill)]
        signal = subprocess.run(argv, capture_output=True, text=True, check=True)
        self.store.tool(dict(self.event, tool_use_id='signal', tool_name='Bash',
                             tool_input={'command': shlex.join(argv)},
                             tool_response={'output': signal.stdout, 'exit_code': 0}))
        self.store.finish(self.event)
        self.assertEqual(list(sr.records(self.store))[0]['skills'][0]['name'], 'sample')

    def test_research_output_cannot_impersonate_signal(self):
        self.store.start(self.event)
        self.store.tool(dict(self.event, tool_input={'command': 'cat README.md'},
                             tool_response={'output': 'SKILL_RUN_APPLIED ' + json.dumps({'skill': str(self.skill)})}))
        self.store.finish(self.event)
        self.assertEqual(list(sr.records(self.store)), [])

    def test_marker_handles_native_wrapped_output(self):
        marker = 'SKILL_RUN_APPLIED {"skill": "/example/SKILL.md"}\n'
        self.assertEqual(sr.marker_from_output('Process exited with code 0\nFinal output:\n' + marker)['skill'], '/example/SKILL.md')
        self.assertEqual(sr.marker_from_output({'content': [{'type': 'text', 'text': marker}]})['skill'], '/example/SKILL.md')
        self.assertIsNone(sr.marker_from_output({'exit_code': 1, 'output': marker}))

    def test_followup_is_linked_but_not_assumed_to_be_feedback(self):
        run = self.capture()
        followup = dict(self.event, turn_id='next', prompt='Too much prose.')
        self.store.start(followup)
        self.store.start(followup)
        self.store.finish(followup)
        f = [r for r in sr.records(self.store) if r['kind'] == 'followup']
        self.assertEqual(len(f), 1)
        self.assertEqual(f[0]['related_run_id'], run)
        self.assertEqual(f[0]['classification'], 'next_user_message_unclassified')

    def test_secrets_redacted_and_large_tools_flagged(self):
        self.store.start(dict(self.event, prompt='api_key=supersecret'))
        self.store.mark('session', 'turn', self.skill)
        self.store.tool(dict(self.event, tool_use_id='secret', tool_input={'authorization': 'Bearer secret'},
                             tool_response={'password': 'my-secret', 'result': 'a' * (sr.MAX_TEXT + 10)}))
        self.store.finish(dict(self.event, last_assistant_message='Bearer abcdefgh'))
        r = list(sr.records(self.store))[0]
        self.assertNotIn('supersecret', json.dumps(r))
        self.assertNotIn('my-secret', json.dumps(r))
        self.assertNotIn('abcdefgh', json.dumps(r))
        self.assertTrue(r['tools'][0]['output']['truncated'])

    def test_excluded_project_never_starts(self):
        self.store.config['excluded_roots'] = [str(self.root)]
        self.assertEqual(self.store.start(self.event), {})
        self.assertIsNone(self.store.load('session', 'turn'))

    def test_storage_refuses_git_worktree(self):
        repo = self.root / 'public-repo'
        (repo / '.git').mkdir(parents=True)
        with self.assertRaises(ValueError):
            sr.Store(repo / 'runs')

    def test_interrupted_and_remote_skill_gaps(self):
        self.store.start(self.event)
        self.store.mark('session', 'turn', uri='skill://example', name='remote')
        self.store.finish(self.event, interrupted=True)
        r = list(sr.records(self.store))[0]
        self.assertEqual(r['outcome'], 'interrupted')
        self.assertIsNone(r['skills'][0]['snapshot'])

    def test_transcript_allowlist_never_copies_reasoning(self):
        path = self.root / 'transcript.jsonl'
        lines = [
            {'type': 'turn_context', 'payload': {'turn_id': 'turn', 'model': 'test-model', 'effort': 'high'}},
            {'type': 'response_item', 'payload': {'type': 'reasoning', 'summary': 'PRIVATE REASONING'}},
            {'type': 'token_usage_record', 'payload': {'turn_id': 'other', 'turn_token_usage': {'input_tokens': 99}}},
            {'type': 'token_usage_record', 'payload': {'turn_id': 'turn', 'turn_token_usage': {'input_tokens': 10}}},
        ]
        path.write_text('\n'.join(json.dumps(x) for x in lines))
        r = sr.transcript_metadata(path, 'turn')
        self.assertEqual(r['effort'], 'high')
        self.assertEqual(r['usage']['input_tokens'], 10)
        self.assertNotIn('PRIVATE REASONING', json.dumps(r))

    def test_upload_failure_retains_outbox_success_archives(self):
        self.capture()
        self.store.config.update(remote='r2:bucket/v1', rclone='/fake/rclone')
        with patch.object(sr.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, '', 'offline')):
            with self.assertRaises(RuntimeError):
                sr.upload(self.store)
        self.assertEqual(self.store.status()['pending_uploads'], 1)
        with patch.object(sr.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, '', '')) as run:
            sr.upload(self.store)
        self.assertIn('--immutable', run.call_args.args[0])
        self.assertEqual(self.store.status()['pending_uploads'], 0)
        self.assertEqual(self.store.status()['archived_uploads'], 1)
        self.assertTrue(self.store.status()['upload_status']['ok'])

    def test_hooks_preserve_existing_and_install_is_idempotent(self):
        existing = {'hooks': {'Stop': [{'hooks': [{'type': 'command', 'command': 'other'}]}]}}
        one = merge_hooks(existing, 'recorder hook')
        two = merge_hooks(one, 'recorder hook')
        self.assertEqual(one, two)
        self.assertEqual(two['hooks']['Stop'][0]['hooks'][0]['command'], 'other')
        self.assertEqual(len(two['hooks']['Stop']), 2)

    def test_hook_failure_does_not_block(self):
        script = Path(sr.__file__)
        p = subprocess.run([sys.executable, str(script), '--home', str(self.root / 'private'), 'hook'],
                           input='not json', text=True, capture_output=True)
        self.assertEqual(p.returncode, 0)
        self.assertIn('systemMessage', json.loads(p.stdout))
        self.assertNotIn('decision', json.loads(p.stdout))

    def test_installed_hook_lifecycle(self):
        home = self.root / 'installed-private'
        codex = self.root / 'codex'
        runtime = install(home, codex)
        hooks = json.loads((codex / 'hooks.json').read_text())['hooks']
        def invoke(event, **extra):
            command = hooks[event][-1]['hooks'][0]['command']
            result = subprocess.run(shlex.split(command), input=json.dumps(
                dict(self.event, hook_event_name=event, **extra)),
                capture_output=True, text=True, check=True)
            return json.loads(result.stdout)
        context = invoke('UserPromptSubmit')
        self.assertIn(str(runtime), context['hookSpecificOutput']['additionalContext'])
        command = [sys.executable, str(runtime), 'signal', '--skill', str(self.skill)]
        marker = subprocess.run(command, capture_output=True, text=True, check=True)
        invoke('PostToolUse', tool_name='Bash', tool_use_id='signal',
               tool_input={'command': shlex.join(command)},
               tool_response={'output': marker.stdout, 'exit_code': 0})
        invoke('Stop', last_assistant_message='Synthetic successful result')
        bundles = list((home / 'outbox').glob('*/*/*.json.gz'))
        self.assertEqual(len(bundles), 1)
        with gzip.open(bundles[0], 'rt') as f:
            record = json.load(f)
        self.assertEqual(record['skills'][0]['name'], 'sample')
        self.assertEqual(record['response']['text'], 'Synthetic successful result')


if __name__ == '__main__':
    unittest.main()
