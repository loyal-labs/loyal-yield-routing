#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('scope', ROOT / 'scripts/production-scope.py')
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)
spec = importlib.util.spec_from_file_location('db_tests', ROOT / 'scripts/ci-db-tests.py')
db_tests = importlib.util.module_from_spec(spec)
spec.loader.exec_module(db_tests)


class ProductionBaselineTest(unittest.TestCase):
    repository = 'loyal-labs/loyal-yield-routing'

    def run_record(self, number):
        return {'id': number, 'run_attempt': 2, 'head_sha': f'{number:040x}', 'event': 'push',
                'head_repository': {'full_name': self.repository}}

    def job_pages(self, run, outcomes):
        jobs = [dict(run_id=run['id'], head_sha=run['head_sha'], status='completed', name=name, conclusion=result)
                for name, result in outcomes.items()]
        return [{'total_count': len(jobs), 'jobs': jobs}]

    def page_output(self, pages):
        return '\n'.join(json.dumps(page) for page in pages)

    def test_green_empty_and_partial_legacy_runs_cannot_hide_missed_service(self):
        complete = {'deploy': 'success', **{f'receipts ({service})': 'success' for service in scope.SERVICES}}
        # A selected worker was skipped, an unrelated service deployed, then a
        # documentation-only run passed. Recompute cumulatively from the full release.
        outcomes = {1: complete, 2: {'deploy': 'skipped', 'receipts': 'skipped'},
                    3: {'deploy': 'success', 'receipts (web)': 'success'},
                    4: {'deploy': 'skipped', 'receipts': 'skipped'}}
        runs = [self.run_record(number) for number in reversed(outcomes)]
        responses = {'repos/' + self.repository + '/actions/workflows/production.yml/runs?branch=main&status=success&per_page=100&page=1':
                     json.dumps({'workflow_runs': runs})}
        for run in runs:
            responses[f"repos/{self.repository}/actions/runs/{run['id']}/attempts/2/jobs?per_page=100"] = self.page_output(self.job_pages(run, outcomes[run['id']]))
        with patch.dict(os.environ, GITHUB_REPOSITORY=self.repository), \
                patch.object(subprocess, 'check_output', side_effect=lambda args, **kw: responses[args[-1]]), \
                patch.object(subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)):
            self.assertEqual(scope.production_base(self.run_record(5)['head_sha']), self.run_record(1)['head_sha'])

    def test_modern_outcome_is_authoritative_and_incomplete_job_history_is_rejected(self):
        run = self.run_record(5)
        complete = {'deploy': 'success', **{f'receipts ({service})': 'success' for service in scope.SERVICES}}
        for outcome, expected in (({'deploy': 'skipped', 'rollout-result': 'success'}, True),
                                  ({**complete, 'rollout-result': 'skipped'}, False)):
            with patch.object(subprocess, 'check_output', return_value=self.page_output(self.job_pages(run, outcome))):
                self.assertEqual(scope.proves_release(self.repository, run), expected)

        pages = self.job_pages(run, complete)
        last_job = pages[0]['jobs'].pop()
        with patch.object(subprocess, 'check_output', return_value=self.page_output(pages)):
            with self.assertRaisesRegex(RuntimeError, 'incomplete job history'):
                scope.proves_release(self.repository, run)
        pages.append({'total_count': pages[0]['total_count'], 'jobs': [last_job]})
        with patch.object(subprocess, 'check_output', return_value=self.page_output(pages)):
            self.assertTrue(scope.proves_release(self.repository, run))


class ScopeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.head = scope.git('rev-parse', 'HEAD').strip()
        imports = json.loads(subprocess.check_output(['go', 'run', 'scripts/ci-go-imports.go', cls.head],
                                                     env=dict(os.environ, GO111MODULE='off', GOTOOLCHAIN='local')))
        cls.owners = scope.dependency_owners(cls.head, imports[cls.head])

    def selected(self, path):
        return scope.select({path}, self.owners, self.owners)

    def test_each_application_and_service_has_its_own_scope(self):
        for path, expected in (
            ('web/apps/web/src/app/page.tsx', {'web'}),
            ('web/apps/loyal/src/main.tsx', {'web'}),
            ('web/apps/admin/src/main.tsx', {'admin'}),
            ('web/apps/dashboard/src/main.tsx', {'stats'}),
            ('go/workers/cmd/loyal-telegram/main.go', {'telegram'}),
            ('go/workers/cmd/loyal-observer/main.go', {'observer'}),
            ('go/workers/cmd/loyal-web/cron.go', {'web'}),
            ('go/workers/cmd/loyal-admin/main.go', {'admin'}),
            ('go/workers/cmd/loyal-stats/main.go', {'stats'}),
            ('go/workers/cmd/loyal-engine/backyard.go', {'backyard', 'earn-max', 'retail'}),
            ('go/workers/cmd/loyal-engine/retail.go', {'backyard', 'earn-max', 'retail'}),
            ('go/workers/cmd/loyal-engine/main.go', {'backyard', 'earn-max', 'retail'}),
            ('crates/loyal-yield-realtime/src/main.rs', {'realtime'}),
        ):
            with self.subTest(path=path):
                deploy, checks, _, _ = self.selected(path)
                self.assertEqual(deploy, expected)
                self.assertEqual(checks, expected)

    def test_real_shared_imports_select_their_consumers(self):
        deploy, _, _, _ = self.selected('go/workers/internal/backyard/vault.go')
        self.assertTrue({'backyard', 'earn-max', 'retail'} <= deploy)
        deploy, checks, _, _ = self.selected('go/workers/internal/chain/client.go')
        self.assertIn('web', deploy)
        self.assertIn('backyard', deploy)
        self.assertNotIn('realtime', checks)
        deploy, checks, _, _ = self.selected('web/packages/solana-rpc/src/index.ts')
        self.assertIn('web', deploy)
        self.assertIn('mobile', checks)
        self.assertNotIn('earn-max', checks)
        _, checks, _, _ = self.selected('web/packages/loyal-actions/src/index.ts')
        self.assertIn('mobile', checks)
        deploy, _, _, _ = self.selected('web/apps/loyal/src/next-compat/navigation.ts')
        self.assertTrue({'web', 'admin'} <= deploy)
        self.assertNotIn('retail', deploy)
        deploy, _, _, _ = self.selected('web/apps/loyal/vite/preset.ts')
        self.assertEqual(deploy, {'web', 'admin', 'stats'})

    def test_test_only_mobile_docs_and_pipeline_changes_do_not_deploy(self):
        for path in ('go/workers/cmd/loyal-web/main_test.go', 'web/apps/mobile/app/index.tsx',
                     'go/workers/cmd/loyal-migrate/main.go',
                     'docs/deploy.md', '.github/workflows/production.yml', 'deploy/hetzner/rollout-health.py'):
            with self.subTest(path=path):
                self.assertEqual(self.selected(path)[0], set())
        self.assertEqual(self.selected('web/apps/mobile/app/index.tsx')[1], {'mobile'})
        self.assertEqual(self.selected('docs/deploy.md')[1], set())
        self.assertTrue(self.selected('.github/workflows/production.yml')[2])
        self.assertEqual(self.selected('web/scripts/observability/check-availability.mjs')[0], set())
        self.assertEqual(self.selected('web/scripts/observability/check-availability.mjs')[1], {'journeys'})
        self.assertIn('journeys', self.selected('web/apps/admin/src/app/(admin)/journeys/page.tsx')[1])
        self.assertTrue(self.selected('deploy/hetzner/retain-web-assets.sh')[2])
        for path in ('crates/loyal-solana-env/src/lib.rs', 'crates/loyal-hub-swap-program/src/lib.rs'):
            self.assertTrue(self.selected(path)[3])

    def test_removed_dependency_is_still_an_affected_consumer(self):
        before = {'web': {'go/workers/internal/old-package'}, 'admin': set()}
        after = {'web': set(), 'admin': set()}
        self.assertEqual(scope.select({'go/workers/internal/old-package/removed.go'}, before, after)[0], {'web'})

    def test_failed_rollout_then_revert_and_rename_keep_every_changed_path(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(['git', '-C', directory, *args], stderr=subprocess.DEVNULL).decode().strip()
            git('init', '-q')
            git('config', 'user.email', 'scope-test@example.invalid')
            git('config', 'user.name', 'scope test')
            file = Path(directory) / 'worker.go'
            file.write_text('before')
            git('add', '.'); git('commit', '-qm', 'baseline'); base = git('rev-parse', 'HEAD')
            file.write_text('partially deployed')
            git('commit', '-qam', 'worker change')
            file.write_text('before')
            git('commit', '-qam', 'revert failed rollout'); reverted = git('rev-parse', 'HEAD')
            git('mv', 'worker.go', 'renamed.go'); git('commit', '-qm', 'rename'); head = git('rev-parse', 'HEAD')
            original = scope.git
            with patch.object(scope, 'git', side_effect=lambda *args: original('-C', directory, *args)):
                self.assertEqual(scope.changed_paths(base, reverted, False), set())
                self.assertEqual(scope.changed_paths(base, reverted, True), {'worker.go'})
                self.assertEqual(scope.changed_paths(base, head, True), {'worker.go', 'renamed.go'})

    def test_required_database_checks_reject_skips_and_only_select_dependencies(self):
        required = db_tests.required_tests(['./internal/webapp'])
        self.assertTrue(required)
        self.assertTrue(all(package == './internal/webapp' for package, _ in required))
        events = [{'Action': 'pass', 'Package': scope.GO_MODULE + '/internal/webapp', 'Test': test} for _, test in required]
        db_tests.verify(events, required)
        events[0]['Action'] = 'skip'
        with self.assertRaisesRegex(RuntimeError, 'skipped or did not run'):
            db_tests.verify(events, required)


if __name__ == '__main__':
    unittest.main()
