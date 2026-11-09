import base64
import gc
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile

BASE = Path(os.environ['VAULT_REPLAY_WORKDIR']).resolve()
sys.path.insert(0, str(BASE / 'deps'))
from solders.account import Account
from solders.compute_budget import ComputeBudget
from solders.litesvm import LiteSVM
from solders.pubkey import Pubkey
from solders.transaction import VersionedTransaction
from solders.transaction_metadata import FailedTransactionMetadata

os.umask(0o077)
cgroup = Path('/sys/fs/cgroup') / Path('/proc/self/cgroup').read_text().strip().split('::', 1)[1].lstrip('/')
limits = {name: (cgroup / name).read_text().strip() for name in ('cpu.max', 'memory.max', 'memory.swap.max', 'pids.max')}
assert limits == {'cpu.max': '50000 100000', 'memory.max': '805306368', 'memory.swap.max': '0', 'pids.max': '64'}, limits
assert Path('/proc/net/route').read_text().count('\n') == 1, 'external networking must be disabled'
file = Path(sys.argv[1])
equity = int(sys.argv[2])
leverage = float(sys.argv[3]) if len(sys.argv) > 3 else 1.5
assert equity in (10000, 50000, 100000) and leverage in (1.5, 2)
f = json.loads(file.read_text())
assert f['status'] == 'EXPORTED' and len(set(f['snapshotSlots'])) == 1
case = Path(tempfile.mkdtemp(prefix=f'{file.stem}-{equity}-', dir=BASE))
output = BASE / f'{file.stem}-{equity}-{leverage}.json'
out = {'lane': f['lane'], 'equity': equity, 'leverage': leverage, 'mode': 'offline-native', 'runtime': 'solders 0.29.0', 'resourceLimits': limits, 'runtimeBudgetFromUnchangedV1Wire': True, 'snapshotSlots': f['snapshotSlots'], 'networkSubmissions': 0, 'setup': [], 'steps': [], 'directory': str(case)}
svm = LiteSVM().with_sigverify(False).with_blockhash_check(False).with_transaction_history(0)
addresses = [a['Address'] for a in f['snapshotAccounts'] if not a.get('Owner', '').startswith('BPFLoader')]
key = Pubkey.from_string
for a in sorted(f['snapshotAccounts'], key=lambda x: x.get('Executable', False)):
    owner = a.get('Owner')
    if owner and owner != 'NativeLoader1111111111111111111111111111111':
        svm.set_account(key(a['Address']), Account(a['Lamports'], base64.b64decode(a.get('Data') or ''), key(owner), a['Executable']))
del f['snapshotAccounts']
gc.collect()


def snapshot():
    accounts = []
    for address in addresses:
        a = svm.get_account(key(address))
        row = {'Address': address, 'Lamports': 0, 'Data': None, 'Executable': False}
        if a is not None:
            row.update(Owner=str(a.owner), Lamports=a.lamports, Data=base64.b64encode(a.data).decode(), Executable=a.executable)
        accounts.append(row)
    return accounts


def digest():
    return hashlib.sha256(json.dumps(snapshot(), sort_keys=True).encode()).hexdigest()


def token_amount(address):
    a = svm.get_account(key(address))
    assert a is not None
    return struct.unpack_from('<Q', a.data, 64)[0]


def set_token_amount(address, amount):
    a = svm.get_account(key(address))
    data = bytearray(a.data)
    struct.pack_into('<Q', data, 64, amount)
    svm.set_account(key(address), Account(a.lamports, bytes(data), a.owner, a.executable, a.rent_epoch))


def adjust_available(address, delta):
    a = svm.get_account(key(address))
    data = bytearray(a.data)
    n = struct.unpack_from('<Q', data, 224)[0] + delta
    assert n >= 0, 'captured pool cannot support the hypothetical existing loan'
    struct.pack_into('<Q', data, 224, n)
    svm.set_account(key(address), Account(a.lamports, bytes(data), a.owner, a.executable, a.rent_epoch))


def transaction(encoded):
    message = base64.b64decode(encoded)
    assert message[0] == 0x81, 'expected exact worker V1 wire'
    wire = message + bytes(64)
    tx = VersionedTransaction.from_bytes(wire)
    assert bytes(tx) == wire, 'wire roundtrip mismatch'
    # This native release needs the inline V1 limits applied to its runtime.
    # Use exactly what the unchanged wire requests, never a larger allowance.
    config = tx.message.config
    budget = ComputeBudget(True)
    budget.compute_unit_limit = config.compute_unit_limit
    budget.heap_size = config.heap_size or 32768
    svm.with_compute_budget(budget)
    return tx


def meta_result(result):
    failed = isinstance(result, FailedTransactionMetadata)
    meta = result.meta() if failed else result
    return {'success': not failed, 'error': str(result.err()) if failed else None, 'logs': meta.logs(), 'computeUnits': meta.compute_units_consumed(), 'networkFeeLamports': meta.fee()}


def send(encoded):
    return meta_result(svm.send_transaction(transaction(encoded)))


