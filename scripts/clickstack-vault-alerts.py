#!/usr/bin/env python3
"""Repair existing RWA monitors from reviewed snapshots; no network unless --apply."""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request

HOST = 'https://observability.5.161.121.229.sslip.io/api/mcp'
DEFAULT_MANIFEST = Path(__file__).resolve().parents[1] / 'deploy/hetzner/monitoring/clickstack-vault-alerts.json'
SEARCH_FIELDS = {'id', 'name', 'select', 'where', 'whereLanguage', 'orderBy', 'sourceId', 'tags', 'filters'}
ALERT_FIELDS = {'id', 'source', 'savedSearchId', 'dashboardId', 'tileId', 'groupBy', 'threshold', 'thresholdType', 'thresholdMax', 'interval', 'scheduleOffsetMinutes', 'scheduleStartAt', 'channel', 'name', 'message'}
META = {'teamId', 'createdAt', 'updatedAt', 'url'}
ALERT_META = {'state', 'executionErrors', 'history', 'note', 'numConsecutiveWindows'}
DASH_FIELDS = {'id', 'name', 'tiles', 'tags', 'filters', 'savedQuery', 'savedQueryLanguage', 'savedFilterValues'}
TILE_FIELDS = {'id', 'name', 'x', 'y', 'w', 'h', 'containerId', 'tabId', 'config'}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def projection(kind, obj):
    require(isinstance(obj, dict), 'Expected full object for ' + kind)
    fields = {'search': SEARCH_FIELDS, 'alert': ALERT_FIELDS, 'dashboard': DASH_FIELDS}[kind]
    known = fields | META | (ALERT_META if kind == 'alert' else set())
    require(not set(obj) - known, 'Unknown ' + kind + ' fields: ' + ', '.join(sorted(set(obj) - known)))
    if kind == 'alert':
        require(obj.get('note') is None and obj.get('numConsecutiveWindows') is None,
                'Alert has policy fields not supported by save API')
    # Alert updatedAt advances on evaluation; compare all configuration instead.
    return {k: copy.deepcopy(v) for k, v in obj.items() if k not in {'createdAt', 'updatedAt', 'url'} | ALERT_META}


def save_args(kind, obj):
    fields = SEARCH_FIELDS if kind == 'search' else ALERT_FIELDS
    return {k: copy.deepcopy(v) for k, v in obj.items() if k in fields}


def load_snapshots(directory):
    result = {}
    lists = {}
    names = {'clickstack_get_saved_search': 'search', 'clickstack_get_alert': 'alert', 'clickstack_get_dashboard': 'dashboard'}
    for path in sorted(Path(directory).glob('*.json')):
        value = json.loads(path.read_text())
        if not isinstance(value, dict):
            continue
        request = value.get('request', {})
        kind = names.get(request.get('name'))
        if not kind:
            continue
        obj = value.get('response')
        ident = request.get('arguments', {}).get('id')
        if ident:
            require(isinstance(obj, dict) and obj.get('id') == ident, 'Malformed detail snapshot: ' + path.name)
            key = (kind, ident)
            require(key not in result, 'Duplicate detail snapshots for ' + ident)
            result[key] = obj
        else:
            require(isinstance(obj, list), 'Expected inventory list: ' + path.name)
            require(kind not in lists, 'Duplicate inventory snapshots for ' + kind)
            lists[kind] = obj
    return result, lists


