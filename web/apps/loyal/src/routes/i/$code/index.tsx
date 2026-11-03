import { createFileRoute, redirect } from "@tanstack/react-router";

import { storeLinkCode } from "@/features/referrals/pending-code";

export const Route = createFileRoute("/i/$code/")({
  beforeLoad: ({ params }) => {
    storeLinkCode(params.code);
    throw redirect({ to: "/app", replace: true });
  },
});
