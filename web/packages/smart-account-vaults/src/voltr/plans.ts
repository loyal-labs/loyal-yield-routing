import {
  createAssociatedTokenAccountIdempotentInstruction,
  createTransferCheckedInstruction,
  getAssociatedTokenAddressSync,
} from "@solana/spl-token";
import {
  type Connection,
  type PublicKey,
  SystemProgram,
  type TransactionInstruction,
} from "@solana/web3.js";

import {
  type EarnMaxDepositQuote,
  EarnMaxDepositQuoteError,
} from "./deposit-quote";
import {
  assertAccountBytes,
  assertAccountU64,
  assertClockSlot,
  assertClockUnixTimestamp,
  assertMintSupply,
  assertTokenAmount,
  IntegerOperator,
} from "./lighthouse";
import {
  depositVaultInstruction,
  readVoltrPosition,
  requestWithdrawVaultInstruction,
  VOLTR_ASSET_DECIMALS,
  VOLTR_ASSET_MINT,
  VOLTR_DEPOSIT_PRICING_RANGES,
  VOLTR_LP_MINT,
  VOLTR_PROGRAM_ID,
  VOLTR_VAULT,
  VOLTR_VAULT_TOTAL_VALUE_OFFSET,
  voltrClaimBlockReason,
  type VoltrClaimBlockReason,
  voltrUserAccounts,
  unprotectedWithdrawVaultInstruction,
} from "./program";

/** `outer` is wallet-signed and runs first; `vault` runs as the index-2
 * smart-account vault inside one executeTransactionSyncV2; `after` runs last. */
export type VoltrPlan = {
  outer: TransactionInstruction[];
  vault: TransactionInstruction[];
  after?: TransactionInstruction[];
  claimProtection?: {
    consent: VoltrWithdrawalConsent;
    receiptBytes: Uint8Array;
    connection: Connection;
    expiresAt: bigint;
    maxFeeLamports: bigint;
    settings: string;
  };
  lifecycleFence?: boolean;
  withdrawalReceiptBytes?: Uint8Array;
};

type PlanContext = { authority: PublicKey; owner: PublicKey };

// Receipt rent the vault PDA pays per request; claim refunds it. The program
// allocates 112 bytes (measured on mainnet), not the SDK's
// getRequestWithdrawVaultReceiptSize() of 106.
const RECEIPT_SPACE = 112;

export type VoltrWithdrawalConsent = {
  authority: string;
  cluster: string;
  destination: string;
  expiresAt: string;
  feeBasisHash: string;
  lpRaw: string;
  maxUserFeeLamports: string;
  minClaimUsdcRaw: string;
  nonce: string;
  owner: string;
  ownerSignature: string;
  program: string;
  referenceSlot: string;
  referenceUsdcRaw: string;
  requestInstruction: number;
  requestSignature: string;
  settings: string;
  vault: string;
  version: string;
  policySeed: string;
};

export function voltrWithdrawalConsentMessage(
  c: VoltrWithdrawalConsent
): Uint8Array {
  return new TextEncoder().encode(
    JSON.stringify([
      "loyal:earn-max:withdrawal-consent:v1",
      c.cluster,
      c.program,
      c.vault,
      c.settings,
      c.owner,
      c.authority,
      c.destination,
      c.requestSignature,
      String(c.requestInstruction),
      c.policySeed,
      c.lpRaw,
      c.referenceUsdcRaw,
      c.referenceSlot,
      c.feeBasisHash,
      c.minClaimUsdcRaw,
      c.maxUserFeeLamports,
      c.version,
      c.nonce,
      c.expiresAt,
    ])
  );
}

export class VoltrWithdrawalConsentError extends Error {
  readonly code: "needs_consent" | "waiting_price" | "expired";
  constructor(code: VoltrWithdrawalConsentError["code"]) {
    super(
      code === "waiting_price"
        ? "This withdrawal is below your approved minimum. Wait or approve a new minimum."
        : code === "expired"
        ? "Your withdrawal approval expired. Review and approve a new minimum."
        : "Review and approve a minimum USDC amount before claiming."
    );
    this.name = "VoltrWithdrawalConsentError";
    this.code = code;
  }
}

