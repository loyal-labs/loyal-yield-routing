import assert from 'node:assert/strict';

export const onreSwapOriginal = operation => ({
  'onre-entry-swap': 'EhNzCAGfDdXKw8QJKqHMdN9tdfRiCjbtfQcwBCYMKA3j',
  'onre-return-swap': 'BLFT8rtg919byj5dCqRy6vVbWmRqqNN336DMERw9Cz64',
})[operation];

// Transform only the first decoded installed constraint. Its existing pubkeys,
// raw cap and slippage bound are retained; the sibling object is untouched.
export function repairOnreSwap(constraints) {
  assert.equal(constraints.length, 2);
  const original=constraints[0];
  assert.equal(original.programId.toBase58(),'JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4');
  assert.equal(original.dataConstraints.length,4);
  const accountConstraints=[[2,1],[3,2],[6,5],[7,6],[8,7],[0,8],[0,9]].map(([old,index])=>{
    const a=original.accountConstraints.find(a=>a.accountIndex===old);
    assert.equal(a.accountConstraint.__kind,'Pubkey');assert.equal(a.accountConstraint.fields[0].length,1);
    return {...a,accountIndex:index};
  });
  assert.equal(original.dataConstraints[1].dataValue.fields[0].toString(), '1000000000000');
  assert.equal(original.dataConstraints[2].dataValue.fields[0],50);
  const slice=(offset,hex)=>({...original.dataConstraints[0],dataOffset:BigInt(offset),dataValue:{__kind:'U8Slice',fields:[Buffer.from(hex,'hex')]}});
  constraints[0]={...original,accountConstraints,dataConstraints:[
    slice(0,'d19853937cfed8e9'),{...original.dataConstraints[1],dataOffset:9n},
    {...original.dataConstraints[2],dataOffset:25n},slice(27,'00000000'),
  ]};
}
