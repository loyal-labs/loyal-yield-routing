import { createFileRoute, redirect } from "@tanstack/react-router";

import { referralsEnabled } from "@/features/referrals/enabled";
import { storeWorkspacePage } from "@/components/wallet-workspace/facelift/shell";

export const Route = createFileRoute("/app/invite/")({
  beforeLoad: () => {
    if (referralsEnabled()) storeWorkspacePage("invite");
    throw redirect({ to: "/app", replace: true });
  },
});
