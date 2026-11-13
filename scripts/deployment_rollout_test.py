#!/usr/bin/env python3
"""Run the real deploy/activation shell scripts against disposable fake hosts."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
OLD, NEW = 'a' * 40, 'b' * 40
SHIM = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
root = Path(os.environ['ROLLOUT_TEST_ROOT'])
name, args = Path(sys.argv[0]).name, sys.argv[1:]
with (root / 'calls').open('a') as log:
    log.write(json.dumps([name, *args]) + '\n')
old, new = 'a' * 40, 'b' * 40
mode, family = os.environ.get('ROLLOUT_TEST_MODE', ''), os.environ.get('ROLLOUT_TEST_FAMILY', 'web')
binary = 'loyal-engine' if family in ('backyard', 'earn-max', 'retail') else 'loyal-' + family
if name == 'git':
    if args[0] == 'rev-parse':
        print('f' * 40 if ':deploy/' in args[-1] else old if args[-1] == 'origin/main' and mode == 'stale' else new)
    elif args[0] == 'archive':
        sys.stdout.buffer.write((root / 'source.tar').read_bytes())
    elif args[0] == 'diff' and mode == 'migration':
        print('migrations/yield/new.sql')
    elif args[0] == 'cat-file' and args[-1].endswith((':.github/workflows/production.yml', ':scripts/production-scope.py')) and mode == 'failed-ci':
        sys.exit(1)
elif name == 'gh':
    if any('actions/artifacts' in arg for arg in args):
        print(0 if mode == 'missing-ci' else 1)
    else:
        print(1 if mode == 'failed-ci' else 0)
elif name == 'ssh':
    if args[1:5] == ['sh', '-s', '--', '/etc/prometheus/loyal.rules.yml']:
        print('0\nx86_64\nrules ' + 'f' * 40)
        print(f'ExecStart=/opt/loyal/releases/{family}/{old}/{binary}')
elif name == 'scp':
    pass
elif name in ('bun', 'make'):
    sys.exit('unexpected rebuild of verified artifacts')
elif name == 'ldd':
    print('libc.so.6 => /lib/libc.so.6')
elif name == 'date':
    tick = root / 'clock'
    now = int(tick.read_text()) + 5 if tick.exists() else 1000
    tick.write_text(str(now))
    print(now)
elif name == 'sleep':
    pass
elif name == 'systemctl':
    state = root / 'state'
    current = root / 'opt/loyal/bin/loyal-yield-realtime'
    if args[0] == 'is-active':
        print('inactive' if mode == 'stopped' else state.read_text() if state.exists() else 'active')
    elif args[0] == 'show':
        if 'Environment' in args:
            print('LOYAL_IMAGE_VERSION=sha-' + old)
        else:
            print('1' if mode == 'restart' and current.read_text() == new else '0')
    elif args[0] == 'stop':
        state.write_text('inactive')
    elif args[0] in ('start', 'restart'):
        state.write_text('active')
elif name == 'curl':
    current = root / 'opt/loyal/bin/loyal-yield-realtime'
    if mode == 'unready' and current.read_text() == new:
        sys.exit(22)
else:
    sys.exit('unexpected command ' + name)
'''


class DeploymentTest(unittest.TestCase):
    def prepare(self, family, mode):
        temp = tempfile.TemporaryDirectory(prefix='rollout-test-')
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        commands = root / 'commands'
        commands.mkdir()
        for name in ('git', 'gh', 'ssh', 'scp', 'bun', 'make', 'ldd', 'date', 'sleep', 'systemctl', 'curl'):
            file = commands / name
            file.write_text(SHIM)
            file.chmod(0o700)
        env = dict(os.environ, PATH=str(commands) + ':/usr/bin:/bin', ROLLOUT_TEST_ROOT=str(root),
                   ROLLOUT_TEST_FAMILY=family, ROLLOUT_TEST_MODE=mode)
        return root, env

    def deploy(self, family='web', mode='ok'):
        root, env = self.prepare(family, mode)
        scripts = root / 'scripts'
        scripts.mkdir()
        source = (ROOT / 'scripts/deploy-workers.sh').read_text().replace('$HOME/.loyal', str(root / 'build'))
        (scripts / 'deploy-workers.sh').write_text(source)
        (scripts / 'ci-tree-key.sh').write_text('#!/bin/sh\necho workers-v2-verified-test\n')
        (scripts / 'ci-tree-key.sh').chmod(0o700)
        with tarfile.open(root / 'source.tar', 'w') as archive:
            for name in ('deploy/hetzner/activate.sh', 'deploy/hetzner/install-rules.sh', 'deploy/hetzner/retain-web-assets.sh'):
                content = b'#!/bin/sh\nexit 0\n'
                info = tarfile.TarInfo(name)
                info.size = len(content)
                archive.addfile(info, io.BytesIO(content))
        artifacts = root / 'artifacts'
        artifacts.mkdir()
        binary = 'loyal-engine' if family in ('retail', 'backyard', 'earn-max') else 'loyal-' + family
        (artifacts / binary).write_text(OLD if mode == 'wrong-artifact' else NEW)
        (artifacts / 'web-assets').mkdir()
        (artifacts / 'web-assets/chunk.js').write_text('verified assets')
        env.update(LOYAL_DEPLOY_ARTIFACT_DIR=str(artifacts), LOYAL_DEPLOY_REQUIRE_HEAD='1',
                   LOYAL_DEPLOY_EXPECTED_RELEASE='c' * 40 if mode == 'concurrent-release' else OLD)
        result = subprocess.run(['bash', str(scripts / 'deploy-workers.sh'), family, NEW],
                                env=env, capture_output=True, text=True, timeout=10)
        calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()]
        return result, calls

    def test_verified_web_and_worker_artifacts_are_uploaded_without_rebuilding(self):
        for family in ('web', 'retail', 'telegram'):
            with self.subTest(family=family):
                result, calls = self.deploy(family)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(any(call[0] in ('bun', 'make') for call in calls))
                self.assertTrue(any(call[0] == 'scp' and '/artifacts/loyal-' in call[2] for call in calls))
                self.assertTrue(any('--setenv=LOYAL_DEPLOY_EXPECTED_RELEASE=' + OLD in call for call in calls))
                self.assertTrue(any('name=production-verified-' + family + '-' + NEW in call for call in calls if call[0] == 'gh'))

    def test_refusals_happen_before_any_artifact_upload(self):
        for family, mode in (('web', 'stale'), ('retail', 'missing-ci'), ('retail', 'failed-ci'),
                             ('retail', 'migration'), ('web', 'concurrent-release'), ('web', 'wrong-artifact')):
            with self.subTest(family=family, mode=mode):
                result, calls = self.deploy(family, mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(call[0] == 'scp' for call in calls), result.stdout + result.stderr)

    def test_main_verification_can_be_reused_after_a_rollout_failure(self):
        result, calls = self.deploy('retail')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        gh_calls = [call for call in calls if call[0] == 'gh']
        self.assertEqual(len(gh_calls), 1)
        self.assertIn('.workflow_run.head_branch == "main"', gh_calls[0][-1])
        self.assertIn('.workflow_run.head_sha == "' + NEW + '"', gh_calls[0][-1])

    def realtime(self, mode='ok', expected=OLD):
        root, env = self.prepare('realtime', mode)
        for path in ('opt/loyal/bin', 'run/lock'):
            (root / path).mkdir(parents=True)
        current = root / 'opt/loyal/bin/loyal-yield-realtime'
        current.write_text(OLD)
        current.chmod(0o555)
        candidate = root / 'opt/loyal/releases/realtime' / NEW / 'loyal-yield-realtime'
        candidate.parent.mkdir(parents=True)
        candidate.write_text(NEW)
        text = (ROOT / 'deploy/hetzner/activate-realtime.sh').read_text()
        for path in ('/opt/loyal', '/etc/systemd/system', '/var/log/loyal-deploy', '/run/lock'):
            text = text.replace(path, str(root / path.lstrip('/')))
        script = root / 'activate-realtime.sh'
        script.write_text(text)
        env['LOYAL_DEPLOY_EXPECTED_RELEASE'] = expected
        result = subprocess.run(['bash', str(script), NEW, hashlib.sha256(NEW.encode()).hexdigest()],
                                env=env, capture_output=True, text=True, timeout=10)
        calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()]
        return result, calls, current.read_text()

    def test_realtime_installs_verified_bytes(self):
        result, _, current = self.realtime()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(current, NEW)

    def test_realtime_restores_previous_bytes_on_restart_or_failed_readiness(self):
        for mode in ('restart', 'unready'):
            with self.subTest(mode=mode):
                result, _, current = self.realtime(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(current, OLD, result.stdout + result.stderr)
                self.assertIn('restored to', result.stdout)

    def test_realtime_does_not_override_stopped_or_concurrently_changed_service(self):
        for mode, expected in (('stopped', OLD), ('ok', 'c' * 40)):
            with self.subTest(mode=mode):
                result, calls, current = self.realtime(mode, expected)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(current, OLD)
                self.assertFalse(any(call[:2] == ['systemctl', 'stop'] for call in calls))


class TreeKeyTest(unittest.TestCase):
    def test_legacy_and_production_keys_cover_runtime_inputs_and_ignore_docs(self):
        with tempfile.TemporaryDirectory(prefix='rollout-tree-key-') as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', '-c', 'user.name=Rollout test', '-c', 'user.email=test@example.invalid', *args], cwd=root, text=True).strip()
            def commit():
                git('add', '.')
                git('commit', '-qm', 'test')
            def key():
                return subprocess.check_output(['bash', str(ROOT / 'scripts/ci-tree-key.sh'), 'HEAD'], cwd=root, text=True).strip()
            git('init', '-q')
            for directory in ('go/workers', '.github/workflows', 'docs'):
                (root / directory).mkdir(parents=True)
            workflow = 'on:\n  push:\n    paths:\n      - go/workers/**\n      - .github/workflows/**\n  workflow_dispatch:\n'
            (root / '.github/workflows/workers-v2-verify.yml').write_text(workflow)
            source = root / 'go/workers/main.go'
            source.write_text('first')
            commit()
            legacy = key()
            (root / 'docs/notes.md').write_text('documentation')
            commit()
            self.assertEqual(key(), legacy)
            (root / '.github/workflows/production.yml').write_text(workflow)
            (root / '.github/workflows/workers-v2-verify.yml').write_text('on:\n  workflow_call:\n')
            commit()
            production = key()
            self.assertNotEqual(production, legacy)
            source.write_text('second')
            commit()
            self.assertNotEqual(key(), production)


if __name__ == '__main__':
    unittest.main()
