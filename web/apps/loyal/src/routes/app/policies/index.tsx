import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/app/policies/page";

export const Route = createFileRoute("/app/policies/")({
  component: Source,
});
