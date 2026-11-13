#!/usr/bin/env python3
"""Select affected production services from committed dependency graphs."""
import argparse
import json
import os
from pathlib import PurePosixPath
import re
import subprocess
import tarfile
import tomllib


SERVICES = ('backyard', 'earn-max', 'retail', 'observer', 'telegram', 'web', 'admin', 'stats', 'realtime')
GO_MODULE = 'github.com/loyal-labs/loyal-yield-routing/go/workers'
GO_ROOTS = {service: 'go/workers/cmd/loyal-' + ('engine' if service in SERVICES[:3] else service)
            for service in SERVICES[:-1]}
APPS = {'web': ('web/apps/loyal', 'web/apps/web'), 'admin': ('web/apps/admin',),
        'stats': ('web/apps/dashboard',), 'mobile': ('web/apps/mobile',)}


def git(*args):
    return subprocess.check_output(['git', *args]).decode()


def read(ref, path):
    result = subprocess.run(['git', 'show', ref + ':' + path], capture_output=True, text=True)
    return result.stdout if result.returncode == 0 else None


def closure(graph, roots):
    visited = set()
    pending = list(roots)
    while pending:
        node = pending.pop()
        if node not in visited:
            visited.add(node)
            pending.extend(graph.get(node, ()))
    return visited


def dependency_owners(ref, imports):
    go = {path: [name.replace(GO_MODULE, 'go/workers', 1) for name in names if name.startswith(GO_MODULE + '/')]
          for path, names in imports.items()}
    owners = {service: closure(go, [root]) for service, root in GO_ROOTS.items()}
    paths = git('ls-tree', '-r', '--name-only', ref).splitlines()
    packages = {}
    for path in paths:
        if path.startswith('web/') and path.endswith('/package.json'):
            package = json.loads(read(ref, path))
            if package.get('name'):
                packages[package['name']] = (str(PurePosixPath(path).parent), package)
    web = {directory: [packages[name][0] for field in ('dependencies', 'devDependencies', 'peerDependencies')
                       for name in package.get(field, {}) if name in packages]
           for directory, package in packages.values()}
    directories = sorted(web, key=len, reverse=True)
    tracked = set(paths)
    def owner(path):
        return next((directory for directory in directories if path.startswith(directory + '/')), None)
    # Mobile also consumes workspace packages through aliases without declaring them in package.json.
    with subprocess.Popen(['git', 'archive', ref, 'web'], stdout=subprocess.PIPE) as archive:
        with tarfile.open(fileobj=archive.stdout, mode='r|') as files:
            for member in files:
                if not member.isfile() or not member.name.endswith(('.ts', '.tsx', '.js', '.mjs', '.cjs', '/tsconfig.json')):
                    continue
                if (member.name.endswith(('.test.ts', '.test.tsx', '.spec.ts', '.spec.tsx'))
                        or any(part in member.name for part in ('/__tests__/', '/e2e/', '/scripts/'))):
                    continue
                source_owner = owner(member.name)
                if not source_owner:
                    continue
                source = files.extractfile(member).read().decode(errors='replace')
                web[source_owner].append(member.name)
                web[member.name] = []
                for name in re.findall(r'''["'](@[^/"']+/[^/"']+)''', source):
                    if name in packages:
                        web[member.name].append(packages[name][0])
                for relative in re.findall(r'''(?:from\s*|import\s*(?:\(\s*)?|require\s*\(\s*)["'](\.{1,2}/[^"']+)["']''', source):
                    target = os.path.normpath(str(PurePosixPath(member.name).parent) + '/' + relative)
                    resolved = next((target + suffix for suffix in ('', '.ts', '.tsx', '.js', '.mjs', '/index.ts', '/index.tsx', '/index.js')
                                     if target + suffix in tracked), None)
                    if resolved:
                        web[member.name].append(resolved)
        if archive.wait() != 0:
            raise RuntimeError('cannot read committed workspace imports')
    for service, roots in APPS.items():
        owners.setdefault(service, set()).update(closure(web, roots))
    cargo = {}
    for path in paths:
        if path.startswith('crates/') and path.endswith('/Cargo.toml'):
            package = tomllib.loads(read(ref, path))
            directory = str(PurePosixPath(path).parent)
            dependencies = list(package.get('dependencies', {}).values()) + list(package.get('build-dependencies', {}).values())
            cargo[directory] = [os.path.normpath(directory + '/' + item['path']) for item in dependencies
                                if isinstance(item, dict) and 'path' in item]
    owners['realtime'] = closure(cargo, ['crates/loyal-yield-realtime'])
    return owners


