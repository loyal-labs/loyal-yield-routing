#!/usr/bin/env python3
"""Create independent Looping monitoring; dry run makes no network calls."""
import argparse
import copy
import importlib.util
import json
import os
from pathlib import Path
import sys

_module = importlib.util.spec_from_file_location('vault_alert_transport', Path(__file__).with_name('clickstack-vault-alerts.py'))
_base = importlib.util.module_from_spec(_module)
_module.loader.exec_module(_base)
Transport = _base.Transport
require = _base.require
fingerprint = _base.fingerprint
HOST = _base.HOST
DEFAULT_MANIFEST = Path(__file__).resolve().parents[1] / 'deploy/hetzner/monitoring/clickstack-looping-alerts.json'
GET = {'dashboard': 'clickstack_get_dashboard', 'alert': 'clickstack_get_alert'}
SAVE = {'dashboard': 'clickstack_save_dashboard', 'alert': 'clickstack_save_alert'}


def build_plan(spec):
    require(set(spec) == {'version', 'teamId', 'dashboard', 'alerts'}, 'Unknown manifest shape')
    require(spec['version'] == 1, 'Unsupported manifest version')
    dashboard = spec['dashboard']
    require(set(dashboard) == {'name', 'tags', 'filters', 'tiles'}, 'Unexpected dashboard fields')
    require(len(dashboard['tiles']) == 2 and len({t['id'] for t in dashboard['tiles']}) == 2, 'Expected two distinct scalar tiles')
    require(len(dashboard['tags']) > 0, 'Dashboard requires a unique identity tag')
    alerts = spec['alerts']
    require(len(alerts) == 2 and len({a['name'] for a in alerts}) == 2, 'Expected two distinct alerts')
    for tile in dashboard['tiles']:
        config = tile['config']
        require(config['configType'] == 'sql' and config['displayType'] == 'number', 'Expected scalar SQL')
        require('GROUP BY' not in config['sqlTemplate'].upper(), 'Absence metrics must aggregate without grouping')
    for alert in alerts:
        require(set(alert) == {'source', 'tileId', 'threshold', 'thresholdType', 'interval', 'channel', 'name', 'message'}, 'Unexpected alert fields')
        require(alert['source'] == 'tile' and alert['tileId'] in {t['id'] for t in dashboard['tiles']}, 'Unknown alert tile')
        require(alert['thresholdType'] == 'below' and alert['interval'] == '5m', 'Unexpected alert cadence')
        require(alert['channel'] == {'type': 'webhook', 'webhookId': '6a60b666dcc64beb0ded8d48'}, 'Notification destination drift')
    return copy.deepcopy(spec)


def canonical(kind, obj, team_id):
    require(isinstance(obj, dict) and isinstance(obj.get('id'), str) and bool(obj['id']), 'Malformed ' + kind + ' detail')
    value = copy.deepcopy(obj)
    if 'teamId' in value:
        require(value.pop('teamId') == team_id, 'Team mismatch')
    elif kind == 'alert':
        raise ValueError('Missing alert teamId')
    for key in ('id', 'createdAt', 'updatedAt', 'url'):
        value.pop(key, None)
    if kind == 'dashboard':
        for key, default in {'savedQuery': None, 'savedQueryLanguage': None, 'savedFilterValues': [], 'containers': []}.items():
            require(value.pop(key, default) == default, 'Dashboard contains unexpected ' + key)
    else:
        for key in ('state', 'executionErrors', 'history'):
            value.pop(key, None)
        for key in ('note', 'numConsecutiveWindows', 'savedSearchId', 'groupBy', 'thresholdMax', 'scheduleStartAt'):
            require(value.pop(key, None) in (None, ''), 'Alert contains unexpected ' + key)
        require(value.pop('scheduleOffsetMinutes', 0) in (None, 0), 'Alert schedule offset differs')
    return value



def dashboard_tile_map(expected, obj, team_id):
    actual = canonical('dashboard', obj, team_id)
    tiles = actual.get('tiles')
    require(isinstance(tiles, list) and len(tiles) == len(expected['tiles']), 'Conflicting existing dashboard tiles')
    require(all(isinstance(t, dict) and isinstance(t.get('id'), str) and t['id'] and isinstance(t.get('name'), str) for t in tiles), 'Malformed dashboard tile identity')
    require(len({t['id'] for t in tiles}) == len(tiles) and len({t['name'] for t in tiles}) == len(tiles), 'Ambiguous dashboard tile identity')
    require(len({t['name'] for t in expected['tiles']}) == len(expected['tiles']), 'Ambiguous manifest tile names')
    by_name = {t['name']: t for t in tiles}
    mapping = {}
    normalized = []
    for wanted in expected['tiles']:
        require(wanted['name'] in by_name, 'Conflicting existing dashboard tile name')
        tile = copy.deepcopy(by_name[wanted['name']])
        mapping[wanted['id']] = tile['id']
        tile['id'] = wanted['id']
        require(tile == wanted, 'Conflicting existing dashboard tile configuration or layout')
        normalized.append(tile)
    actual['tiles'] = normalized
    require(actual == expected, 'Conflicting existing dashboard; no updates are allowed')
    return mapping


