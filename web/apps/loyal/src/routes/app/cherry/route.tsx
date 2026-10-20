import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/app/cherry/layout";

import { headFromMetadata } from "../../../next-compat/metadata";

export const Route = createFileRoute("/app/cherry")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => headFromMetadata(source.metadata),
});
