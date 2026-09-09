#!/usr/bin/env python3
"""Shared-input economic decision diagnostic, not observer or cutover acceptance.

Generate inputs without asking either planner for decisions. Compare exact
selected order, route amounts, APYs, gain, priority and fee caps. A disagreement
returns nonzero; never treat known differences as passing acceptance.
"""
import argparse
import hashlib
import json
from pathlib import Path

USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
USDT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB'
STABLES = {'USDC': USDC, 'USDT': USDT,
           'CASH': 'CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH',
           'USDG': '2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH',
           'PYUSD': '2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo',
           'USDS': 'USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA'}


def fixtures():
    cases = []
    def case(name, cross=False, amount=9_000_000_000, count=1, supply=1_000_000_000_000,
             source_apy=81, target_apy=919, alternative=False, inflow=0, outflow=0):
        reserves = {
            'a': {'mint': USDC, 'supply': supply, 'apy': source_apy, 'inflow': 0, 'outflow': outflow},
            'b': {'mint': USDT if cross else USDC, 'supply': supply, 'apy': target_apy, 'inflow': inflow, 'outflow': 0},
        }
        if alternative:
            reserves['c'] = {'mint': USDC, 'supply': supply, 'apy': 500, 'inflow': 0, 'outflow': 0}
        cases.append({'name': name, 'reserves': reserves, 'vaults': [
            {'id': i+1, 'source': 'a', 'targets': ['b', 'c'] if alternative else ['b'],
             'amount': amount, 'collateral': amount, 'tenant': 'shared'} for i in range(count)]})
    for cross in (False, True):
        lane = 'cross' if cross else 'same'
        case(lane+'-profitable', cross)
        case(lane+'-unprofitable', cross, target_apy=81)
        case(lane+'-capacity-full', cross, inflow=20_000_000_000)
        case(lane+'-committed-inflow', cross, inflow=10_000_000_000)
        case(lane+'-committed-source-outflow', cross, outflow=100_000_000_000)
        case(lane+'-wave-capacity', cross, count=3)
        case(lane+'-alternative-after-capacity', cross, count=3, alternative=True)
        case(lane+'-large-reserve', cross, supply=1_000_000_000_000_000, amount=5_000_000_000_000)
        case(lane+'-tenant-conflict-limit', cross, count=65, amount=50_000_000)
        for amount in (999_999, 1_000_000, 10_000_000, 30_000_000, 50_000_000, 100_000_000, 500_000_000):
            for apy in (82, 200, 919):
                case(f'{lane}-threshold-{amount}-{apy}', cross, amount=amount, target_apy=apy)
    # Same-mint and cross-mint compete for the very same target capacity.
    case('mixed-source-capacity', False, count=3)
    mixed = cases[-1]
    mixed['reserves']['d'] = {**mixed['reserves']['a'], 'mint': USDT}
    mixed['vaults'][1]['source'] = 'd'
    # All six same-mint routes and all 30 directed cross-mint combinations.
    for source_name, source_mint in STABLES.items():
        for target_name, target_mint in STABLES.items():
            case(f'mint-{source_name}-to-{target_name}')
            cases[-1]['reserves']['a']['mint'] = source_mint
            cases[-1]['reserves']['b']['mint'] = target_mint
    # Repricing contention: unequal amounts and source yields, fixed seed.
    import random
    rng = random.Random(20260909)
    for i in range(32):
        case(f'repricing-{i:02d}', count=5)
        c = cases[-1]
        c['reserves']['d'] = {**c['reserves']['a'], 'apy': rng.choice([25, 150, 300])}
        for v in c['vaults']:
            v['source'] = rng.choice(['a', 'd'])
            v['amount'] = v['collateral'] = rng.choice([1_000_000_000, 5_000_000_000, 9_000_000_000])
    assert len({c['name'] for c in cases}) == len(cases)
    return {'schemaVersion': 1, 'cases': cases}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def load_json(text):
    def fields(pairs):
        out = {}
        for key, value in pairs:
            require(key not in out, 'duplicate JSON field')
            out[key] = value
        return out
    return json.loads(text, object_pairs_hook=fields)


def compare(fixture, rust, go):
    inputs = Path(fixture).read_bytes()
    cases = load_json(inputs)['cases']
    expected = [c['name'] for c in cases]
    require(len(expected) == len(set(expected)) and expected, 'missing or duplicate fixture cases')
    outputs = []
    for path, implementation in ((rust, 'rust'), (go, 'go')):
        artifact = load_json(Path(path).read_text())
        require(set(artifact) == {'schemaVersion', 'implementation', 'fixtureSha256', 'cases'}, 'unknown artifact fields')
        require(type(artifact['schemaVersion']) is int and artifact['schemaVersion'] == 1 and artifact['implementation'] == implementation, 'wrong producer/schema')
        require(artifact['fixtureSha256'] == hashlib.sha256(inputs).hexdigest(), 'fixture digest mismatch')
        require([c['name'] for c in artifact['cases']] == expected, 'missing/reordered/duplicate cases')
        for c in artifact['cases']:
            require(set(c) == {'name', 'selected'} and isinstance(c['selected'], list), 'invalid case')
            vaults = set()
            for d in c['selected']:
                require(set(d) == {'vaultId', 'source', 'target', 'route', 'amount', 'sourceApy', 'targetApy', 'edge', 'netGain', 'priority', 'feeCap'}, 'invalid decision fields')
                for key in ('vaultId', 'amount', 'sourceApy', 'targetApy', 'edge', 'netGain', 'priority', 'feeCap'):
                    require(type(d[key]) is int and d[key] >= 0, 'invalid numeric decision field')
                require(d['vaultId'] > 0 and d['vaultId'] not in vaults, 'invalid/duplicate selected vault')
                vaults.add(d['vaultId'])
                require(d['route'] in ('same_mint', 'cross_mint_jupiter'), 'unknown route')
                require(all(isinstance(d[k], str) and d[k] for k in ('source', 'target')), 'missing reserve')
        outputs.append(artifact['cases'])
    differences = [{'name': r['name'], 'rust': r['selected'], 'go': g['selected']}
                   for r, g in zip(*outputs) if r != g]
    return {'fixtureSha256': hashlib.sha256(inputs).hexdigest(), 'caseCount': len(cases),
            'matchingCases': len(cases)-len(differences), 'mismatchingCases': len(differences),
            'differences': differences}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--generate')
    parser.add_argument('--fixture')
    parser.add_argument('--rust')
    parser.add_argument('--go')
    args = parser.parse_args()
    if args.generate:
        Path(args.generate).write_text(json.dumps(fixtures(), sort_keys=True, indent=2)+'\n')
        return 0
    report = compare(args.fixture, args.rust, args.go)
    print(json.dumps(report, indent=2))
    return int(bool(report['mismatchingCases']))


if __name__ == '__main__':
    raise SystemExit(main())
