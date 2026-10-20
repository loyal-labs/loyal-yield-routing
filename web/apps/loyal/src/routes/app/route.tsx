import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/app/layout";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/app")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => headFromMetadata(source.metadata),
});
