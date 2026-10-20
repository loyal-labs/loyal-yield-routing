import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/app/settings/page";

export const Route = createFileRoute("/app/settings/")({
  component: Source,
});
