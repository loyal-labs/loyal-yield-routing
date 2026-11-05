#!/usr/bin/env python3
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

MODULE = importlib.util.spec_from_file_location('vault_alerts', Path(__file__).with_name('clickstack-vault-alerts.py'))
repair = importlib.util.module_from_spec(MODULE)
MODULE.loader.exec_module(repair)
SPEC = json.loads(repair.DEFAULT_MANIFEST.read_text())


def fixtures():
    objects = {}
    inventories = {'search': [], 'alert': []}
    for rule in SPEC['searches']:
        obj = {'id': rule['id'], 'name': 'Original search ' + rule['id'], 'where': "ServiceName = 'backyard-rwa-worker'", 'whereLanguage': 'sql', 'sourceId': SPEC['sourceId'], 'teamId': SPEC['teamId'], 'filters': [], 'select': 'Timestamp, Body', 'tags': ['existing'], 'updatedAt': 'before'}
        objects['search', obj['id']] = obj
        inventories['search'].append({'id': obj['id'], 'name': obj['name']})
    for rule in SPEC['alerts']:
        obj = dict(rule['policy'], id=rule['id'], name='Original alert', message='Old Render message', teamId=SPEC['teamId'], channel={'type': 'webhook', 'webhookId': 'existing-only'}, state='ALERT', history=[], updatedAt='evaluation-time', note=None, numConsecutiveWindows=None)
        objects['alert', obj['id']] = obj
        inventories['alert'].append({'id': obj['id'], 'name': obj['name']})
    config = copy.deepcopy(SPEC['navConfig'])
    config['sqlTemplate'] = 'old query'
    objects['dashboard', SPEC['dashboardId']] = {'id': SPEC['dashboardId'], 'name': 'Keep dashboard title', 'tags': ['keep'], 'filters': [], 'tiles': [{'id': SPEC['tileId'], 'name': 'Keep tile title', 'x': 0, 'y': 0, 'w': 12, 'h': 3, 'config': config}, {'id': 'unrelated', 'name': 'Unrelated', 'config': {'displayType': 'markdown', 'markdown': 'Keep me'}}]}
    return objects, inventories


class FakeTransport:
    def __init__(self, objects):
        self.objects = copy.deepcopy(objects)
        self.writes = []

    def call(self, name, args):
        if name in repair.GET.values():
            kind = next(k for k, v in repair.GET.items() if v == name)
            return copy.deepcopy(self.objects[kind, args['id']])
        self.writes.append((name, copy.deepcopy(args)))
        if name == 'clickstack_patch_dashboard':
            dashboard = self.objects['dashboard', args['dashboardId']]
            tile = next(t for t in dashboard['tiles'] if t['id'] == args['tileId'])
            tile.update(copy.deepcopy(args['tile']))
        else:
            kind = 'search' if name == 'clickstack_save_saved_search' else 'alert'
            self.objects[kind, args['id']].update(copy.deepcopy(args))
        return {'success': True}


