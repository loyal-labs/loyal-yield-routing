import { createRouter, RouterProvider } from "@tanstack/react-router";
import { Geist, Geist_Mono } from "next/font/google";
import { createRoot } from "react-dom/client";

import { PagePending } from "@/components/layout/page-pending";
import { registerRouter } from "../../loyal/src/next-compat/navigation";
import { routeTree } from "./routeTree.gen";

document.body.classList.add(Geist().variable, Geist_Mono().variable);

const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  defaultPendingComponent: PagePending,
  defaultPendingMs: 200,
  defaultStaleTime: 30_000,
  scrollRestoration: true,
});
registerRouter(router);

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
