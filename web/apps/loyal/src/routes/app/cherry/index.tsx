import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/app/cherry/page";

export const Route = createFileRoute("/app/cherry/")({
  component: Source,
});
