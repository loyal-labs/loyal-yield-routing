import { getConnection } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";

import { prepareEarnRefund, type EarnRefundPrepareRequest } from "./earn-api";
import { signEarnAuth, withEarnAuth } from "./earn-auth";
import { signAndSendPreparedOperations } from "./send-prepared";
import { hydratePreparedOperation } from "./wire";

export type EarnRefundResult = {
  signature: string;
};

export async function executeEarnRefund(args: {
  signer: Signer;
  request: EarnRefundPrepareRequest;
}): Promise<EarnRefundResult> {
  const prepareAuth = await signEarnAuth(args.signer, "earn-refund-prepare");
  const response = await withEarnAuth(
    args.signer,
    prepareAuth,
    "earn-refund-prepare",
    (auth) => prepareEarnRefund({ auth, request: args.request }),
  );
  const wire =
    response.preparedVaultRefund ??
    response.preparedRefund ??
    response.preparedRecurringDelegationRefund;
  if (!wire) {
    throw new Error("Refund preparation returned no transaction.");
  }

  const [sent] = await signAndSendPreparedOperations({
    connection: getConnection(),
    signer: args.signer,
    operations: [hydratePreparedOperation(wire.prepared)],
  });
  return { signature: sent.signature };
}
