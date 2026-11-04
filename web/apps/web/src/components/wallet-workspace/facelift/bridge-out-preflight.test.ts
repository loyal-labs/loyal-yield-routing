import { expect, test } from "bun:test";
import { PublicKey } from "@solana/web3.js";

import {
  type AccountState,
  judge,
  MAX_OWNER_LAMPORT_FALL,
  type Simulated,
  type Snapshot,
} from "./bridge-out-preflight";
import recorded from "./bridge-out-preflight.testdata.json";

const OWNER = recorded.owner;
const AMOUNT = BigInt(recorded.amountRaw);
const ONE = BigInt(1);
const USDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDT = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const TOKEN_PROGRAM = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const USER_ATA = "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce";
const VAULT_ATA = "4nvJ5zWdVspxJiNZzB127U6amPH98SFFkBx2JZrAduia";
const STRANGER = "7uTT8Xi5RWXzy7h9XL244GRgEycDYDhLjr3ZyNdXi8pZ";

const MINT_AT = 0;
const OWNER_AT = 32;
const AMOUNT_AT = 64;
const DELEGATE_OPTION_AT = 72;
const CLOSE_AUTHORITY_OPTION_AT = 129;

type Raw = { lamports: number; owner: string; data: string };

function decode(raw: Raw): AccountState {
  return {
    lamports: raw.lamports,
    owner: raw.owner,
    data: new Uint8Array(Buffer.from(raw.data, "base64")),
  };
}

function load(): {
  pre: Map<string, AccountState | null>;
  post: Map<string, AccountState | null>;
} {
  const side = (o: Record<string, Raw>) =>
    new Map<string, AccountState | null>(
      Object.entries(o).map(([k, v]) => [k, decode(v)])
    );
  return { pre: side(recorded.pre), post: side(recorded.post) };
}

function mutate(
  map: Map<string, AccountState | null>,
  address: string,
  f: (a: AccountState) => void
) {
  const a = map.get(address);
  if (!a) throw new Error(`no ${address}`);
  const copy = { ...a, data: new Uint8Array(a.data) };
  f(copy);
  map.set(address, copy);
}

function tokenAccount(
  mint: string,
  owner: string,
  amount: bigint
): AccountState {
  const data = new Uint8Array(165);
  data.set(new PublicKey(mint).toBytes(), MINT_AT);
  data.set(new PublicKey(owner).toBytes(), OWNER_AT);
  new DataView(data.buffer).setBigUint64(AMOUNT_AT, amount, true);
  data[108] = 1;
  return { lamports: 2039280, owner: TOKEN_PROGRAM, data };
}

function run(
  pre: Snapshot,
  post: Snapshot,
  opts: { err?: unknown; amount?: bigint } = {}
) {
  const sim: Simulated = { err: opts.err ?? null, accounts: post };
  return judge(pre, sim, OWNER, opts.amount ?? AMOUNT, USDC);
}

function refused(v: ReturnType<typeof judge>, reason: RegExp) {
  expect(v.ok).toBe(false);
  if (!v.ok) expect(v.reason).toMatch(reason);
}

test("the recorded deposit passes with its network fee", () => {
  const { pre, post } = load();
  expect(run(pre, post)).toEqual({ ok: true, feeLamports: BigInt(5000) });
});

test("an amount off by one either way refuses", () => {
  const { pre, post } = load();
  refused(run(pre, post, { amount: AMOUNT + ONE }), /owner USDC fell by/);
  refused(run(pre, post, { amount: AMOUNT - ONE }), /owner USDC fell by/);
});

test("a delegate set on the user's token account refuses", () => {
  const { pre, post } = load();
  mutate(post, USER_ATA, (a) => {
    a.data[DELEGATE_OPTION_AT] = 1;
    a.data[DELEGATE_OPTION_AT + 4] = 9;
  });
  refused(run(pre, post), /owner USDC account changed beyond its balance/);
});

test("a close authority set on the user's token account refuses", () => {
  const { pre, post } = load();
  mutate(post, USER_ATA, (a) => {
    a.data[CLOSE_AUTHORITY_OPTION_AT] = 1;
    a.data[CLOSE_AUTHORITY_OPTION_AT + 4] = 9;
  });
  refused(run(pre, post), /owner USDC account changed beyond its balance/);
});

test("a changed token owner on the user's token account refuses", () => {
  const { pre, post } = load();
  mutate(post, USER_ATA, (a) => {
    a.data[OWNER_AT] ^= 0xff;
  });
  refused(run(pre, post), /owner USDC account changed beyond its balance/);
});

test("a second token account of the user being drained refuses", () => {
  const { pre, post } = load();
  const other = "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin";
  pre.set(other, tokenAccount(USDT, OWNER, BigInt(5_000_000)));
  post.set(other, tokenAccount(USDT, OWNER, BigInt(0)));
  refused(run(pre, post), /owner token account .* changed/);
});

test("a second account gaining USDC refuses", () => {
  const { pre, post } = load();
  const other = "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin";
  pre.set(other, tokenAccount(USDC, STRANGER, BigInt(0)));
  post.set(other, tokenAccount(USDC, STRANGER, AMOUNT));
  refused(run(pre, post), /2 accounts gained USDC/);
});

test("the wallet's owner program changed refuses", () => {
  const { pre, post } = load();
  mutate(post, OWNER, (a) => {
    a.owner = TOKEN_PROGRAM;
  });
  refused(run(pre, post), /reassigned or given data/);
});

test("the wallet losing lamports above the cap refuses", () => {
  const { pre, post } = load();
  const before = pre.get(OWNER) as AccountState;
  mutate(post, OWNER, (a) => {
    a.lamports = before.lamports - MAX_OWNER_LAMPORT_FALL - 1;
  });
  refused(run(pre, post), /owner lamports fell by/);
});

test("an unrelated account gaining lamports refuses", () => {
  const { pre, post } = load();
  pre.set(STRANGER, {
    lamports: 1000,
    owner: "11111111111111111111111111111111",
    data: new Uint8Array(),
  });
  post.set(STRANGER, {
    lamports: 1001,
    owner: "11111111111111111111111111111111",
    data: new Uint8Array(),
  });
  refused(run(pre, post), /changed/);
});

test("a failed simulation refuses", () => {
  const { pre, post } = load();
  refused(
    run(pre, post, { err: { InstructionError: [0, "Custom"] } }),
    /simulation failed/
  );
});

test("a missing post entry refuses", () => {
  const { pre, post } = load();
  post.delete(VAULT_ATA);
  refused(run(pre, post), /post state/);
});
