import "./polyfills";

import { createRouter, RouterProvider } from "@tanstack/react-router";
import { GeistMono } from "geist/font/mono";
import { GeistSans } from "geist/font/sans";
import { createRoot } from "react-dom/client";

import { registerRouter } from "./next-compat/navigation";
import { routeTree } from "./routeTree.gen";

document.body.classList.add(GeistSans.variable, GeistMono.variable);

const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  scrollRestoration: true,
  defaultPendingMs: 0,
  defaultPendingMinMs: 0,
});
registerRouter(router);

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(
  <RouterProvider router={router} />
);