const MAX_U64 = (BigInt(1) << BigInt(64)) - BigInt(1);

function pinnedTotalValue(quote: EarnMaxDepositQuote): bigint {
  const pinned = new Map<number, number>();
  for (const pin of quote.vaultPins)
    pin.bytes.forEach((byte, i) => pinned.set(pin.offset + i, byte));
  for (const [start, end] of VOLTR_DEPOSIT_PRICING_RANGES)
    for (let at = start; at < end; at++)
      if (!pinned.has(at)) throw new EarnMaxDepositQuoteError("invalid_quote");
  let value = BigInt(0);
  for (let i = 7; i >= 0; i--)
    value =
      (value << BigInt(8)) |
      BigInt(pinned.get(VOLTR_VAULT_TOTAL_VALUE_OFFSET + i) ?? 0);
  return value;
}

/** Official deposit: Lighthouse asserts the certified pricing basis before the
 * deposit and the certified LP and supply bounds after it, in one transaction.
 * Direct Voltr deposits outside this client are not covered. */
export function voltrDepositPlan(
  { authority, owner }: PlanContext,
  quote: EarnMaxDepositQuote,
  nowSec = BigInt(Math.floor(Date.now() / 1000))
): VoltrPlan {
  const user = voltrUserAccounts(authority);
  const amount = quote.amountRaw;
  const totalValue = pinnedTotalValue(quote);
  if (
    !quote.vault.equals(VOLTR_VAULT) ||
    !quote.lpMint.equals(VOLTR_LP_MINT) ||
    !quote.authority.equals(authority) ||
    !quote.lpAta.equals(user.lpAta) ||
    amount <= BigInt(0) ||
    quote.lpMinRaw <= BigInt(0) ||
    quote.lpMinRaw > quote.lpMaxRaw ||
    quote.lpBaselineRaw + quote.lpMaxRaw > MAX_U64 ||
    quote.supplyMaxRaw < quote.lpSupplyRaw + quote.lpMinRaw ||
    totalValue + amount > MAX_U64 ||
    quote.notAfterSlot < quote.certifiedSlot
  )
    throw new EarnMaxDepositQuoteError("invalid_quote");
  if (quote.expiresAt <= nowSec)
    throw new EarnMaxDepositQuoteError("quote_expired");
  const { equal, greaterThanOrEqual, lessThanOrEqual } = IntegerOperator;
  return {
    outer: [
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        user.assetAta,
        authority,
        VOLTR_ASSET_MINT
      ),
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        user.lpAta,
        authority,
        VOLTR_LP_MINT
      ),
      createTransferCheckedInstruction(
        getAssociatedTokenAddressSync(VOLTR_ASSET_MINT, owner),
        VOLTR_ASSET_MINT,
        user.assetAta,
        owner,
        amount,
        VOLTR_ASSET_DECIMALS
      ),
      assertAccountBytes(VOLTR_VAULT, quote.vaultPins),
      assertMintSupply(VOLTR_LP_MINT, {
        operator: equal,
        value: quote.lpSupplyRaw,
      }),
      assertTokenAmount(user.lpAta, [
        { operator: equal, value: quote.lpBaselineRaw },
      ]),
      assertClockSlot({ operator: lessThanOrEqual, value: quote.notAfterSlot }),
      assertClockUnixTimestamp({
        operator: lessThanOrEqual,
        value: quote.expiresAt,
      }),
    ],
    vault: [depositVaultInstruction(authority, amount)],
    after: [
      assertTokenAmount(user.lpAta, [
        {
          operator: greaterThanOrEqual,
          value: quote.lpBaselineRaw + quote.lpMinRaw,
        },
        {
          operator: lessThanOrEqual,
          value: quote.lpBaselineRaw + quote.lpMaxRaw,
        },
      ]),
      assertMintSupply(VOLTR_LP_MINT, {
        operator: lessThanOrEqual,
        value: quote.supplyMaxRaw,
      }),
      assertAccountU64(VOLTR_VAULT, VOLTR_VAULT_TOTAL_VALUE_OFFSET, {
        operator: equal,
        value: totalValue + amount,
      }),
    ],
  };
}