def changed_paths(base, head, cumulative):
    if not cumulative:
        return set(filter(None, git('diff', '--name-only', '--no-renames', '-z', base, head).split('\0')))
    # Include intermediate changes even when reverted after a partial deployment.
    paths = set()
    for commit in git('rev-list', '--first-parent', base + '..' + head).splitlines():
        paths.update(filter(None, git('diff-tree', '--no-commit-id', '--name-only', '--no-renames', '-r', '-z', commit + '^', commit).split('\0')))
    return paths


def select(paths, before, after):
    deploy, checks = set(), set()
    contracts = False
    rust = False
    for path in paths:
        deployable = True
        affected = {service for service in after.keys() | before.keys()
                    if any(path == directory or path.startswith(directory + '/')
                           for directory in after.get(service, set()) | before.get(service, set()))}
        test_only = (path.endswith(('_test.go', '.test.ts', '.test.tsx', '.spec.ts', '.spec.tsx'))
                     or any(part in path for part in ('/__tests__/', '/tests/', '/e2e/', '/scripts/')))
        if path.startswith(('.github/workflows/', 'scripts/production-', 'scripts/ci-', 'scripts/deployment_rollout',
                            'scripts/production_rollout', 'deploy/hetzner/rollout-', 'deploy/hetzner/activate',
                            'deploy/hetzner/retain-web-assets.sh', 'deploy/hetzner/install-rules.sh',
                            'scripts/deploy-workers.sh', 'scripts/verify-workers-v2.sh')):
            contracts = True
            checks.update(SERVICES)
            checks.add('mobile')
            # CI/deployment orchestration changes do not change the running application.
            continue
        if path in ('go/workers/go.mod', 'go/workers/go.sum'):
            affected.update(SERVICES[:-1])
        elif path.startswith('go/workers/') and path not in ('go/workers/Dockerfile', 'go/workers/Makefile') and not affected:
            affected.update(SERVICES[:3] if path.startswith('go/workers/cmd/loyal-engine/') else SERVICES[:-1])
            deployable = False  # Unreferenced tools/packages still get checked, but change no service binary.
        if path in ('web/package.json', 'web/bun.lock', 'web/bun.lockb', 'web/tsconfig.json', 'web/turbo.json') or path.startswith('scripts/web/'):
            affected.update(('web', 'admin', 'stats', 'mobile'))
        if path.startswith('web/apps/loyal/vite/') or path in ('web/apps/loyal/vite.preset.ts', 'web/apps/loyal/package.json', 'web/apps/loyal/bun.lock'):
            affected.update(('web', 'admin', 'stats'))
        if path.startswith('web/apps/telegram/'):
            checks.add('telegram')
        if path.startswith(('web/apps/admin/src/app/(admin)/journeys/', 'web/scripts/observability/')):
            checks.add('journeys')
        if path.startswith(('migrations/', 'web/apps/telegram/drizzle/')):
            affected.update(SERVICES)
        if path.startswith('deploy/hetzner/systemd/'):
            affected.update(service for service in SERVICES if '/loyal-' + service in path)
            if '/loyal-web-cron' in path:
                affected.add('web')
            contracts = True
        if path.startswith('deploy/hetzner/monitoring/'):
            contracts = True
            continue
        if path.startswith('scripts/') and not path.startswith('scripts/web/'):
            contracts = True
            checks.update(SERVICES[:-1])
        if path in ('Cargo.toml', 'Cargo.lock', 'rust-toolchain.toml', 'Dockerfile.light-workers'):
            affected.add('realtime')
        if path.startswith(('crates/squads-test-harness/', 'crates/mock-yield-protocols-program/', 'crates/loyal-actions/',
                            'crates/loyal-solana-env/', 'crates/loyal-hub-swap-program/',
                            'crates/loyal-hub-abi/', 'crates/loyal-voltr-rwa-nav-adaptor/')) or path in ('Cargo.toml', 'Cargo.lock', 'rust-toolchain.toml', 'scripts/build-svm-fixtures.sh'):
            rust = True
            checks.update(SERVICES[:3])
        if path in ('go/workers/Dockerfile', 'go/workers/Makefile'):
            affected.update(SERVICES[:5])
        if path.startswith('web/packages/') and not affected:
            affected.update(('web', 'admin', 'stats', 'mobile'))
        # Documentation never changes deployed bytes; directories above may also own docs.
        if path.endswith(('.md', '.mdx')) or path.startswith(('docs/', '.agents/', '.codex/')):
            continue
        checks.update(affected)
        if not test_only and deployable:
            deploy.update(affected & set(SERVICES))
    return deploy, checks, contracts, rust


