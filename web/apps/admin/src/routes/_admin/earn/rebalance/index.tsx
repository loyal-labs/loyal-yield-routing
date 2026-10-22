import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/earn/rebalance/page";
import Loading from "@/app/(admin)/earn/rebalance/loading";

export const Route = createFileRoute("/_admin/earn/rebalance/")({
  component: Source,
  pendingComponent: Loading,
});
