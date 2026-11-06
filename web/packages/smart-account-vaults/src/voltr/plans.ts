import {
  createAssociatedTokenAccountIdempotentInstruction,
  createAssociatedTokenAccountInstruction,
  createCloseAccountInstruction,
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
  depositVaultInstruction,
  readVoltrPosition,
  requestWithdrawVaultInstruction,
  VOLTR_ASSET_DECIMALS,
  VOLTR_ASSET_MINT,
  VOLTR_LP_MINT,
  VOLTR_PROGRAM_ID,
  VOLTR_VAULT,
  voltrClaimBlockReason,
  type VoltrClaimBlockReason,
  voltrUserAccounts,
  unprotectedWithdrawVaultInstruction,
} from "./program";

/** `outer` is wallet-signed and runs first; `vault` runs as the index-2
 * smart-account vault inside one executeTransactionSyncV2. */
export type VoltrPlan = {
  outer: TransactionInstruction[];
  vault: TransactionInstruction[];
  claimProtection?: {
    consent: VoltrWithdrawalConsent;
    receiptBytes: Uint8Array;
    connection: Connection;
    expiresAt: bigint;
    maxFeeLamports: bigint;
    settings: string;
  };
  lifecycleFence?: boolean;
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
  c: VoltrWithdrawalConsent,
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
    ]),
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
          : "Review and approve a minimum USDC amount before claiming.",
    );
    this.name = "VoltrWithdrawalConsentError";
    this.code = code;
  }
}

export function voltrDepositPlan(
  { authority, owner }: PlanContext,
  amountRaw: bigint,
): VoltrPlan {
  const user = voltrUserAccounts(authority);
  return {
    outer: [
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        user.assetAta,
        authority,
        VOLTR_ASSET_MINT,
      ),
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        user.lpAta,
        authority,
        VOLTR_LP_MINT,
      ),
      createTransferCheckedInstruction(
        getAssociatedTokenAddressSync(VOLTR_ASSET_MINT, owner),
        VOLTR_ASSET_MINT,
        user.assetAta,
        owner,
        amountRaw,
        VOLTR_ASSET_DECIMALS,
      ),
    ],
    vault: [depositVaultInstruction(authority, amountRaw)],
  };
}

export async function voltrRequestWithdrawalPlan(
  connection: Connection,
  { authority, owner }: PlanContext,
  amountRaw: bigint | "max",
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
              VOLTR_LP_MINT,
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
        : "Vault liquidity could not be verified. Check status before claiming.",
    );
    this.name = "VoltrClaimLiquidityError";
  }
}

export async function voltrClaimPlan(
  connection: Connection,
  { authority, owner }: PlanContext,
  consent?: VoltrWithdrawalConsent,
): Promise<VoltrPlan> {
  if (!consent) throw new VoltrWithdrawalConsentError("needs_consent");
  if (
    consent.owner !== owner.toBase58() ||
    consent.authority !== authority.toBase58() ||
    consent.destination !== owner.toBase58() ||
    consent.cluster !== "mainnet-beta" ||
    consent.program !== VOLTR_PROGRAM_ID.toBase58() ||
    consent.vault !== VOLTR_VAULT.toBase58()
  ) {
    throw new VoltrWithdrawalConsentError("needs_consent");
  }
  if (BigInt(consent.expiresAt) <= BigInt(Math.floor(Date.now() / 1000))) {
    throw new VoltrWithdrawalConsentError("expired");
  }
  const position = await readVoltrPosition(connection, authority);
  const pending = position.withdrawal;
  const blocked = voltrClaimBlockReason(position);
  if (!pending || blocked === "not_ready") {
    throw new Error("Earn MAX withdrawal is not claimable yet.");
  }
  if (blocked) throw new VoltrClaimLiquidityError(blocked);
  const floor = BigInt(consent.minClaimUsdcRaw);
  if (floor <= BigInt(0) || BigInt(consent.lpRaw) !== pending.escrowedLpRaw) {
    throw new VoltrWithdrawalConsentError("needs_consent");
  }
  if (pending.payoutRaw < floor)
    throw new VoltrWithdrawalConsentError("waiting_price");
  const walletAta = getAssociatedTokenAddressSync(VOLTR_ASSET_MINT, owner);
  const assetAta = voltrUserAccounts(authority).assetAta;
  const rent = await connection.getMinimumBalanceForRentExemption(165);
  return {
    claimProtection: {
      consent,
      receiptBytes: pending.receiptBytes,
      connection,
      expiresAt: BigInt(consent.expiresAt),
      maxFeeLamports: BigInt(consent.maxUserFeeLamports),
      settings: consent.settings,
    },
    outer: [
      createAssociatedTokenAccountIdempotentInstruction(
        owner,
        walletAta,
        owner,
        VOLTR_ASSET_MINT,
      ),
      ...(position.authorityLamports >= rent
        ? []
        : [
            SystemProgram.transfer({
              fromPubkey: owner,
              toPubkey: authority,
              lamports: rent - position.authorityLamports,
            }),
          ]),
    ],
    vault: [
      ...(position.assetAtaExists
        ? [
            ...(position.assetRaw > BigInt(0)
              ? [
                  createTransferCheckedInstruction(
                    assetAta,
                    VOLTR_ASSET_MINT,
                    walletAta,
                    authority,
                    position.assetRaw,
                    VOLTR_ASSET_DECIMALS,
                  ),
                ]
              : []),
            createCloseAccountInstruction(assetAta, authority, authority),
          ]
        : []),
      createAssociatedTokenAccountInstruction(
        authority,
        assetAta,
        authority,
        VOLTR_ASSET_MINT,
      ),
      unprotectedWithdrawVaultInstruction(authority),
      createTransferCheckedInstruction(
        voltrUserAccounts(authority).assetAta,
        VOLTR_ASSET_MINT,
        walletAta,
        authority,
        floor,
        VOLTR_ASSET_DECIMALS,
      ),
    ],
  };
}

/** Recover only existing user-owned USDC; never consumes a withdrawal receipt. */
export async function voltrRecoverClaimRemainderPlan(
  connection: Connection,
  { authority, owner }: PlanContext,
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
        VOLTR_ASSET_MINT,
      ),
    ],
    vault: [
      createTransferCheckedInstruction(
        voltrUserAccounts(authority).assetAta,
        VOLTR_ASSET_MINT,
        walletAta,
        authority,
        position.assetRaw,
        VOLTR_ASSET_DECIMALS,
      ),
    ],
  };
}
