import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/app/invite/page";

export const Route = createFileRoute("/app/invite/")({
  component: Source,
});
