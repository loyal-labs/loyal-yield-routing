import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source from "@/app/zh/layout";

export const Route = createFileRoute("/zh")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
});
