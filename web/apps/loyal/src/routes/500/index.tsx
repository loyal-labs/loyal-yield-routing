import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/500/page";

// The page exports no metadata, so Next serves the root layout's head and
// injects robots noindex into the /500 route itself.
export const Route = createFileRoute("/500/")({
  component: Source,
  head: () => ({ meta: [{ name: "robots", content: "noindex" }], links: [] }),
});