def build(action, amount=0, **overrides):
    value = dict(Action=action, Lane=f['lane'], Slot=svm.get_clock().slot, Amount=amount, Accounts=snapshot(), Template=f['guardedExit']['Swap'], DebtTemplate=f.get('debtSwap'), ResidueTemplate=f.get('residueSwap'))
    value.update(overrides)
    (case / 'input.json').write_text(json.dumps(value))
    (case / 'output.json').unlink(missing_ok=True)
    env = {'PATH': '/usr/local/bin:/usr/bin:/bin', 'GOMAXPROCS': '1', 'GOMEMLIMIT': '64MiB', 'OFFLINE_REPLAY_INPUT': str(case / 'input.json'), 'OFFLINE_REPLAY_OUTPUT': str(case / 'output.json')}
    run = subprocess.run([str(BASE / 'offline-probe'), '-test.run=^TestOfflineLeveragedReplayStep$', '-test.timeout=15s'], env=env, capture_output=True, text=True, timeout=20)
    with (case / 'go.log').open('a') as log:
        log.write(run.stdout + run.stderr)
    assert run.returncode == 0, f'Go driver failed: {run.returncode}: {run.stdout}{run.stderr}'
    return json.loads((case / 'output.json').read_text())


def inspect():
    state = build('inspect')
    assert not state.get('error'), state.get('error')
    return state


def setup(encoded):
    result = send(encoded)
    out['setup'].append(result)
    assert result['success'], f"setup failed: {result['error']}"


def save():
    out['memoryPeakBytes'] = int((cgroup / 'memory.peak').read_text())
    out['memoryEvents'] = (cgroup / 'memory.events').read_text()
    tmp = output.with_suffix('.tmp')
    tmp.write_text(json.dumps(out, indent=2))
    tmp.replace(output)


