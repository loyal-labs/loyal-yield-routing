import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/landing/page";

export const Route = createFileRoute("/landing/")({
  component: Source,
});
