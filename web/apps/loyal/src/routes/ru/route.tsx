import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source from "@/app/ru/layout";

export const Route = createFileRoute("/ru")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
});
