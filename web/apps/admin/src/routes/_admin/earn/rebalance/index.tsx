import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/earn/rebalance/page";

export const Route = createFileRoute("/_admin/earn/rebalance/")({
  component: Source,
});
