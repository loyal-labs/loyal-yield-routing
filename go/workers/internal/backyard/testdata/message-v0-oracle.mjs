// Independent installed Solana SDK serialization oracle; no network or signing.
import {AddressLookupTableAccount, MessageV0, PublicKey, TransactionInstruction} from '@solana/web3.js';
const input = JSON.parse(await Bun.stdin.text());
const tables = input.tables.map(t => new AddressLookupTableAccount({key:new PublicKey(t.address), state:AddressLookupTableAccount.deserialize(Buffer.from(t.data, 'base64'))}));
const instructions = input.instructions.map(i => new TransactionInstruction({programId:new PublicKey(i.program), keys:i.accounts.map(a => ({pubkey:new PublicKey(a.key), isSigner:a.signer, isWritable:a.writable})), data:Buffer.from(i.data, 'base64')}));
// Capture-only keys participate in compilation but not the final instruction.
const capture = input.capture ?? [];
for (const key of capture) instructions.at(-1).keys.push({pubkey:new PublicKey(key), isSigner:false, isWritable:false});
const message = MessageV0.compile({payerKey:new PublicKey(input.payer), recentBlockhash:input.blockhash, instructions, addressLookupTableAccounts:tables});
if (capture.length) message.compiledInstructions.at(-1).accountKeyIndexes.splice(-capture.length);
process.stdout.write(Buffer.from(message.serialize()).toString('base64'));
