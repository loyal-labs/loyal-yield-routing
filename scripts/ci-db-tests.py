#!/usr/bin/env python3
"""Prove the selected services' required database regressions actually ran."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def required_tests(packages):
    required = json.loads(Path(__file__).with_name('ci-db-tests.json').read_text())
    return {(package, test) for package, tests in required.items()
            if package in packages or './...' in packages for test in tests}


def verify(events, expected):
    passed = {(event['Package'].replace('github.com/loyal-labs/loyal-yield-routing/go/workers/', './'), event['Test'])
              for event in events if event.get('Action') == 'pass' and event.get('Test')}
    missing = expected - passed
    if missing:
        raise RuntimeError('required database tests failed, skipped or did not run: ' + ', '.join(test for _, test in sorted(missing)))


if __name__ == '__main__':
    packages = json.loads(os.environ.get('WORKERS_V2_PACKAGES', '["./..."]'))
    expected = required_tests(packages)
    if not expected:
        print('No database regression gates belong to the selected packages.')
        sys.exit(0)
    with tempfile.NamedTemporaryFile(prefix='loyal-db-tests-', suffix='.jsonl', delete=False) as log:
        result = subprocess.run(['go', 'test', '-json', '-p', '1', '-count=1', '-run',
                                 '^(' + '|'.join(sorted({test for _, test in expected})) + ')$',
                                 *sorted({package for package, _ in expected})], stdout=log, stderr=subprocess.STDOUT)
    events = []
    for line in Path(log.name).read_text().splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            pass
    if result.returncode:
        print(f'Database regression command failed; full log: {log.name}', file=sys.stderr)
        for event in events:
            if event.get('Action') == 'fail':
                print(event.get('Package'), event.get('Test', ''), file=sys.stderr)
        sys.exit(result.returncode)
    verify(events, expected)
    print(f'{len(expected)} required database regressions passed without skips.')
    Path(log.name).unlink()
