import { expect, test } from "bun:test";
import { PublicKey } from "@solana/web3.js";

import {
  type AccountState,
  judge,
  MAX_OWNER_LAMPORT_FALL,
  runBridgeOutPreflight,
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
const SYSTEM_PROGRAM_ID = "11111111111111111111111111111111";
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

test("a busy batch endpoint is a retryable refusal and a forbidden one is not", async () => {
  const realFetch = globalThis.fetch;
  const connection = {
    rpcEndpoint: "http://rpc.test",
    getLatestBlockhash: async () => ({ blockhash: "11111111111111111111111111111111" }),
  } as never;
  const quote = {
    lookupTables: [],
    instructions: [
      { programId: SYSTEM_PROGRAM_ID, keys: [], data: "00" },
    ],
  } as never;
  const verdictFor = async (status: number) => {
    globalThis.fetch = Object.assign(
      async () => new Response("", { status }),
      { preconnect: realFetch.preconnect }
    );
    try {
      return await runBridgeOutPreflight({
        connection,
        owner: new PublicKey(OWNER),
        quote,
        amountRaw: AMOUNT,
      });
    } finally {
      globalThis.fetch = realFetch;
    }
  };
  for (const status of [429, 503]) {
    const v = await verdictFor(status);
    expect(v.ok).toBe(false);
    if (!v.ok) expect(v.retry).toBe(true);
  }
  const forbidden = await verdictFor(403);
  expect(forbidden.ok).toBe(false);
  if (!forbidden.ok) expect(forbidden.retry).toBe(false);
});

const OTHER = "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin";
const SWAPPED = [USER_ATA, VAULT_ATA];

function setAmount(a: AccountState, amount: bigint) {
  new DataView(a.data.buffer, a.data.byteOffset).setBigUint64(
    AMOUNT_AT,
    amount,
    true
  );
}

function amountOf(a: AccountState) {
  return new DataView(a.data.buffer, a.data.byteOffset).getBigUint64(
    AMOUNT_AT,
    true
  );
}

const plain = (lamports: number): AccountState => ({
  lamports,
  owner: SYSTEM_PROGRAM_ID,
  data: new Uint8Array(),
});

type Maps = {
  pre: Map<string, AccountState | null>;
  post: Map<string, AccountState | null>;
};

const judgeRows: [string, (m: Maps) => void, RegExp, bigint?][] = [
  [
    "a negative amount that the balances happen to match",
    ({ pre, post }) => {
      for (const k of SWAPPED) {
        const before = pre.get(k) as AccountState;
        pre.set(k, post.get(k) as AccountState);
        post.set(k, before);
      }
    },
    /amount is not positive/,
    -AMOUNT,
  ],
  [
    "an extra account only in the post state",
    ({ post }) => post.set(OTHER, null),
    /does not cover the writable set/,
  ],
  [
    "a post state keyed by other accounts, same size",
    ({ pre, post }) => {
      pre.set(STRANGER, null);
      post.set(OTHER, null);
    },
    /no post state for/,
  ],
  [
    "an account created by the transaction",
    ({ pre, post }) => {
      pre.set(STRANGER, null);
      post.set(STRANGER, plain(1000));
    },
    /was created/,
  ],
  [
    "an account closed by the transaction",
    ({ pre, post }) => {
      pre.set(STRANGER, plain(1000));
      post.set(STRANGER, null);
    },
    /was closed/,
  ],
  [
    "lamports beyond exact integer range",
    ({ pre, post }) => {
      pre.set(STRANGER, plain(2 ** 53));
      post.set(STRANGER, plain(2 ** 53));
    },
    /beyond exact range/,
  ],
  [
    "a wallet owned by a program",
    ({ pre, post }) => {
      for (const side of [pre, post])
        mutate(side, OWNER, (a) => {
          a.owner = TOKEN_PROGRAM;
        });
    },
    /plain system account/,
  ],
  [
    "the wallet gaining lamports",
    ({ pre, post }) => {
      const before = pre.get(OWNER) as AccountState;
      mutate(post, OWNER, (a) => {
        a.lamports = before.lamports + 1;
      });
    },
    /gained lamports/,
  ],
  [
    "a second owner USDC account drained by the same amount",
    ({ pre, post }) => {
      pre.set(OTHER, tokenAccount(USDC, OWNER, AMOUNT));
      post.set(OTHER, tokenAccount(USDC, OWNER, BigInt(0)));
    },
    /two owner USDC accounts/,
  ],
  [
    "a stranger's token account of another mint changing",
    ({ pre, post }) => {
      pre.set(OTHER, tokenAccount(USDT, STRANGER, BigInt(5_000_000)));
      post.set(OTHER, tokenAccount(USDT, STRANGER, BigInt(0)));
    },
    /of another mint changed/,
  ],
  [
    "a delegate set on the vault account",
    ({ post }) =>
      mutate(post, VAULT_ATA, (a) => {
        a.data[DELEGATE_OPTION_AT] = 1;
      }),
    /USDC account .* changed beyond its balance/,
  ],
  [
    "the vault gaining one more than the amount",
    ({ post }) =>
      mutate(post, VAULT_ATA, (a) => setAmount(a, amountOf(a) + ONE)),
    /moved by/,
  ],
  [
    "the wallet missing from the set",
    ({ pre, post }) => {
      pre.delete(OWNER);
      post.delete(OWNER);
    },
    /owner is not in the set/,
  ],
  [
    "the owner USDC account missing from the set",
    ({ pre, post }) => {
      pre.delete(USER_ATA);
      post.delete(USER_ATA);
    },
    /owner USDC account is not in the set/,
  ],
];

test("each tampered account state is refused for its own reason", () => {
  for (const [name, tamper, reason, amount] of judgeRows) {
    const maps = load();
    tamper(maps);
    const v = run(maps.pre, maps.post, { amount });
    expect(v.ok, name).toBe(false);
    if (!v.ok) expect(v.reason, name).toMatch(reason);
  }
});

const SLOT = 400_000_000;
const WIRE = (raw: Raw) => ({
  lamports: raw.lamports,
  owner: raw.owner,
  data: [raw.data, "base64"],
});

type Reply = { id: number; result?: any; error?: unknown }[];

async function preflightOver(
  tamper: (reply: Reply) => unknown = () => undefined,
  quoteOver: Record<string, unknown> = {},
  tableValue: unknown = undefined
) {
  const realFetch = globalThis.fetch;
  const requests: any[] = [];
  const [wallet, ata, vault] = Object.keys(recorded.pre);
  const connection = {
    rpcEndpoint: "http://rpc.test",
    getLatestBlockhash: async () => ({
      blockhash: "11111111111111111111111111111111",
    }),
    getAddressLookupTable: async () => ({ value: tableValue }),
  } as never;
  const quote = {
    lookupTables: [],
    instructions: [
      {
        programId: "99vQwtBwYtrqqD9YSXbdum3KBdxPAVxYTaQ3cfnJSrN2",
        keys: [wallet, ata, vault].map((pubkey, i) => ({
          pubkey,
          isSigner: i === 0,
          isWritable: true,
        })),
        data: "00",
      },
    ],
    ...quoteOver,
  } as never;
  globalThis.fetch = Object.assign(
    async (_: unknown, init?: RequestInit): Promise<Response> => {
      const req = JSON.parse(String(init?.body));
      requests.push(req);
      const addresses: string[] = req[0].params[0];
      const side = (o: Record<string, Raw>) =>
        addresses.map((a) => WIRE(o[a] as Raw));
      const reply: Reply = [
        { id: 1, result: { context: { slot: SLOT }, value: side(recorded.pre) } },
        {
          id: 2,
          result: {
            context: { slot: SLOT },
            value: { err: null, accounts: side(recorded.post) },
          },
        },
      ];
      return (tamper(reply) as Response | undefined) ?? Response.json(reply);
    },
    { preconnect: realFetch.preconnect }
  );
  try {
    const verdict = await runBridgeOutPreflight({
      connection,
      owner: new PublicKey(OWNER),
      quote,
      amountRaw: AMOUNT,
    });
    return { verdict, requests };
  } finally {
    globalThis.fetch = realFetch;
  }
}

test("the batch asks for the writable set at confirmed with no replacement and returns the simulated bytes", async () => {
  const { verdict, requests } = await preflightOver();
  expect(verdict.ok).toBe(true);
  const [reads, simulates] = requests[0];
  expect(reads.params[0]).toEqual(Object.keys(recorded.pre));
  expect(reads.params[1].commitment).toBe("confirmed");
  expect(simulates.params[1].commitment).toBe("confirmed");
  expect(simulates.params[1].sigVerify).toBe(false);
  expect(simulates.params[1].replaceRecentBlockhash).toBeUndefined();
  if (verdict.ok)
    expect(Buffer.from(verdict.transaction.serialize()).toString("base64")).toBe(
      simulates.params[0]
    );
});

const canonicalTail = () => {
  const text = recorded.pre[VAULT_ATA as keyof typeof recorded.pre].data;
  const alphabet =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const at = text.length - 3;
  return (
    text.slice(0, at) + alphabet[alphabet.indexOf(text[at] as string) + 1] + "=="
  );
};

const wireRows: [string, (r: Reply) => void, RegExp, boolean?][] = [
  [
    "a pre answer without a slot",
    (r) => {
      r[0].result.context = {};
    },
    /pre state came back malformed/,
  ],
  [
    "a third answer in the batch",
    (r) => {
      r.push({ id: 3, result: null });
    },
    /not one answer per call/,
  ],
  [
    "an error answer for the simulation",
    (r) => {
      r[1] = { id: 2, error: { code: -32005 } };
    },
    /batch call 2 failed/,
  ],
  [
    "answers from two slots",
    (r) => {
      r[1].result.context.slot = SLOT + 1;
    },
    /never shared a slot/,
    true,
  ],
  [
    "a post state one account short",
    (r) => {
      r[1].result.value.accounts.pop();
    },
    /no post state/,
  ],
  [
    "post accounts as an object that carries the right length",
    (r) => {
      const list = r[1].result.value.accounts;
      r[1].result.value.accounts = Object.assign({}, list, {
        length: list.length,
      });
    },
    /no post state/,
  ],
  [
    "account data with a third element",
    (r) => {
      r[0].result.value[1].data.push("x");
    },
    /an account came back malformed/,
  ],
  [
    "account data in another encoding",
    (r) => {
      r[0].result.value[1].data[1] = "base58";
    },
    /an account came back malformed/,
  ],
  [
    "account data with characters outside base64",
    (r) => {
      r[0].result.value[1].data[0] = "!!!!";
    },
    /an account came back malformed/,
  ],
  [
    "account data that decodes to bytes it does not re-encode to",
    (r) => {
      r[0].result.value[2].data[0] = canonicalTail();
    },
    /not canonical base64/,
  ],
  [
    "a reported size that disagrees with the decoded data",
    (r) => {
      r[0].result.value[1].space = 1;
    },
    /disagrees with its size/,
  ],
  [
    "a pre state that is not a list",
    (r) => {
      r[0].result.value = {};
    },
    /simulation answer came back malformed/,
  ],
  [
    "a simulation value that is not an object",
    (r) => {
      r[1].result.value = "ok";
    },
    /simulation answer came back malformed/,
  ],
  [
    "a simulation value without an err",
    (r) => {
      delete r[1].result.value.err;
    },
    /simulation answer came back malformed/,
  ],
];

test("each malformed or inconsistent batch reply is refused for its own reason", async () => {
  for (const [name, tamper, reason, retry] of wireRows) {
    const { verdict } = await preflightOver(tamper);
    expect(verdict.ok, name).toBe(false);
    if (!verdict.ok) {
      expect(verdict.reason, name).toMatch(reason);
      expect(verdict.retry, name).toBe(retry ?? false);
    }
  }
});

test("a lookup table that does not resolve refuses", async () => {
  const { verdict } = await preflightOver(
    undefined,
    { lookupTables: [STRANGER] },
    null
  );
  expect(verdict.ok).toBe(false);
  if (!verdict.ok) expect(verdict.reason).toMatch(/does not resolve/);
});

test("a preflight that never answers is refused when its timer fires", async () => {
  const realSetTimeout = globalThis.setTimeout;
  let fire: (() => void) | undefined;
  globalThis.setTimeout = Object.assign(
    (fn: () => void, ms?: number) => {
      if (ms !== undefined && ms >= 10_000) {
        fire = fn;
        return 0;
      }
      return realSetTimeout(fn, ms);
    },
    { preconnect: undefined }
  ) as never;
  try {
    const pending = preflightOver(() => new Promise(() => undefined) as never);
    await new Promise((r) => realSetTimeout(r, 50));
    fire?.();
    const { verdict } = await pending;
    expect(verdict.ok).toBe(false);
    if (!verdict.ok) expect(verdict.reason).toBe("preflight timed out");
  } finally {
    globalThis.setTimeout = realSetTimeout;
  }
}, 2000);