export async function voltrRequestWithdrawalPlan(
  connection: Connection,
  { authority, owner }: PlanContext,
  amountRaw: bigint | "max"
): Promise<VoltrPlan> {
  const position = await readVoltrPosition(connection, authority);
  if (position.withdrawal) {
    throw new Error("A withdrawal is already in progress.");
  }
  const withdrawAll = amountRaw === "max";
  // isWithdrawAll still rejects a zero amount (InvalidAmount), so pass the LP.
  const lp = withdrawAll ? position.lpRaw : position.assetsToLp(amountRaw);
  const amountLpRaw = lp < position.lpRaw ? lp : position.lpRaw;
  if (amountLpRaw <= BigInt(0)) {
    throw new Error("Nothing to withdraw.");
  }
  // The vault PDA pays the receipt rent and must stay rent-exempt itself.
  const [receipt, floor] = await Promise.all([
    connection.getMinimumBalanceForRentExemption(RECEIPT_SPACE),
    connection.getMinimumBalanceForRentExemption(0),
  ]);
  const needed = receipt + floor;
  const user = voltrUserAccounts(authority);
  return {
    lifecycleFence: true,
    outer: [
      // Voltr requires the escrow LP account (owned by the receipt PDA) to
      // exist before the request; it persists across requests.
      ...(position.escrowLpAtaExists
        ? []
        : [
            createAssociatedTokenAccountIdempotentInstruction(
              owner,
              user.escrowLpAta,
              user.receipt,
              VOLTR_LP_MINT
            ),
          ]),
      ...(position.authorityLamports >= needed
        ? []
        : [
            SystemProgram.transfer({
              fromPubkey: owner,
              lamports: needed - position.authorityLamports,
              toPubkey: authority,
            }),
          ]),
    ],
    vault: [
      requestWithdrawVaultInstruction({
        amountLpRaw,
        authority,
        payer: authority,
        withdrawAll,
      }),
    ],
  };
}

export class VoltrClaimLiquidityError extends Error {
  constructor(reason: Exclude<VoltrClaimBlockReason, "not_ready">) {
    super(
      reason === "insufficient_liquidity"
        ? "The vault does not have enough available USDC to pay this withdrawal yet. Check again later."
        : "Vault liquidity could not be verified. Check status before claiming."
    );
    this.name = "VoltrClaimLiquidityError";
  }
}

const CLAIM_HELD_MESSAGES = {
  claim_waiting_nav_publication:
    "Earn MAX is waiting for the next vault price update. Your withdrawal stays pending. Try claiming again in a few minutes.",
} as const;

export type VoltrClaimHeldReason = keyof typeof CLAIM_HELD_MESSAGES;

/** The server held a signed claim before broadcast; nothing was sent. */
export class VoltrClaimHeldError extends Error {
  readonly code: VoltrClaimHeldReason;
  constructor(code: VoltrClaimHeldReason) {
    super(CLAIM_HELD_MESSAGES[code]);
    this.name = "VoltrClaimHeldError";
    this.code = code;
  }
}

export function voltrClaimHeld(code: unknown): VoltrClaimHeldError | null {
  return typeof code === "string" && code in CLAIM_HELD_MESSAGES
    ? new VoltrClaimHeldError(code as VoltrClaimHeldReason)
    : null;
}