def build_plan(objects, inventories, spec):
    require(set(spec) == {'teamId', 'sourceId', 'connectionId', 'dashboardId', 'tileId', 'searches', 'alerts', 'navConfig', 'looping'}, 'Unknown manifest shape')
    for kind in ('alert', 'search'):
        require(kind in inventories, 'Missing complete ' + kind + ' inventory snapshot')
        require(all(isinstance(x, dict) and isinstance(x.get('id'), str) for x in inventories[kind]), 'Malformed inventory')
    operations = []

    def get(kind, ident):
        require((kind, ident) in objects, 'Missing full ' + kind + ' snapshot: ' + ident)
        obj = objects[(kind, ident)]
        projection(kind, obj)
        if kind != 'dashboard':
            require(obj.get('teamId') == spec['teamId'], 'Team mismatch: ' + ident)
            require(any(x['id'] == ident for x in inventories[kind]), 'Object absent from inventory: ' + ident)
        return obj

    def add(kind, before, after, tool, args, rollback):
        if projection(kind, before) == projection(kind, after):
            return
        operations.append({'kind': kind, 'id': before['id'], 'before': projection(kind, before),
                           'after': projection(kind, after), 'tool': tool, 'arguments': args,
                           'rollback': {'name': tool, 'arguments': rollback}})

    for rule in spec['searches']:
        before = get('search', rule['id'])
        require(before.get('sourceId') == spec['sourceId'] and before.get('filters') == [], 'Source or structured filters changed: ' + rule['id'])
        require(all(k in before for k in ('name', 'where', 'whereLanguage')), 'Incomplete saved search')
        after = copy.deepcopy(before)
        after.update(where=rule['where'], whereLanguage='sql')
        add('search', before, after, 'clickstack_save_saved_search', save_args('search', after), save_args('search', before))

    before = get('dashboard', spec['dashboardId'])
    tiles = before.get('tiles')
    require(isinstance(tiles, list) and len({t['id'] for t in tiles}) == len(tiles), 'Malformed dashboard tiles')
    tile = next((t for t in tiles if t.get('id') == spec['tileId']), None)
    require(tile is not None and not set(tile) - TILE_FIELDS, 'Missing or unknown target tile shape')
    config = tile.get('config', {})
    require(config.get('configType') == 'sql' and config.get('displayType') == 'number' and config.get('sourceId') == spec['sourceId'] and config.get('connectionId') == spec['connectionId'], 'Unexpected target tile source or shape')
    require(not set(config) - {'configType', 'displayType', 'sourceId', 'connectionId', 'sqlTemplate', 'numberFormat'}, 'Unknown NAV config fields; review before replacing')
    require(set(spec['navConfig']) == set(config), 'NAV config shape drift')
    after = copy.deepcopy(before)
    next(t for t in after['tiles'] if t['id'] == spec['tileId'])['config'] = spec['navConfig']
    args = {'dashboardId': before['id'], 'tileId': tile['id'], 'tile': {'config': spec['navConfig']}}
    rollback = {'dashboardId': before['id'], 'tileId': tile['id'], 'tile': {'config': tile['config']}}
    add('dashboard', before, after, 'clickstack_patch_dashboard', args, rollback)

    for rule in spec['alerts']:
        before = get('alert', rule['id'])
        for field, expected in rule['policy'].items():
            require(before.get(field) == expected, 'Alert policy drift: ' + rule['id'] + ' ' + field)
        channel = before.get('channel', {})
        require(set(channel) == {'type', 'webhookId'} and channel.get('type') == 'webhook' and channel.get('webhookId'), 'Unknown notification channel')
        require(not before.get('groupBy') or rule['policy']['source'] != 'saved_search' or rule['policy']['thresholdType'] != 'below', 'Grouped absence alert is unsafe')
        after = copy.deepcopy(before)
        after['message'] = rule['message']
        add('alert', before, after, 'clickstack_save_alert', save_args('alert', after), save_args('alert', before))
    return {'version': 1, 'endpoint': HOST, 'operations': operations,
            'looping': spec['looping'], 'atomicity': 'API has no compare-and-set. All configuration is checked before writes and immediately before each write; an in-flight race remains possible.'}


