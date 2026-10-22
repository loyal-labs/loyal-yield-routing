import {
  Keypair,
  PublicKey,
  Transaction,
  VersionedTransaction,
} from "@solana/web3.js";

export interface Signer {
  readonly publicKey: PublicKey;
  readonly kind: "local" | "seed-vault" | "mwa" | "deeplink" | "privy";
  signMessage(bytes: Uint8Array): Promise<Uint8Array>;
  signTransaction<T extends Transaction | VersionedTransaction>(
    tx: T,
  ): Promise<T>;
  signAllTransactions<T extends Transaction | VersionedTransaction>(
    txs: T[],
  ): Promise<T[]>;
}

type TweetNaclSign = {
  detached: (message: Uint8Array, secretKey: Uint8Array) => Uint8Array;
};

// Lazy-loaded so tweetnacl's Buffer access never runs at module top-level.
async function getTweetNaclSign(): Promise<TweetNaclSign> {
  const mod = (await import("tweetnacl")) as unknown as {
    sign?: TweetNaclSign;
    default?: { sign?: TweetNaclSign };
  };
  if (typeof mod.sign?.detached === "function") return mod.sign;
  if (typeof mod.default?.sign?.detached === "function") {
    return mod.default.sign;
  }
  throw new Error("tweetnacl sign.detached is unavailable");
}

export class LocalKeypairSigner implements Signer {
  readonly kind = "local" as const;

  constructor(readonly keypair: Keypair) {}

  get publicKey(): PublicKey {
    return this.keypair.publicKey;
  }

  async signMessage(bytes: Uint8Array): Promise<Uint8Array> {
    const sign = await getTweetNaclSign();
    return sign.detached(bytes, this.keypair.secretKey);
  }

  async signTransaction<T extends Transaction | VersionedTransaction>(
    tx: T,
  ): Promise<T> {
    if (tx instanceof VersionedTransaction) {
      tx.sign([this.keypair]);
    } else {
      (tx as Transaction).partialSign(this.keypair);
    }
    return tx;
  }

  async signAllTransactions<T extends Transaction | VersionedTransaction>(
    txs: T[],
  ): Promise<T[]> {
    for (const tx of txs) {
      await this.signTransaction(tx);
    }
    return txs;
  }

  getSecretKeyHex(): string {
    return Array.from(this.keypair.secretKey)
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
  }
}

type TweetNaclVerify = (
  message: Uint8Array,
  signature: Uint8Array,
  publicKey: Uint8Array,
) => boolean;

async function getTweetNaclVerify(): Promise<TweetNaclVerify> {
  const mod = (await import("tweetnacl")) as unknown as {
    sign?: { detached?: { verify?: TweetNaclVerify } };
    default?: { sign?: { detached?: { verify?: TweetNaclVerify } } };
  };
  const verify =
    mod.sign?.detached?.verify ?? mod.default?.sign?.detached?.verify;
  if (typeof verify !== "function") {
    throw new Error("tweetnacl sign.detached.verify is unavailable");
  }
  return verify;
}

/**
 * Assert an external wallet actually signed `tx` with `publicKey`. Wallet apps
 * (MWA or deeplink) may return a modified transaction, so after adopting the
 * returned message + signatures this check fails with a precise error instead
 * of an opaque RPC preflight failure. `index` is 0-based and only used for the
 * error copy.
 */
export async function assertOwnVersionedSignature(
  tx: VersionedTransaction,
  publicKey: PublicKey,
  index: number,
): Promise<void> {
  const address = publicKey.toBase58();
  const signerIndex = tx.message.staticAccountKeys.findIndex((key) =>
    key.equals(publicKey),
  );
  if (
    signerIndex < 0 ||
    signerIndex >= tx.message.header.numRequiredSignatures
  ) {
    throw new Error(
      `Wallet returned transaction ${index + 1} without ${address} as a signer.`,
    );
  }
  const verify = await getTweetNaclVerify();
  const valid = verify(
    tx.message.serialize(),
    tx.signatures[signerIndex],
    publicKey.toBytes(),
  );
  if (!valid) {
    throw new Error(
      `The wallet returned an invalid signature for ${address} on transaction ${index + 1}. It may have signed with a different account — reset your wallet in Settings and reconnect.`,
    );
  }
}

/**
 * Adopt a wallet-returned transaction onto the caller's object. The
 * wallet-returned bytes are the signed truth — an external wallet may return
 * a modified transaction (Lighthouse guards, priority fees), so grafting only
 * its signatures onto the original message would break signature verification.
 * Callers keep using the objects they passed in (e.g. serializing the original
 * after signTransaction), so adopt the wallet's message AND signatures onto
 * the inputs, then verify our signature locally to fail with a precise error
 * instead of an opaque RPC preflight failure.
 */
export async function adoptWalletReturnedTransaction(
  tx: Transaction | VersionedTransaction,
  source: Transaction | VersionedTransaction,
  publicKey: PublicKey,
  index: number,
): Promise<void> {
  if (tx instanceof VersionedTransaction) {
    const src = source as VersionedTransaction;
    tx.message = src.message;
    tx.signatures = src.signatures;
    await assertOwnVersionedSignature(tx, publicKey, index);
  } else {
    (tx as Transaction).signatures = (source as Transaction).signatures;
  }
}
