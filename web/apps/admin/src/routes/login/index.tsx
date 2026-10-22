import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/login/page";

export const Route = createFileRoute("/login/")({
  component: Source,
});
