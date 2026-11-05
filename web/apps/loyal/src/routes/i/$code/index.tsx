import { createFileRoute, redirect } from "@tanstack/react-router";

import {
  normalizeCode,
  storeLinkCode,
} from "@/features/referrals/pending-code";

export const Route = createFileRoute("/i/$code/")({
  beforeLoad: ({ params }) => {
    const code = normalizeCode(params.code);
    if (!code) throw redirect({ to: "/app", replace: true });
    storeLinkCode(code);
    throw redirect({ to: "/app", search: { ref: code }, replace: true });
  },
});