export async function voltrClaimPlan(
  connection: Connection,
  { authority, owner }: PlanContext,
  consent?: VoltrWithdrawalConsent
): Promise<VoltrPlan> {
  if (
    consent &&
    (consent.owner !== owner.toBase58() ||
      consent.authority !== authority.toBase58() ||
      consent.destination !== owner.toBase58() ||
      consent.cluster !== "mainnet-beta" ||
      consent.program !== VOLTR_PROGRAM_ID.toBase58() ||
      consent.vault !== VOLTR_VAULT.toBase58())
  ) {
    throw new VoltrWithdrawalConsentError("needs_consent");
  }
  if (
    consent &&
    BigInt(consent.expiresAt) <= BigInt(Math.floor(Date.now() / 1000))
  ) {
    throw new VoltrWithdrawalConsentError("expired");
  }
  const position = await readVoltrPosition(connection, authority);
  const pending = position.withdrawal;
  const blocked = voltrClaimBlockReason(position);
  if (!pending || blocked === "not_ready") {
    throw new Error("Earn MAX withdrawal is not claimable yet.");
  }
  if (blocked) throw new VoltrClaimLiquidityError(blocked);
  // The wallet approves the current payout; one raw unit covers integer rounding.
  const floor = consent
    ? BigInt(consent.minClaimUsdcRaw)
    : pending.payoutRaw > BigInt(1)
    ? pending.payoutRaw - BigInt(1)
    : pending.payoutRaw;
  if (
    floor <= BigInt(0) ||
    (consent && BigInt(consent.lpRaw) !== pending.escrowedLpRaw)
  ) {
    throw new VoltrWithdrawalConsentError("needs_consent");
  }
  if (pending.payoutRaw < floor)
    throw new VoltrWithdrawalConsentError("waiting_price");
  const walletAta = getAssociatedTokenAddressSync(VOLTR_ASSET_MINT, owner);
  const assetAta = voltrUserAccounts(authority).assetAta;
  // USDC already in the authority ATA is the baseline: the payout transfer
  // moves baseline plus floor, so only new proceeds can meet the floor and a
  // later donation can only add to what the owner receives.
  const baseline = position.assetAtaExists ? position.assetRaw : BigInt(0);
  if (baseline + floor > MAX_U64)
    throw new VoltrWithdrawalConsentError("needs_consent");
  return {
    lifecycleFence: true,
    withdrawalReceiptBytes: pending.receiptBytes,
    claimProtection: consent
      ? {
          consent,
          receiptBytes: pending.receiptBytes,
          connection,
          expiresAt: BigInt(consent.expiresAt),
          maxFeeLamports: BigInt(consent.maxUserFeeLamports),
          settings: consent.settings,
        }
      : undefined,
    outer: [
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        walletAta,
        owner,
        VOLTR_ASSET_MINT
      ),
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        assetAta,
        authority,
        VOLTR_ASSET_MINT
      ),
    ],
    vault: [
      unprotectedWithdrawVaultInstruction(authority),
      createTransferCheckedInstruction(
        assetAta,
        VOLTR_ASSET_MINT,
        walletAta,
        authority,
        baseline + floor,
        VOLTR_ASSET_DECIMALS
      ),
    ],
  };
}

/** Recover only existing user-owned USDC; never consumes a withdrawal receipt. */
export async function voltrRecoverClaimRemainderPlan(
  connection: Connection,
  { authority, owner }: PlanContext
): Promise<VoltrPlan> {
  const position = await readVoltrPosition(connection, authority);
  if (position.assetRaw <= BigInt(0))
    throw new Error("No remaining USDC to recover.");
  const walletAta = getAssociatedTokenAddressSync(VOLTR_ASSET_MINT, owner);
  return {
    outer: [
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        walletAta,
        owner,
        VOLTR_ASSET_MINT
      ),
    ],
    vault: [
      createTransferCheckedInstruction(
        voltrUserAccounts(authority).assetAta,
        VOLTR_ASSET_MINT,
        walletAta,
        authority,
        position.assetRaw,
        VOLTR_ASSET_DECIMALS
      ),
    ],
  };
}
