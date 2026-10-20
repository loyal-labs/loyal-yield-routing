import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/500/page";

import { headFromMetadata } from "../../next-compat/metadata";

// The page exports no metadata, so Next serves the root layout's head and
// injects robots noindex into the /500 route itself.
export const Route = createFileRoute("/500/")({
  component: Source,
  head: () => headFromMetadata({ robots: { index: false } }),
});
