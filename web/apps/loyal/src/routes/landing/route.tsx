import { createFileRoute, Outlet } from "@tanstack/react-router";

import Source, * as source from "@/app/landing/layout";

import { pageHead } from "../../next-compat/metadata";

export const Route = createFileRoute("/landing")({
  component: () => (
    <Source>
      <Outlet />
    </Source>
  ),
  head: () => pageHead(source.metadata),
});
