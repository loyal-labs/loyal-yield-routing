#!/usr/bin/env python3
"""Read-only host preflight and bounded observation; never sends transactions."""
import argparse
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import subprocess
import time
import urllib.parse
import urllib.request


HERE = Path(__file__).resolve().parent
FAMILIES = ('backyard', 'earn-max', 'retail', 'observer', 'telegram', 'web', 'admin', 'stats', 'realtime')
# The deployed vault identities from go/workers/internal/backyard/vault.go.
VAULTS = {
    'backyard': ('looping', 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY', 'GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1'),
    'earn-max': ('rwa', 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh', 'HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA'),
}
METRICS_PORTS = {'backyard': 9103, 'earn-max': 9105, 'retail': 9102, 'observer': 9101}
ADDRESS_VARS = {'web': 'LOYAL_WEB_ADDRESS', 'admin': 'LOYAL_ADMIN_ADDRESS', 'stats': 'LOYAL_STATS_ADDRESS', 'telegram': 'LOYAL_TELEGRAM_ADDRESS'}
# Required by the production features we run, including optional-at-startup integrations.
CREDENTIALS = {
    'backyard': 'BACKYARD_DATABASE_URL BACKYARD_SOLANA_RPC_URL BACKYARD_POLICY_KEYPAIR BACKYARD_HELIUS_API_KEY JUPITER_API_KEY',
    'retail': 'RETAIL_DATABASE_URL RETAIL_TIMESCALE_DATABASE_URL RETAIL_SOLANA_RPC_URL RETAIL_JUPITER_API_KEY RETAIL_DELEGATE_KEYPAIR RETAIL_FEE_PAYER_KEYPAIR RETAIL_LOOKUP_MANAGER_KEYPAIR RETAIL_PRIVY_APP_ID RETAIL_PRIVY_APP_SECRET RETAIL_RELAY_API_KEY RETAIL_SWEEP_NOTIFY_ENDPOINT RETAIL_SWEEP_NOTIFY_SECRET',
    'observer': 'HELIUS_API_KEY SOLANA_RPC_URL NEON_DATABASE_URL TIMESCALEDB_URL',
    'telegram': 'TELEGRAM_APP_DATABASE_URL TELEGRAM_BOT_TOKEN TELEGRAM_WEBHOOK_SECRET MESSAGE_ENCRYPTION_KEY REDPILL_AI_API_KEY R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY',
    'web': 'WEB_APP_DATABASE_URL AUTH_JWT_SECRET WEB_SOLANA_RPC_URL COINGECKO_API_KEY OBSERVABILITY_OTLP_ENDPOINT OBSERVABILITY_INGESTION_API_KEY CAP_SECRET WEB_YIELD_DATABASE_URL EARN_YIELD_ROUTER_PUBLIC_KEY SMART_ACCOUNT_SPONSOR_PK REALTIME_AUTH_SECRET TIMESCALEDB_URL PRIVY_APP_ID PRIVY_APP_SECRET WEB_EVM_RPC_URLS RELAY_API_KEY ONESIGNAL_APP_ID ONESIGNAL_REST_API_KEY',
    'admin': 'ADMIN_USER ADMIN_PASSWORD ADMIN_APP_DATABASE_URL ADMIN_YIELD_DATABASE_URL ADMIN_TIMESCALE_DATABASE_URL CLICKSTACK_READ_KEY R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY',
    'stats': 'STATS_APP_DATABASE_URL STATS_YIELD_DATABASE_URL',
    'realtime': 'realtime-envelope',
}
CREDENTIALS['earn-max'] = CREDENTIALS['backyard']
CRONS = {
    'smart-account-reconcile': 'WEB_APP_DATABASE_URL WEB_YIELD_DATABASE_URL WEB_SOLANA_RPC_URL SMART_ACCOUNT_SPONSOR_PK',
    'earn-yield-joy': 'WEB_APP_DATABASE_URL WEB_YIELD_DATABASE_URL TIMESCALEDB_URL WEB_SOLANA_RPC_URL ONESIGNAL_APP_ID ONESIGNAL_REST_API_KEY MIXPANEL_PROJECT_TOKEN',
    'earn-events-mixpanel': 'WEB_APP_DATABASE_URL WEB_YIELD_DATABASE_URL MIXPANEL_PROJECT_TOKEN',
}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def command(*args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, timeout=30, **kwargs)
    # Child diagnostics can include database connection details; keep them off CI logs.
    require(result.returncode == 0, f'{Path(args[0]).name} failed (exit {result.returncode})')
    return result.stdout


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def unit_environment(unit):
    text = command('systemctl', 'show', unit, '--property=Environment', '--value')
    return dict(item.split('=', 1) for item in shlex.split(text) if '=' in item)


def unit_properties(unit, properties):
    text = command('systemctl', 'show', unit, '--property=' + properties)
    return dict(line.split('=', 1) for line in text.splitlines() if '=' in line)


def credential_references(unit):
    references = {}
    for line in command('systemctl', 'cat', unit).splitlines():
        if not line.startswith('LoadCredentialEncrypted='):
            continue
        value = line.split('=', 1)[1]
        if not value:
            references.clear()
            continue
        for entry in shlex.split(value):
            name, separator, path = entry.partition(':')
            require(separator and path.startswith('/etc/credstore.encrypted/'), f'{unit}: unsupported encrypted credential reference')
            references[name] = path
    return references


def check_credentials(unit, required):
    references = credential_references(unit)
    missing = set(required.split()) - references.keys()
    require(not missing, f'{unit}: missing credentials: ' + ', '.join(sorted(missing)))
    # Check ciphertext usability, not merely filenames. Values never leave host memory.
    for name, path in references.items():
        value = command('systemd-creds', 'decrypt', '--name=' + name, path, '-')
        require(bool(value.strip()), f'{unit}: empty credential {name}')
        if name == 'realtime-envelope':
            values = json.loads(value)['values']
            require(all(values.get(key) for key in ('NEON_DATABASE_URL', 'REALTIME_AUTH_SECRET', 'REALTIME_ALLOWED_ORIGINS')),
                    f'{unit}: incomplete realtime credential envelope')


def cron_jobs():
    jobs = ['smart-account-reconcile', 'earn-yield-joy']
    optional = unit_properties('loyal-web-cron-earn-events-mixpanel.timer', 'LoadState')
    if optional.get('LoadState') != 'not-found':
        jobs.append('earn-events-mixpanel')
    return jobs


def configuration(family):
    check_credentials(f'loyal-{family}.service', CREDENTIALS[family])
    if family != 'web':
        return
    for unit in ('loyal-web.service', 'loyal-web-cron@earn-yield-joy.service'):
        env = unit_environment(unit)
        missing = [name for name in ('ONESIGNAL_EARNINGS_CHANNEL_ID', 'ONESIGNAL_ALERTS_CHANNEL_ID') if not env.get(name, '').strip()]
        require(not missing, f'{unit}: missing configuration: ' + ', '.join(missing))
    for job in cron_jobs():
        check_credentials(f'loyal-web-cron@{job}.service', CRONS[job])


def wait_for_crons():
    deadline = time.monotonic() + 600
    while True:
        running = False
        for job in cron_jobs():
            state = unit_properties(f'loyal-web-cron@{job}.service', 'ActiveState,Result')
            require(state.get('Result') == 'success' and state.get('ActiveState') != 'failed', f'{job}: scheduled job failed')
            running |= state.get('ActiveState') in ('active', 'activating')
        if not running:
            return
        require(time.monotonic() < deadline, 'scheduled jobs did not finish before deployment')
        time.sleep(5)


def cron_snapshot(release, before=None):
    results = {}
    uptime = float(Path('/proc/uptime').read_text().split()[0])
    for job in cron_jobs():
        unit = f'loyal-web-cron@{job}.service'
        timer = unit_properties(f'loyal-web-cron-{job}.timer', 'ActiveState,UnitFileState')
        require(timer.get('ActiveState') == 'active' and timer.get('UnitFileState') == 'enabled', f'{job}: cron timer is not enabled and active')
        state = unit_properties(unit, 'Result,ExecMainStatus,ActiveState,ExecMainStartTimestampMonotonic,ExecMainExitTimestampMonotonic,ExecStart')
        require(state.get('Result') == 'success' and state.get('ExecMainStatus') == '0'
                and state.get('ActiveState') in ('inactive', 'activating', 'active'), f'{job}: scheduled job failed')
        require(f'/opt/loyal/releases/web/{release}/loyal-web cron ' in state.get('ExecStart', ''),
                f'{job}: cron release differs from web; resolve its recovery override before deployment')
        started = int(state.get('ExecMainStartTimestampMonotonic', '0')) / 1e6
        finished = int(state.get('ExecMainExitTimestampMonotonic', '0')) / 1e6
        running = state['ActiveState'] in ('activating', 'active')
        require((running and started > 0 and 0 <= uptime - started <= 600)
                or (not running and finished > 0 and 0 <= uptime - finished <= 1800), f'{job}: no recent scheduled run')
        if before is not None:
            require(job in before, f'{job}: scheduled jobs changed during deployment')
            require(not journal_failures(unit, before[job]['cursor']), f'{job}: error since preflight; inspect its journal')
        cursor = re.search(r'^-- cursor: (.+)$', command('journalctl', '-u', unit, '-n', '0', '--show-cursor', '--no-pager'), re.M)
        require(cursor is not None, f'{job}: journal cursor unavailable')
        results[job] = {'cursor': cursor[1], 'started_at': time.time() - uptime + started,
                        'completed': finished >= started and state['ActiveState'] == 'inactive'}
    return results


def database_snapshot(family, since):
    _, route, vault = VAULTS[family]
    url = urllib.parse.urlsplit((Path(os.environ['CREDENTIALS_DIRECTORY']) / 'BACKYARD_DATABASE_URL').read_text().strip())
    options = urllib.parse.parse_qs(url.query)
    require(url.scheme in ('postgres', 'postgresql') and url.hostname and url.username and url.path.lstrip('/'), 'invalid database credential')
    require(set(options) <= {'sslmode'}, 'unsupported database connection options')
    # Match the host's cutover probe; PGDATABASE alone does not expand a URI.
    env = {key: value for key, value in os.environ.items() if not key.startswith('PG')}
    env.update(PGHOST=url.hostname, PGPORT=str(url.port or 5432),
               PGDATABASE=urllib.parse.unquote(url.path.lstrip('/')),
               PGUSER=urllib.parse.unquote(url.username), PGPASSWORD=urllib.parse.unquote(url.password or ''),
               PGSSLMODE=options.get('sslmode', ['require'])[0], PGCONNECT_TIMEOUT='10', PGAPPNAME='loyal-rollout-check')
    raw = command('psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1', '-v', f'route={route}',
                  '-v', f'vault={vault}', '-v', f'since={since}',
                  '-f', str(HERE / 'rollout-snapshot.sql'), env=env)
    require(bool(raw.strip()), 'vault route is missing')
    data = json.loads(raw)
    observed = dt.datetime.fromisoformat(data['observed_at'].replace('Z', '+00:00')).timestamp()
    require(isinstance(data['observed_slot'], int) and data['observed_slot'] > 0, 'vault observation has no slot')
    require(all(re.fullmatch(r'[0-9]+', str(data['position'].get(key, ''))) for key in ('collateral', 'debt')),
            'vault position balances are missing or invalid')
    return {
        'observed_at': observed, 'observed_slot': data['observed_slot'],
        'position_fingerprint': fingerprint(data['position']),
        'intent_fingerprint': fingerprint([data['targets'], data['exits'], data['permissions']]),
        'permission_rows': len(data['permissions']),
        'recovery': data['recovery'], 'failed_or_held': data['failed_or_held'], 'inflight': data['inflight'],
    }


def vault_snapshot(family, since):
    unit = f'loyal-{family}.service'
    require(unit_environment(unit).get('BACKYARD_VAULT') == VAULTS[family][0], 'unit selects the wrong vault')
    credentials = re.findall(r'^LoadCredentialEncrypted=(BACKYARD_DATABASE_URL:\S+)$', command('systemctl', 'cat', unit), re.M)
    require(bool(credentials), 'vault database credential reference is missing')
    raw = command('systemd-run', '--quiet', '--wait', '--pipe', '--collect',
                  '-p', 'DynamicUser=yes', '-p', f'LoadCredentialEncrypted={credentials[-1]}',
                  '-p', f'BindReadOnlyPaths={HERE}:/run/loyal-rollout-check',
                  '/usr/bin/python3', '/run/loyal-rollout-check/rollout-health.py', 'database', family, '--since', str(since))
    return json.loads(raw)


def metrics(family):
    with urllib.request.urlopen(f'http://127.0.0.1:{METRICS_PORTS[family]}/metrics', timeout=10) as response:
        text = response.read().decode()
    found = {}
    for line in text.splitlines():
        if not line or line.startswith('#'):
            continue
        match = re.fullmatch(r'([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})?\s+(\S+)(?:\s+\S+)?', line)
        require(match is not None, 'malformed worker metrics')
        name, labels, value = match.groups()
        try:
            number = float(value)
        except ValueError:
            raise RuntimeError('malformed metric value') from None
        if name.startswith(('loyal_lane_', 'loyal_family_', 'loyal_backyard_')):
            require(math.isfinite(number), f'nonfinite {name}')
            found[name + (labels or '')] = number
    return found


def journal_failures(unit, cursor, stopping_invocation=None):
    lines = command('journalctl', '-u', unit, '--after-cursor', cursor, '--no-pager', '-o', 'json')
    failures = []
    for line in lines.splitlines():
        entry = json.loads(line)
        message = entry.get('MESSAGE', '')
        if not isinstance(message, str):
            failures.append('unreadable journal message')
            continue
        try:
            event = json.loads(message)
        except ValueError:
            event = {}
        if not isinstance(event, dict):
            event = {}
        # Older web releases logged normal metrics shutdown as an error.
        if (unit == 'loyal-web.service' and stopping_invocation
                and entry.get('_SYSTEMD_INVOCATION_ID') == stopping_invocation
                and event.get('msg') == 'loyal-web metrics' and event.get('error') == 'context canceled'):
            continue
        name = str(event.get('msg', event.get('message', '')))
        if (str(event.get('level', '')).lower() in ('error', 'fatal', 'panic')
                or name in ('backyard_operation_failed_after_send', 'backyard_worker_exit', 'backyard_latched')
                or (unit.startswith('loyal-web-cron@') and (
                    'wallet failed' in name or (isinstance(event.get('errors'), int) and event['errors'] > 0)))
                or entry.get('PRIORITY') in ('0', '1', '2', '3')
                or re.search(r'\b(panic:|FATAL|ERROR)\b', message)):
            # Never forward raw journal messages or their transaction/credential fields.
            failures.append('worker error/exit in journal')
    return failures


def snapshot(family, since, cursor=None, allow_startup=False, stopping_invocation=None, crons_before=None):
    unit = f'loyal-{family}.service'
    props = command('systemctl', 'show', unit, '--property=ActiveState,SubState,NRestarts,MainPID,InvocationID')
    state = dict(line.split('=', 1) for line in props.splitlines() if '=' in line)
    require(state.get('ActiveState') == 'active' and state.get('SubState') == 'running', f'{unit} is not running')
    require(int(state['NRestarts']) == 0, f'{unit} restarted')
    require(int(state['MainPID']) > 0 and bool(state['InvocationID']), 'missing process identity')
    if cursor:
        stopped = stopping_invocation if state['InvocationID'] != stopping_invocation else None
        require(not journal_failures(unit, cursor, stopped), f'{unit}: error/exit since preflight; inspect its journal')
    raw_cursor = command('journalctl', '-u', unit, '-n', '0', '--show-cursor', '--no-pager')
    current_cursor = re.search(r'^-- cursor: (.+)$', raw_cursor, re.M)
    require(current_cursor is not None, 'journal cursor is unavailable')
    result = {'family': family, 'at': time.time(), 'invocation': state['InvocationID'], 'cursor': current_cursor[1]}
    if family == 'realtime':
        tag = unit_environment(unit).get('LOYAL_IMAGE_VERSION', '')
        require(re.fullmatch(r'sha-[0-9a-f]{40}', tag) is not None, 'realtime release identity is missing')
        result['release'] = tag[4:]
        binary = Path('/opt/loyal/bin/loyal-yield-realtime')
    else:
        dropin = Path(f'/etc/systemd/system/{unit}.d/40-verified-release.conf').read_text()
        match = re.search(rf'^ExecStart=(/opt/loyal/releases/{re.escape(family)}/([0-9a-f]{{40}})/loyal-[a-z-]+)$', dropin, re.M)
        require(match is not None, 'verified release drop-in is missing or invalid')
        binary = Path(match[1])
        result['release'] = match[2]
    executable = Path(f'/proc/{state["MainPID"]}/exe').resolve()
    require(executable == binary, 'running executable differs from verified release')
    require(binary.is_file() and os.access(binary, os.X_OK), 'previous release binary is unavailable')
    result['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
    if family in METRICS_PORTS:
        values = metrics(family)
        progress = 'loyal_family_last_progress_timestamp_seconds' if family == 'observer' else 'loyal_lane_last_success_timestamp_seconds'
        lanes = {key: value for key, value in values.items() if key.startswith(progress)}
        ready = bool(lanes) and all(value != 0 for value in lanes.values())
        require(ready or allow_startup, 'worker lane observations are missing')
        require(all(0 <= result['at'] - value <= 300 for value in lanes.values() if value != 0), 'worker lane observation is stale')
        result['lane_observed_at'] = max(lanes.values()) if ready else 0
        result['failures'] = {key: value for key, value in values.items() if key.startswith('loyal_family_failed_total')}
    if family in VAULTS:
        result['vault'] = vault_snapshot(family, since)
        vault = result['vault']
        require(0 <= time.time() - vault['observed_at'] <= 120, 'vault position observation is stale')
        require(vault['recovery'] == 0, 'vault has a manual recovery latch')
        require(vault['failed_or_held'] == 0, 'vault has a failed/held operation since preflight')
    if family in ADDRESS_VARS or family == 'realtime':
        address = '127.0.0.1:10000' if family == 'realtime' else unit_environment(unit).get(ADDRESS_VARS[family], '')
        require(re.fullmatch(r'127\.0\.0\.1:[0-9]+', address) is not None, 'loopback readiness address is missing')
        path = '/healthz' if family == 'telegram' else '/readyz'
        with urllib.request.urlopen(f'http://{address}{path}', timeout=10) as response:
            if family in ('web', 'admin', 'stats'):
                require(response.headers.get('X-Loyal-Release') in (result['release'], 'sha-' + result['release']), 'readiness serves a different release')
    if family == 'web':
        result['crons'] = cron_snapshot(result['release'], crons_before)
    return result


def check_sample(before, current, sha, watch_started, previous=None):
    require(current['release'] == sha, 'wrong release during observation')
    require(current['binary_sha256'] == before.get('expected_binary_sha256', before['binary_sha256']),
            'running binary differs from the verified artifact')
    if previous:
        require(current['invocation'] == previous['invocation'], 'worker exited/restarted during observation')
    same_process = current['invocation'] == before['invocation']
    baseline = before.get('failures', {}) if same_process else {}
    require(all(value <= baseline.get(key, 0) for key, value in current.get('failures', {}).items()), 'worker failure counter advanced')
    fresh = current.get('lane_observed_at', current['at']) > watch_started
    if 'vault' in current:
        require(current['vault']['intent_fingerprint'] == before['vault']['intent_fingerprint'], 'vault targets or permissions changed during rollout')
        fresh = fresh and current['vault']['observed_at'] > watch_started
    if 'crons' in current:
        require(current['crons'].keys() == before['crons'].keys(), 'scheduled jobs changed during deployment')
        fresh = fresh and all(job['completed'] and job['started_at'] > watch_started for job in current['crons'].values())
    return fresh


def watch(family, sha, baseline, seconds=120):
    before = json.loads(Path(baseline).read_text())
    require(before['family'] == family, 'wrong preflight family')
    started = time.time()
    observing = None
    previous = None
    fresh = False
    while True:
        current = snapshot(family, before['at'], before['cursor'], allow_startup=observing is None,
                           stopping_invocation=before['invocation'], crons_before=before.get('crons'))
        fresh = check_sample(before, current, sha, observing if observing is not None else started, previous) or fresh
        previous = current
        if observing is None:
            if current.get('lane_observed_at', current['at']) > 0:
                observing = time.time()
            else:
                require(current['invocation'] != before['invocation'], 'existing worker lane observations are missing')
                require(time.time() - started < 60, 'worker startup observation timed out')
        if observing is not None and time.time() - observing >= seconds:
            if not fresh and family == 'web' and time.time() - started < 1500:
                time.sleep(5)
                continue
            require(fresh, 'no fresh observation from the deployed process')
            print(json.dumps({'family': family, 'release': sha, 'observed_seconds': seconds, 'fresh': True}), flush=True)
            return
        time.sleep(5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('snapshot', 'database', 'watch'))
    parser.add_argument('family', choices=FAMILIES)
    parser.add_argument('--since', type=float)
    parser.add_argument('--sha')
    parser.add_argument('--baseline')
    args = parser.parse_args()
    if args.mode != 'database':
        configuration(args.family)
    if args.mode == 'snapshot' and args.family == 'web':
        wait_for_crons()
    since = args.since if args.since is not None else time.time() - 120
    if args.mode == 'watch':
        require(args.sha is not None and re.fullmatch('[0-9a-f]{40}', args.sha) is not None and args.baseline, 'watch requires a SHA and baseline')
        watch(args.family, args.sha, args.baseline)
    else:
        data = database_snapshot(args.family, since) if args.mode == 'database' else snapshot(args.family, since)
        print(json.dumps(data, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Unexpected child/network/parse exceptions can contain connection details.
        print('ROLLOUT BLOCKED: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), flush=True)
        raise SystemExit(1)
