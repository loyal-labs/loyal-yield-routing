#!/usr/bin/env python3
"""Run ONLY against the marked disposable loopback DB described in README."""
import base64
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request

root = Path(__file__).resolve().parents[2]
db = urllib.parse.urlparse(os.environ['NEON_DATABASE_URL'])
endpoint = urllib.parse.urlparse(os.environ['REALTIME_URL'])
if not (
    os.environ.get('REALTIME_FIXTURE_ONLY') == 'true'
    and db.scheme in ('postgres', 'postgresql') and db.hostname == '127.0.0.1'
    and db.path == '/realtime_fixture' and not db.query and not db.fragment
    and db.username == 'realtime_fixture' and db.password is None
    and db.port is not None and 1 <= db.port <= 65535
    and endpoint.scheme == 'http' and endpoint.hostname == '127.0.0.1'
    and endpoint.path in ('', '/') and not endpoint.query and not endpoint.fragment
    and not endpoint.username and not endpoint.password
    and endpoint.port is not None and 1 <= endpoint.port <= 65535
):
    raise RuntimeError('requires explicit synthetic disposable loopback fixture inputs')
secret = 'synthetic-realtime-fixture-key-000000000000'
base_env = {'PATH': os.environ['PATH'], 'NEON_DATABASE_URL': db.geturl(),
            'REALTIME_URL': endpoint.geturl(), 'REALTIME_FIXTURE_ONLY': 'true',
            'REALTIME_AUTH_SECRET': secret,
            'REALTIME_ALLOWED_ORIGINS': 'https://allowed.fixture.invalid',
            'REALTIME_ALLOWED_ORIGIN': 'https://allowed.fixture.invalid',
            'REALTIME_HEARTBEAT_SECONDS': '1', 'PORT': str(endpoint.port),
            'REALTIME_AUTH_PREVIOUS_SECRET': ''}

psql_env = {'PATH': os.environ['PATH'], 'PGPASSFILE': '/dev/null'}
psql_args = ['psql', '-XAtw', '-v', 'ON_ERROR_STOP=1', db.geturl()]
def sql(query):
    return subprocess.check_output(psql_args + ['-c', query], env=psql_env,
                                   text=True, timeout=15).strip()

connected_identity = json.loads(sql("""SELECT json_build_object(
    'database',current_database(),'address',host(inet_server_addr()),
    'port',inet_server_port(),'marker',shobj_description(oid,'pg_database'))
    FROM pg_database WHERE datname=current_database()"""))
if connected_identity != dict(database='realtime_fixture', address='127.0.0.1',
                              port=db.port, marker='loyal-realtime-disposable-fixture'):
    raise RuntimeError('actual SQL endpoint is not the marked disposable loopback fixture')
# urllib otherwise consults ambient proxy variables in standalone invocations.
local_http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def until(check, seconds=8):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if check(): return
        time.sleep(.05)
    raise AssertionError('fixture condition timed out')

def token():
    now = int(time.time())
    claims = dict(v=1, iss='loyal-apps', aud='loyal-yield-realtime', iat=now, exp=now+300,
                  walletAddress='11111111111111111111111111111111',
                  settingsPda='SysvarRent111111111111111111111111111111111',
                  earnVaultAddress='SysvarC1ock11111111111111111111111111111111',
                  solanaEnv='mainnet-beta', scopes=['earn','autodeposit'], clientKind='web')
    encode = lambda b: base64.urlsafe_b64encode(b).decode().rstrip('=')
    payload = encode(json.dumps(claims).encode())
    return payload + '.' + encode(hmac.new(secret.encode(), payload.encode(), hashlib.sha256).digest())

def interrupted(_signal, _frame):
    raise KeyboardInterrupt('scratch verifier interrupted')
signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGINT, interrupted)
log_dir = Path(os.environ.get('REALTIME_FIXTURE_LOG_DIR', '/tmp')).resolve()
processes = []
def start(cleanup='false', user=None):
    env = {**base_env, 'REALTIME_RETENTION_CLEANUP_ENABLED': cleanup}
    if user:
        env['NEON_DATABASE_URL'] = db._replace(netloc=f'{user}@127.0.0.1:{db.port}').geturl()
    log = open(log_dir/f'realtime-{len(processes)}.log', 'w')
    p = subprocess.Popen([str(root/'target/debug/loyal-yield-realtime')], env=env, stdout=log, stderr=log)
    processes.append((p, log))
    def ready():
        if p.poll() is not None:
            raise RuntimeError('fixture realtime process exited before readiness')
        if f'listening on 0.0.0.0:{endpoint.port}' not in Path(log.name).read_text():
            return False
        try:
            return local_http.open(endpoint.geturl()+'/readyz', timeout=.3).status == 200
        except Exception: return False
    until(ready)
    return p

