import { createFileRoute, redirect } from "@tanstack/react-router";

import { storeWorkspacePage } from "@/components/wallet-workspace/facelift/shell";

export const Route = createFileRoute("/app/invite/")({
  beforeLoad: () => {
    storeWorkspacePage("invite");
    throw redirect({ to: "/app", replace: true });
  },
});
