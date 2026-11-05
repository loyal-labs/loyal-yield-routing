import "./polyfills";

import { RouterProvider } from "@tanstack/react-router";
import { GeistMono } from "geist/font/mono";
import { GeistSans } from "geist/font/sans";
import { createRoot, hydrateRoot } from "react-dom/client";

import { registerRouter } from "./next-compat/navigation";
import { createAppRouter } from "./router";

document.body.classList.add(GeistSans.variable, GeistMono.variable);

const router = createAppRouter();
registerRouter(router);

const root = document.getElementById("root")!;
const app = <RouterProvider router={router} />;

if (root.hasAttribute("data-prerendered")) {
  router.ssr = { manifest: undefined };
  void router.load().then(() => hydrateRoot(root, app));
} else {
  createRoot(root).render(app);
}
