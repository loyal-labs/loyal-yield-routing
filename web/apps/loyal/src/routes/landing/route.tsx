import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/landing/layout";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/landing")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => headFromMetadata(source.metadata),
});
