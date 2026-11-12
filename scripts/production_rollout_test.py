#!/usr/bin/env python3
"""Exercise rollout decisions with fake host/process boundaries, never production."""
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


health = load('rollout_health', 'deploy/hetzner/rollout-health.py')
rollout = load('production_rollout', 'scripts/production-rollout.py')
OLD, NEW = 'a' * 40, 'b' * 40


def sample(release=OLD, invocation='old', at=1000):
    return {'family': 'backyard', 'release': release, 'invocation': invocation,
            'at': at, 'cursor': 'cursor-before', 'binary_sha256': 'c' * 64,
            'lane_observed_at': at, 'failures': {},
            'vault': {'observed_at': at, 'intent_fingerprint': 'same', 'inflight': 0}}


class HealthTest(unittest.TestCase):
    def test_database_uses_decoded_credentials_without_arguments_or_session_options(self):
        with tempfile.TemporaryDirectory() as directory:
            url = 'postgresql://rollout:p%40ss@db.example:6432/loyal?sslmode=require'
            (Path(directory) / 'BACKYARD_DATABASE_URL').write_text(url)
            data = {'observed_at': '2026-11-12T20:00:00Z', 'observed_slot': 42,
                    'position': {'collateral': '1', 'debt': '0', 'status': 'active'},
                    'targets': [], 'exits': [], 'permissions': [], 'recovery': 0, 'failed_or_held': 0, 'inflight': 0}
            with patch.dict(os.environ, {'CREDENTIALS_DIRECTORY': directory, 'PGOPTIONS': '-c default_transaction_read_only=on'}), \
                 patch.object(health, 'command', return_value=json.dumps(data)) as command:
                result = health.database_snapshot('backyard', 0)
            env = command.call_args.kwargs['env']
            self.assertEqual((env['PGHOST'], env['PGPORT'], env['PGDATABASE']), ('db.example', '6432', 'loyal'))
            self.assertEqual(env['PGPASSWORD'], 'p@ss')
            self.assertNotIn('PGOPTIONS', env)
            self.assertNotIn(url, str(command.call_args.args))
            self.assertEqual(result['observed_slot'], 42)

    def test_new_process_requires_fresh_observation(self):
        before, current = sample(), sample(NEW, 'new', 1001)
        self.assertFalse(health.check_sample(before, current, NEW, 1002))
        current['lane_observed_at'] = current['vault']['observed_at'] = 1003
        self.assertTrue(health.check_sample(before, current, NEW, 1002))

    def test_wrong_release_restart_failure_and_permission_change_stop_watch(self):
        for mutate in (
            lambda s: s.update(release=OLD),
            lambda s: s.update(binary_sha256='d' * 64),
            lambda s: s.update(invocation='restarted'),
            lambda s: s['failures'].update({'loyal_family_failed_total{code="timeout"}': 1}),
            lambda s: s['vault'].update(intent_fingerprint='changed'),
        ):
            before, previous = sample(), sample(NEW, 'new', 1003)
            current = copy.deepcopy(previous)
            mutate(current)
            with self.assertRaises(RuntimeError):
                health.check_sample(before, current, NEW, 1002, previous)

    def test_existing_process_historical_failures_do_not_mask_a_new_failure(self):
        before = sample(NEW)
        before['failures'] = {'failed': 5}
        current = copy.deepcopy(before)
        self.assertTrue(health.check_sample(before, current, NEW, 999))
        current['failures']['failed'] = 6
        with self.assertRaisesRegex(RuntimeError, 'failure counter'):
            health.check_sample(before, current, NEW, 999)

    def test_failed_nav_at_info_level_is_still_failure(self):
        for event in ({'msg': 'backyard_operation_failed_after_send', 'level': 'INFO'},
                      {'msg': 'backyard_worker_exit', 'level': 'WARN'},
                      {'msg': 'some_error', 'level': 'ERROR'}):
            with patch.object(health, 'command', return_value=json.dumps({'MESSAGE': json.dumps(event)})):
                self.assertTrue(health.journal_failures('loyal-backyard.service', 'cursor'))

    def test_normal_wait_is_not_a_failed_operation(self):
        entry = {'MESSAGE': json.dumps({'msg': 'backyard_heartbeat', 'hold': 'no_room', 'level': 'INFO'})}
        with patch.object(health, 'command', return_value=json.dumps(entry)):
            self.assertEqual(health.journal_failures('loyal-backyard.service', 'cursor'), [])

    def test_missing_and_nonfinite_metrics_cannot_satisfy_health(self):
        for raw in (b'not a metric', b'loyal_family_failed_total NaN'):
            response = unittest.mock.MagicMock()
            response.__enter__.return_value.read.return_value = raw
            with patch.object(health.urllib.request, 'urlopen', return_value=response), self.assertRaises(RuntimeError):
                health.metrics('backyard')

    def test_watch_returns_only_after_window_and_fresh_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = Path(directory) / 'before.json'
            baseline.write_text(json.dumps(sample()))
            clock = [1001]
            def sleep(seconds):
                clock[0] += seconds
            with patch.object(health.time, 'time', side_effect=lambda: clock[0]), \
                 patch.object(health.time, 'sleep', side_effect=sleep), \
                 patch.object(health, 'snapshot', side_effect=lambda *args, **kwargs: sample(NEW, 'new', clock[0])), \
                 patch('sys.stdout', new_callable=io.StringIO) as out:
                health.watch('backyard', NEW, baseline, seconds=10)
                self.assertEqual(clock[0], 1011)
                self.assertTrue(json.loads(out.getvalue())['fresh'])

    def test_startup_wait_preserves_full_observation_window_and_has_a_deadline(self):
        for delay, expected, error in ((15, 1026, None), (65, 1061, 'startup observation timed out')):
            with self.subTest(delay=delay), tempfile.TemporaryDirectory() as directory:
                baseline = Path(directory) / 'before.json'
                baseline.write_text(json.dumps(sample()))
                clock = [1001]
                def sleep(seconds):
                    clock[0] += seconds
                def snapshot(*args, **kwargs):
                    current = sample(NEW, 'new', clock[0])
                    if clock[0] < 1001 + delay:
                        self.assertTrue(kwargs['allow_startup'])
                        current['lane_observed_at'] = 0
                    return current
                with patch.object(health.time, 'time', side_effect=lambda: clock[0]), \
                     patch.object(health.time, 'sleep', side_effect=sleep), \
                     patch.object(health, 'snapshot', side_effect=snapshot), \
                     patch('sys.stdout', new_callable=io.StringIO):
                    if error:
                        with self.assertRaisesRegex(RuntimeError, error):
                            health.watch('backyard', NEW, baseline, seconds=10)
                    else:
                        health.watch('backyard', NEW, baseline, seconds=10)
                self.assertEqual(clock[0], expected)

    def test_startup_wait_stops_on_failure_restart_or_changed_permissions(self):
        for mutate in (
            lambda s: s.update(invocation='restarted'),
            lambda s: s['failures'].update({'failed': 1}),
            lambda s: s['vault'].update(intent_fingerprint='changed'),
        ):
            with tempfile.TemporaryDirectory() as directory:
                baseline = Path(directory) / 'before.json'
                baseline.write_text(json.dumps(sample()))
                first = sample(NEW, 'new', 1001)
                first['lane_observed_at'] = 0
                second = copy.deepcopy(first)
                mutate(second)
                with patch.object(health.time, 'time', return_value=1001), \
                     patch.object(health.time, 'sleep') as sleep, \
                     patch.object(health, 'snapshot', side_effect=[first, second]), \
                     self.assertRaises(RuntimeError):
                    health.watch('backyard', NEW, baseline)
                sleep.assert_called_once_with(5)

    def test_startup_snapshot_keeps_vault_and_journal_guards_and_strict_preflight(self):
        binary = Path(f'/opt/loyal/releases/backyard/{NEW}/loyal-engine')
        state = 'ActiveState=active\nSubState=running\nNRestarts=0\nMainPID=123\nInvocationID=new'
        for allow, lanes, failed, journal, error in (
            (False, {}, 0, [], 'observations are missing'),
            (True, {}, 0, [], None),
            (True, {'loyal_lane_last_success_timestamp_seconds': 0}, 0, [], None),
            (True, {}, 1, [], 'failed/held operation'),
            (True, {}, 0, ['error'], 'error/exit'),
            (True, {'loyal_lane_last_success_timestamp_seconds': 1}, 0, [], 'observation is stale'),
        ):
            with self.subTest(allow=allow, lanes=lanes, failed=failed, journal=journal):
                vault = dict(sample()['vault'], recovery=0, failed_or_held=failed)
                with patch.object(health, 'command', side_effect=[state, '-- cursor: now']), \
                     patch.object(health, 'journal_failures', return_value=journal), \
                     patch.object(health, 'metrics', return_value=lanes), \
                     patch.object(health, 'vault_snapshot', return_value=vault) as probe, \
                     patch.object(health.time, 'time', return_value=1001), \
                     patch.object(Path, 'read_text', return_value=f'ExecStart={binary}'), \
                     patch.object(Path, 'resolve', return_value=binary), \
                     patch.object(Path, 'is_file', return_value=True), \
                     patch.object(Path, 'read_bytes', return_value=b'release'), \
                     patch.object(health.os, 'access', return_value=True):
                    if error:
                        with self.assertRaisesRegex(RuntimeError, error):
                            health.snapshot('backyard', 1000, 'cursor', allow_startup=allow)
                    else:
                        current = health.snapshot('backyard', 1000, 'cursor', allow_startup=allow)
                        self.assertEqual(current['lane_observed_at'], 0)
                        probe.assert_called_once_with('backyard', 1000)

    def test_watch_stops_on_first_failure_without_waiting_out_window(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = Path(directory) / 'before.json'
            baseline.write_text(json.dumps(sample()))
            with patch.object(health.time, 'time', return_value=1001), \
                 patch.object(health.time, 'sleep') as sleep, \
                 patch.object(health, 'snapshot', side_effect=RuntimeError('held operation')), \
                 self.assertRaisesRegex(RuntimeError, 'held operation'):
                health.watch('backyard', NEW, baseline)
            sleep.assert_not_called()

    def test_unavailable_database_does_not_expose_child_diagnostics(self):
        result = subprocess.CompletedProcess(['psql'], 1, '', 'password=secret')
        with patch.object(health.subprocess, 'run', return_value=result), self.assertRaises(RuntimeError) as caught:
            health.command('psql', '-X')
        self.assertNotIn('secret', str(caught.exception))


class PipelineTest(unittest.TestCase):
    def exercise(self, fail_watch=None, fail_preflight=None, stale=False, fail_activation=None):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        directory = Path(temp.name)
        for binary in set(rollout.BINARY.values()):
            (directory / binary).write_text('artifact')
        calls = []
        deployed = set()
        def run(*args, **kwargs):
            calls.append(args)
            if args[:3] == ('git', 'rev-parse', 'HEAD'):
                return NEW
            if args[:3] == ('git', 'rev-parse', 'origin/main'):
                return OLD if stale else NEW
            if args[:2] == ('git', 'diff'):
                return ''
            if args[:2] == ('bash', 'scripts/deploy-workers.sh'):
                if args[2] == fail_activation:
                    raise RuntimeError('activation refused')
                deployed.add(args[2])
            return ''
        def ssh(host, *args, **kwargs):
            calls.append(('ssh', *args))
            if args[0] == 'mktemp':
                return '/tmp/loyal-rollout.TEST'
            if args[0] == 'python3' and args[2] == 'snapshot':
                family = args[3]
                if family == fail_preflight:
                    raise RuntimeError('preflight failed')
                data = sample(NEW if family in deployed else OLD)
                data['family'] = family
                return json.dumps(data)
            if args[0] == 'systemd-run' and '/tmp/loyal-rollout.TEST/activate-realtime.sh' in args:
                deployed.add('realtime')
            if args[0] == 'python3' and args[2] == 'watch' and args[3] == fail_watch:
                raise RuntimeError('first failed operation')
            return ''
        with patch.object(rollout, 'run', side_effect=run), patch.object(rollout, 'ssh', side_effect=ssh), \
             patch.object(rollout, 'rollback') as rollback, patch.object(rollout, 'summary'), \
             patch.dict(os.environ, {'LOYAL_DEPLOY_ARTIFACT_DIR': str(directory)}):
            try:
                rollout.rollout(NEW)
                error = None
            except RuntimeError as caught:
                error = caught
            return calls, deployed, rollback.call_args_list, error

    def test_success_requires_all_preflights_before_any_service_deploys(self):
        calls, deployed, restored, error = self.exercise()
        self.assertIsNone(error)
        self.assertEqual(deployed, set(rollout.FAMILIES))
        first_deploy = next(i for i, call in enumerate(calls) if call[:2] == ('bash', 'scripts/deploy-workers.sh'))
        checked = {call[4] for call in calls[:first_deploy] if call[:2] == ('ssh', 'python3') and call[3] == 'snapshot'}
        self.assertEqual(checked, set(rollout.FAMILIES))
        watches = [call[4] for call in calls if call[:2] == ('ssh', 'python3') and call[3] == 'watch']
        self.assertEqual(watches, list(rollout.FAMILIES))
        self.assertFalse(restored)

    def test_failed_watch_stops_before_next_service_and_requests_guarded_rollback(self):
        _, deployed, restored, error = self.exercise(fail_watch='backyard')
        self.assertIsNotNone(error)
        self.assertEqual(deployed, {'backyard'})
        self.assertEqual(len(restored), 1)
        self.assertEqual(restored[0].args[2], 'backyard')

    def test_late_preflight_failure_changes_no_service(self):
        _, deployed, restored, error = self.exercise(fail_preflight='realtime')
        self.assertIsNotNone(error)
        self.assertFalse(deployed)
        self.assertFalse(restored)

    def test_failed_activation_halts_without_a_second_activation(self):
        _, deployed, restored, error = self.exercise(fail_activation='backyard')
        self.assertIsNotNone(error)
        self.assertFalse(deployed)
        self.assertFalse(restored)

    def test_superseded_commit_never_reaches_host(self):
        calls, deployed, _, error = self.exercise(stale=True)
        self.assertIsNotNone(error)
        self.assertFalse(deployed)
        self.assertFalse(any(call[0] == 'ssh' for call in calls))

    def test_migration_or_unit_change_requires_explicit_rollout(self):
        for path in ('migrations/yield/0143_example.sql', 'deploy/hetzner/systemd/loyal-retail.service'):
            def run(*args, **kwargs):
                return path if args[:2] == ('git', 'diff') else ''
            with patch.object(rollout, 'run', side_effect=run), self.assertRaisesRegex(RuntimeError, 'explicit rollout'):
                rollout.check_previous('retail', sample(), NEW)

    def test_same_sha_cannot_silently_reuse_different_binary_bytes(self):
        with self.assertRaisesRegex(RuntimeError, 'different bytes'):
            rollout.check_previous('retail', sample(NEW), NEW, 'd' * 64)


if __name__ == '__main__':
    unittest.main()
