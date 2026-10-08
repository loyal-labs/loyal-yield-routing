// Independent generated SDK encoding, starting with the actual installed
// policy account. No RPC, key material, signing or installation.
import {generated} from '@loyal-labs/loyal-smart-accounts-core';
import {PublicKey} from '@solana/web3.js';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import assert from 'node:assert/strict';
import {onreSwapOriginal,repairOnreSwap} from './onre-swap-repair-oracle.mjs';
const root=new URL('../../../../../',import.meta.url);
const raw=readFileSync(new URL('docs/evidence/backyard-rwa-go/policy-install-readback-v1.json',root));
assert.equal(createHash('sha256').update(raw).digest('hex'),'22993e6659dcf9e17bc324ef870ab429c459a0f2e89e91d1e41021ab111c2215');
const installed=JSON.parse(raw);
const state=JSON.parse(readFileSync(new URL('docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json',root)));
const lane=state.preflight.bindings.data.lanes.find(l=>l.lane==='OnRe/ONyc/USDC');
const inputs=JSON.parse(await Bun.stdin.text());
for(const input of inputs){
  const swap=onreSwapOriginal(input.operation);
  const row=installed.operations.find(r=>swap?r.policyAddress===swap:r.policyName===`lane/OnRe/ONyc/USDC/${input.operation}`);
  assert.ok(row);
  const bytes=Buffer.from(row.dataBase64,'base64');
  const [policy]=generated.Policy.deserialize(bytes);
  assert.equal(policy.policyState.__kind,'ProgramInteraction');
  if(swap)repairOnreSwap(policy.policyState.fields[0].instructionsConstraints);
  else {
  const spec=lane.operations.find(o=>o.operation===input.operation);
  const changed=[];
  for(const constraint of policy.policyState.fields[0].instructionsConstraints[0].accountConstraints){
    const keys=constraint.accountConstraint.fields[0];
    assert.equal(keys.length,1);
    const key=new PublicKey(spec.accounts[constraint.accountIndex].address);
    if(!keys[0].equals(key))changed.push(constraint.accountIndex);
    keys[0]=key;
  }
  assert.deepEqual(changed,input.operation==='borrow'?[12,13]:[9,10]);
  }
  policy.seed=BigInt(input.seed);
  const seed=Buffer.alloc(8);seed.writeBigUInt64LE(policy.seed);
  const [,bump]=PublicKey.findProgramAddressSync([Buffer.from('smart_account'),Buffer.from('policy'),policy.settings.toBuffer(),seed],new PublicKey(row.owner));
  policy.bump=bump;
  policy.start=BigInt(input.start);
  const encoded=policy.serialize()[0];
  const padded=Buffer.alloc(swap?1383:bytes.length);assert.ok(encoded.length<=padded.length);encoded.copy(padded);
  assert.equal(padded.toString('base64'),input.data);
}
process.stdout.write(JSON.stringify({passed:inputs.length}));