class Transport:
    def __init__(self):
        self.key = os.environ.get('LOYAL_CLICKSTACK_API_KEY')
        require(bool(self.key), 'LOYAL_CLICKSTACK_API_KEY is required only for --apply')

    def call(self, name, arguments):
        body = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': name, 'arguments': arguments}}
        request = urllib.request.Request(HOST, data=json.dumps(body).encode(), headers={
            'Authorization': 'Bearer ' + self.key, 'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream'})
        # Disable redirects so credentials never follow a different host.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None
        try:
            with urllib.request.build_opener(NoRedirect).open(request, timeout=50) as response:
                raw = response.read().decode()
        except (urllib.error.URLError, OSError):
            raise ValueError('ClickStack request failed; inspect server separately (response suppressed)') from None
        lines = [line[6:] for line in raw.splitlines() if line.startswith('data: ')]
        try:
            packet = json.loads(lines[-1] if lines else raw)
            require('error' not in packet, 'ClickStack RPC error (response suppressed)')
            result = packet['result']
            require(not result.get('isError'), 'ClickStack tool error (response suppressed)')
            for item in result.get('content', []):
                if item.get('type') == 'text':
                    value = json.loads(item['text'])
                    require(not isinstance(value, dict) or not any(k in value for k in ('error', 'errors')), 'ClickStack operation error (response suppressed)')
                    return value
            raise ValueError('Unknown ClickStack response envelope')
        except (KeyError, json.JSONDecodeError):
            raise ValueError('Unknown ClickStack response envelope') from None


GET = {'search': 'clickstack_get_saved_search', 'alert': 'clickstack_get_alert', 'dashboard': 'clickstack_get_dashboard'}


def apply_plan(plan, transport, journal_path):
    journal = {'version': 1, 'endpoint': HOST, 'completed': [], 'status': 'preflight'}

    def record():
        Path(journal_path).write_text(json.dumps(journal, indent=2) + '\n')

    def current(op):
        return projection(op['kind'], transport.call(GET[op['kind']], {'id': op['id']}))

    pending = []
    for op in plan['operations']:
        live = current(op)
        if live == op['after']:
            continue
        require(live == op['before'], 'Configuration drift before apply: ' + op['id'])
        pending.append(op)
    record()
    try:
        for op in pending:
            live = current(op)
            if live == op['after']:
                continue
            require(live == op['before'], 'Configuration drift immediately before write: ' + op['id'])
            # Record intended mutation before sending, including ambiguous timeout recovery.
            journal.update(status='writing', in_flight=op)
            record()
            transport.call(op['tool'], op['arguments'])
            require(current(op) == op['after'], 'Read-back mismatch after write: ' + op['id'])
            journal['completed'].append(op)
            journal.pop('in_flight', None)
            record()
        journal['status'] = 'complete'
        record()
    except Exception:
        journal['status'] = 'incomplete; inspect in_flight before retry or rollback'
        record()
        raise
    return len(journal['completed'])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--snapshots', required=True, type=Path)
    parser.add_argument('--manifest', type=Path, default=DEFAULT_MANIFEST)
    parser.add_argument('--output', required=True, type=Path, help='Exact plan and reverse rollback payloads; do not overwrite an applied journal')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--journal', type=Path, help='Required fresh file for --apply; preserves partial-write evidence')
    args = parser.parse_args(argv)
    require(not args.output.exists(), 'Output already exists; choose a fresh evidence path')
    if args.apply:
        require(args.journal is not None and not args.journal.exists(), '--apply requires a fresh --journal path')
        require(args.output.resolve() != args.journal.resolve(), 'Plan and journal must use different paths')
    objects, inventories = load_snapshots(args.snapshots)
    plan = build_plan(objects, inventories, json.loads(args.manifest.read_text()))
    plan['rollback_order'] = [op['rollback'] for op in reversed(plan['operations'])]
    plan['fingerprint'] = fingerprint(plan['operations'])
    args.output.write_text(json.dumps(plan, indent=2) + '\n')
    if args.apply:
        count = apply_plan(plan, Transport(), args.journal)
        print('Applied and read back ' + str(count) + ' changes; looping coverage remains deferred.')
    else:
        print('Dry run: ' + str(len(plan['operations'])) + ' changes; no network calls. Plan: ' + str(args.output))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except ValueError as error:
        print('Aborted: ' + str(error), file=sys.stderr)
        sys.exit(1)