def plan(base, head, cumulative=False):
    imports = json.loads(subprocess.check_output(['go', 'run', 'scripts/ci-go-imports.go', base, head],
                                                env=dict(os.environ, GO111MODULE='off', GOTOOLCHAIN='local')))
    before, after = dependency_owners(base, imports[base]), dependency_owners(head, imports[head])
    paths = changed_paths(base, head, cumulative)
    deploy, checks, contracts, rust = select(paths, before, after)
    worker_services = [service for service in SERVICES[:5] if service in checks]
    packages = {service: sorted('./' + path.removeprefix('go/workers/') for path in after[service] | {GO_ROOTS[service]}
                                 if path.startswith('go/workers/') and not path.endswith('.go') and path in imports[head])
                for service in SERVICES[:-1]}
    owned_packages = {package for selected in packages.values() for package in selected}
    extra_packages = set()
    for path in paths:
        if path.startswith('go/workers/'):
            for parent in PurePosixPath(path).parents:
                if str(parent) in imports[head]:
                    package = './' + str(parent).removeprefix('go/workers/')
                    if package not in owned_packages:
                        extra_packages.add(package)
                    break
    return {
        'sha': head, 'base': base, 'services': [service for service in SERVICES if service in deploy],
        'checks': sorted(checks), 'contracts': contracts, 'rust': rust,
        'workers': bool(worker_services), 'engine': bool(set(SERVICES[:3]) & checks),
        'worker_services': worker_services,
        'worker_packages': sorted({package for service in worker_services for package in packages[service]} | extra_packages),
        'web': bool(set(('web', 'admin', 'stats', 'mobile')) & checks),
        'web_services': [service for service in ('web', 'admin', 'stats') if service in checks],
        'web_packages': {service: packages[service] for service in ('web', 'admin', 'stats')},
        'mobile': 'mobile' in checks, 'journeys': bool({'web', 'journeys'} & checks), 'realtime': 'realtime' in checks,
    }


def production_base(head):
    repository = os.environ['GITHUB_REPOSITORY']
    page = 1
    while True:
        result = json.loads(subprocess.check_output(['gh', 'api',
            f'repos/{repository}/actions/workflows/production.yml/runs?branch=main&status=success&per_page=100&page={page}']))
        runs = result['workflow_runs']
        for run in runs:
            if run['event'] not in ('push', 'workflow_dispatch') or run['head_repository']['full_name'] != repository:
                continue
            sha = run['head_sha']
            if sha == head:
                continue
            if subprocess.run(['git', 'merge-base', '--is-ancestor', sha, head], capture_output=True).returncode == 0:
                return sha
        if len(runs) < 100:
            raise RuntimeError('no successful ancestor production run; establish the deployment baseline explicitly')
        page += 1


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base')
    parser.add_argument('--head', default='HEAD')
    parser.add_argument('--production', action='store_true')
    parser.add_argument('--output')
    args = parser.parse_args()
    head = git('rev-parse', args.head).strip()
    base = args.base or (production_base(head) if args.production else None)
    if not base:
        parser.error('--base is required for PR scope')
    result = plan(git('rev-parse', base).strip(), head, args.production)
    encoded = json.dumps(result, sort_keys=True)
    if args.output:
        with open(args.output, 'w') as out:
            out.write(encoded + '\n')
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as out:
            for key, value in result.items():
                out.write(f'{key}={json.dumps(value, separators=(",", ":")) if not isinstance(value, str) else value}\n')
    print(encoded)
