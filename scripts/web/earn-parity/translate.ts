import { PublicKey } from "../../../web/node_modules/@solana/web3.js";
import { pda as squads } from "../../../web/packages/loyal-smart-accounts/src/index";

export type RecordedFrom = { walletAddress: string; settingsPda: string; policySeeds: number[]; reserves: string[] };

const KLEND = new PublicKey("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD");
const FARMS = new PublicKey("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr");
const ATA_PROGRAM = new PublicKey("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
const TOKEN = new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
const TOKEN_2022 = new PublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
const SAFE_MARKETS = [
  "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
  "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
  "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
  "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
  "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
].map((market) => new PublicKey(market));
const PRODUCT_MINTS: [string, PublicKey][] = [
  ["CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", TOKEN_2022],
  ["2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", TOKEN_2022],
  ["2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", TOKEN_2022],
  ["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", TOKEN],
  ["Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", TOKEN],
  ["USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", TOKEN],
];

export type ReserveFacts = { farms: PublicKey[]; collateralMint: PublicKey };

export function reserveFacts(data: Buffer): ReserveFacts {
  const key = (offset: number) => new PublicKey(data.subarray(offset, offset + 32));
  return { farms: [key(64), key(96)].filter((farm) => !farm.equals(PublicKey.default)), collateralMint: key(8 + 2552) };
}

function pda(seeds: Uint8Array[], program: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync(seeds, program)[0];
}

function ata(owner: PublicKey, mint: PublicKey, tokenProgram: PublicKey): PublicKey {
  return pda([owner.toBytes(), tokenProgram.toBytes(), mint.toBytes()], ATA_PROGRAM);
}

function identityKeys(wallet: PublicKey, settings: PublicKey, from: RecordedFrom, reserves: ReserveFacts[]): PublicKey[] {
  const vaults = [0, 1].map((accountIndex) => squads.getSmartAccountPda({ settingsPda: settings, accountIndex })[0]);
  const vault = vaults[1];
  const obligations = SAFE_MARKETS.map((market) =>
    pda([Uint8Array.of(0), Uint8Array.of(0), vault.toBytes(), market.toBytes(), PublicKey.default.toBytes(), PublicKey.default.toBytes()], KLEND)
  );
  return [
    wallet,
    settings,
    ...vaults,
    ...from.policySeeds.map((policySeed) => squads.getPolicyPda({ settingsPda: settings, policySeed })[0]),
    ...obligations,
    pda([Buffer.from("user_meta"), vault.toBytes()], KLEND),
    ...PRODUCT_MINTS.flatMap(([mint, program]) => [vault, wallet, vaults[0]].map((owner) => ata(owner, new PublicKey(mint), program))),
    ...reserves.flatMap((reserve) => [
      ata(vault, reserve.collateralMint, TOKEN),
      ...reserve.farms.flatMap((farm) => obligations.map((obligation) => pda([Buffer.from("user"), farm.toBytes(), obligation.toBytes()], FARMS))),
    ]),
  ];
}

export type KeyMap = { toSource: Map<string, string>; toTarget: Map<string, string> };

export function keyMap(target: { walletAddress: string; settingsPda: string }, from: RecordedFrom, reserves: ReserveFacts[]): KeyMap {
  const source = identityKeys(new PublicKey(from.walletAddress), new PublicKey(from.settingsPda), from, reserves);
  const ours = identityKeys(new PublicKey(target.walletAddress), new PublicKey(target.settingsPda), from, reserves);
  const toSource = new Map<string, string>();
  const toTarget = new Map<string, string>();
  ours.forEach((key, index) => {
    toSource.set(key.toBase58(), source[index].toBase58());
    toTarget.set(source[index].toBase58(), key.toBase58());
  });
  return { toSource, toTarget };
}

function swapBytes(bytes: Buffer, map: Map<string, string>): Buffer {
  const out = Buffer.from(bytes);
  for (const [from, to] of map) {
    const needle = new PublicKey(from).toBuffer();
    const replacement = new PublicKey(to).toBuffer();
    for (let at = out.indexOf(needle); at !== -1; at = out.indexOf(needle, at + 32)) replacement.copy(out, at);
  }
  return out;
}

export function swapText(text: string, map: Map<string, string>): string {
  return text.replace(/[1-9A-HJ-NP-Za-km-z]{32,44}/g, (token) => map.get(token) ?? token);
}

const BASE64_BLOB = /^[A-Za-z0-9+/]{64,}={0,2}$/;

export function swapDeep(value: unknown, map: Map<string, string>): unknown {
  if (typeof value === "string") {
    if (map.has(value)) return map.get(value);
    if (BASE64_BLOB.test(value) && value.length % 4 === 0) return swapBytes(Buffer.from(value, "base64"), map).toString("base64");
    return value;
  }
  if (Array.isArray(value)) return value.map((inner) => swapDeep(inner, map));
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, inner]) => [key, swapDeep(inner, map)]));
  }
  return value;
}