class RepairTests(unittest.TestCase):
    def setUp(self):
        self.objects, self.inventories = fixtures()
        self.plan = repair.build_plan(self.objects, self.inventories, SPEC)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_dry_run_has_no_transport_and_no_remote_writes(self):
        for (kind, ident), obj in self.objects.items():
            path = self.root / (kind + '-' + ident + '.json')
            path.write_text(json.dumps({'request': {'name': repair.GET[kind], 'arguments': {'id': ident}}, 'response': obj}))
        for kind, items in self.inventories.items():
            (self.root / (kind + '-list.json')).write_text(json.dumps({'request': {'name': repair.GET[kind], 'arguments': {}}, 'response': items}))
        out = self.root / 'plan.json'
        with patch.object(repair, 'Transport', side_effect=AssertionError('dry-run attempted network')):
            self.assertEqual(repair.main(['--snapshots', str(self.root), '--output', str(out)]), 0)
        evidence = json.loads(out.read_text())
        self.assertEqual(len(evidence['operations']), 9)
        self.assertEqual(evidence['rollback_order'][0], evidence['operations'][-1]['rollback'])

    def test_apply_repeat_is_idempotent_and_preserves_unrelated_state(self):
        fake = FakeTransport(self.objects)
        dashboard_before = copy.deepcopy(fake.objects['dashboard', SPEC['dashboardId']])
        self.assertEqual(repair.apply_plan(self.plan, fake, self.root / 'journal1.json'), 9)
        self.assertEqual(repair.apply_plan(self.plan, fake, self.root / 'journal2.json'), 0)
        self.assertEqual(len(fake.writes), 9)
        dashboard_after = fake.objects['dashboard', SPEC['dashboardId']]
        self.assertEqual(dashboard_before['tiles'][1], dashboard_after['tiles'][1])
        self.assertEqual(dashboard_before['name'], dashboard_after['name'])
        for rule in SPEC['alerts']:
            alert = fake.objects['alert', rule['id']]
            self.assertEqual(alert['channel'], {'type': 'webhook', 'webhookId': 'existing-only'})
            for field, expected in rule['policy'].items():
                self.assertEqual(alert[field], expected)
        rebuilt = repair.build_plan(fake.objects, self.inventories, SPEC)
        self.assertEqual(rebuilt['operations'], [])

    def test_preflight_drift_writes_nothing(self):
        fake = FakeTransport(self.objects)
        fake.objects['alert', SPEC['alerts'][-1]['id']]['channel']['webhookId'] = 'concurrent-change'
        with self.assertRaisesRegex(ValueError, 'drift before apply'):
            repair.apply_plan(self.plan, fake, self.root / 'drift.json')
        self.assertEqual(fake.writes, [])

    def test_drift_after_preflight_is_caught(self):
        fake = FakeTransport(self.objects)
        original = fake.call
        reads = 0
        def call(name, args):
            nonlocal reads
            if name in repair.GET.values():
                reads += 1
                if reads == 10:
                    fake.objects['search', args['id']]['tags'] = ['concurrent']
            return original(name, args)
        fake.call = call
        with self.assertRaisesRegex(ValueError, 'immediately before write'):
            repair.apply_plan(self.plan, fake, self.root / 'drift2.json')
        self.assertEqual(fake.writes, [])

    def test_alert_evaluations_do_not_create_false_config_drift(self):
        fake = FakeTransport(self.objects)
        for (kind, ident), obj in fake.objects.items():
            if kind == 'alert':
                obj.update(updatedAt='later-evaluation', state='OK', history=[{'counts': 0}])
        self.assertEqual(repair.apply_plan(self.plan, fake, self.root / 'evaluation.json'), 9)

    def test_unknown_shapes_and_policy_changes_abort(self):
        for mutate in [lambda o: o['search', SPEC['searches'][0]['id']].update(extra='unknown'), lambda o: o['alert', SPEC['alerts'][0]['id']].update(threshold=99), lambda o: o['search', SPEC['searches'][0]['id']].update(teamId='other-team'), lambda o: o['search', SPEC['searches'][0]['id']].update(filters=[{'type': 'sql', 'condition': 'obsolete'}])]:
            with self.subTest(mutate=mutate):
                objects = copy.deepcopy(self.objects)
                mutate(objects)
                with self.assertRaises(ValueError):
                    repair.build_plan(objects, self.inventories, SPEC)

    def test_rollback_payloads_restore_config(self):
        fake = FakeTransport(self.objects)
        repair.apply_plan(self.plan, fake, self.root / 'applied.json')
        for op in reversed(self.plan['operations']):
            # Operators must check current == after before invoking each rollback.
            self.assertEqual(repair.projection(op['kind'], fake.objects[op['kind'], op['id']]), op['after'])
            fake.call(op['rollback']['name'], op['rollback']['arguments'])
        for key, before in self.objects.items():
            self.assertEqual(repair.projection(key[0], fake.objects[key]), repair.projection(key[0], before))

    def test_failed_readback_keeps_inflight_rollback_evidence(self):
        fake = FakeTransport(self.objects)
        original = fake.call
        def call(name, args):
            if name not in repair.GET.values():
                raise ValueError('simulated ambiguous write')
            return original(name, args)
        fake.call = call
        path = self.root / 'failed.json'
        with self.assertRaisesRegex(ValueError, 'ambiguous'):
            repair.apply_plan(self.plan, fake, path)
        journal = json.loads(path.read_text())
        self.assertIn('in_flight', journal)
        self.assertIn('rollback', journal['in_flight'])
        self.assertEqual(journal['completed'], [])


if __name__ == '__main__':
    unittest.main()
