"""Synthetic comparator controls, not planner evidence."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('gate', Path(__file__).with_name('compare-fleet-decisions.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class DecisionComparisonTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = self.root/'fixture.json'
        self.fixture.write_text(json.dumps({'cases': [{'name': 'fixture'}]}))
        self.digest = hashlib.sha256(self.fixture.read_bytes()).hexdigest()
        self.decision = {'vaultId': 1, 'source': 'a', 'target': 'b', 'route': 'same_mint',
                         'amount': 1, 'sourceApy': 1, 'targetApy': 2, 'edge': 1,
                         'netGain': 1, 'priority': 1, 'feeCap': 1}
        self.rust = {'schemaVersion': 1, 'implementation': 'rust', 'fixtureSha256': self.digest,
                     'cases': [{'name': 'fixture', 'selected': [self.decision]}]}
        self.go = copy.deepcopy(self.rust)
        self.go['implementation'] = 'go'

    def compare(self):
        for name, artifact in [('rust', self.rust), ('go', self.go)]:
            (self.root/(name+'.json')).write_text(json.dumps(artifact))
        return gate.compare(self.fixture, self.root/'rust.json', self.root/'go.json')

    def test_equal_and_every_decision_field_matters(self):
        self.assertEqual(self.compare()['mismatchingCases'], 0)
        baseline = copy.deepcopy(self.go)
        for key, value in self.decision.items():
            self.go = copy.deepcopy(baseline)
            replacement = value+1 if type(value) is int else value+'-different'
            if key == 'route': replacement = 'cross_mint_jupiter'
            self.go['cases'][0]['selected'][0][key] = replacement
            self.assertEqual(self.compare()['mismatchingCases'], 1, key)

    def test_order_and_missing_selection(self):
        self.go['cases'][0]['selected'] = []
        self.assertEqual(self.compare()['mismatchingCases'], 1)
        second = {**self.decision, 'vaultId': 2}
        self.go['cases'][0]['selected'] = [second, self.decision]
        self.rust['cases'][0]['selected'].append(second)
        self.assertEqual(self.compare()['mismatchingCases'], 1)

    def test_incomplete_or_stale_artifacts_fail_closed(self):
        baseline = copy.deepcopy(self.go)
        for change in (lambda a: a.update(fixtureSha256='stale'),
                       lambda a: a.update(implementation='rust'),
                       lambda a: a.update(schemaVersion=True),
                       lambda a: a.update(cases=[]),
                       lambda a: a['cases'].append(a['cases'][0]),
                       lambda a: a['cases'][0]['selected'].append(a['cases'][0]['selected'][0]),
                       lambda a: a.update(extra='untrusted'),
                       lambda a: a['cases'][0]['selected'][0].update(feeCap=True)):
            self.go = copy.deepcopy(baseline)
            change(self.go)
            with self.assertRaises(ValueError): self.compare()
        with self.assertRaises(ValueError): gate.load_json('{"x":1,"x":2}')

    def test_idle_cannot_invent_a_reserve_or_source_yield(self):
        for artifact in (self.rust, self.go):
            artifact['cases'][0]['selected'][0].update(route='idle_vault_deposit', source='', sourceApy=0)
        self.assertEqual(self.compare()['mismatchingCases'], 0)
        for change in ({'source': 'fabricated'}, {'sourceApy': 1}):
            baseline = copy.deepcopy(self.go)
            self.go['cases'][0]['selected'][0].update(change)
            with self.assertRaises(ValueError): self.compare()
            self.go = baseline

    def test_fixture_contains_all_mint_pairs_and_unique_cases(self):
        cases = gate.fixtures()['cases']
        names = [c['name'] for c in cases]
        self.assertEqual(len(names), len(set(names)))
        for source in gate.STABLES:
            for target in gate.STABLES:
                self.assertIn(f'mint-{source}-to-{target}', names)
            self.assertIn(f'idle-{source}-9000000000', names)
        self.assertIn('idle-and-reserve-shared-capacity', names)
        self.assertIn('same-vault-idle-wins', names)
        self.assertIn('same-vault-reserve-wins', names)


if __name__ == '__main__':
    unittest.main()
