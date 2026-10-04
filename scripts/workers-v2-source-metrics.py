#!/usr/bin/env python3
"""Count physical non-test Go source; report only, never a correctness gate."""
import json
from collections import defaultdict
from pathlib import Path

root = Path(__file__).resolve().parents[1]
packages = defaultdict(lambda: {'files': 0, 'physical_lines': 0})
for path in sorted((root / 'go/workers/internal').rglob('*.go')):
    if path.name.endswith('_test.go'):
        continue
    package = path.relative_to(root / 'go/workers/internal').parts[0]
    packages[package]['files'] += 1
    packages[package]['physical_lines'] += len(path.read_text().splitlines())
print(json.dumps({
    'scope': 'go/workers/internal, non-test Go; includes comments and SQL',
    'packages': dict(sorted(packages.items())),
    'total_files': sum(p['files'] for p in packages.values()),
    'total_physical_lines': sum(p['physical_lines'] for p in packages.values()),
}, indent=2))
