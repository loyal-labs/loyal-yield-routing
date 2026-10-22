import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/app/layout";
import { WorkspaceSkeleton } from "@/components/wallet-workspace/facelift/workspace-skeleton";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/app")({
  pendingComponent: WorkspaceSkeleton,
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => pageHead(source.metadata),
});
