#!/usr/bin/env python3
"""Exercise the real activation script with local paths and fake systemd commands."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ACTIVATE = Path(__file__).resolve().parents[1] / 'deploy/hetzner/activate.sh'
OLD, NEW = 'a' * 40, 'b' * 40
SHIM = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
root = Path(os.environ['ACTIVATION_TEST_ROOT'])
name, args = Path(sys.argv[0]).name, sys.argv[1:]
with (root / 'calls').open('a') as log:
    log.write(json.dumps([name, *args]) + '\\n')
state = root / 'state'
mode = os.environ['ACTIVATION_TEST_MODE']
vault = os.environ['ACTIVATION_TEST_VAULT']
if name == 'sleep':
    pass
elif name == 'date':
    if args == ['+%s']:
        clock = root / 'clock'
        tick = int(clock.read_text()) + 10 if clock.exists() else 1000
        clock.write_text(str(tick))
        print(tick)
    else:
        print('2026-11-06 12:00:00')
elif name == 'systemctl':
    if args[0] == 'is-active':
        print(state.read_text() if state.exists() else 'active')
    elif args[0] == 'show':
        print('0')
    elif args[0] == 'cat':
        print('LoadCredentialEncrypted=BACKYARD_DATABASE_URL:/fake/encrypted/database')
    elif args[0] == 'stop':
        state.write_text('inactive')
    elif args[0] in ('start', 'restart'):
        state.write_text('active')
elif name == 'systemd-run':
    binary = next(arg for arg in args if arg.endswith('/loyal-engine'))
    if mode == 'legacy' and '/'+ 'a'*40 +'/' in binary:
        sys.exit(2)
    if args[-4:] != ['backyard', 'in-flight', '--vault', vault]:
        sys.exit('refusing unscoped or wrong-vault gate')
    stopped = state.exists() and state.read_text() == 'inactive'
    print('1' if mode == 'after-stop-pending' and stopped else '0')
elif name == 'journalctl':
    if mode == 'wrong-start':
        vault = 'looping' if vault == 'rwa' else 'rwa'
    print(json.dumps({'msg': 'backyard_worker_start', 'vault': vault, 'owner': 'worker:sha-' + 'b'*40}, separators=(',', ':')))
else:
    sys.exit('unexpected command ' + name)
'''


class ActivationTest(unittest.TestCase):
    def run_activation(self, family, mode='ok'):
        temp = tempfile.TemporaryDirectory(prefix='vault-activation-')
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        text = ACTIVATE.read_text()
        for path in ('/etc/systemd/system', '/opt/loyal', '/var/log/loyal-deploy', '/run', '/root'):
            text = text.replace(path, str(root / path.lstrip('/')))
        script = root / 'activate.sh'
        script.write_text(text)
        (root / 'run/lock').mkdir(parents=True)
        (root / 'root').mkdir()
        commands = root / 'commands'
        commands.mkdir()
        for name in ('systemctl', 'systemd-run', 'journalctl', 'sleep', 'date'):
            command = commands / name
            command.write_text(SHIM)
            command.chmod(0o700)
        release = root / 'opt/loyal/releases' / family
        for sha in (OLD, NEW):
            binary = release / sha / 'loyal-engine'
            binary.parent.mkdir(parents=True)
            binary.write_text('#!/bin/sh\nexit 0\n')
            binary.chmod(0o700)
        dropin = root / f'etc/systemd/system/loyal-{family}.service.d/40-verified-release.conf'
        dropin.parent.mkdir(parents=True)
        dropin.write_text(f'[Service]\nExecStart=\nExecStart={release}/{OLD}/loyal-engine\n')
        env = dict(os.environ, PATH=str(commands) + ':/usr/bin:/bin',
                   ACTIVATION_TEST_ROOT=str(root), ACTIVATION_TEST_MODE=mode,
                   ACTIVATION_TEST_VAULT='rwa' if family == 'earn-max' else 'looping')
        result = subprocess.run(['sh', str(script), family, NEW,
                                 hashlib.sha256(binary.read_bytes()).hexdigest()],
                                env=env, capture_output=True, text=True, timeout=10)
        self.assertTrue((root / 'calls').exists(), result.stdout + result.stderr)
        calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()]
        return result, calls, dropin.read_text()

    def test_each_service_deploys_with_its_own_gate_and_start(self):
        for family in ('earn-max', 'backyard'):
            with self.subTest(family=family):
                result, calls, dropin = self.run_activation(family)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(NEW, dropin)
                self.assertIn(['systemctl', 'stop', 'loyal-' + family], calls)
                for call in calls:
                    if call[0] == 'systemd-run':
                        self.assertEqual(call[-2:], ['--vault', 'rwa' if family == 'earn-max' else 'looping'])
                    if call[:2] in (['systemctl', 'stop'], ['systemctl', 'start']):
                        self.assertEqual(call[2], 'loyal-' + family)

    def test_old_combined_worker_requires_coordinated_cutover(self):
        result, calls, dropin = self.run_activation('backyard', 'legacy')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('previous release cannot pass the vault-scoped gate', result.stdout)
        self.assertFalse(any(call[:2] == ['systemctl', 'stop'] for call in calls))
        self.assertIn(OLD, dropin)

    def test_pending_work_after_stop_keeps_previous_release(self):
        result, calls, dropin = self.run_activation('earn-max', 'after-stop-pending')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("in-flight after stop is '1'", result.stdout)
        self.assertIn(['systemctl', 'start', 'loyal-earn-max'], calls)
        self.assertIn(OLD, dropin)

    def test_other_vault_start_does_not_pass_health_gate(self):
        result, calls, dropin = self.run_activation('earn-max', 'wrong-start')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('rolled back', result.stdout)
        self.assertIn(['systemctl', 'restart', 'loyal-earn-max'], calls)
        self.assertIn(OLD, dropin)


if __name__ == '__main__':
    unittest.main()
