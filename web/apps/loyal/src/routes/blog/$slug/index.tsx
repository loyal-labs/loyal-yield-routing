import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/blog/[slug]/page";

export const Route = createFileRoute("/blog/$slug/")({
  component: Source,
});
