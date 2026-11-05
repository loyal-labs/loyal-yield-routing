#!/usr/bin/env python3
import copy
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

MODULE = importlib.util.spec_from_file_location('looping_alerts', Path(__file__).with_name('clickstack-looping-alerts.py'))
repair = importlib.util.module_from_spec(MODULE)
MODULE.loader.exec_module(repair)
SPEC = json.loads(repair.DEFAULT_MANIFEST.read_text())
FIXTURES = Path(__file__).resolve().parent / 'fixtures'
SCHEMAS = FIXTURES / 'clickstack-save-schemas.json'


def validate(value, schema, root):
    """Validate the schema keywords reachable from these concrete create payloads."""
    if '$ref' in schema:
        target = root
        for key in schema['$ref'].split('/')[1:]:
            target = target[int(key)] if isinstance(target, list) else target[key.replace('~1', '/').replace('~0', '~')]
        validate(value, target, root)
        return
    if 'anyOf' in schema:
        for option in schema['anyOf']:
            try:
                validate(value, option, root)
                return
            except AssertionError:
                pass
        raise AssertionError('No anyOf schema matched')
    allowed = {'type', 'const', 'enum', 'properties', 'required', 'additionalProperties', 'items', 'minItems', 'maxItems', 'minLength', 'maxLength', 'minimum', 'maximum', 'description', 'title', '$schema', 'format', 'default'}
    assert not set(schema) - allowed, 'Unsupported validation keywords: ' + str(set(schema) - allowed)
    types = {'object': dict, 'array': list, 'string': str, 'number': (int, float), 'integer': int, 'boolean': bool, 'null': type(None)}
    if 'type' in schema:
        assert isinstance(value, types[schema['type']]), 'Wrong type'
    if 'const' in schema:
        assert value == schema['const']
    if 'enum' in schema:
        assert value in schema['enum']
    for key, operation in [('minLength', lambda x, y: len(x) >= y), ('maxLength', lambda x, y: len(x) <= y), ('minItems', lambda x, y: len(x) >= y), ('maxItems', lambda x, y: len(x) <= y), ('minimum', lambda x, y: x >= y), ('maximum', lambda x, y: x <= y)]:
        if key in schema:
            assert operation(value, schema[key]), key
    if isinstance(value, dict):
        properties = schema.get('properties', {})
        assert not set(schema.get('required', [])) - set(value), 'Missing required field'
        if schema.get('additionalProperties') is False:
            assert not set(value) - set(properties), 'Unknown field'
        for key, item in value.items():
            if key in properties:
                validate(item, properties[key], root)
    if isinstance(value, list) and 'items' in schema:
        for item in value:
            validate(item, schema['items'], root)


class FakeServer:
    """Snapshot-shaped inventories/details over the actual JSON-RPC transport."""
    def __init__(self):
        self.objects = {'dashboard': {}, 'alert': {}}
        self.writes = []
        self.calls = []
        self.lose_response = None
        self.duplicate_on_create = False
        self.before_write = None
        self.transport = repair.Transport.__new__(repair.Transport)
        self.transport.key = 'fake-key-for-test-only'

    def call(self, name, args):
        self.calls.append((name, copy.deepcopy(args)))
        if name in repair.GET.values():
            kind = next(k for k, v in repair.GET.items() if v == name)
            if 'id' in args:
                return copy.deepcopy(self.objects[kind][args['id']])
            return [{k: copy.deepcopy(obj[k]) for k in ('id', 'name', 'tags') if k in obj} for obj in self.objects[kind].values()]
        kind = next(k for k, v in repair.SAVE.items() if v == name)
        assert 'id' not in args, 'Updates forbidden'
        if self.before_write:
            self.before_write(name, args)
        self.writes.append((name, copy.deepcopy(args)))
        ident = ('d' if kind == 'dashboard' else 'a') + str(len(self.objects[kind]) + 1)
        obj = copy.deepcopy(args)
        obj.update(id=ident, url='https://example.invalid/object')
        if kind == 'dashboard':
            for index, tile in enumerate(obj['tiles']):
                tile['id'] = ident + '-server-tile-' + str(index)
            obj.update(savedQuery=None, savedQueryLanguage=None, savedFilterValues=[])
        else:
            obj.update(teamId=SPEC['teamId'], note=None, numConsecutiveWindows=None, state='OK', executionErrors=[], history=[], createdAt='before', updatedAt='after')
        self.objects[kind][ident] = obj
        if self.duplicate_on_create:
            self.objects[kind][ident + '-duplicate'] = dict(copy.deepcopy(obj), id=ident + '-duplicate')
        if self.lose_response == kind:
            self.lose_response = None
            raise urllib.error.URLError('simulated lost response')
        # Response is intentionally opaque: create readback must discover the ID.
        return {'success': True}

    def open(self, request, timeout):
        assert request.get_header('Authorization') == 'Bearer fake-key-for-test-only'
        payload = json.loads(request.data)
        result = self.call(payload['params']['name'], payload['params']['arguments'])
        packet = {'jsonrpc': '2.0', 'id': payload['id'], 'result': {'content': [{'type': 'text', 'text': json.dumps(result)}]}}
        return io.BytesIO(('event: message\ndata: ' + json.dumps(packet) + '\n\n').encode())


class LoopingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.plan = repair.build_plan(SPEC)
        self.server = FakeServer()
        self.patch = patch.object(repair._base.urllib.request, 'build_opener', return_value=self.server)
        self.patch.start()
        self.addCleanup(self.patch.stop)

    def apply(self, name='journal.json'):
        return repair.apply_plan(self.plan, self.server.transport, self.root / name)

    def test_dryrun_constructs_no_transport_or_files(self):
        with patch.object(repair, 'Transport', side_effect=AssertionError('network')), patch('sys.stdout', new=io.StringIO()):
            self.assertEqual(repair.main([]), 0)
        self.assertEqual(list(self.root.iterdir()), [])
        self.assertEqual(self.server.calls, [])

    def test_create_then_already_exists_reuses_exact_objects(self):
        first = self.apply()
        self.assertEqual(len(self.server.writes), 3)
        second = self.apply('rerun.json')
        self.assertEqual(len(self.server.writes), 3)
        self.assertTrue(all(x['reused'] for x in second['completed']))
        self.assertEqual([x['id'] for x in first['completed']], [x['id'] for x in second['completed']])
        self.assertEqual([len(self.server.objects[k]) for k in ('dashboard', 'alert')], [1, 2])

    def test_intent_is_durable_before_every_write(self):
        def check(name, args):
            journal = json.loads((self.root / 'journal.json').read_text())
            self.assertEqual(journal['in_flight'], {'name': name, 'arguments': args})
            self.assertEqual(journal['status'], 'writing')
        self.server.before_write = check
        self.apply()

    def test_lost_response_on_each_kind_recovers_without_duplicate(self):
        for kind in ('dashboard', 'alert'):
            with self.subTest(kind=kind):
                self.server.objects = {'dashboard': {}, 'alert': {}}
                self.server.writes = []
                self.server.lose_response = kind
                path = kind + '-lost.json'
                with self.assertRaisesRegex(ValueError, 'request failed'):
                    self.apply(path)
                journal = json.loads((self.root / path).read_text())
                self.assertIn('in_flight', journal)
                self.assertNotIn('fake-key-for-test-only', json.dumps(journal))
                self.apply(kind + '-recovered.json')
                self.assertEqual(len(self.server.writes), 3)

    def test_conflicting_names_or_tags_fail_closed(self):
        self.apply()
        for kind in ('dashboard', 'alert'):
            with self.subTest(kind=kind):
                objects = copy.deepcopy(self.server.objects)
                obj = next(iter(self.server.objects[kind].values()))
                if kind == 'dashboard':
                    obj['tiles'][0]['config']['sqlTemplate'] = 'conflict'
                else:
                    obj['threshold'] = 999
                with self.assertRaisesRegex(ValueError, 'Conflicting'):
                    self.apply(kind + '-conflict.json')
                self.assertEqual(len(self.server.writes), 3)
                self.server.objects = objects
        self.server.objects['dashboard']['d1']['name'] = 'Renamed dashboard'
        with self.assertRaisesRegex(ValueError, 'Conflicting'):
            self.apply('renamed.json')

    def test_duplicate_identity_fails_closed_before_create(self):
        self.apply()
        self.server.objects['alert']['duplicate'] = dict(copy.deepcopy(self.server.objects['alert']['a1']), id='duplicate')
        with self.assertRaisesRegex(ValueError, 'Ambiguous'):
            self.apply('duplicates.json')
        self.assertEqual(len(self.server.writes), 3)

    def test_duplicate_created_concurrently_is_detected(self):
        self.server.duplicate_on_create = True
        with self.assertRaisesRegex(ValueError, 'Ambiguous'):
            self.apply()
        self.assertEqual(len(self.server.writes), 1)
        with self.assertRaisesRegex(ValueError, 'Ambiguous'):
            self.apply('retry.json')
        self.assertEqual(len(self.server.writes), 1)

    def test_existing_journal_is_never_overwritten(self):
        path = self.root / 'journal.json'
        path.write_text('preserve earlier evidence')
        with self.assertRaises(FileExistsError):
            self.apply()
        self.assertEqual(path.read_text(), 'preserve earlier evidence')
        self.assertEqual(self.server.calls, [])

    def test_transport_disables_redirects_and_suppresses_errors(self):
        handlers = []
        def opener(handler):
            handlers.append(handler)
            return self.server
        with patch.object(repair._base.urllib.request, 'build_opener', side_effect=opener):
            self.server.transport.call(repair.GET['dashboard'], {})
        self.assertIsNone(handlers[0]().redirect_request(None, None, 302, '', {}, 'https://other.invalid'))
        with patch.object(self.server, 'open', return_value=io.BytesIO(json.dumps({'jsonrpc': '2.0', 'id': 1, 'result': {'isError': True, 'content': [{'type': 'text', 'text': 'fake-key-for-test-only'}]}}).encode())):
            with self.assertRaisesRegex(ValueError, '^ClickStack tool error \\(response suppressed\\)$'):
                self.server.transport.call(repair.GET['dashboard'], {})

    def test_actual_catalog_validates_all_payloads_and_rejects_bad_id(self):
        schemas = json.loads(SCHEMAS.read_text())
        self.apply()
        for name, args in self.server.writes:
            validate(args, schemas[name], schemas[name])
        bad = copy.deepcopy(self.plan['dashboard'])
        bad['tiles'][0]['id'] = 'x' * 37
        with self.assertRaises(AssertionError):
            validate(bad, schemas[repair.SAVE['dashboard']], schemas[repair.SAVE['dashboard']])

    def test_real_captured_dashboard_is_reused_and_alerts_use_actual_tile_ids(self):
        capture = FIXTURES / 'clickstack-looping-created.json'
        actual = json.loads(capture.read_text())['response']
        # The capture predates the product-name correction; preserve its raw provenance.
        self.plan['dashboard']['name'] = actual['name']
        self.server.objects['dashboard'][actual['id']] = actual
        journal = self.apply()
        self.assertEqual([name for name, args in self.server.writes], [repair.SAVE['alert']] * 2)
        expected_ids = {tile['id'] for tile in actual['tiles']}
        self.assertEqual({args['tileId'] for name, args in self.server.writes}, expected_ids)
        self.assertEqual(set(journal['tileIds'].values()), expected_ids)
        self.assertEqual(self.server.objects['dashboard'][actual['id']], actual)
        self.apply('rerun-captured.json')
        self.assertEqual(len(self.server.writes), 2)

    def test_existing_alert_with_wrong_actual_tile_fails_closed(self):
        self.apply()
        self.server.objects['alert']['a1']['tileId'] = 'wrong-server-tile'
        with self.assertRaisesRegex(ValueError, 'Conflicting'):
            self.apply('wrong-tile.json')
        self.assertEqual(len(self.server.writes), 3)

    def test_duplicate_tile_names_or_changed_layout_fail_closed(self):
        self.apply()
        original = copy.deepcopy(self.server.objects)
        for mutation in ('duplicate-name', 'layout'):
            self.server.objects = copy.deepcopy(original)
            tiles = self.server.objects['dashboard']['d1']['tiles']
            if mutation == 'duplicate-name':
                tiles[1]['name'] = tiles[0]['name']
            else:
                tiles[0]['x'] = 4
            with self.assertRaises(ValueError):
                self.apply(mutation + '.json')
            self.assertEqual(len(self.server.writes), 3)

    def test_nav_sql_is_exact_rwa_query_with_vault_substitution(self):
        rwa = json.loads(repair.DEFAULT_MANIFEST.with_name('clickstack-vault-alerts.json').read_text())
        self.assertEqual(self.plan['dashboard']['tiles'][1]['config'], dict(rwa['navConfig'], sqlTemplate=rwa['navConfig']['sqlTemplate'].replace("= 'rwa'", "= 'looping'")))


if __name__ == '__main__':
    unittest.main()