def stop(p):
    before=time.monotonic(); p.send_signal(signal.SIGTERM); p.wait(timeout=4)
    assert p.returncode == 0
    return round(time.monotonic()-before, 3)

measurements = {'preflight_sql_identity': connected_identity,
                'runtime_environment_names': sorted(base_env),
                'psql_environment_names': sorted(psql_env)}
blocker = None
replay_fixture = False
try:
    sql("INSERT INTO loyal_yield.realtime_events(event_type,scope,reason,created_at) VALUES ('fixture.expired','fixture','fixture',now()-interval '8 days')")
    p=start()
    measurements['expired_rows_before_smoke']=int(sql("SELECT count(*) FROM loyal_yield.realtime_events WHERE created_at < now()-interval '7 days'"))
    assert measurements['expired_rows_before_smoke'] > 0
    smoke = subprocess.run(['bun', 'scripts/hetzner-realtime/verify-sse-fixture.ts'], cwd=root, env=base_env, check=True, timeout=60, capture_output=True, text=True)
    print(smoke.stdout, file=sys.stderr, end='')
    measurements['direct_sql_auth_origin_live_delivery_replay'] = 'passed'
    measurements['expired_rows_after_disabled']=int(sql("SELECT count(*) FROM loyal_yield.realtime_events WHERE created_at < now()-interval '7 days'"))
    assert measurements['expired_rows_after_disabled'] == measurements['expired_rows_before_smoke']
    measurements['cleanup_disabled_retains_expired_rows'] = True
    # A long-lived stream closes on SIGTERM, independent of its 300-second token.
    connection=http.client.HTTPConnection(endpoint.hostname, endpoint.port, timeout=4)
    connection.request('GET','/events',headers={'Authorization':'Bearer '+token()})
    response=connection.getresponse(); assert response.status == 200
    measurements['live_sse_shutdown_seconds']=stop(p)
    measurements['live_sse_status']=response.status
    measurements['live_sse_body_bytes_at_eof']=len(response.read())
    connection.close()
    continuity_cursor=sql('SELECT MAX(id) FROM loyal_yield.realtime_events WHERE deliverable')
    offline_id=sql("""SELECT loyal_yield.emit_realtime_event(
      p_event_type=>'fixture.shutdown_replay',p_scope=>'autodeposit',p_reason=>'offline_gap',
      p_solana_env=>'mainnet-beta',p_wallet_address=>'11111111111111111111111111111111',
      p_settings_pda=>'SysvarRent111111111111111111111111111111111',
      p_smart_account_address=>'SysvarC1ock11111111111111111111111111111111')""")
    p=start('true')
    reconnect=http.client.HTTPConnection(endpoint.hostname,endpoint.port,timeout=4)
    reconnect.request('GET','/events',headers={'Authorization':'Bearer '+token(),'Last-Event-ID':continuity_cursor})
    replay_response=reconnect.getresponse();assert replay_response.status == 200
    block=[]
    while True:
        line=replay_response.readline().decode().strip()
        if not line and block: break
        if line: block.append(line)
    parsed=dict(line.split(': ',1) for line in block)
    payload=json.loads(parsed['data'])
    assert parsed['id'] == offline_id and payload['eventId'] == offline_id
    measurements['sigterm_restart_replay']=dict(cursor=continuity_cursor,sql_event_id=offline_id,
                                                received_sse=parsed,status=replay_response.status)
    reconnect.close()
    until(lambda: sql("SELECT count(*) FROM loyal_yield.realtime_events WHERE created_at < now()-interval '7 days'") == '0')
    measurements['expired_rows_after_enabled']=int(sql("SELECT count(*) FROM loyal_yield.realtime_events WHERE created_at < now()-interval '7 days'"))
    measurements['cleanup_enabled_deletes_expired_rows']=True
    stop(p)
    # Force latest_event_id to wait on a real PostgreSQL table lock.
    p=start()
    replay_cursor=str(int(sql('SELECT MIN(id) FROM loyal_yield.realtime_events WHERE deliverable'))-1)
    blocker=subprocess.Popen(psql_args + ['-c','BEGIN; LOCK TABLE loyal_yield.realtime_events IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(60);'],env=psql_env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    until(lambda: sql("SELECT count(*) FROM pg_locks WHERE relation='loyal_yield.realtime_events'::regclass AND mode='AccessExclusiveLock' AND granted") == '1')
    result={}
    def request():
        before=time.monotonic()
        c=http.client.HTTPConnection(endpoint.hostname,endpoint.port,timeout=15)
        try:
            c.request('GET','/events',headers={'Authorization':'Bearer '+token(),'Last-Event-ID':replay_cursor})
            result['status']=c.getresponse().status
            result['duration_seconds']=round(time.monotonic()-before,3)
        finally: c.close()
    t=threading.Thread(target=request);t.start()
    until(lambda: int(sql("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT MAX(id)%'")) > 0)
    t.join(12); assert result.get('status') == 503
    measurements['blocked_admission_timeout_seconds']=result['duration_seconds']
    measurements['blocked_admission_timeout_status']=result['status']
    result.clear();t=threading.Thread(target=request);t.start()
    until(lambda: int(sql("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT MAX(id)%'")) > 0)
    measurements['blocked_admission_shutdown_seconds']=stop(p)
    t.join(2); assert result.get('status') == 503
    measurements['blocked_admission_shutdown_status']=result['status']
    sql("SELECT pg_terminate_backend(pid) FROM pg_locks WHERE relation='loyal_yield.realtime_events'::regclass AND mode='AccessExclusiveLock' AND granted AND pid <> pg_backend_pid()")
    blocker.wait(timeout=3);blocker=None
    # Delay ONLY replay SELECTs via an isolated RLS fixture. MAX/MIN stay fast.
    replay_fixture = True
    sql("""CREATE ROLE realtime_fixture_reader LOGIN;
      GRANT USAGE ON SCHEMA loyal_yield TO realtime_fixture_reader;
      GRANT SELECT ON loyal_yield.realtime_events TO realtime_fixture_reader;
      CREATE FUNCTION loyal_yield.fixture_replay_delay() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
      BEGIN IF current_query() LIKE '%ORDER BY id ASC%' THEN PERFORM pg_sleep(30); END IF; RETURN TRUE; END $$;
      ALTER TABLE loyal_yield.realtime_events ENABLE ROW LEVEL SECURITY;
      CREATE POLICY fixture_replay_delay ON loyal_yield.realtime_events FOR SELECT TO realtime_fixture_reader USING (loyal_yield.fixture_replay_delay());""")
    p=start(user='realtime_fixture_reader');result.clear()
    t=threading.Thread(target=request);t.start()
    until(lambda: int(sql("SELECT count(*) FROM pg_stat_activity WHERE usename='realtime_fixture_reader' AND wait_event='PgSleep' AND query LIKE '%ORDER BY id ASC%'")) > 0)
    measurements['blocked_replay_shutdown_seconds']=stop(p)
    t.join(2);assert result.get('status') == 503
    measurements['blocked_replay_shutdown_status']=result['status']
    sql("DROP POLICY fixture_replay_delay ON loyal_yield.realtime_events; ALTER TABLE loyal_yield.realtime_events DISABLE ROW LEVEL SECURITY; DROP FUNCTION loyal_yield.fixture_replay_delay(); DROP OWNED BY realtime_fixture_reader; DROP ROLE realtime_fixture_reader;")
    replay_fixture = False
    print(json.dumps(dict(scope='realtime_local_fixture',service_id='srv-d966hcpkh4rs73da0j4g',
      collected_at=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()), source_identity='fabd187b84dbda902dcff2ef4e7c68765961845b',
      target_identity='local-uncommitted-patch', measurements=measurements,verdict='PASS',
      limitations=['loopback HTTP only; no proxy, TLS, browser, mobile or production cutover proof'])))
finally:
    if blocker and blocker.poll() is None: blocker.terminate();blocker.wait(timeout=3)
    for p,log in processes:
        if p.poll() is None: p.terminate();p.wait(timeout=5)
        if p.poll() is None: raise RuntimeError('realtime child still active')
        log.close()
    if replay_fixture:
        sql("DROP POLICY IF EXISTS fixture_replay_delay ON loyal_yield.realtime_events; ALTER TABLE loyal_yield.realtime_events DISABLE ROW LEVEL SECURITY; DROP FUNCTION IF EXISTS loyal_yield.fixture_replay_delay(); DROP OWNED BY realtime_fixture_reader; DROP ROLE realtime_fixture_reader;")
