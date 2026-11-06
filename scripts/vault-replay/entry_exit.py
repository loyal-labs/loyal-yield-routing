from pathlib import Path
source=Path(__file__).with_name('withdrawal.py')
exec(compile(source.read_text().split('\ntry:\n    for message')[0],str(source),'exec'))
# The purchase and sale execute against this same captured bank. Only the
# hypothetical depositor's cash is seeded; pool balances are never altered.
for message in f.get('initializationMessages',[]): setup(message)
setup(f['refreshMessage'])
state=inspect()
start_collateral=token_amount(state['collateralAccount'])
start_cash=token_amount(state['cashAccount'])
set_token_amount(state['cashAccount'],start_cash+equity*1_000_000)
spent=0
buys=[]
for i in range(equity//1000):
 before=token_amount(state['collateralAccount'])
 result=send(f['buy']['message'])
 buys.append(result)
 if not result['success']: break
 assert token_amount(state['collateralAccount'])>before
 spent+=1_000_000_000
purchased=token_amount(state['collateralAccount'])-start_collateral
assert purchased>0,buys
post_purchase_digest=digest()
quotes={}
rows=[]
for i in range(10):
 # Pass the post-purchase pool state and the pre-purchase custody balance.
 # Adding the actual purchased amount reconstructs the complete holding.
 accounts=snapshot()
 for account in accounts:
  if account['Address']==state['collateralAccount']:
   data=bytearray(base64.b64decode(account['Data']))
   struct.pack_into('<Q',data,64,start_collateral)
   account['Data']=base64.b64encode(data).decode()
 screen=build('screen',Accounts=accounts,AddedCollateral=purchased,QuoteOutputs=quotes)
 assert not screen.get('error'),screen
 request=screen.get('quoteRequest')
 if not request: break
 assert not request['fromDebt'] and not request['toDebt'],request
 q=build('quote',request['amount'])
 assert not q.get('error'),q
 source_balance=token_amount(state['collateralAccount'])
 cash_before=token_amount(state['cashAccount'])
 try:
  set_token_amount(state['collateralAccount'],request['amount'])
  simulated=svm.simulate_transaction(transaction(q['message']))
 finally:
  set_token_amount(state['collateralAccount'],source_balance)
 assert digest()==post_purchase_digest
 assert not isinstance(simulated,FailedTransactionMetadata),meta_result(simulated)
 balances=dict(simulated.post_accounts())
 result=struct.unpack_from('<Q',balances[key(state['cashAccount'])].data,64)[0]-cash_before
 quotes[request['key']]=result
 rows.append({'input':request['amount'],'output':result})
else: raise AssertionError('unbounded quote collection')
# Execute the actual whole-position exit used by the screen. This is a stress
# price measurement; production sells capped portions instead.
q=build('quote',purchased)
before=token_amount(state['cashAccount'])
result=send(q['message'])
assert result['success'],result
received=token_amount(state['cashAccount'])-before
report={'lane':f['lane'],'equityRequested':equity,'spentRaw':spent,'purchasedRaw':purchased,'cashRecoveredRaw':received,'realizedCostRaw':spent-received,'purchases':len([x for x in buys if x['success']]),'purchaseHold':None if buys[-1]['success'] else buys[-1],'screen':screen,'quotes':rows,'postPurchasePoolStatePreserved':True,'snapshotSlots':f['snapshotSlots'],'wirePreserved':True,'networkSubmissions':0,'memoryPeakBytes':int((cgroup/'memory.peak').read_text()),'memoryEvents':(cgroup/'memory.events').read_text()}
(BASE/f'entry-exit-{file.stem}-{equity}.json').write_text(json.dumps(report,indent=2))
print(json.dumps({k:v for k,v in report.items() if k not in ('purchaseHold','screen')}))
print(json.dumps({'eligible':screen.get('eligible'),'screenReason':screen.get('screenReason')}))
