from pathlib import Path
prefix = Path(__file__).with_name('withdrawal.py').read_text().split('\ntry:\n    for message')[0]
exec(compile(prefix, str(Path(__file__).with_name('withdrawal.py')), 'exec'))
if f['lane']=='AUTO/AUTO/PYUSD':
    import copy
    # Use the already captured PYUSD/USDC Manifest leg, with its original
    # account list and pool state. No new liquidity or improved price is seeded.
    template = copy.deepcopy(f['guardedExit']['Swap'])
    ix = template['Instruction']; ac = ix['accounts']
    start = next(i for i,a in enumerate(ac) if a['pubkey']=='MNFSTqtC93rEfYHB6hF82sKdZpUDFWkViLByLd1k1Ms')
    assert start>12
    fixed=copy.deepcopy(ac[:12])
    fixed[2]['pubkey']=f['debtSwap']['Instruction']['accounts'][5]['pubkey']
    fixed[3]['pubkey']=fixed[2]['pubkey']
    fixed[6]['pubkey']='2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo'
    fixed[8]['pubkey']='TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb'
    remaining=ac[start:]
    for account in remaining:
        if account['pubkey']=='48XF4k6hk7p41XcKdXDxgEYYCvoDe5QrWAH9bdUfA6fQ': account['pubkey']=fixed[2]['pubkey']
        if account['pubkey']==fixed[0]['pubkey']: account['pubkey']=fixed[1]['pubkey']
    ix['accounts']=fixed+remaining
    data=base64.b64decode(ix['data'])
    assert struct.unpack_from('<I',data,31)[0]==3
    assert data[-6:-4]==bytes.fromhex('7401')
    ix['data']=base64.b64encode(data[:31]+struct.pack('<I',1)+data[-6:-2]+bytes([0,1])).decode()
    template['Action']='SWAP_DEBT_TO_USDC_STEP'
    template['Policy']=f['residueSwap']['Policy']
    f['residueSwap']=template
for message in f.get('initializationMessages', []):
    setup(message)
setup(f['refreshMessage'])
state = inspect()
rows = []
def quote(amount, mode):
    q = build('quote', amount, QuoteToDebt=mode=='debt', QuoteDebtToUSDC=mode=='residue')
    assert not q.get('error'), q
    source = state['debtCashAccount'] if mode=='residue' else state['collateralAccount']
    target = state['debtCashAccount'] if mode=='debt' else state['cashAccount']
    orig, before, digest_before = token_amount(source), token_amount(target), digest()
    try:
        set_token_amount(source, amount)
        result = svm.simulate_transaction(transaction(q['message']))
    finally:
        set_token_amount(source, orig)
    assert digest() == digest_before
    row = {'mode':mode,'input':amount}
    if isinstance(result, FailedTransactionMetadata):
        row.update(meta_result(result))
        rows.append(row)
        raise Exception(row)
    balances = dict(result.post_accounts())
    value = struct.unpack_from('<Q', balances[key(target)].data, 64)[0]-before
    row['output']=value
    rows.append(row)
    return value

quotes={}
fund=f['portionCollateralRaw']*int(equity*leverage/1000)
for i in range(10):
    screen=build('screen', AddedCollateral=fund, AddedDebt=int(equity*(leverage-1)*1000000), QuoteOutputs=quotes)
    assert not screen.get('error'), screen
    request=screen.get('quoteRequest')
    if not request: break
    mode='residue' if request['fromDebt'] else 'debt' if request['toDebt'] else 'cash'
    quotes[request['key']]=quote(request['amount'], mode)
else: raise Exception('checker exceeded bounded quote count')
result={'lane':f['lane'],'equity':equity,'leverage':leverage,'screen':screen,'quotes':rows,'memoryPeakBytes':int((cgroup/'memory.peak').read_text()),'memoryEvents':(cgroup/'memory.events').read_text()}
(BASE/f'corrected-screen-{file.stem}-{equity}-{leverage}.json').write_text(json.dumps(result,indent=2))
print(json.dumps(result))
