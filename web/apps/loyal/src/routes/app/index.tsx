import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/app/page";

export const Route = createFileRoute("/app/")({
  component: Source,
});
