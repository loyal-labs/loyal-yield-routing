import { createRouter, RouterProvider } from "@tanstack/react-router";
import { GeistMono } from "geist/font/mono";
import { GeistSans } from "geist/font/sans";
import { createRoot } from "react-dom/client";

import { routeTree } from "./routeTree.gen";

// apps/web puts the font variables on <body>; portals (Privy, dialogs) render there too.
document.body.classList.add(GeistSans.variable, GeistMono.variable);

const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
