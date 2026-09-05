// Installed SDK Settings layout oracle; retained public account input only.
import {generated} from '@loyal-labs/loyal-smart-accounts-core';
import {PublicKey} from '@solana/web3.js';
const raw=Buffer.from(await Bun.stdin.text(),'base64');
const admin='BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ';
const cases=[];
const add=(name,change,next)=>{
  const [value]=generated.Settings.deserialize(raw);
  change(value);
  cases.push({name,data:value.serialize()[0].toString('base64'),next:next?.toString()??''});
};
add('retained',()=>{},140n);
add('option-none',v=>v.archivalAuthority=null,140n);
add('seed-byte-boundary',v=>v.policySeed=255n,256n);
add('seed-max-minus-one',v=>v.policySeed=2n**64n-2n,2n**64n-1n);
add('seed-overflow',v=>v.policySeed=2n**64n-1n);
add('seed-absent',v=>v.policySeed=null);
add('old-seed',v=>v.policySeed=138n);
add('different-authority',v=>v.settingsAuthority=new PublicKey(admin));
add('threshold',v=>v.threshold=2);
add('timelock',v=>v.timeLock=1);
add('missing-create',v=>v.signers[0].permissions.mask=6);
add('missing-vote',v=>v.signers[0].permissions.mask=5);
add('missing-execute',v=>v.signers[0].permissions.mask=3);
add('new-signer',v=>v.signers[0].key=PublicKey.default);
add('extra-signer',v=>v.signers.push({key:PublicKey.default,permissions:{mask:7}}));
add('reserved-extension',v=>v.reserved2=1);
process.stdout.write(JSON.stringify(cases));
