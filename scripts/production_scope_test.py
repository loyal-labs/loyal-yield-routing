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
