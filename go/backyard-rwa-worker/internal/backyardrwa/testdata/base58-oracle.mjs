// Installed Solana SDK oracle; no RPC or signing.
import {PublicKey} from '@solana/web3.js';
const input = JSON.parse(await Bun.stdin.text());
process.stdout.write(JSON.stringify(input.map(data => new PublicKey(Buffer.from(data, 'base64')).toBase58())));
