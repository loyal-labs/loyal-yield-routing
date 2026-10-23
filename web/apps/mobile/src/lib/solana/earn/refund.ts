import {
  hydratePreparedOperation,
  type WirePreparedLoyalSmartAccountsOperation,
} from "@loyal-labs/smart-account-vaults";
import { getConnection } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";

import { prepareEarnRefund, type EarnRefundPrepareRequest } from "./earn-api";
import { ensureEarnSession } from "./earn-session";
import { signAndSendPreparedOperations } from "./send-prepared";

export type EarnRefundResult = {
  signature: string;
};

export async function executeEarnRefund(args: {
  signer: Signer;
  request: EarnRefundPrepareRequest;
}): Promise<EarnRefundResult> {
  await ensureEarnSession(args.signer);
  const response = await prepareEarnRefund({
    request: args.request,
    walletAddress: args.signer.publicKey.toBase58(),
  });
  const wire = response.preparedVaultRefund;
  if (!wire) {
    throw new Error("Refund preparation returned no transaction.");
  }

  const [sent] = await signAndSendPreparedOperations({
    connection: getConnection(),
    signer: args.signer,
    operations: [hydratePreparedOperation(
        wire.prepared as WirePreparedLoyalSmartAccountsOperation,
      )],
  });
  return { signature: sent.signature };
}