def find_existing(kind, expected, transport, team_id):
    rows = transport.call(GET[kind], {})
    require(isinstance(rows, list), 'Expected complete ' + kind + ' inventory list')
    require(all(isinstance(row, dict) and isinstance(row.get('id'), str) and isinstance(row.get('name'), str) for row in rows), 'Malformed inventory row')
    require(len({row['id'] for row in rows}) == len(rows), 'Duplicate inventory IDs')
    if kind == 'dashboard':
        candidates = [row for row in rows if row['name'] == expected['name'] or set(row.get('tags', [])) & set(expected['tags'])]
    else:
        # Summaries omit tile IDs, so inspect each alert to catch renamed collisions.
        details = [transport.call(GET[kind], {'id': row['id']}) for row in rows]
        require(all(isinstance(obj, dict) and obj.get('id') == row['id'] for row, obj in zip(rows, details)), 'Inventory/detail identity mismatch')
        candidates = [obj for obj in details if obj.get('name') == expected['name'] or obj.get('tileId') == expected['tileId']]
    require(len(candidates) <= 1, 'Ambiguous duplicate ' + kind + ' identity')
    if not candidates:
        return None
    obj = transport.call(GET[kind], {'id': candidates[0]['id']})
    require(obj.get('id') == candidates[0]['id'], 'Detail identity mismatch')
    if kind == 'dashboard':
        dashboard_tile_map(expected, obj, team_id)
    else:
        require(canonical(kind, obj, team_id) == expected, 'Conflicting existing ' + kind + '; no updates are allowed')
    return obj


def apply_plan(plan, transport, journal_path):
    journal_path = Path(journal_path)
    journal = {'version': 1, 'endpoint': HOST, 'manifestFingerprint': fingerprint(plan), 'status': 'preflight', 'completed': []}
    # Exclusive creation preserves previous attempts, including lost-response evidence.
    with journal_path.open('x') as stream:
        stream.write(json.dumps(journal, indent=2) + '\n')
        stream.flush()
        os.fsync(stream.fileno())

    def record():
        with journal_path.open('w') as stream:
            stream.write(json.dumps(journal, indent=2) + '\n')
            stream.flush()
            os.fsync(stream.fileno())

    def ensure(kind, expected):
        ident = find_existing(kind, expected, transport, plan['teamId'])
        if ident:
            journal['completed'].append({'kind': kind, 'id': ident['id'], 'reused': True})
            record()
            return ident
        # Check inventory once more immediately before the intent/write pair.
        ident = find_existing(kind, expected, transport, plan['teamId'])
        if ident:
            journal['completed'].append({'kind': kind, 'id': ident['id'], 'reused': True})
            record()
            return ident
        journal.update(status='writing', in_flight={'name': SAVE[kind], 'arguments': expected})
        record()
        transport.call(SAVE[kind], expected)  # Discover IDs by readback, never assume save response shape.
        ident = find_existing(kind, expected, transport, plan['teamId'])
        require(ident is not None, 'Created ' + kind + ' absent from readback; inspect before rerun')
        journal['completed'].append({'kind': kind, 'id': ident['id'], 'reused': False})
        journal.pop('in_flight', None)
        record()
        return ident

    try:
        dashboard = ensure('dashboard', plan['dashboard'])
        dashboard_id = dashboard['id']
        tile_ids = dashboard_tile_map(plan['dashboard'], dashboard, plan['teamId'])
        journal['tileIds'] = tile_ids
        record()
        for alert in plan['alerts']:
            # Recheck semantics and server-assigned tile identity before dependent writes.
            current = find_existing('dashboard', plan['dashboard'], transport, plan['teamId'])
            require(current is not None and current['id'] == dashboard_id and dashboard_tile_map(plan['dashboard'], current, plan['teamId']) == tile_ids, 'Dashboard changed before alert creation')
            ensure('alert', dict(alert, dashboardId=dashboard_id, tileId=tile_ids[alert['tileId']]))
        journal['status'] = 'complete; run query_tile on both scalar tiles for live SQL validation'
        record()
    except Exception:
        journal['status'] = 'incomplete; inspect in_flight; rerun with a fresh journal to recover by inventory'
        record()
        raise
    return journal


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', type=Path, default=DEFAULT_MANIFEST)
    parser.add_argument('--output', type=Path, help='Optional fresh local plan file')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--journal', type=Path, help='Required fresh file for --apply; never overwrite earlier evidence')
    args = parser.parse_args(argv)
    require(not args.apply or args.journal is not None, '--apply requires --journal')
    require(not args.apply or not args.journal.exists(), 'Journal exists; choose a fresh path')
    require(args.output is None or args.journal is None or args.output.resolve() != args.journal.resolve(), 'Plan and journal paths must differ')
    plan = build_plan(json.loads(args.manifest.read_text()))
    if args.output:
        with args.output.open('x') as stream:
            stream.write(json.dumps(plan, indent=2) + '\n')
    if args.apply:
        journal = apply_plan(plan, Transport(), args.journal)
        print(json.dumps({'status': journal['status'], 'objects': journal['completed'], 'tileIds': journal['tileIds']}, indent=2))
    else:
        print(json.dumps({'mode': 'dry-run; no network calls', 'plan': plan}, indent=2))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError) as error:
        print('Aborted: ' + str(error), file=sys.stderr)
        sys.exit(1)
