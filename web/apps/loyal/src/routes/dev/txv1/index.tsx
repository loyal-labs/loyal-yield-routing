import { createFileRoute } from "@tanstack/react-router";

import NotFound from "@/app/not-found";
import { TxV1WalletProbe } from "@/app/dev/txv1/txv1-wallet-probe";
import * as source from "@/app/dev/txv1/page";
import { getPublicEnv } from "@/lib/core/config/public";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/dev/txv1/")({
  component: () => {
    const env = getPublicEnv();
    if (env.appEnvironment === "prod") {
      return <NotFound />;
    }
    return <TxV1WalletProbe rpcEndpoint={env.solanaRpcEndpoint} />;
  },
  head: () => headFromMetadata(source.metadata),
});
