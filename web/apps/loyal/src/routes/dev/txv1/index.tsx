import { createFileRoute } from "@tanstack/react-router";

import NotFound from "@/app/not-found";
import { TxV1WalletProbe } from "@/app/dev/txv1/txv1-wallet-probe";
import * as source from "@/app/dev/txv1/page";
import { getPublicEnv } from "@/lib/core/config/public";

import { headFromMetadata } from "../../../next-compat/metadata";

// apps/web src/app/dev/txv1/page.tsx is a server component: it resolves the
// public env, 404s on prod, and otherwise renders the wallet probe. Here the
// env is the NEXT_PUBLIC_* set Vite inlined, and a 404 is apps/web's
// not-found.tsx rendered in place (the root notFoundComponent renders outside
// PublicEnvProvider, which its Footer needs).
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
