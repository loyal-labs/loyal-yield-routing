#!/usr/bin/env python3
"""Fixed disposable lease harness; no caller database URL accepted."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import tempfile
from urllib.parse import urlencode

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--pg-bindir', required=True)
    parser.add_argument('--ack-isolated', action='store_true')
    args = parser.parse_args()
    pg = Path(args.pg_bindir)
    output = dict(scope='backyard-route-lease-fixture', service_id=None,
        collected_at=dt.datetime.now(dt.timezone.utc).isoformat(),
        source_identity={'worktree': str(ROOT), 'deployed_commit': 'f821a78f6a5c0507eb1dada10fd300df1277f639',
                         'identity_patch': 'current worktree'},
        target_identity=None, measurements={}, verdict='BLOCKED',
        limitations=['Lease exclusion/fencing only; broader financial recovery is not proven.'])
    if not args.ack_isolated:
        output['limitations'].append('isolated fixture acknowledgement required')
    elif not all((pg / name).is_file() for name in ('initdb', 'pg_ctl', 'psql')):
        output['limitations'].append('local PostgreSQL server binaries required')
    else:
        # No inherited PG bindings, production URLs, OP or signer/provider keys.
        env = {'HOME': os.environ['HOME'], 'PATH': str(pg) + os.pathsep + os.defpath +
               ':/opt/homebrew/bin:' + str(Path(os.environ['HOME']) / '.cargo/bin'),
               'LANG': 'C', 'LC_ALL': 'C', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'local'}
        if not shutil.which('go', path=env['PATH']):
            output['limitations'].append('local Go toolchain required')
            print(json.dumps(output))
            return 2
        with tempfile.TemporaryDirectory(prefix='backyard-lease-', dir='/tmp') as scratch:
            scratch = Path(scratch)
            data, sock = scratch / 'data', scratch / 'socket'
            sock.mkdir()
            env['GOCACHE'] = str(scratch / 'go-cache')
            started = False
            stage = 'postgres-setup'
            try:
                subprocess.run([str(pg / 'initdb'), '-D', str(data), '-A', 'trust', '--no-locale', '-E', 'UTF8'],
                               env=env, check=True, capture_output=True, timeout=30)
                subprocess.run([str(pg / 'pg_ctl'), '-D', str(data), '-l', str(scratch / 'postgres.log'),
                    '-o', f"-F -k {sock} -c listen_addresses=''", '-w', 'start'],
                    env=env, check=True, capture_output=True, timeout=30)
                started = True
                stage = 'fixture-ddl'
                psql = [str(pg / 'psql'), '-XqAt', '-v', 'ON_ERROR_STOP=1', '-h', str(sock)]
                subprocess.run(psql + ['-d', 'postgres', '-c', 'CREATE DATABASE backyard_route_lease_fixture'],
                               env=env, check=True, capture_output=True, timeout=10)
                subprocess.run(psql + ['-d', 'backyard_route_lease_fixture'], input=(HERE / 'lease.sql').read_text(),
                               text=True, env=env, check=True, capture_output=True, timeout=10)
                env['BACKYARD_RWA_TEST_DATABASE_URL'] = 'postgresql:///backyard_route_lease_fixture?' + urlencode({'host': str(sock)})
                env['BACKYARD_ROUTE_LEASE_ISOLATED_FIXTURE'] = '1'
                stage = 'go-lease-contract'
                command = ['go', 'test', '-race', './internal/backyardrwa', '-count=1', '-timeout=90s',
                           '-run', '^(TestRouteLeaseAgainstDatabase|TestRouteLeaseIsolatedContentionAndStaleFencing)$', '-v']
                process = subprocess.Popen(command, cwd=ROOT / 'go/backyard-rwa-worker', env=env,
                                           text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
                try:
                    stdout, stderr = process.communicate(timeout=240)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        process.communicate(timeout=10)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.communicate()
                    raise
                output['measurements'] = {'command': command, 'exit_code': process.returncode,
                                          'stdout_tail': stdout[-5000:], 'stderr_tail': stderr[-2000:]}
                output['verdict'] = 'PASS' if process.returncode == 0 else 'FAIL'
                if process.returncode and any(s in stderr for s in ('module lookup disabled by GOPROXY=off', 'requires go >=', 'cannot find package')):
                    output['verdict'] = 'BLOCKED'
                    output['limitations'].append('offline Go toolchain/module prerequisite missing')
            except (OSError, subprocess.SubprocessError) as error:
                output['verdict'] = 'BLOCKED' if stage == 'postgres-setup' else 'FAIL'
                output['limitations'].append(stage + ' did not complete: ' + type(error).__name__)
            finally:
                if started:
                    subprocess.run([str(pg / 'pg_ctl'), '-D', str(data), '-m', 'immediate', '-w', 'stop'],
                                   env=env, check=True, capture_output=True, timeout=30)
    print(json.dumps(output))
    return {'PASS': 0, 'FAIL': 1, 'BLOCKED': 2}[output['verdict']]


if __name__ == '__main__':
    sys.exit(main())
