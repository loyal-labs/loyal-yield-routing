import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source from "@/app/(admin)/layout";

export const Route = createFileRoute("/_admin")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
});
