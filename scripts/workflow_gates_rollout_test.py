"""Exercise the production workflow gates with GitHub's recorded scoped-run outcomes."""
import json
import os
from pathlib import Path
import subprocess
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
JOBS = yaml.safe_load((ROOT / '.github/workflows/production.yml').read_text())['jobs']


def run_step(job, needs):
    return subprocess.run(
        ['bash', '-eo', 'pipefail', '-c', JOBS[job]['steps'][-1]['run']],
        cwd=ROOT, env=dict(os.environ, NEEDS_JSON=json.dumps(needs), GITHUB_EVENT_NAME='push'),
        capture_output=True, text=True,
    ).returncode


class WorkflowGates(unittest.TestCase):
    def test_release_jobs_override_implicit_success_and_require_their_direct_dependencies(self):
        for job in ('receipts', 'deploy'):
            needs = ['scope', 'verified'] + (['receipts'] if job == 'deploy' else [])
            self.assertEqual(JOBS[job]['needs'], needs)
            conditions = JOBS[job]['if'].removeprefix('${{ ').removesuffix(' }}').split(' && ')
            self.assertCountEqual(conditions, [
                '!cancelled()', "github.ref == 'refs/heads/main'", "needs.scope.outputs.services != '[]'",
                *(f"needs.{name}.result == 'success'" for name in needs),
            ])
        self.assertEqual(JOBS['rollout-result']['needs'], ['scope', 'verified', 'receipts', 'deploy'])
        self.assertEqual(JOBS['rollout-result']['if'], "${{ !cancelled() && github.ref == 'refs/heads/main' }}")

    def test_verified_accepts_intentional_skips_and_rejects_invalid_scope(self):
        checks = {'contracts': False, 'workers': True, 'web': True, 'journeys': False, 'realtime': False}
        outputs = {job: str(selected).lower() for job, selected in checks.items()}
        outputs.update(worker_services='["earn-max"]', web_services='["web"]')
        needs = {job: {'result': 'success' if selected else 'skipped'} for job, selected in checks.items()}
        needs.update(scope={'result': 'success', 'outputs': outputs}, **{'merge-checks': {'result': 'success'}})
        for value, ok in [(['earn-max', 'web'], True), ([], True), (None, False), ('web', False),
                          (['unknown'], False), (['admin'], False), (['earn-max', 'earn-max'], False)]:
            with self.subTest(value=value):
                outputs['services'] = json.dumps(value)
                self.assertEqual(run_step('verified', needs) == 0, ok)
        outputs['services'] = '["earn-max","web"]'
        needs['workers']['result'] = 'skipped'
        self.assertNotEqual(run_step('verified', needs), 0)

    def test_terminal_check_proves_deployment_or_empty_scope(self):
        for services, receipts, deploy, expected in [
            ('["earn-max"]', 'success', 'success', True), ('[]', 'skipped', 'skipped', True),
            ('["earn-max"]', 'skipped', 'skipped', False), ('["earn-max"]', 'failure', 'skipped', False),
            ('["earn-max"]', 'success', 'cancelled', False), ('[]', 'skipped', 'success', False),
            ('null', 'skipped', 'skipped', False), ('', 'skipped', 'skipped', False),
        ]:
            with self.subTest(services=services, receipts=receipts, deploy=deploy):
                needs = {'scope': {'result': 'success', 'outputs': {'services': services}},
                         'verified': {'result': 'success'}, 'receipts': {'result': receipts},
                         'deploy': {'result': deploy}}
                self.assertEqual(run_step('rollout-result', needs) == 0, expected)
                for prerequisite in ('scope', 'verified'):
                    needs[prerequisite]['result'] = 'failure'
                    self.assertNotEqual(run_step('rollout-result', needs), 0)
                    needs[prerequisite]['result'] = 'success'


if __name__ == '__main__':
    unittest.main()
