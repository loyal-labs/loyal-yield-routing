import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/500/page";

export const Route = createFileRoute("/500/")({
  component: Source,
});
