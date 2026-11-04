import type { BridgeOutQuoteResponse } from "@loyal-labs/api";
import { TOKEN_MINTS } from "@loyal-labs/wallet-core/constants";
import {
  type AddressLookupTableAccount,
  type Connection,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";

export type AccountState = {
  lamports: number;
  owner: string;
  data: Uint8Array;
};

export type Snapshot = ReadonlyMap<string, AccountState | null>;

export type Simulated = { err: unknown; accounts: Snapshot };

export type Verdict =
  | { ok: true; feeLamports: bigint }
  | { ok: false; reason: string };

export type PreflightVerdict =
  | { ok: true; feeLamports: bigint; transaction: VersionedTransaction }
  | { ok: false; reason: string; retry: boolean };

type Quote = Pick<BridgeOutQuoteResponse, "instructions" | "lookupTables">;

/**
 * One signature's base fee, 5000 lamports. The real deposit_token simulation
 * on mainnet (BO4 deltas.txt, 6 cells) charged the wallet exactly this and
 * moved no rent: the vault token account already exists and the user's does
 * too. A fall above it means rent or a priority fee we did not compile in.
 */
export const MAX_OWNER_LAMPORT_FALL = 5000;

const PREFLIGHT_TIMEOUT_MS = 20_000;
const MAX_SLOT_ATTEMPTS = 4;

const SYSTEM_PROGRAM = "11111111111111111111111111111111";
const TOKEN_PROGRAM = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const TOKEN_2022_PROGRAM = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";

const TOKEN_ACCOUNT_LEN = 165;
const TOKEN_MINT_AT = 0;
const TOKEN_OWNER_AT = 32;
const TOKEN_AMOUNT_AT = 64;
const TOKEN_AMOUNT_END = 72;
const TOKEN_2022_ACCOUNT_TYPE_AT = 165;
const TOKEN_2022_ACCOUNT_TYPE = 2;

const USDC_MINT = TOKEN_MINTS.USDC;
const ZERO = BigInt(0);

type TokenView = { mint: string; owner: string; amount: bigint };

function refuse(reason: string): Verdict {
  return { ok: false, reason };
}

function parseToken(a: AccountState): TokenView | null {
  const isToken =
    (a.owner === TOKEN_PROGRAM && a.data.length === TOKEN_ACCOUNT_LEN) ||
    (a.owner === TOKEN_2022_PROGRAM &&
      a.data.length >= TOKEN_ACCOUNT_LEN &&
      (a.data.length === TOKEN_ACCOUNT_LEN ||
        a.data[TOKEN_2022_ACCOUNT_TYPE_AT] === TOKEN_2022_ACCOUNT_TYPE));
  if (!isToken) return null;
  const view = new DataView(a.data.buffer, a.data.byteOffset, a.data.length);
  return {
    mint: new PublicKey(
      a.data.subarray(TOKEN_MINT_AT, TOKEN_MINT_AT + 32)
    ).toBase58(),
    owner: new PublicKey(
      a.data.subarray(TOKEN_OWNER_AT, TOKEN_OWNER_AT + 32)
    ).toBase58(),
    amount: view.getBigUint64(TOKEN_AMOUNT_AT, true),
  };
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

function sameExceptAmount(a: AccountState, b: AccountState): boolean {
  return (
    a.lamports === b.lamports &&
    a.owner === b.owner &&
    a.data.length === b.data.length &&
    bytesEqual(
      a.data.subarray(0, TOKEN_AMOUNT_AT),
      b.data.subarray(0, TOKEN_AMOUNT_AT)
    ) &&
    bytesEqual(
      a.data.subarray(TOKEN_AMOUNT_END),
      b.data.subarray(TOKEN_AMOUNT_END)
    )
  );
}

function identical(a: AccountState, b: AccountState): boolean {
  return (
    a.lamports === b.lamports &&
    a.owner === b.owner &&
    bytesEqual(a.data, b.data)
  );
}

/**
 * Decides from the writable set's state before and after one simulation.
 * Only an account in the transaction's writable set can change, so judging
 * that set covers every write the program could make, including an Approve
 * or SetAuthority on the user's token account signed by the user.
 */
export function judge(
  pre: Snapshot,
  post: Simulated,
  owner: string,
  amountRaw: bigint,
  usdcMint: string = USDC_MINT
): Verdict {
  if (post.err !== null) return refuse("simulation failed");
  if (amountRaw <= ZERO) return refuse("amount is not positive");
  if (post.accounts.size !== pre.size) {
    return refuse("post state does not cover the writable set");
  }

  let feeLamports: bigint | undefined;
  let ownerUsdcSeen = false;
  let gains = 0;

  for (const [address, before] of pre) {
    if (!post.accounts.has(address)) {
      return refuse(`no post state for ${address}`);
    }
    const after = post.accounts.get(address) ?? null;
    if (before === null) {
      if (after !== null) return refuse(`${address} was created`);
      continue;
    }
    if (after === null) return refuse(`${address} was closed`);
    if (
      !Number.isSafeInteger(before.lamports) ||
      !Number.isSafeInteger(after.lamports)
    ) {
      return refuse(`${address} lamports are beyond exact range`);
    }

    if (address === owner) {
      if (before.owner !== SYSTEM_PROGRAM || before.data.length !== 0) {
        return refuse("owner is not a plain system account");
      }
      if (
        after.owner !== before.owner ||
        !bytesEqual(after.data, before.data)
      ) {
        return refuse("owner account was reassigned or given data");
      }
      const fall = before.lamports - after.lamports;
      if (fall < 0) return refuse("owner gained lamports");
      if (fall > MAX_OWNER_LAMPORT_FALL) {
        return refuse(
          `owner lamports fell by ${fall}, above ${MAX_OWNER_LAMPORT_FALL}`
        );
      }
      feeLamports = BigInt(fall);
      continue;
    }

    const tokenBefore = parseToken(before);
    const tokenAfter = parseToken(after);
    if (tokenBefore === null || tokenAfter === null) {
      if (tokenBefore !== tokenAfter) {
        return refuse(`${address} changed its kind`);
      }
      if (!identical(before, after)) return refuse(`${address} changed`);
      continue;
    }

    if (tokenBefore.owner === owner && tokenBefore.mint === usdcMint) {
      if (ownerUsdcSeen) return refuse("two owner USDC accounts in the set");
      ownerUsdcSeen = true;
      if (!sameExceptAmount(before, after)) {
        return refuse("owner USDC account changed beyond its balance");
      }
      if (tokenBefore.amount - tokenAfter.amount !== amountRaw) {
        return refuse(
          `owner USDC fell by ${
            tokenBefore.amount - tokenAfter.amount
          }, not ${amountRaw}`
        );
      }
      continue;
    }

    if (tokenBefore.owner === owner) {
      if (!identical(before, after)) {
        return refuse(`owner token account ${address} changed`);
      }
      continue;
    }

    if (tokenBefore.mint !== usdcMint) {
      if (!identical(before, after)) {
        return refuse(`token account ${address} of another mint changed`);
      }
      continue;
    }

    if (!sameExceptAmount(before, after)) {
      return refuse(`USDC account ${address} changed beyond its balance`);
    }
    const delta = tokenAfter.amount - tokenBefore.amount;
    if (delta === ZERO) continue;
    if (delta !== amountRaw) {
      return refuse(
        `USDC account ${address} moved by ${delta}, not ${amountRaw}`
      );
    }
    gains++;
  }

  if (feeLamports === undefined) return refuse("owner is not in the set");
  if (!ownerUsdcSeen) return refuse("owner USDC account is not in the set");
  if (gains !== 1) return refuse(`${gains} accounts gained USDC, want 1`);
  return { ok: true, feeLamports };
}

export function compile(
  quote: Quote,
  owner: PublicKey,
  blockhash: string,
  lookupTables: AddressLookupTableAccount[]
): VersionedTransaction {
  if (!quote.instructions || quote.instructions.length === 0) {
    throw new Error("quote has no instructions");
  }
  const instructions = quote.instructions.map(
    (ix) =>
      new TransactionInstruction({
        programId: new PublicKey(ix.programId),
        keys: (ix.keys ?? []).map((k) => ({
          pubkey: new PublicKey(k.pubkey),
          isSigner: k.isSigner,
          isWritable: k.isWritable,
        })),
        data: Buffer.from(ix.data, "hex"),
      })
  );
  return new VersionedTransaction(
    new TransactionMessage({
      payerKey: owner,
      recentBlockhash: blockhash,
      instructions,
    }).compileToV0Message(lookupTables)
  );
}

function writableSet(
  tx: VersionedTransaction,
  lookupTables: AddressLookupTableAccount[]
): PublicKey[] {
  const keys = tx.message.getAccountKeys({
    addressLookupTableAccounts: lookupTables,
  });
  const out: PublicKey[] = [];
  for (let i = 0; i < keys.length; i++) {
    const key = keys.get(i);
    if (key && tx.message.isAccountWritable(i)) out.push(key);
  }
  return out;
}

async function resolveLookupTables(
  connection: Connection,
  addresses: readonly string[]
): Promise<AddressLookupTableAccount[]> {
  return Promise.all(
    addresses.map(async (address) => {
      const res = await connection.getAddressLookupTable(
        new PublicKey(address),
        {
          commitment: "confirmed",
        }
      );
      if (!res.value)
        throw new Error(`lookup table ${address} does not resolve`);
      return res.value;
    })
  );
}

type WireAccount = {
  lamports: number;
  owner: string;
  data: [string, string];
};

type Slotted<T> = { context: { slot: number }; value: T };

type SimulationValue = {
  err: unknown;
  accounts: (WireAccount | null)[] | null;
};

function toState(a: WireAccount | null): AccountState | null {
  return a
    ? {
        lamports: a.lamports,
        owner: a.owner,
        data: Buffer.from(a.data[0], "base64"),
      }
    : null;
}

function slotted(result: unknown, what: string): Slotted<unknown> {
  const r = result as Slotted<unknown> | null;
  if (typeof r?.context?.slot !== "number" || r.value === undefined) {
    throw new Error(`${what} came back malformed`);
  }
  return r;
}

async function readAndSimulate(
  endpoint: string,
  tx: VersionedTransaction,
  writable: PublicKey[],
  addresses: string[]
): Promise<{ pre: Slotted<(WireAccount | null)[]>; sim: Slotted<SimulationValue> }> {
  const res = await fetch(endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify([
      {
        jsonrpc: "2.0",
        id: 1,
        method: "getMultipleAccounts",
        params: [
          writable.map((k) => k.toBase58()),
          { commitment: "confirmed", encoding: "base64" },
        ],
      },
      {
        jsonrpc: "2.0",
        id: 2,
        method: "simulateTransaction",
        params: [
          Buffer.from(tx.serialize()).toString("base64"),
          {
            encoding: "base64",
            commitment: "confirmed",
            sigVerify: false,
            accounts: { encoding: "base64", addresses },
          },
        ],
      },
    ]),
  });
  if (!res.ok) throw new Error(`batch refused with HTTP ${res.status}`);
  const body: unknown = await res.json();
  if (!Array.isArray(body) || body.length !== 2) {
    throw new Error("batch refused: not one answer per call");
  }
  const answer = (id: number) => {
    const item = body.find((x) => x?.id === id);
    if (!item || item.error || item.result === undefined) {
      throw new Error(`batch call ${id} failed`);
    }
    return item.result;
  };
  return {
    pre: slotted(answer(1), "pre state") as Slotted<(WireAccount | null)[]>,
    sim: slotted(answer(2), "simulation") as Slotted<SimulationValue>,
  };
}

async function simulateAndJudge(
  connection: Connection,
  quote: Quote,
  owner: PublicKey,
  amountRaw: bigint
): Promise<PreflightVerdict> {
  const lookupTables = await resolveLookupTables(
    connection,
    quote.lookupTables ?? []
  );
  const { blockhash } = await connection.getLatestBlockhash("confirmed");
  const tx = compile(quote, owner, blockhash, lookupTables);
  const writable = writableSet(tx, lookupTables);
  const addresses = writable.map((k) => k.toBase58());

  // Judged only when both answers share a slot, so no other transaction can
  // sit between the two states.
  for (let attempt = 0; attempt < MAX_SLOT_ATTEMPTS; attempt++) {
    const { pre, sim } = await readAndSimulate(
      connection.rpcEndpoint,
      tx,
      writable,
      addresses
    );
    if (sim.context.slot !== pre.context.slot) continue;

    const postAccounts = sim.value.accounts;
    if (
      !postAccounts ||
      postAccounts.length !== addresses.length ||
      pre.value.length !== addresses.length
    ) {
      return {
        ok: false,
        reason: "simulation returned no post state",
        retry: false,
      };
    }
    const preMap = new Map<string, AccountState | null>();
    const postMap = new Map<string, AccountState | null>();
    addresses.forEach((address, i) => {
      preMap.set(address, toState(pre.value[i] ?? null));
      postMap.set(address, toState(postAccounts[i] ?? null));
    });
    const verdict = judge(
      preMap,
      { err: sim.value.err, accounts: postMap },
      owner.toBase58(),
      amountRaw
    );
    return verdict.ok
      ? { ...verdict, transaction: tx }
      : { ok: false, reason: verdict.reason, retry: false };
  }
  return {
    ok: false,
    reason: "simulation and pre state never shared a slot",
    retry: true,
  };
}

/**
 * Simulates the quoted deposit read-only and returns the transaction that was
 * simulated, so the caller signs those exact bytes. Every failure, including a
 * thrown RPC error or a timeout, is a refusal. Never signs or sends.
 */
export async function runBridgeOutPreflight(input: {
  connection: Connection;
  owner: PublicKey;
  quote: BridgeOutQuoteResponse;
  amountRaw: bigint;
}): Promise<PreflightVerdict> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<PreflightVerdict>((resolve) => {
    timer = setTimeout(
      () =>
        resolve({ ok: false, reason: "preflight timed out", retry: false }),
      PREFLIGHT_TIMEOUT_MS
    );
  });
  try {
    return await Promise.race([
      simulateAndJudge(
        input.connection,
        input.quote,
        input.owner,
        input.amountRaw
      ),
      timeout,
    ]);
  } catch (e) {
    return {
      ok: false,
      reason: `preflight failed: ${e instanceof Error ? e.message : String(e)}`,
      retry: false,
    };
  } finally {
    clearTimeout(timer);
  }
}
