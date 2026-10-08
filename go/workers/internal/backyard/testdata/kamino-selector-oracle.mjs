import {createRequire} from 'node:module';
import assert from 'node:assert/strict';
const require = createRequire(new URL('../../../../../tools/backyard-voltr/package.json',import.meta.url));
const {initObligation} = require('@kamino-finance/klend-sdk/dist/@codegen/klend/instructions/initObligation');
const {Reserve} = require('@kamino-finance/klend-sdk/dist/@codegen/klend/accounts/Reserve');
const {Obligation} = require('@kamino-finance/klend-sdk/dist/@codegen/klend/accounts/Obligation');
const {LendingMarket} = require('@kamino-finance/klend-sdk/dist/@codegen/klend/accounts/LendingMarket');
const {ReserveConfig} = require('@kamino-finance/klend-sdk/dist/@codegen/klend/types/ReserveConfig');
const {PublicKey} = require('@solana/web3.js');
const input=JSON.parse(await Bun.stdin.text());
for (const row of input.instructions) {
 const a=row.accounts;
 const ix=initObligation({args:{tag:1,id:0}},{obligationOwner:{address:a[0].address},feePayer:{address:a[1].address},obligation:a[2].address,lendingMarket:a[3].address,seed1Account:a[4].address,seed2Account:a[5].address,ownerUserMetadata:a[6].address,rent:a[7].address,systemProgram:a[8].address});
 assert.equal(ix.programAddress,row.program);
 assert.equal(Buffer.from(ix.data).toString('base64'),row.data);
 assert.deepEqual(ix.accounts.map(a=>({address:a.address,signer:(a.role&2)!==0,writable:(a.role&1)!==0})),a);
 const key=(s)=>new PublicKey(s).toBuffer();
 assert.equal(PublicKey.findProgramAddressSync([Buffer.from([1]),Buffer.from([0]),key(a[0].address),key(a[3].address),key(a[4].address),key(a[5].address)],new PublicKey(row.program))[0].toBase58(),a[2].address);
}
const r=(field)=>8+Reserve.layout.offsetOf(field);
const c=(field)=>r('config')+ReserveConfig.layout().offsetOf(field);
assert.deepEqual(input.offsets,{
 elevationGroup:8+Obligation.layout.offsetOf('elevationGroup'),outsideUsed:r('borrowedAmountOutsideElevationGroup'),
 outsideLimit:c('borrowLimitOutsideElevationGroup'),disableCross:c('disableUsageAsCollOutsideEmode'),
 debtWithdrawalCap:c('debtWithdrawalCap'),borrowFactor:c('borrowFactorPct'),loanToValue:c('loanToValuePct'),
 // Newer program versions use the first trailing padding word for the queue.
 queuedCollateral:r('padding'),
 globalBorrowValue:8+LendingMarket.layout.offsetOf('globalAllowedBorrowValue'),
 minimumRemainingValue:8+LendingMarket.layout.offsetOf('minNetValueInObligationSf'),
});
console.log('SDK initializer and capacity offsets match');
