// Independent installed SDK + retained catalog oracle. No RPC, signer or send.
import { generated } from '@loyal-labs/loyal-smart-accounts-core';
import { executeSettingsTransactionSync } from '@loyal-labs/loyal-smart-accounts-core/internal';
import { PublicKey, SystemProgram, Transaction, Message } from '@solana/web3.js';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import assert from 'node:assert/strict';
import {onreSwapOriginal,repairOnreSwap} from './onre-swap-repair-oracle.mjs';

const input = JSON.parse(await Bun.stdin.text());
const root = new URL('../../../../../', import.meta.url);
const raw = readFileSync(new URL('docs/evidence/backyard-rwa-go/policy-compiled-v1.json', root));
assert.equal(createHash('sha256').update(raw).digest('hex'), '8322303a592ea5441f433edf6e40246ccb8f466fde2a0d9d5544d3e76b6a88bd');
const catalog = JSON.parse(raw);
const state = JSON.parse(readFileSync(new URL('docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json', root)));
const lane = state.preflight.bindings.data.lanes.find(l => l.lane === 'OnRe/ONyc/USDC');
const rows = input.map(r => {
  const swap=onreSwapOriginal(r.Operation);
  assert.ok(swap||['borrow', 'repay'].includes(r.Operation));
  const original = catalog.policies.find(p => swap?p.policy===swap:p.name === `lane/OnRe/ONyc/USDC/${r.Operation}`);
  const instruction = original.createInstruction;
  const [args] = generated.syncSettingsTransactionArgsBeet.deserialize(Buffer.from(instruction.dataBase64, 'base64').subarray(8));
  assert.equal(args.actions.length, 1);
  const action = args.actions[0];
  assert.equal(action.__kind, 'PolicyCreate');
  const payload = action.policyCreationPayload.fields[0];
  const constraints = payload.instructionsConstraints;
  if(swap)repairOnreSwap(constraints);
  else {
  assert.equal(constraints.length, 1);
  const observed = lane.operations.find(o => o.operation === r.Operation);
  const changed = [];
  for (const constraint of constraints[0].accountConstraints) {
    const addresses = constraint.accountConstraint.fields[0];
    assert.equal(addresses.length, 1);
    const address = observed.accounts[constraint.accountIndex].address;
    if (addresses[0].toBase58() !== address) changed.push(constraint.accountIndex);
    constraint.accountConstraint.fields[0] = [new PublicKey(address)];
  }
  assert.deepEqual(changed, r.Operation === 'borrow' ? [12, 13] : [9, 10]);
  }
  action.seed = BigInt(r.Seed);
  const settings = new PublicKey(instruction.accounts[0].address);
  const admin = new PublicKey(instruction.accounts[1].address);
  const program = new PublicKey(instruction.programId);
  const seed = Buffer.alloc(8);
  seed.writeBigUInt64LE(action.seed);
  const [policy] = PublicKey.findProgramAddressSync([Buffer.from('smart_account'), Buffer.from('policy'), settings.toBuffer(), seed], program);
  const create = executeSettingsTransactionSync({settingsPda: settings, signers: [admin], actions: [action], feePayer: admin, programId: program,
    remainingAccounts: [{pubkey: policy, isSigner: false, isWritable: true}]});
  const transfer = SystemProgram.transfer({fromPubkey: admin, toPubkey: policy, lamports: BigInt(r.FundingLamports)});
  // Legacy permits different ordering inside a role group. Compare decoded
  // keys/data/privileges against independently constructed SDK instructions,
  // then require exact SDK serialization round-trip of the Go message.
  const view = message => ({header: message.header, blockhash: message.recentBlockhash,
    payer: message.accountKeys[0].toBase58(), instructions: Transaction.populate(message).instructions.map(ix => ({
      program: ix.programId.toBase58(), data: ix.data.toString('base64'), keys: ix.keys.map(k => ({address: k.pubkey.toBase58(), signer: k.isSigner, writable: k.isWritable}))}))});
  assert.equal(r.Messages.length, 2);
  for (const [i, ix] of [transfer, create].entries()) {
    const expected = new Transaction({feePayer: admin, recentBlockhash: r.RecentBlockhash}).add(ix).compileMessage();
    const message = Message.from(Buffer.from(r.Messages[i], 'base64'));
    assert.deepEqual(view(message), view(expected));
    assert.equal(message.serialize().toString('base64'), r.Messages[i]);
    assert.ok(65 + message.serialize().length <= 1232);
  }
  return {policy: policy.toBase58(), data: create.data.toString('base64'),
    messages: r.Messages};
});
process.stdout.write(JSON.stringify(rows));
