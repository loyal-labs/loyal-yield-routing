#!/usr/bin/env python3
"""Deploy verified main artifacts serially, with host preflight and observation."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
FAMILIES = ('backyard', 'earn-max', 'retail', 'observer', 'telegram', 'web', 'admin', 'stats', 'realtime')
BINARY = dict.fromkeys(('backyard', 'earn-max', 'retail'), 'loyal-engine')
BINARY.update({family: 'loyal-' + family for family in ('observer', 'telegram', 'web', 'admin', 'stats')})
BINARY['realtime'] = 'loyal-yield-realtime'
SHA = re.compile('[0-9a-f]{40}')


def run(*args, capture=True, **kwargs):
    result = subprocess.run(args, cwd=kwargs.pop('cwd', ROOT), text=True, check=True,
                            stdout=subprocess.PIPE if capture else None, **kwargs)
    return result.stdout.strip() if capture else None


def ssh(host, *args, **kwargs):
    return run('ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', host, shlex.join(args), **kwargs)


def require_head(sha):
    run('git', 'fetch', '-q', 'origin', 'main')
    if run('git', 'rev-parse', 'origin/main') != sha:
        raise RuntimeError('main advanced; refusing a superseded release')


def check_previous(family, before, sha, expected_digest=None):
    previous = before['release']
    if SHA.fullmatch(previous) is None:
        raise RuntimeError(f'{family}: invalid previous release')
    if previous == sha and expected_digest and before['binary_sha256'] != expected_digest:
        raise RuntimeError(f'{family}: this SHA already runs different bytes; require a new commit')
    run('git', 'cat-file', '-e', previous + '^{commit}')
    # A deployment must not silently replace an unmerged or newer production build.
    run('git', 'merge-base', '--is-ancestor', previous, sha)
    changes = run('git', 'diff', '--name-only', previous, sha, '--',
                  'migrations/', 'web/apps/telegram/drizzle/',
                  'web/apps/web/src/lib/yield-optimization/migrations/', 'deploy/hetzner/systemd/')
    if changes:
        raise RuntimeError(f'{family}: schema or service configuration changed since its release; explicit rollout required:\n{changes}')


def summary(message):
    print(message, flush=True)
    if path := os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(path, 'a') as out:
            out.write(message + '\n\n')


def rollback(host, stage, family, sha, before):
    previous = before['release']
    if previous == sha:
        summary(f'{family}: already ran this release; no different release to restore.')
        return
    summary(f'{family}: requesting guarded restoration to {previous}.')
    script = f'/opt/loyal/releases/{family}/{sha}/activate.sh'
    args = ('/bin/sh', script, family, previous, before['binary_sha256'], 'running')
    if family == 'realtime':
        args = ('/bin/bash', stage + '/activate-realtime.sh', previous, before['binary_sha256'])
    ssh(host, 'systemd-run', '--quiet', '--wait', '--pipe', '--collect',
        f'--setenv=LOYAL_DEPLOY_EXPECTED_RELEASE={sha}', *args, capture=False)
    restored = json.loads(ssh(host, 'python3', stage + '/rollout-health.py', 'snapshot', family))
    if restored['release'] != previous or restored['binary_sha256'] != before['binary_sha256']:
        raise RuntimeError(f'{family}: restoration could not be verified')
    summary(f'{family}: previous binary restored and service health verified.')


def rollout(sha, preflight_only=False, services=FAMILIES):
    if (not isinstance(services, (list, tuple)) or any(not isinstance(family, str) or family not in FAMILIES for family in services)
            or len(set(services)) != len(services)):
        raise RuntimeError('invalid production service selection')
    if SHA.fullmatch(sha) is None or run('git', 'rev-parse', 'HEAD') != sha:
        raise RuntimeError('run from the exact full merged commit SHA')
    require_head(sha)
    if not services:
        summary('No production services changed. No host access or deployment performed.')
        return
    artifacts = Path(os.environ.get('LOYAL_DEPLOY_ARTIFACT_DIR', ''))
    digests = {}
    if not preflight_only:
        if not os.environ.get('LOYAL_DEPLOY_ARTIFACT_DIR'):
            raise RuntimeError('LOYAL_DEPLOY_ARTIFACT_DIR must contain this run\'s verified artifacts')
        manifests = sorted({('workers' if family in FAMILIES[:5] else family) + '-SHA256SUMS' for family in services})
        run('sha256sum', '-c', *manifests, cwd=artifacts)
        for name in {BINARY[family] for family in services}:
            if not (artifacts / name).is_file():
                raise RuntimeError(f'missing verified artifact {name}')
        digests = {family: hashlib.sha256((artifacts / BINARY[family]).read_bytes()).hexdigest() for family in services}
    host = os.environ.get('LOYAL_WORKERS_SSH', 'loyal-workers')
    stage = ssh(host, 'mktemp', '-d', '/tmp/loyal-rollout.XXXXXXXX')
    if not re.fullmatch(r'/tmp/loyal-rollout\.[a-zA-Z0-9]+', stage):
        raise RuntimeError('unexpected host staging directory')
    try:
        ssh(host, 'chmod', '755', stage)
        run('scp', '-q', *(str(ROOT / 'deploy/hetzner' / path) for path in
                          ('rollout-health.py', 'rollout-snapshot.sql', 'activate-realtime.sh')), host + ':' + stage + '/')
        # Finish every preflight before changing the first service.
        for family in services:
            before = json.loads(ssh(host, 'python3', stage + '/rollout-health.py', 'snapshot', family))
            check_previous(family, before, sha, digests.get(family))
            summary(f'{family}: preflight passed on {before["release"]}.')
        if preflight_only:
            summary('Read-only preflight passed for selected services. No deployment performed.')
            return
        for family in services:
            require_head(sha)
            before = json.loads(ssh(host, 'python3', stage + '/rollout-health.py', 'snapshot', family))
            check_previous(family, before, sha, digests[family])
            before['expected_binary_sha256'] = digests[family]
            baseline = stage + '/' + family + '.json'
            ssh(host, 'tee', baseline, input=json.dumps(before))
            summary(f'{family}: deploying {sha}.')
            activated = False
            try:
                if family == 'realtime':
                    directory = '/opt/loyal/releases/realtime/' + sha
                    ssh(host, 'mkdir', '-p', directory)
                    run('scp', '-q', str(artifacts / BINARY[family]), host + ':' + directory + '/loyal-yield-realtime')
                    ssh(host, 'systemd-run', '--quiet', '--wait', '--pipe', '--collect',
                        f'--setenv=LOYAL_DEPLOY_EXPECTED_RELEASE={before["release"]}',
                        '/bin/bash', stage + '/activate-realtime.sh', sha, digests[family], capture=False)
                else:
                    env = dict(os.environ, LOYAL_DEPLOY_REQUIRE_HEAD='1', LOYAL_DEPLOY_EXPECTED_RELEASE=before['release'])
                    run('bash', 'scripts/deploy-workers.sh', family, sha, env=env, capture=False)
                activated = True
                ssh(host, 'python3', stage + '/rollout-health.py', 'watch', family,
                    '--sha', sha, '--baseline', baseline, capture=False)
            except Exception:
                summary(f'ROLLOUT HALTED at {family}. Remaining services will not deploy.')
                if not activated:
                    summary(f'{family}: activation did not report success. Inspect its host log for the guarded restoration result; no second activation attempted.')
                    raise
                try:
                    rollback(host, stage, family, sha, before)
                except Exception:
                    summary(f'{family}: safe restoration could not be verified. Preserve pending work and inspect host deployment logs; no further restart attempted.')
                raise
            summary(f'{family}: {sha} verified through the post-deploy watch.')
    finally:
        ssh(host, 'rm', '-rf', '--', stage)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('sha')
    parser.add_argument('--preflight-only', action='store_true')
    parser.add_argument('--services-json', default=json.dumps(FAMILIES))
    args = parser.parse_args()
    try:
        rollout(args.sha, args.preflight_only, json.loads(args.services_json))
    except Exception as error:
        summary('ROLLOUT FAILED: ' + str(error))
        sys.exit(1)