try:
    for message in f.get('initializationMessages', []):
        setup(message)
    setup(f['refreshMessage'])
    state = inspect()
    before_funding = snapshot()
    original_debt = state['position']['DebtRaw']
    fund = f['portionCollateralRaw'] * int(equity * leverage / 1000)
    out['hypotheticalCollateralAddedRaw'] = fund
    set_token_amount(state['collateralAccount'], token_amount(state['collateralAccount']) + fund)
    deposit = build('deposit', fund)
    assert not deposit.get('error'), deposit.get('error')
    setup(deposit['message'])
    debt = int(equity * (leverage - 1) * 1000000)
    if os.environ.get('VAULT_REPLAY_CARRY_STRESS') == '1':
        # Charge six hours at 100% APR before the first sale. Repaying during
        # the unwind can only reduce this fixed-rate carry charge.
        carry = math.ceil(debt * math.expm1(6 / (365 * 24)))
        debt += carry
        out['carryStress'] = {'seconds': 21600, 'annualBPS': 10000, 'chargedUpfrontRaw': carry}
    borrow = build('borrow', debt)
    assert not borrow.get('error'), borrow.get('error')
    first = send(borrow['message'])
    out['initialBorrowAttempt'] = first
    if not first['success']:
        assert any('BorrowingAboveUtilizationRateDisabled' in line for line in first['logs']), first['error']
        added = 1000000000000000
        reserve, supply = borrow['debtReserve'], borrow['debtLiquiditySupply']
        adjust_available(reserve, added)
        set_token_amount(supply, token_amount(supply) + added)
        borrow = build('borrow', debt)
        assert not borrow.get('error'), borrow.get('error')
        setup(borrow['message'])
        adjust_available(reserve, -added)
        set_token_amount(supply, token_amount(supply) - added)
        out['hypotheticalSetupLiquidity'] = {'raw': added, 'removedBeforeWithdrawal': True}
    else:
        out['setup'].append(first)
    state = inspect()
    out['borrowedPosition'] = state
    out['hypotheticalCashRemovedRaw'] = token_amount(state['cashAccount'])
    set_token_amount(state['cashAccount'], 0)
    if state['debtCashAccount'] != state['cashAccount']:
        out['hypotheticalDebtCashRemovedRaw'] = token_amount(state['debtCashAccount'])
        set_token_amount(state['debtCashAccount'], 0)
    state = inspect()
    out['start'] = state
    quotes = {}
    out['simulatedQuotes'] = []
    for _ in range(4):
        screen = build('screen', state['collateralRaw'], Accounts=before_funding, AddedCollateral=fund, AddedDebt=state['position']['DebtRaw']-original_debt, QuoteOutputs=quotes)
        out['investmentScreen'] = screen
        request = screen.get('quoteRequest')
        if not request:
            break
        q = build('quote', request['amount'], QuoteToDebt=request['toDebt'])
        assert not q.get('error'), q.get('error')
        original = token_amount(state['collateralAccount'])
        recipient = state['debtCashAccount'] if request['toDebt'] else state['cashAccount']
        before = token_amount(recipient)
        before_digest = digest()
        try:
            set_token_amount(state['collateralAccount'], request['amount'])
            simulated = svm.simulate_transaction(transaction(q['message']))
        finally:
            set_token_amount(state['collateralAccount'], original)
        assert digest() == before_digest, 'quote probe changed the bank'
        if isinstance(simulated, FailedTransactionMetadata):
            quote = dict(request, **meta_result(simulated))
            out['simulatedQuotes'].append(quote)
            break
        balances = dict(simulated.post_accounts())
        after = struct.unpack_from('<Q', balances[key(recipient)].data, 64)[0]
        quote = dict(request, **meta_result(simulated.meta()), receivedRaw=after-before)
        out['simulatedQuotes'].append(quote)
        if not request['toDebt']:
            out['fullExitQuote'] = quote
        quotes[request['key']] = after-before
    assert not out['investmentScreen'].get('error'), out['investmentScreen'].get('error')
    for i in range(400):
        residue = state['collateralRaw'] == 0 and state['position']['DebtRaw'] == 0 and state['debtCashAccount'] != state['cashAccount'] and state['debtCashRaw'] > 0
        if state['collateralRaw'] == 0 and state['position']['DebtRaw'] == 0 and not residue:
            out['status'] = 'COMPLETED'
            break
        step = build('residue' if residue else 'withdraw')
        if step.get('error'):
            out['status'], out['hold'] = 'HELD_BEFORE_SEND', step['error']
            break
        assert step.get('persistSignValidated'), 'wire validation missing'
        relevant = (state['cashAccount'], state['collateralAccount'], state['obligation'], state['debtCashAccount'])
        before_accounts = {a: svm.get_account(key(a)) for a in relevant}
        recipient = state['debtCashAccount'] if step['kind'] == 2 else state['cashAccount']
        before_cash = token_amount(recipient)
        result = send(step['message'])
        after = inspect()
        received = token_amount(recipient)-before_cash
        repaid = state['position']['DebtRaw']-after['position']['DebtRaw']
        row = dict(index=i+1, kind=step['kind'], sourceAmount=step['amountRaw'], requiredFloor=step['floorRaw'], receivedRaw=received, debtRepaidRaw=repaid, remainingDebtRaw=after['position']['DebtRaw'], remainingCollateralRaw=after['collateralRaw'], **result)
        if result['success']:
            assert received + repaid >= step['floorRaw'], 'real conversion proceeds below floor'
            assert after['collateralRaw'] < state['collateralRaw'] or after['position']['DebtRaw'] < state['position']['DebtRaw'] or (residue and after['debtCashRaw'] < state['debtCashRaw']), 'no financial progress'
        else:
            assert all(svm.get_account(key(a)) == old for a, old in before_accounts.items()), 'failed step changed vault balances or debt'
            row['failedStepPreservedBalances'] = True
            failure = next((line for line in result['logs'] if ' failed:' in line), '')
            out['status'] = 'PRICE_REFUSED' if failure == 'Program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 failed: custom program error: 0x1771' else 'EXECUTION_FAILED'
        out['steps'].append(row)
        state = after
        out['end'] = state
        if state['cashRaw'] >= 10_000_000_000 and 'partialFunding' not in out:
            out['partialFunding'] = {'step': i+1, 'cashRaw': state['cashRaw'], 'remainingCollateralRaw': state['collateralRaw'], 'remainingDebtRaw': state['position']['DebtRaw']}

        if (i+1) % 10 == 0:
            save()
            print(json.dumps({'progress': i+1, 'lane': f['lane'], 'equity': equity, 'debt': state['position']['DebtRaw'], 'memoryPeakMiB': round(out['memoryPeakBytes']/1048576, 1)}), flush=True)
        if not result['success']:
            break
    out.setdefault('status', 'STEP_LIMIT')
    out['end'] = state
    if out['status'] == 'COMPLETED':
        assert state['position']['DebtRaw'] == 0 and state['collateralRaw'] == 0
    previous_path = file.with_name(f'{file.stem}.leveraged-{equity}-{leverage}.json')
    if previous_path.exists() and os.environ.get('VAULT_REPLAY_CARRY_STRESS') != '1':
        previous = json.loads(previous_path.read_text())
        if previous.get('status') in ('COMPLETED', 'PRICE_REFUSED'):
            assert out['status'] == previous['status'], 'native outcome differs from reference'
            assert len(out['steps']) == len(previous['steps']), 'native step count differs from reference'
            for field in ('cashRaw', 'collateralRaw'):
                assert state[field] == previous['end'][field], 'native balance differs from reference'
            assert state['position']['DebtRaw'] == previous['end']['position']['DebtRaw'], 'native debt differs from reference'
            out['matchesPriorCompletedResult'] = True
except Exception as error:
    out['status'], out['error'] = 'TEST_FAILED', repr(error)
finally:
    save()
    print(json.dumps({'lane': f['lane'], 'equity': equity, 'leverage': leverage, 'status': out['status'], 'error': out.get('error'), 'hold': out.get('hold'), 'steps': len(out['steps']), 'memoryPeakMiB': round(out['memoryPeakBytes']/1048576, 1), 'output': str(output)}), flush=True)
if out['status'] in ('TEST_FAILED', 'EXECUTION_FAILED', 'STEP_LIMIT'):
    raise SystemExit(1)
