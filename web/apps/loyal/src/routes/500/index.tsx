import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/500/page";

import { headFromMetadata } from "../../next-compat/metadata";

export const Route = createFileRoute("/500/")({
  component: Source,
  head: () => headFromMetadata({ robots: { index: false } }),
});
