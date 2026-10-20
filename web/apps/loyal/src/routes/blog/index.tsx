import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/blog/page";

export const Route = createFileRoute("/blog/")({
  component: Source,
});
