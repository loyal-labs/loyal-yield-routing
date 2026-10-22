import "@/app/globals.css";

import { createRootRoute, HeadContent, Outlet } from "@tanstack/react-router";

export const Route = createRootRoute({
  component: () => (
    <>
      <HeadContent />
      <Outlet />
    </>
  ),
  notFoundComponent: () => <p className="p-6 text-sm">Not found.</p>,
  errorComponent: ({ error }) => (
    <p className="p-6 text-sm text-destructive">{error instanceof Error ? error.message : String(error)}</p>
  ),
});
