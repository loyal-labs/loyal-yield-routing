import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/app/cherry/layout";

import { pageHead } from "../../../next-compat/metadata";

export const Route = createFileRoute("/app/cherry")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => pageHead(source.metadata),
});
